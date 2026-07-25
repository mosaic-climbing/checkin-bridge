package watchdog

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/notify"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// notifierRecorder gives tests a real *notify.Notifier backed by an
// httptest server, plus access to what was sent. Using the real type
// (not an interface) keeps the watchdog's dependency surface honest.
type sentEvent struct {
	Title    string
	Priority string
}

func notifierRecorder(t *testing.T) (*notify.Notifier, *[]sentEvent) {
	t.Helper()
	var events []sentEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events = append(events, sentEvent{
			Title:    r.Header.Get("X-Title"),
			Priority: r.Header.Get("X-Priority"),
		})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return notify.New(srv.URL, "test-topic", "", discardLogger()), &events
}

// newTestWatchdog builds a watchdog with an injectable clock starting
// at a fixed instant.
func newTestWatchdog(t *testing.T, cfg Config, met *metrics.Registry, n *notify.Notifier, lastPoll func() time.Time) (*Watchdog, *time.Time) {
	t.Helper()
	current := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	w := New(cfg, Deps{
		Metrics:         met,
		Notifier:        n,
		LastPollSuccess: lastPoll,
		Logger:          discardLogger(),
		now:             func() time.Time { return current },
	})
	return w, &current
}

func TestCheck_FreshSyncNoAlert(t *testing.T) {
	met := metrics.New()
	n, events := notifierRecorder(t)
	w, now := newTestWatchdog(t, Config{}, met, n, nil)

	met.Gauge("last_sync_completed_at").SetInt(now.Add(-1 * time.Hour).Unix())
	w.check(context.Background())

	if len(*events) != 0 {
		t.Fatalf("events = %+v, want none for a 1h-old sync", *events)
	}
}

func TestCheck_StaleSyncAlertsOnceThenRenotifies(t *testing.T) {
	met := metrics.New()
	n, events := notifierRecorder(t)
	w, now := newTestWatchdog(t, Config{RenotifyInterval: 6 * time.Hour}, met, n, nil)

	met.Gauge("last_sync_completed_at").SetInt(now.Add(-30 * time.Hour).Unix())

	w.check(context.Background())
	if len(*events) != 1 {
		t.Fatalf("after first check: %d events, want 1", len(*events))
	}
	if (*events)[0].Title != "Status sync is stale" || (*events)[0].Priority != "high" {
		t.Errorf("event = %+v", (*events)[0])
	}

	// Five minutes later, still stale: no re-alert (inside renotify window).
	*now = now.Add(5 * time.Minute)
	w.check(context.Background())
	if len(*events) != 1 {
		t.Fatalf("after second check: %d events, want still 1 (renotify suppressed)", len(*events))
	}

	// Seven hours later: renotify fires.
	*now = now.Add(7 * time.Hour)
	w.check(context.Background())
	if len(*events) != 2 {
		t.Fatalf("after renotify window: %d events, want 2", len(*events))
	}
}

func TestCheck_RecoverySendsResolvedNotice(t *testing.T) {
	met := metrics.New()
	n, events := notifierRecorder(t)
	w, now := newTestWatchdog(t, Config{}, met, n, nil)

	met.Gauge("last_sync_completed_at").SetInt(now.Add(-30 * time.Hour).Unix())
	w.check(context.Background())
	if len(*events) != 1 {
		t.Fatalf("setup: %d events, want 1", len(*events))
	}

	// Sync completes; next check should send exactly one recovery notice.
	met.Gauge("last_sync_completed_at").SetInt(now.Unix())
	w.check(context.Background())
	if len(*events) != 2 {
		t.Fatalf("after recovery: %d events, want 2", len(*events))
	}
	if got := (*events)[1].Title; got != "Status sync is stale — recovered" {
		t.Errorf("recovery title = %q", got)
	}

	// Still fine on the following tick: no more notifications.
	w.check(context.Background())
	if len(*events) != 2 {
		t.Fatalf("steady state: %d events, want 2", len(*events))
	}
}

func TestCheck_NeverSyncedUsesBootGrace(t *testing.T) {
	met := metrics.New() // gauge never set
	n, events := notifierRecorder(t)
	w, now := newTestWatchdog(t, Config{}, met, n, nil)

	// Just booted: inside grace, no alert.
	w.check(context.Background())
	if len(*events) != 0 {
		t.Fatalf("just-booted: %d events, want 0", len(*events))
	}

	// 30h after boot with no sync ever: alert.
	*now = now.Add(30 * time.Hour)
	w.check(context.Background())
	if len(*events) != 1 {
		t.Fatalf("30h no-sync: %d events, want 1", len(*events))
	}
}

func TestCheck_PollerStaleness(t *testing.T) {
	met := metrics.New()
	n, events := notifierRecorder(t)

	var lastPoll atomic.Value
	lastPoll.Store(time.Time{})
	pollFn := func() time.Time { return lastPoll.Load().(time.Time) }

	w, now := newTestWatchdog(t, Config{}, met, n, pollFn)
	// Keep sync fresh so only the poller condition is in play.
	met.Gauge("last_sync_completed_at").SetInt(now.Unix())

	// Fresh poll: fine.
	lastPoll.Store(now.Add(-30 * time.Second))
	w.check(context.Background())
	if len(*events) != 0 {
		t.Fatalf("fresh poll: %d events, want 0", len(*events))
	}

	// 15 minutes without a successful poll: alert.
	lastPoll.Store(now.Add(-15 * time.Minute))
	w.check(context.Background())
	if len(*events) != 1 {
		t.Fatalf("stale poll: %d events, want 1", len(*events))
	}
	if (*events)[0].Title != "Tap poller is stale" {
		t.Errorf("title = %q", (*events)[0].Title)
	}
}

func TestCheck_NilNotifierStillSafe(t *testing.T) {
	met := metrics.New()
	w, now := newTestWatchdog(t, Config{}, met, nil, nil)
	met.Gauge("last_sync_completed_at").SetInt(now.Add(-48 * time.Hour).Unix())
	// Must not panic; alert becomes a log line only.
	w.check(context.Background())
}

func TestCheck_HeartbeatPingsURL(t *testing.T) {
	var pings atomic.Int64
	hb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pings.Add(1)
	}))
	defer hb.Close()

	met := metrics.New()
	w, now := newTestWatchdog(t, Config{HeartbeatURL: hb.URL}, met, nil, nil)
	met.Gauge("last_sync_completed_at").SetInt(now.Unix())

	w.check(context.Background())
	w.check(context.Background())
	if got := pings.Load(); got != 2 {
		t.Errorf("heartbeat pings = %d, want 2 (one per check)", got)
	}
}

func TestCheck_HeartbeatFailureDoesNotPanicOrAlert(t *testing.T) {
	hb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	hb.Close() // connection refused

	met := metrics.New()
	n, events := notifierRecorder(t)
	w, now := newTestWatchdog(t, Config{HeartbeatURL: hb.URL}, met, n, nil)
	met.Gauge("last_sync_completed_at").SetInt(now.Unix())

	w.check(context.Background())
	if len(*events) != 0 {
		t.Errorf("heartbeat failure should not push notifications, got %+v", *events)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	met := metrics.New()
	w, _ := newTestWatchdog(t, Config{Interval: 10 * time.Millisecond}, met, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestNew_Defaults(t *testing.T) {
	w := New(Config{}, Deps{Metrics: metrics.New(), Logger: discardLogger()})
	if w.cfg.Interval != 5*time.Minute {
		t.Errorf("Interval default = %v", w.cfg.Interval)
	}
	if w.cfg.SyncStalenessMax != 26*time.Hour {
		t.Errorf("SyncStalenessMax default = %v", w.cfg.SyncStalenessMax)
	}
	if w.cfg.PollStalenessMax != 10*time.Minute {
		t.Errorf("PollStalenessMax default = %v", w.cfg.PollStalenessMax)
	}
	if w.cfg.RenotifyInterval != 6*time.Hour {
		t.Errorf("RenotifyInterval default = %v", w.cfg.RenotifyInterval)
	}
}
