// Package watchdog is the bridge's self-monitoring loop.
//
// Every tick (default 5 minutes) it checks the two liveness signals that
// have historically failed silently — the daily status sync and the tap
// poller — and pushes an alert when either goes stale. It also pings an
// external dead-man endpoint (healthchecks.io or compatible): the one
// failure mode an in-process watchdog structurally cannot report is its
// own death, so the external service alerts when the pings *stop*.
//
// Alerting is stateful per condition: one push when a condition becomes
// active, a re-notify every RenotifyInterval while it persists, and a
// recovery push when it clears. Without the state machine a stale sync
// would page the operator every five minutes for a day — exactly the
// noise that trains people to mute the channel.
package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/notify"
)

// Config tunes the watchdog. Zero values pick the documented defaults.
type Config struct {
	// Interval between checks. Default 5m.
	Interval time.Duration
	// SyncStalenessMax is how old last_sync_completed_at may be before
	// alerting. Default 26h — one daily sync cycle plus slack, per C3 in
	// docs/architecture-review.md.
	SyncStalenessMax time.Duration
	// PollStalenessMax is how long the tap poller may go without a
	// *successful* poll before alerting. Successful means the UA-Hub
	// fetch returned without error — zero events is fine (quiet gym),
	// which is why this keys on poll success, not event arrival.
	// Default 10m.
	PollStalenessMax time.Duration
	// RenotifyInterval is how often a still-active condition re-alerts.
	// Default 6h.
	RenotifyInterval time.Duration
	// HeartbeatURL is the external dead-man ping URL (healthchecks.io or
	// compatible). Empty disables the heartbeat.
	HeartbeatURL string
}

// Deps are the watchdog's inputs. Metrics and Logger are required;
// Notifier may be nil (alert pushes become no-ops but the log lines and
// heartbeat still happen); LastPollSuccess may be nil (poller check
// skipped — used by tests and any future poller-less deployment).
type Deps struct {
	Metrics         *metrics.Registry
	Notifier        *notify.Notifier
	LastPollSuccess func() time.Time
	Logger          *slog.Logger

	// now is injectable for tests. Defaults to time.Now.
	now func() time.Time
	// heartbeatClient is injectable for tests. Defaults to a 10s-timeout
	// client.
	heartbeatClient *http.Client
}

// Watchdog runs the periodic self-checks. Construct with New, run with
// Run (bg.Group-compatible signature).
type Watchdog struct {
	cfg  Config
	deps Deps

	startedAt time.Time
	// conditions tracks per-key alert state across ticks.
	conditions map[string]*conditionState
}

type conditionState struct {
	active       bool
	lastNotified time.Time
}

// New constructs a Watchdog. Panics on nil Metrics or Logger — both are
// wiring bugs, not runtime conditions.
func New(cfg Config, deps Deps) *Watchdog {
	if deps.Metrics == nil {
		panic("watchdog: Deps.Metrics is required")
	}
	if deps.Logger == nil {
		panic("watchdog: Deps.Logger is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.SyncStalenessMax <= 0 {
		cfg.SyncStalenessMax = 26 * time.Hour
	}
	if cfg.PollStalenessMax <= 0 {
		cfg.PollStalenessMax = 10 * time.Minute
	}
	if cfg.RenotifyInterval <= 0 {
		cfg.RenotifyInterval = 6 * time.Hour
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.heartbeatClient == nil {
		deps.heartbeatClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Watchdog{
		cfg:        cfg,
		deps:       deps,
		startedAt:  deps.now(),
		conditions: make(map[string]*conditionState),
	}
}

// Run ticks until ctx is cancelled. bg.Group-compatible: always returns
// nil on clean cancellation.
func (w *Watchdog) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	// Immediate first check so a bridge that boots broken alerts within
	// one interval of the *condition's* grace window, not interval+grace.
	w.check(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.check(ctx)
		}
	}
}

// check runs one evaluation pass. Split from Run so tests can drive
// ticks deterministically.
func (w *Watchdog) check(ctx context.Context) {
	now := w.deps.now()

	// ── Status-sync staleness ────────────────────────────────
	// last_sync_completed_at is stamped (unix seconds) at the end of
	// every successful statusync.RunSync. Zero means "no successful run
	// since boot" — grace-period that against process start so a freshly
	// booted bridge isn't instantly stale, but a bridge that never
	// completes its first sync still alerts.
	syncAge, syncKnown := w.sinceUnixGauge("last_sync_completed_at", now)
	if !syncKnown {
		syncAge = now.Sub(w.startedAt)
	}
	w.condition(ctx, "sync-stale", syncAge > w.cfg.SyncStalenessMax, notify.Event{
		Title:    "Status sync is stale",
		Body:     fmt.Sprintf("No completed status sync for %s (limit %s). Membership changes are NOT propagating to the door.", syncAge.Round(time.Minute), w.cfg.SyncStalenessMax),
		Priority: notify.PriorityHigh,
		Tags:     []string{"warning", "arrows_counterclockwise"},
	})

	// ── Tap-poller staleness ─────────────────────────────────
	if w.deps.LastPollSuccess != nil {
		last := w.deps.LastPollSuccess()
		pollAge := now.Sub(w.startedAt)
		if !last.IsZero() {
			pollAge = now.Sub(last)
		}
		w.condition(ctx, "poller-stale", pollAge > w.cfg.PollStalenessMax, notify.Event{
			Title:    "Tap poller is stale",
			Body:     fmt.Sprintf("No successful UA-Hub poll for %s (limit %s). Taps are not being observed or recorded.", pollAge.Round(time.Second), w.cfg.PollStalenessMax),
			Priority: notify.PriorityHigh,
			Tags:     []string{"warning", "door"},
		})
	}

	// ── External dead-man heartbeat ──────────────────────────
	// Unconditional: the heartbeat means "process alive and watchdog
	// looping", nothing more. Specific degradations alert via ntfy
	// above; total death is what the external service catches.
	w.heartbeat(ctx)
}

// condition drives the per-key alert state machine.
func (w *Watchdog) condition(ctx context.Context, key string, active bool, ev notify.Event) {
	st := w.conditions[key]
	if st == nil {
		st = &conditionState{}
		w.conditions[key] = st
	}
	now := w.deps.now()

	switch {
	case active && (!st.active || now.Sub(st.lastNotified) >= w.cfg.RenotifyInterval):
		st.active = true
		st.lastNotified = now
		w.deps.Logger.Warn("watchdog condition active", "condition", key, "title", ev.Title)
		_ = w.deps.Notifier.Send(ctx, ev)
	case !active && st.active:
		st.active = false
		w.deps.Logger.Info("watchdog condition recovered", "condition", key)
		_ = w.deps.Notifier.Send(ctx, notify.Event{
			Title:    ev.Title + " — recovered",
			Body:     "Condition cleared.",
			Priority: notify.PriorityDefault,
			Tags:     []string{"white_check_mark"},
		})
	}
}

// sinceUnixGauge reads a unix-seconds gauge and returns its age. known
// is false when the gauge is zero/unset.
func (w *Watchdog) sinceUnixGauge(name string, now time.Time) (age time.Duration, known bool) {
	v := w.deps.Metrics.Gauge(name).Value()
	if v <= 0 {
		return 0, false
	}
	return now.Sub(time.Unix(int64(v), 0)), true
}

func (w *Watchdog) heartbeat(ctx context.Context) {
	if w.cfg.HeartbeatURL == "" {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, w.cfg.HeartbeatURL, nil)
	if err != nil {
		w.deps.Logger.Warn("heartbeat request build failed", "error", err)
		return
	}
	resp, err := w.deps.heartbeatClient.Do(req)
	if err != nil {
		// Warn, not Error: a missed ping is exactly what the external
		// service exists to notice; locally it's advisory.
		w.deps.Logger.Warn("dead-man heartbeat failed", "error", err)
		return
	}
	resp.Body.Close()
}
