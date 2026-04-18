// Mosaic Climbing – UniFi Access ↔ Redpoint HQ Check-in Bridge
//
// A single-binary service that connects your G2 Pro reader + UA-Hub
// to Redpoint HQ. Members tap their NFC card and walk in.
//
// Build:  go build -o mosaic-bridge ./cmd/bridge
// Run:    ./mosaic-bridge
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/api"
	"github.com/mosaic-climbing/checkin-bridge/internal/auditlog"
	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/cache"
	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/config"
	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/recheck"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// Build-time ldflags inject these. See .github/workflows/{ci,release}.yml.
//
//	-ldflags "-X main.version=$TAG -X main.buildTime=$TS"
var (
	version   = "dev"
	buildTime = "unknown"
)

// bgMetricsAdapter bridges bg.Group's tiny Metrics interface to the
// metrics.Registry. It encodes the goroutine name as a Prometheus
// label (`bg_goroutines_running{name="x"}`) so each tracked task gets
// its own time series while sharing one TYPE declaration.
//
// We keep it local to cmd/bridge (rather than living in internal/bg)
// so the bg package stays free of any metrics-package dependency —
// bg can be reused or unit-tested without dragging in the registry.
type bgMetricsAdapter struct {
	reg *metrics.Registry
}

func (a bgMetricsAdapter) gauge(name string) *metrics.Gauge {
	// Quote the label value the way Prometheus expects so
	// PrometheusText() emits a syntactically valid line. The base
	// name is stripped by prometheusBaseName when the TYPE comment
	// is rendered, so all per-name series share one
	// `# TYPE bg_goroutines_running gauge` header.
	return a.reg.Gauge(fmt.Sprintf(`bg_goroutines_running{name=%q}`, name))
}

func (a bgMetricsAdapter) Inc(name string) { a.gauge(name).Inc() }
func (a bgMetricsAdapter) Dec(name string) { a.gauge(name).Dec() }

func main() {
	// ── CLI flags ────────────────────────────────────────────
	// -version exits before loading config so deploy/macbook/update.sh
	// can ask "what's installed?" without needing .env to be valid.
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()
	if showVersion {
		fmt.Printf("mosaic-bridge %s (built %s)\n", version, buildTime)
		return
	}

	// ── Load config ──────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// ── Logger ───────────────────────────────────────────────
	logLevel := slog.LevelInfo
	if cfg.Bridge.LogLevel == "debug" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	logger.Info("════════════════════════════════════════")
	logger.Info("  Mosaic Climbing – Check-in Bridge v2")
	logger.Info("════════════════════════════════════════")
	logger.Info("build info",
		"version", version,
		"buildTime", buildTime,
		"configHash", cfg.NonSecretHash(),
	)

	if cfg.Bridge.ShadowMode {
		logger.Warn("╔══════════════════════════════════════════════╗")
		logger.Warn("║  SHADOW MODE ACTIVE                          ║")
		logger.Warn("║  • No door unlocks will be sent to UniFi     ║")
		logger.Warn("║  • No check-ins will be recorded in Redpoint ║")
		logger.Warn("║  • No UniFi user status changes will be made ║")
		logger.Warn("║  All decisions are logged only.              ║")
		logger.Warn("╚══════════════════════════════════════════════╝")
	}

	logger.Info("config loaded",
		"unifiHost", cfg.UniFi.Host,
		"redpointUrl", cfg.Redpoint.APIURL,
		"facilityCode", cfg.Redpoint.FacilityCode,
		"gateId", cfg.Redpoint.GateID,
		"dataDir", cfg.Bridge.DataDir,
		"syncInterval", cfg.Sync.Interval,
	)

	// ── Root context with cancellation ───────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Data directory ───────────────────────────────────────
	if err := os.MkdirAll(cfg.Bridge.DataDir, 0o700); err != nil {
		logger.Error("cannot create data directory", "error", err)
		os.Exit(1)
	}
	_ = os.Chmod(cfg.Bridge.DataDir, 0o700)

	if cfg.Redpoint.GateID == "" {
		logger.Warn("REDPOINT_GATE_ID not set – check-ins won't record in Redpoint")
		logger.Warn("run: curl http://localhost:" + fmt.Sprintf("%d", cfg.Bridge.Port) + "/gates")
	}

	// ── Unified SQLite store (v2) ────────────────────────────
	db, err := store.Open(cfg.Bridge.DataDir, logger.With("component", "store"))
	if err != nil {
		logger.Error("store init failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// ── Audit logger ─────────────────────────────────────────
	auditLogger, err := auditlog.Open(cfg.Bridge.DataDir)
	if err != nil {
		logger.Error("audit log init failed", "error", err)
		os.Exit(1)
	}
	defer auditLogger.Close()

	// ── Metrics registry ─────────────────────────────────────
	met := metrics.New()

	// ── Clients ──────────────────────────────────────────────
	unifiClient := unifi.NewClient(
		cfg.UniFi.WSURL(),
		cfg.UniFi.BaseURL(),
		cfg.UniFi.APIToken,
		cfg.Bridge.UnlockDurationMs,
		cfg.UniFi.TLSFingerprint,
		logger.With("component", "unifi"),
	)

	redpointClient := redpoint.NewClient(
		cfg.Redpoint.GraphQLURL(),
		cfg.Redpoint.APIKey,
		cfg.Redpoint.FacilityCode,
		logger.With("component", "redpoint"),
	)
	// A5: wire the request-latency histogram + outcome counters. Done
	// here (not in NewClient) so the redpoint package keeps its
	// construction signature stable and remains usable in unit tests
	// without a metrics registry.
	redpointClient.SetMetrics(met)

	// ── Card mapper (JSON overrides file) ────────────────────
	cardMapper, err := cardmap.New(
		cfg.Bridge.DataDir,
		logger.With("component", "cardmap"),
	)
	if err != nil {
		logger.Error("card mapper init failed", "error", err)
		os.Exit(1)
	}

	// ── Syncers and ingester ─────────────────────────────────
	syncer := cache.NewSyncer(
		db,
		redpointClient,
		cache.SyncConfig{
			SyncInterval: cfg.Sync.Interval,
			PageSize:     cfg.Sync.PageSize,
		},
		logger.With("component", "syncer"),
	)

	ingester := ingest.NewIngester(
		unifiClient,
		redpointClient,
		db,
		logger.With("component", "ingest"),
	)

	statusSyncer := statusync.New(
		unifiClient,
		redpointClient,
		db,
		statusync.Config{
			SyncInterval:       cfg.Sync.Interval,
			RateLimitDelay:     200 * time.Millisecond,
			SyncTimeLocal:      cfg.Sync.TimeLocal,
			UnmatchedGraceDays: cfg.Bridge.UnmatchedGraceDays,
		},
		logger.With("component", "statusync"),
	)

	// ── Recheck service (denied-tap live recheck) ────────────
	// Extracted from statusync as part of A3. The recheck is a distinct
	// business rule from the daily-sync loop — it owns its own breaker
	// (guarding live Redpoint calls) and its own freshness config
	// (MaxStaleness). statusSyncer above only handles the daily cadence.
	//
	// Passing concrete client types here relies on structural satisfaction
	// of the narrow interfaces defined in internal/recheck (Store,
	// RedpointClient, UnifiClient) — no runtime coupling to those
	// interfaces outside the recheck package.
	rechecker := recheck.New(
		db,
		redpointClient,
		unifiClient,
		recheck.Config{
			MaxStaleness: cfg.Bridge.RecheckMaxStaleness,
			ShadowMode:   cfg.Bridge.ShadowMode,
			// BreakerThreshold / BreakerCooldown left at defaults (5, 60s)
			// to match pre-A3 behaviour. Promote to config fields if an
			// operator ever needs to tune these independently.
		},
		logger.With("component", "recheck"),
	)

	// ── Check-in handler ─────────────────────────────────────
	handler := checkin.NewHandler(
		unifiClient,
		redpointClient,
		cardMapper,
		db,
		cfg.Redpoint.GateID,
		logger.With("component", "checkin"),
	)
	handler.SetRechecker(rechecker)
	handler.SetShadowMode(cfg.Bridge.ShadowMode)
	handler.SetMetrics(met)
	statusSyncer.SetShadowMode(cfg.Bridge.ShadowMode)
	statusSyncer.SetMetrics(met)

	// (Reconnect-backfill OnReconnect registration moved to after
	// bgGroup is constructed — see "WebSocket reconnect backfill"
	// below. The backfill needs to be supervised by bg.Group so
	// shutdowns drain it cleanly and the per-name gauge can see it.)

	// ── Session manager ──────────────────────────────────────
	// The HMAC signing key is persisted to <DataDir>/session.key (mode 0600,
	// atomic write on first boot, reused afterward). Without persistence,
	// every restart invalidated all staff sessions — S4 in the review.
	sessionKeyPath := filepath.Join(cfg.Bridge.DataDir, "session.key")
	sessionMgr, err := api.NewSessionManagerWithKeyFile(cfg.Bridge.StaffPassword, sessionKeyPath)
	if err != nil {
		logger.Error("session manager init failed", "error", err, "keyPath", sessionKeyPath)
		os.Exit(1)
	}
	// Start the login-tracker janitor so stale per-IP rate-limit entries are
	// swept on a schedule. Without this, the tracker map grows unbounded as
	// unique source IPs probe the login endpoint (S2 in docs/architecture-review.md).
	sessionMgr.StartJanitor(ctx)
	// Enable secure cookies if HTTPS mode is enabled (S7 in docs/architecture-review.md).
	sessionMgr.SetSecureCookies(cfg.Bridge.HTTPS)

	// ── HTMX UI handler ─────────────────────────────────────
	uiHandler, err := ui.New()
	if err != nil {
		logger.Error("UI init failed", "error", err)
		os.Exit(1)
	}

	// ── IP allowlist ─────────────────────────────────────────
	allowedNets, err := api.ParseAllowedNetworks(cfg.Bridge.AllowedNetworks)
	if err != nil {
		logger.Error("invalid ALLOWED_NETWORKS", "error", err)
		os.Exit(1)
	}
	if len(allowedNets) > 0 {
		var cidrStrs []string
		for _, n := range allowedNets {
			cidrStrs = append(cidrStrs, n.String())
		}
		logger.Info("IP allowlist enabled", "networks", cidrStrs)
	}

	// Surface-area warning: the public data plane is reachable off-host
	// AND there is no CIDR allowlist. The staff UI is still password-
	// protected and admin endpoints still require ADMIN_API_KEY, but any
	// host on the routable network can probe /ui/login and /health. On
	// the UDM Pro's nspawn container in particular, "" or "0.0.0.0"
	// means the whole LAN. Operators who intentionally expose the
	// dashboard should set ALLOWED_NETWORKS to the staff subnet; loud
	// log-at-boot is cheaper than a quiet misconfiguration.
	if cfg.Bridge.BindAddr != "127.0.0.1" && cfg.Bridge.BindAddr != "localhost" && len(allowedNets) == 0 {
		logger.Warn("public listener reachable off-host with no IP allowlist",
			"bind_addr", cfg.Bridge.BindAddr,
			"hint", "set ALLOWED_NETWORKS to the staff subnet, or BIND_ADDR=127.0.0.1")
	}

	// ── Trusted reverse-proxy CIDRs (S1) ─────────────────────
	// Empty list is the default and correct stance when nothing proxies
	// the bridge. When populated, X-Forwarded-For / X-Real-IP headers
	// are honoured only for requests whose peer IP falls inside one of
	// these networks. Without this gate, any attacker reachable on the
	// listening socket could forge an allowlisted client identity and
	// bypass both the IP allowlist and the per-IP login rate-limit.
	trustedProxies, err := api.ParseAllowedNetworks(cfg.Bridge.TrustedProxies)
	if err != nil {
		logger.Error("invalid TRUSTED_PROXIES", "error", err)
		os.Exit(1)
	}
	if len(trustedProxies) > 0 {
		var cidrStrs []string
		for _, n := range trustedProxies {
			cidrStrs = append(cidrStrs, n.String())
		}
		logger.Info("trusted reverse-proxy CIDRs configured", "networks", cidrStrs)
	} else {
		logger.Info("no trusted proxies configured; X-Forwarded-For / X-Real-IP will be ignored")
	}

	// ── HTTPS-aware security mode (S7) ───────────────────────
	if cfg.Bridge.HTTPS {
		logger.Info("HTTPS-aware mode enabled", "cookies_secure", true, "hsts", true)
	}

	// ── Background group for supervised goroutines ───────────
	// Centralizes long-running tasks (directory sync, cache sync, status sync,
	// reconnect backfill) under a single context and shutdown gate.
	// S6 in docs/architecture-review.md.
	//
	// A2: pass a Metrics adapter so each Go(name, …) call publishes a
	// per-name gauge bg_goroutines_running{name="…"}. The metrics
	// package's prometheusBaseName strips the {…} suffix so the TYPE
	// line is emitted once per base name even though the time series
	// are split per goroutine name. The adapter lives here (not in the
	// bg package) so internal/bg keeps no hard dependency on the
	// metrics registry — bg only knows about a tiny Inc/Dec interface.
	bgGroup := bg.NewWithMetrics(
		ctx,
		logger.With("component", "bg"),
		bgMetricsAdapter{reg: met},
	)

	// ── WebSocket reconnect backfill ─────────────────────────
	// Optional: backfill missed tap events on WebSocket reconnect so the
	// audit trail survives brief UA-Hub outages. The door obviously can't
	// unlock retroactively, but the checkins table, shadow-decisions panel,
	// and Redpoint records stay complete. Gated behind a config flag
	// because replaying during a partial-recovery window could double-
	// record Redpoint check-ins.
	//
	// A2: prior to A2 the backfill ran inside an anonymous `go cb(...)`
	// fired from internal/unifi/client.go — outside any supervised
	// context, invisible to the gauge, and not drained on shutdown. The
	// callback is now invoked synchronously by the unifi client (see
	// OnReconnect contract); we hand the actual REST work to bg.Group
	// so it shows up as bg_goroutines_running{name="reconnect-backfill"}
	// and Shutdown waits for it before exit.
	if cfg.Bridge.BackfillOnReconnect {
		logger.Info("WebSocket reconnect backfill enabled")
		unifiClient.OnReconnect(func(lastEventAt time.Time) {
			// If we never saw an event before the outage, conservatively
			// don't backfill — we'd flood the handler with everything
			// since boot. An operator can still hand-replay from the
			// REST API if needed.
			if lastEventAt.IsZero() {
				logger.Warn("reconnect backfill skipped: no prior event timestamp")
				return
			}
			// Give the outage a small overlap window so we don't miss
			// an event that landed right at reconnection; dedup on the
			// consumer side handles re-seeing the boundary event.
			since := lastEventAt.Add(-5 * time.Second)

			// Dispatch to bg.Group so the read loop is not stalled (the
			// callback contract requires non-blocking) and so the work
			// is drained on shutdown. bgCtx tracks the group's context
			// — if the bridge starts shutting down mid-backfill we
			// abandon the rest cleanly rather than racing past the
			// deadline.
			bgGroup.Go("reconnect-backfill", func(bgCtx context.Context) error {
				backfillCtx, cancelBackfill := context.WithTimeout(bgCtx, 60*time.Second)
				defer cancelBackfill()
				events, err := unifiClient.FetchAccessLogsSince(backfillCtx, since)
				if err != nil {
					logger.Error("reconnect backfill fetch failed",
						"since", since.UTC().Format(time.RFC3339),
						"error", err,
					)
					return nil // already logged; don't double-log via bg.Warn
				}
				logger.Info("reconnect backfill: replaying missed events",
					"since", since.UTC().Format(time.RFC3339),
					"count", len(events),
				)
				for _, evt := range events {
					if bgCtx.Err() != nil {
						logger.Warn("reconnect backfill interrupted by shutdown",
							"replayed", len(events)-1, // best-effort count
						)
						return nil
					}
					// Each replayed event gets its own 15s context, mirroring
					// the live OnEvent path in HandleEvent's Start() wrapper.
					// Parented on bgCtx so a shutdown also drops in-flight
					// HandleEvent calls.
					eventCtx, cancel := context.WithTimeout(bgCtx, 15*time.Second)
					handler.HandleEvent(eventCtx, evt)
					cancel()
				}
				logger.Info("reconnect backfill complete", "replayed", len(events))
				return nil
			})
		})
	}

	// ── API server ───────────────────────────────────────────
	apiServer := api.NewServer(
		handler,
		unifiClient,
		redpointClient,
		cardMapper,
		syncer,
		statusSyncer,
		ingester,
		sessionMgr,
		auditLogger,
		cfg.Redpoint.GateID,
		logger.With("component", "api"),
		db,
		uiHandler,
		met,
		trustedProxies,
		bgGroup,
		cfg.Bridge.EnableTestHooks,
		cfg.Bridge.AllowNewMembers,
		cfg.Bridge.DefaultAccessPolicyIDs,
	)

	// Build HTTP handler chain
	var httpHandler http.Handler = apiServer
	httpHandler = api.RecoveryMiddleware(logger.With("component", "api"), httpHandler)
	httpHandler = api.SecurityMiddleware(api.SecurityConfig{
		AdminAPIKey:     cfg.Bridge.AdminAPIKey,
		Sessions:        sessionMgr,
		AllowedNetworks: allowedNets,
		TrustedProxies:  trustedProxies,
		Logger:          logger.With("component", "security"),
		HTTPS:           cfg.Bridge.HTTPS,
	}, httpHandler)
	httpHandler = api.RequestLogger(logger.With("component", "api"), trustedProxies, httpHandler)

	// Build control-plane handler chain. Same middleware order as the
	// public chain (RequestLogger → Security → Recovery → app), but the
	// security middleware is ControlSecurityMiddleware: admin Bearer key
	// only, no session / CSRF / /ui fall-through. Binds to ControlBindAddr
	// (default 127.0.0.1) on ControlPort. See A1 in
	// docs/architecture-review.md.
	var controlHandler http.Handler = apiServer.ControlHandler()
	controlHandler = api.RecoveryMiddleware(logger.With("component", "api-control"), controlHandler)
	controlHandler = api.ControlSecurityMiddleware(api.ControlSecurityConfig{
		AdminAPIKey:     cfg.Bridge.AdminAPIKey,
		AllowedNetworks: allowedNets,
		TrustedProxies:  trustedProxies,
		Logger:          logger.With("component", "security-control"),
		HTTPS:           cfg.Bridge.HTTPS,
	}, controlHandler)
	controlHandler = api.RequestLogger(logger.With("component", "api-control"), trustedProxies, controlHandler)

	// Add metrics endpoint (TODO: wire in uiHandler when UI routes are ready)
	_ = uiHandler // TODO: register ui.Handler routes on mux
	mux := http.NewServeMux()
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Snapshot WebSocket client state into gauges on each scrape so
		// Prometheus sees up-to-date values. Using gauges (not counters) so
		// the source of truth stays in the unifi.Client — we're not trying
		// to maintain a parallel counter that could drift.
		if unifiClient != nil {
			connected := 0.0
			if unifiClient.Connected() {
				connected = 1.0
			}
			met.Gauge("ws_connected").Set(connected)
			met.Gauge("ws_reconnect_count").SetInt(unifiClient.ReconnectCount())
			met.Gauge("ws_messages_received").SetInt(unifiClient.MessagesReceived())
			met.Gauge("ws_events_processed").SetInt(unifiClient.EventsProcessed())
		}
		w.Header().Set("Content-Type", "text/plain; version=0.04")
		w.Write([]byte(met.PrometheusText()))
	}))
	mux.Handle("/", httpHandler)

	// ADMIN_API_KEY is required by config.validate(); logging here just
	// confirms the invariant held in main so grep-able startup output stays
	// consistent with historical logs.
	logger.Info("admin API authentication enabled")

	// ── Start services ───────────────────────────────────────
	logger.Info("starting mosaic checkin bridge v2",
		"port", cfg.Bridge.Port,
		"dataDir", cfg.Bridge.DataDir,
		"gateID", cfg.Redpoint.GateID,
	)

	// Connect to UniFi WebSocket
	handler.Start()
	go unifiClient.Connect(ctx)

	// Start background syncers via supervised group
	bgGroup.Go("cache-syncer", syncer.Run)
	bgGroup.Go("statusync", statusSyncer.Run)

	// ── HTTP server with graceful shutdown ────────────────────
	// Default bind is 127.0.0.1 (loopback-only). Operators who need LAN
	// reachability must set BIND_ADDR explicitly AND set ALLOWED_NETWORKS
	// to the staff subnet; the startup warning above fires if only one is
	// set. On the UDM Pro the nspawn container shares host networking, so
	// 127.0.0.1 is strongly recommended unless ALLOWED_NETWORKS is set.
	//
	// WriteTimeout is intentionally tight (30s) for the public data plane:
	// every route here — /ui/*, /health, /checkins, /directory/search —
	// responds in well under a second, so anything slower is either a
	// slow-client attack or a bug. (The control plane below matches 30s
	// too; the longer-running sync endpoints complete well under a minute.)
	addr := fmt.Sprintf("%s:%d", cfg.Bridge.BindAddr, cfg.Bridge.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64KB
	}

	go func() {
		logger.Info("HTTP server listening", "addr", "http://"+addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
			cancel() // trigger shutdown
		}
	}()

	// ── Control-plane HTTP server ────────────────────────────
	// Bound to ControlBindAddr (default 127.0.0.1) on ControlPort so the
	// routes that cause physical-world side effects (POST /unlock,
	// devhooks /test-checkin) are reachable only from the bridge host
	// itself. Operators who need remote access should SSH-tunnel or front
	// with a reverse proxy on the same host. Timeouts match the public
	// plane's short-timeout routes (both control routes complete in well
	// under a minute).
	controlAddr := fmt.Sprintf("%s:%d", cfg.Bridge.ControlBindAddr, cfg.Bridge.ControlPort)
	controlSrv := &http.Server{
		Addr:              controlAddr,
		Handler:           controlHandler,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	go func() {
		logger.Info("control-plane HTTP server listening", "addr", "http://"+controlAddr)
		if err := controlSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("control-plane HTTP server error", "error", err)
			cancel()
		}
	}()

	// ── Wait for shutdown signal ─────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
		logger.Info("context cancelled, shutting down")
	}

	// ── Graceful shutdown sequence ───────────────────────────
	logger.Info("starting graceful shutdown...")

	// 1. Stop accepting new HTTP requests on both the public and control
	//    planes. Shut control down first so a still-running sync can't
	//    start fresh UniFi writes after the public side has drained; the
	//    handler.Shutdown drain step below then catches any tail of
	//    in-flight work either server kicked off.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := controlSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("control-plane HTTP shutdown error", "error", err)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP shutdown error", "error", err)
	}

	// 2. Close WebSocket first so no new tap events start new async writes
	//    while we're trying to drain. Done before cancel() so the WS reader
	//    sees a clean close rather than a context-cancel error.
	unifiClient.Close()

	// 3. Drain in-flight async Redpoint writes. The door has already
	//    unlocked for these members; skipping the drain means their
	//    check-in never lands in HQ and they appear as a no-show today.
	//    Budget 8s out of the overall 10s shutdown window; if that's not
	//    enough, a slow GraphQL endpoint is the likely cause and we log
	//    rather than hang the process.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 8*time.Second)
	if err := handler.Shutdown(drainCtx); err != nil {
		logger.Warn("async Redpoint drain incomplete — some check-ins may not reach HQ",
			"error", err,
		)
	} else {
		logger.Info("async Redpoint drain complete")
	}
	drainCancel()

	// 4. Drain background goroutines (directory sync, cache sync, status sync).
	//    Budget 30s for these to finish gracefully; if a mid-sync shutdown
	//    occurs, syncs can finish their current page rather than being torn.
	//    S6 in docs/architecture-review.md.
	bgShutdownCtx, bgShutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := bgGroup.Shutdown(bgShutdownCtx); err != nil {
		logger.Warn("background group drain incomplete — some background work may not have finished",
			"error", err,
		)
	} else {
		logger.Info("background group drain complete")
	}
	bgShutdownCancel()

	// 5. Cancel all remaining context-based goroutines.
	//    At this point bgGroup has already shut down its goroutines, so this
	//    primarily affects any direct context-dependent tasks still running.
	cancel()

	// 6. Wait for the login-tracker janitor to return. Its ctx was the same
	//    root ctx cancelled above, so this is just the join — we give it a
	//    short deadline so a wedged goroutine can't hang shutdown.
	janitorCtx, janitorCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := sessionMgr.Shutdown(janitorCtx); err != nil {
		logger.Warn("session-manager janitor did not stop in time", "error", err)
	}
	janitorCancel()

	// 7. Resources closed by defers (auditLogger, db)
	logger.Info("shutdown complete")
}
