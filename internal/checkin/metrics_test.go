package checkin

// A5 observability tests for the redpoint_async_writes_in_flight gauge.
//
// The gauge mirrors handler.asyncWG depth — operators graph it to spot
// when Redpoint is backing up (sustained >5 is the alert threshold from
// architecture-review §A5). The invariant under test is that the gauge
// value EQUALS the WG counter at every moment: incremented in lockstep
// with WG.Add(1), decremented in the same defer stack as WG.Done().
//
// We exercise the live invariant with an httptest.Server that BLOCKS the
// createCheckIn mutation on a release channel. That gives the test a
// determined window where N goroutines are stuck inside the redpoint
// client; we assert gauge==N, then release, then assert gauge==0 after
// Shutdown drains the WG. Polling the gauge to reach N (with a deadline)
// races cleanly with goroutine startup — no sleeps, no flake.
//
// We also pin nil-metrics safety on this code path, mirroring the same
// guarantee redpoint.observeDuration enforces.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// blockingCheckInServer returns an httptest.Server whose createCheckIn
// handler waits on `release` before replying. inFlight tracks how many
// requests are currently parked, which gives the test a second view of
// the same invariant the gauge measures.
func blockingCheckInServer(release <-chan struct{}, inFlight *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"createCheckIn": map[string]any{
					"__typename": "CreateCheckInResult",
					"recordId":   "checkin-blocked",
					"record": map[string]any{
						"id":     "checkin-blocked",
						"status": "OK",
					},
				},
			},
		})
	}))
}

// waitFor polls cond at 1ms intervals until it returns true or the deadline
// elapses. Returns true on success, false on timeout. Used to synchronise
// the test on goroutine startup without inserting blind sleeps.
func waitFor(deadline time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(1 * time.Millisecond)
	}
	return cond()
}

// TestAsyncGauge_TracksInFlightRedpointWrites — fires N taps that each
// dispatch a goroutine into a blocked Redpoint server, asserts the gauge
// reaches N, then releases the server and asserts the gauge returns to 0
// after Shutdown drains the WG. This is the headline A5 invariant: gauge
// value == asyncWG counter at every observable moment.
func TestAsyncGauge_TracksInFlightRedpointWrites(t *testing.T) {
	release := make(chan struct{})
	var serverInFlight atomic.Int64
	srv := blockingCheckInServer(release, &serverInFlight)
	defer srv.Close()

	h, db, _ := setupHandler(t)
	h.gateID = "gate-test" // non-empty triggers the async path
	h.redpointClient = redpoint.NewClient(srv.URL, "k", "F", discardLogger())
	reg := metrics.New()
	h.metrics = reg

	ctx := context.Background()
	if err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "TAGAA",
		CustomerID:  "cust-aa",
		FirstName:   "Alice",
		LastName:    "Async",
		BadgeStatus: "ACTIVE",
		Active:      true,
	}); err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}

	const N = 3
	for i := 0; i < N; i++ {
		h.HandleEvent(ctx, unifi.AccessEvent{
			CredentialID: "TAGAA",
			AuthType:     "NFC",
			Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		})
	}

	g := reg.Gauge("redpoint_async_writes_in_flight")
	// Wait until ALL N goroutines have actually parked inside the Redpoint
	// HTTP request — not just been spawned. The gauge increment is
	// synchronous in unlockAndRecord (so it may briefly exceed serverInFlight
	// during goroutine startup); converging on serverInFlight==N avoids
	// that benign race and asserts the strong invariant: while N writes are
	// truly mid-flight, the gauge reads N.
	if !waitFor(2*time.Second, func() bool { return serverInFlight.Load() == int64(N) }) {
		t.Fatalf("server never saw %d in-flight requests (gauge=%g, server in-flight=%d)",
			N, g.Value(), serverInFlight.Load())
	}
	if got := g.Value(); got != float64(N) {
		t.Errorf("gauge while %d requests parked = %g, want %d", N, got, N)
	}

	close(release)

	sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Shutdown(sCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if g.Value() != 0 {
		t.Errorf("gauge after Shutdown = %g, want 0", g.Value())
	}
}

// TestAsyncGauge_NilMetrics_AsyncDispatchSafe — pins that dispatch works
// when no metrics registry is attached. The Inc/Dec wraps must
// short-circuit cleanly, exactly like observeDuration in redpoint.
func TestAsyncGauge_NilMetrics_AsyncDispatchSafe(t *testing.T) {
	release := make(chan struct{})
	close(release) // never block — we just want the dispatch to complete
	var serverInFlight atomic.Int64
	srv := blockingCheckInServer(release, &serverInFlight)
	defer srv.Close()

	h, db, _ := setupHandler(t)
	h.gateID = "gate-test"
	h.redpointClient = redpoint.NewClient(srv.URL, "k", "F", discardLogger())
	// no SetMetrics — h.metrics stays nil

	ctx := context.Background()
	if err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "TAGBB",
		CustomerID:  "cust-bb",
		BadgeStatus: "ACTIVE",
		Active:      true,
	}); err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}

	h.HandleEvent(ctx, unifi.AccessEvent{
		CredentialID: "TAGBB",
		AuthType:     "NFC",
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	})

	sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Shutdown(sCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestAsyncGauge_BackfillEvent_DoesNotIncrement — backfill events skip
// Redpoint recording entirely (see handler.unlockAndRecord), so they
// must NOT touch the gauge. If they did, replaying a long outage's
// worth of events would falsely inflate operator-visible "Redpoint
// backed up" alerts.
func TestAsyncGauge_BackfillEvent_DoesNotIncrement(t *testing.T) {
	// Use a server that fails fast — we should never reach it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("backfill event reached Redpoint server, should have skipped")
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h, db, _ := setupHandler(t)
	h.gateID = "gate-test"
	h.redpointClient = redpoint.NewClient(srv.URL, "k", "F", discardLogger())
	reg := metrics.New()
	h.metrics = reg

	ctx := context.Background()
	if err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "TAGCC",
		CustomerID:  "cust-cc",
		BadgeStatus: "ACTIVE",
		Active:      true,
	}); err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}

	h.HandleEvent(ctx, unifi.AccessEvent{
		CredentialID: "TAGCC",
		AuthType:     "NFC",
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		IsBackfill:   true,
	})

	// Drain anything just in case so a stray goroutine can't race the assertion.
	sCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := h.Shutdown(sCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	g := reg.Gauge("redpoint_async_writes_in_flight")
	if g.Value() != 0 {
		t.Errorf("gauge after backfill = %g, want 0 (backfill must skip async)", g.Value())
	}
}
