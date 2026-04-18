package redpoint

// Tests for the A5 observability hookup on redpoint.Client. The
// instrumentation is scoped to the single `exec()` funnel, so a few
// lookups exercise every path the operator cares about:
//
//   - success path observes the histogram and bumps
//     redpoint_requests_total.
//   - application-level "customer not found" is STILL a successful
//     transport call (HTTP 200, GraphQL data with null customer) and
//     must register as success — we do NOT want the error counter
//     climbing every time a stranger taps. That property directly
//     mirrors the breaker-logic invariant in recheck.Service.
//   - transport / HTTP-error paths bump redpoint_request_errors_total.
//     We cover a 500 here; the retry wrapper also exercises this
//     surface in retry_test.go.
//
// Nil-metrics safety is covered by every other test in this package —
// newTestClient leaves c.metrics nil and the existing suite passes
// unchanged, which proves observeDuration short-circuits cleanly.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
)

// withMetrics returns a client wired to a fresh metrics.Registry plus
// the registry itself so the test can read counter/histogram values.
func withMetrics(t *testing.T, f *testutil.FakeRedpoint) (*Client, *metrics.Registry) {
	t.Helper()
	c := newTestClient(t, f)
	reg := metrics.New()
	c.SetMetrics(reg)
	return c, reg
}

// TestMetrics_SuccessPath_IncrementsSuccessCounterAndHistogram — the
// happy path records one observation in the latency histogram AND one
// success in redpoint_requests_total, with the error counter untouched.
func TestMetrics_SuccessPath_IncrementsSuccessCounterAndHistogram(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()
	f.AddCustomer(testutil.FakeCustomer{
		ID: "rp-1", ExternalID: "ext-1",
		FirstName: "A", LastName: "B", Active: true, Badge: "ACTIVE",
	})

	c, reg := withMetrics(t, f)
	_, err := c.LookupByExternalID(context.Background(), "ext-1")
	if err != nil {
		t.Fatal(err)
	}

	hist := reg.Histogram("redpoint_request_duration_seconds", redpointLatencyBuckets)
	if hist.Count() != 1 {
		t.Errorf("histogram count = %d, want 1", hist.Count())
	}
	if hist.Sum() <= 0 {
		t.Errorf("histogram sum = %g, want > 0 (timing non-zero)", hist.Sum())
	}
	if got := reg.Counter("redpoint_requests_total").Value(); got != 1 {
		t.Errorf("requests_total = %d, want 1", got)
	}
	if got := reg.Counter("redpoint_request_errors_total").Value(); got != 0 {
		t.Errorf("request_errors_total = %d, want 0", got)
	}
}

// TestMetrics_CustomerNotFound_CountsAsSuccess — a Redpoint GraphQL
// response of `data.customerByExternalId: null` is a well-formed reply,
// not an upstream failure. The error counter MUST stay at zero —
// otherwise every stranger tapping an unknown card looks like a
// Redpoint outage and trips observability alerts.
func TestMetrics_CustomerNotFound_CountsAsSuccess(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()
	// no AddCustomer → fake returns null for the query.

	c, reg := withMetrics(t, f)
	got, err := c.LookupByExternalID(context.Background(), "ext-nobody")
	if err != nil {
		t.Fatalf("LookupByExternalID: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil customer, got %+v", got)
	}
	if got := reg.Counter("redpoint_requests_total").Value(); got != 1 {
		t.Errorf("requests_total = %d, want 1 (null-customer is still a success)", got)
	}
	if got := reg.Counter("redpoint_request_errors_total").Value(); got != 0 {
		t.Errorf("request_errors_total = %d, want 0", got)
	}
	hist := reg.Histogram("redpoint_request_duration_seconds", redpointLatencyBuckets)
	if hist.Count() != 1 {
		t.Errorf("histogram count = %d, want 1", hist.Count())
	}
}

// TestMetrics_TransportError_IncrementsErrorCounter — a server-side
// 500 must bump the error counter AND still observe the duration
// (operators care about slow-failure shape, not just error rate).
//
// We use a httptest.Server that returns 500 unconditionally because
// testutil.FakeRedpoint is modelled on valid GraphQL replies. We hit
// execWithRetry through LookupByExternalID, which retries 3x on 5xx —
// so the expected counter value is maxAttempts (3). Accept that value
// because the retry wrapper is the production path; the per-attempt
// instrumentation granularity matches what operators will see.
func TestMetrics_TransportError_IncrementsErrorCounter(t *testing.T) {
	// Override backoff so the 3 retries don't slow the test. Can't go
	// lower than ~1ms because backoffFor uses rand.Int64N(base/2) which
	// panics on zero — so 1ms gives 0.5ms jitter floor.
	savedBackoff := backoffBase
	backoffBase = 1 * 1_000_000 // 1ms
	defer func() { backoffBase = savedBackoff }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewClient(srv.URL, "k", "F", logger)
	reg := metrics.New()
	c.SetMetrics(reg)

	_, err := c.LookupByExternalID(context.Background(), "whatever")
	if err == nil {
		t.Fatal("expected error from 500 server")
	}

	if got := reg.Counter("redpoint_requests_total").Value(); got != 0 {
		t.Errorf("requests_total = %d, want 0", got)
	}
	if got := reg.Counter("redpoint_request_errors_total").Value(); got != int64(maxAttempts) {
		t.Errorf("request_errors_total = %d, want %d (one per retry attempt)", got, maxAttempts)
	}
	hist := reg.Histogram("redpoint_request_duration_seconds", redpointLatencyBuckets)
	if hist.Count() != int64(maxAttempts) {
		t.Errorf("histogram count = %d, want %d (each attempt observed)", hist.Count(), maxAttempts)
	}
}

// TestMetrics_NilRegistry_IsSafe — calling exec with c.metrics == nil
// must not panic. All existing tests rely on this but an explicit
// assertion pins the invariant.
func TestMetrics_NilRegistry_IsSafe(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()
	f.AddCustomer(testutil.FakeCustomer{ID: "rp-1", ExternalID: "ext-1", Active: true, Badge: "ACTIVE"})

	c := newTestClient(t, f) // no SetMetrics
	if _, err := c.LookupByExternalID(context.Background(), "ext-1"); err != nil {
		t.Fatalf("LookupByExternalID: %v", err)
	}
}
