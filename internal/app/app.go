// Package app assembles every subsystem the bridge needs and exposes a
// thin Run / Close surface for cmd/bridge.
//
// Pre-PR4 cmd/bridge/main.go was ~890 lines: parse config, build clients,
// build syncers, build api.Server, build HTTP servers, start goroutines,
// orchestrate graceful shutdown. The wiring drowned out main's actual
// job (parse flags, build, run, exit) and made the dependency graph
// hard to read at a glance.
//
// This package owns the wiring. main.go is now <100 lines: build an App
// from config, Run it, Close it on exit. The dependency order is
// explicit (BuildOptions / Build sequence), the lifecycle is one method
// (Run), and main.go is finally readable as the boring entrypoint it
// should always have been.
package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/api"
	"github.com/mosaic-climbing/checkin-bridge/internal/auditlog"
	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
	"log/slog"
)

// App is the fully-assembled bridge runtime. Build returns a value of
// this type; cmd/bridge calls Run and then Close. The fields are
// unexported because nothing outside this package should reach into a
// running app — Run owns the lifecycle.
type App struct {
	logger *slog.Logger

	store *store.Store
	audit *auditlog.Logger

	unifi   *unifi.Client
	handler *checkin.Handler

	apiServer  *api.Server
	httpSrv    *http.Server
	controlSrv *http.Server

	sessions *api.SessionManager

	bgGroup *bg.Group

	// schedulers is the set of bg.Group.Go calls Run will dispatch.
	// Captured at Build time so Run is a no-op-friendly "start
	// everything pre-decided". Pre-PR4 main.go interleaved scheduler
	// configuration with HTTP server setup; pulling them into a slice
	// of named closures here is what makes Run a tight 30 lines.
	schedulers []scheduledTask

	// onConnect is the OnReconnect callback registered against the
	// unifi client at build time. nil if BackfillOnReconnect was
	// disabled in config. Held on the App for symmetry with the rest
	// of the lifecycle, even though the registration happens once
	// inside Build.
	onConnect func(time.Time)
}

// scheduledTask is a named bg.Group.Go invocation. Build constructs
// these from the four Sync.Interval-bound schedulers (cache, statusync,
// ua-hub-mirror, directory-syncer, unifi-ingest) plus the unifi-poller.
type scheduledTask struct {
	name string
	fn   func(context.Context) error
}

// Run starts every long-running subsystem and blocks until ctx is
// cancelled or a SIGINT/SIGTERM lands. Returns nil on a clean shutdown,
// or an error if a startup step failed (HTTP listener bind, etc.).
//
// Caller responsibility:
//
//   - ctx must be cancellable; Run installs a signal handler that
//     cancels it on SIGINT/SIGTERM. The same ctx is propagated to
//     every supervised goroutine so a parent-driven shutdown also
//     drains the background work.
//   - Close MUST be deferred by the caller. Run only handles the
//     time-bounded shutdown sequence; persistent resources (db,
//     audit log) are released by Close.
func (a *App) Run(parentCtx context.Context) error {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// The check-in handler registers OnEvent on the unifi client.
	// Done before the WS connection so the very first event lands
	// against a wired handler — no race window between "connected"
	// and "ready to handle".
	a.handler.Start()
	go a.unifi.Connect(ctx)

	for _, t := range a.schedulers {
		a.bgGroup.Go(t.name, t.fn)
	}

	// HTTP listeners. Errors from ListenAndServe propagate via the
	// ctx-cancellation goroutines below — a bind failure is fatal,
	// but a clean Shutdown isn't.
	httpErr := make(chan error, 1)
	go func() {
		a.logger.Info("HTTP server listening", "addr", "http://"+a.httpSrv.Addr)
		err := a.httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("HTTP server error", "error", err)
			httpErr <- err
		}
		cancel()
	}()
	go func() {
		a.logger.Info("control-plane HTTP server listening", "addr", "http://"+a.controlSrv.Addr)
		err := a.controlSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("control-plane HTTP server error", "error", err)
			httpErr <- err
		}
		cancel()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		a.logger.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
		a.logger.Info("context cancelled, shutting down")
	}

	a.shutdown()

	// Surface the first listener error if there was one. We only read
	// non-blocking — by here the goroutines have either returned ok
	// or pushed a single error. A second cancel-driven exit on the
	// other listener is expected and not interesting.
	select {
	case err := <-httpErr:
		return err
	default:
		return nil
	}
}

// shutdown runs the time-bounded drain sequence. Each phase has its own
// budget so a wedged subsystem can't hold the others hostage past the
// outer deadline.
//
// Order matters here:
//
//  1. Stop accepting new requests on both HTTP listeners.
//  2. Close the WebSocket so no new tap events start new async writes.
//  3. Drain async Redpoint writes (the door already opened for these
//     members; losing the record means a paying member shows as
//     no-show in HQ).
//  4. Drain the supervised background group.
//  5. Stop the session-manager janitor.
func (a *App) shutdown() {
	// (1) HTTP servers.
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer httpCancel()
	if err := a.controlSrv.Shutdown(httpCtx); err != nil {
		a.logger.Error("control-plane HTTP shutdown error", "error", err)
	}
	if err := a.httpSrv.Shutdown(httpCtx); err != nil {
		a.logger.Error("HTTP shutdown error", "error", err)
	}

	// (2) WebSocket. Close before the WG drain so no new dispatches
	// appear after we've measured the depth.
	a.unifi.Close()

	// (3) Async Redpoint write drain. Budget 8s of the 10s window so
	// the bg group still has time below.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 8*time.Second)
	if err := a.handler.Shutdown(drainCtx); err != nil {
		a.logger.Warn("async Redpoint drain incomplete — some check-ins may not reach HQ",
			"error", err,
		)
	} else {
		a.logger.Info("async Redpoint drain complete")
	}
	drainCancel()

	// (4) Supervised goroutines. 30s for these to finish their current
	// page; a fresh-tick scheduler can be cancelled mid-page safely
	// (the next process boot resumes from the last persisted cursor).
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := a.bgGroup.Shutdown(bgCtx); err != nil {
		a.logger.Warn("background group drain incomplete — some background work may not have finished",
			"error", err,
		)
	} else {
		a.logger.Info("background group drain complete")
	}
	bgCancel()

	// (5) Session-manager janitor. Short window; a wedged janitor
	// shouldn't hold up the whole shutdown.
	janitorCtx, janitorCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := a.sessions.Shutdown(janitorCtx); err != nil {
		a.logger.Warn("session-manager janitor did not stop in time", "error", err)
	}
	janitorCancel()
}

// Close releases persistent resources. Call as a deferred cleanup from
// cmd/bridge after Run returns. Safe to call multiple times — each
// underlying Close is idempotent.
//
// Distinct from shutdown() which runs inside Run: shutdown drains
// active goroutines and HTTP listeners, Close releases files (db,
// audit log) that survive across the run.
func (a *App) Close() {
	if a.audit != nil {
		_ = a.audit.Close()
	}
	if a.store != nil {
		_ = a.store.Close()
	}
}
