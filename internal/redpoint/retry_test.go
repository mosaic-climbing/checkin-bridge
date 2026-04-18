package redpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// P5 — Retry/backoff in the Redpoint client.
//
// These tests cover the classification table, the backoff loop, the
// context-cancellation hooks, and the wiring of every public method
// through execWithRetry (so an outage on a single call type doesn't
// silently bypass the retry layer).

// withFastBackoff shrinks the global backoff base for the duration of a
// test. Default is 200ms; tests don't need a real wall-clock backoff to
// verify that one happens, so we collapse it to a few microseconds. A
// t.Cleanup restores the previous value to keep tests independent.
func withFastBackoff(t *testing.T) {
	t.Helper()
	prev := backoffBase
	backoffBase = 100 * time.Microsecond
	t.Cleanup(func() { backoffBase = prev })
}

// newClientFor constructs a Client pointed at the given test server URL,
// with a discard logger so test output stays clean.
func newClientFor(t *testing.T, url string) *Client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewClient(url, "test-key", "TEST", logger)
}

// graphqlOK writes a minimal GraphQL success response so unmarshalling
// in the client's specific methods (LookupByExternalID, etc.) succeeds.
// The "data" payload is intentionally generic; tests that need typed
// data for the public method should use the FakeRedpoint instead.
func graphqlOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `{"data":{}}`)
}

// ─── Classification: retryable() ──────────────────────────────

func TestRetryable_NetworkErrorRetries(t *testing.T) {
	if !retryable(&transportError{Err: errors.New("connection refused")}) {
		t.Error("transportError should be retryable")
	}
}

func TestRetryable_429Retries(t *testing.T) {
	if !retryable(&httpError{Status: 429, Body: "rate limited"}) {
		t.Error("HTTP 429 should be retryable")
	}
}

func TestRetryable_5xxRetries(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		if !retryable(&httpError{Status: code}) {
			t.Errorf("HTTP %d should be retryable", code)
		}
	}
}

func TestRetryable_4xxIsPermanent(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 422} {
		if retryable(&httpError{Status: code}) {
			t.Errorf("HTTP %d should NOT be retryable", code)
		}
	}
}

func TestRetryable_ContextCancelIsPermanent(t *testing.T) {
	// Even though the underlying transport error looks retryable, a
	// context-canceled chain must short-circuit — the caller has given
	// up and no amount of waiting will help.
	wrapped := &transportError{Err: fmt.Errorf("dial: %w", context.Canceled)}
	if retryable(wrapped) {
		t.Error("context.Canceled in chain should make error non-retryable")
	}
	wrapped = &transportError{Err: fmt.Errorf("dial: %w", context.DeadlineExceeded)}
	if retryable(wrapped) {
		t.Error("context.DeadlineExceeded in chain should make error non-retryable")
	}
}

func TestRetryable_GraphQLAndUnknownErrorsArePermanent(t *testing.T) {
	// Plain errors (GraphQL application errors, JSON unmarshal failures)
	// have neither *httpError nor *transportError on the chain; they
	// are not retryable — we'd just burn time and re-fail.
	if retryable(errors.New("graphql errors: [bad query]")) {
		t.Error("plain error should not be retryable")
	}
}

// ─── Backoff: backoffFor() ────────────────────────────────────

func TestBackoffFor_Doubles(t *testing.T) {
	// With base 1ms and ±25% jitter, attempt 2 should average around
	// twice attempt 1 and attempt 3 around four times. Sample many
	// times to wash out individual jitter draws.
	prev := backoffBase
	backoffBase = 1 * time.Millisecond
	defer func() { backoffBase = prev }()

	const samples = 500
	var sum1, sum2, sum3 time.Duration
	for i := 0; i < samples; i++ {
		sum1 += backoffFor(1)
		sum2 += backoffFor(2)
		sum3 += backoffFor(3)
	}
	avg1 := sum1 / samples
	avg2 := sum2 / samples
	avg3 := sum3 / samples

	// Allow a ±20% slack on top of the ±25% jitter — these are loose
	// invariants meant to fail loud only if the formula is genuinely
	// wrong (off-by-one, wrong shift direction, missing scaling).
	if avg2 < (avg1*3)/2 || avg2 > (avg1*5)/2 {
		t.Errorf("avg2=%v, avg1=%v: expected ~2x", avg2, avg1)
	}
	if avg3 < (avg1*3) || avg3 > (avg1*5) {
		t.Errorf("avg3=%v, avg1=%v: expected ~4x", avg3, avg1)
	}
}

func TestBackoffFor_JitterStaysWithin25Percent(t *testing.T) {
	prev := backoffBase
	backoffBase = 1 * time.Second
	defer func() { backoffBase = prev }()

	for i := 0; i < 200; i++ {
		w := backoffFor(1) // base 1s
		if w < 750*time.Millisecond || w > 1250*time.Millisecond {
			t.Errorf("backoffFor(1)=%v, want within [750ms, 1250ms]", w)
		}
	}
}

// ─── Loop behaviour: execWithRetry ────────────────────────────

// flakingServer replies with the given sequence of HTTP statuses, one
// per call. Once the sequence is exhausted, every subsequent call
// returns 200 OK with an empty data envelope. attempts() returns the
// number of calls made — tests use it to verify retry counts.
type flakingServer struct {
	statuses []int
	calls    int64
}

func newFlakingServer(t *testing.T, statuses ...int) (*flakingServer, *httptest.Server) {
	fs := &flakingServer{statuses: statuses}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt64(&fs.calls, 1) - 1
		if int(idx) < len(fs.statuses) && fs.statuses[idx] != http.StatusOK {
			w.WriteHeader(fs.statuses[idx])
			io.WriteString(w, fmt.Sprintf("synthetic error %d", fs.statuses[idx]))
			return
		}
		graphqlOK(w)
	}))
	t.Cleanup(s.Close)
	return fs, s
}

func (fs *flakingServer) attempts() int64 { return atomic.LoadInt64(&fs.calls) }

func TestExecWithRetry_RecoversAfter429(t *testing.T) {
	withFastBackoff(t)
	fs, s := newFlakingServer(t, 429, 200) // first call rate-limited, second OK
	c := newClientFor(t, s.URL)

	if _, err := c.execWithRetry(context.Background(), "{}", nil); err != nil {
		t.Fatalf("expected recovery after 429, got %v", err)
	}
	if got := fs.attempts(); got != 2 {
		t.Errorf("calls = %d, want 2 (one 429 + one success)", got)
	}
}

func TestExecWithRetry_RecoversAfter500(t *testing.T) {
	withFastBackoff(t)
	fs, s := newFlakingServer(t, 500, 503, 200) // two 5xx, then OK
	c := newClientFor(t, s.URL)

	if _, err := c.execWithRetry(context.Background(), "{}", nil); err != nil {
		t.Fatalf("expected recovery after 5xx, got %v", err)
	}
	if got := fs.attempts(); got != 3 {
		t.Errorf("calls = %d, want 3 (two 5xx + one success)", got)
	}
}

func TestExecWithRetry_FailsAfterMaxAttempts(t *testing.T) {
	withFastBackoff(t)
	fs, s := newFlakingServer(t, 503, 503, 503, 503, 503) // never recovers
	c := newClientFor(t, s.URL)

	_, err := c.execWithRetry(context.Background(), "{}", nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	var herr *httpError
	if !errors.As(err, &herr) || herr.Status != 503 {
		t.Errorf("err = %v, want *httpError{503}", err)
	}
	if got := fs.attempts(); got != int64(maxAttempts) {
		t.Errorf("calls = %d, want %d", got, maxAttempts)
	}
}

func TestExecWithRetry_DoesNotRetry4xx(t *testing.T) {
	withFastBackoff(t)
	fs, s := newFlakingServer(t, 401) // permanent
	c := newClientFor(t, s.URL)

	_, err := c.execWithRetry(context.Background(), "{}", nil)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	var herr *httpError
	if !errors.As(err, &herr) || herr.Status != 401 {
		t.Errorf("err = %v, want *httpError{401}", err)
	}
	if got := fs.attempts(); got != 1 {
		t.Errorf("calls = %d, want 1 (4xx must not retry)", got)
	}
}

func TestExecWithRetry_DoesNotRetry404(t *testing.T) {
	withFastBackoff(t)
	fs, s := newFlakingServer(t, 404)
	c := newClientFor(t, s.URL)

	_, err := c.execWithRetry(context.Background(), "{}", nil)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if got := fs.attempts(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// TestExecWithRetry_RetriesTransportError covers the case where the
// server hangs up before writing a response — the http.Client surfaces
// it as a transport error, which the wrapper should treat as retryable.
func TestExecWithRetry_RetriesTransportError(t *testing.T) {
	withFastBackoff(t)
	var calls int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			// Hijack and close the conn mid-request to force a transport
			// error on the client's read of the response.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter doesn't support Hijacker")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			return
		}
		graphqlOK(w)
	}))
	t.Cleanup(s.Close)

	c := newClientFor(t, s.URL)
	if _, err := c.execWithRetry(context.Background(), "{}", nil); err != nil {
		t.Fatalf("expected recovery after transport error, got %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestExecWithRetry_HonoursContextCancellation(t *testing.T) {
	// Server always 503s; canceling the context mid-backoff should
	// abort retries promptly rather than running out the schedule.
	withFastBackoff(t)
	// Use a slightly-larger base so the cancellation window is
	// observable (50ms is shorter than the wait between attempts).
	prev := backoffBase
	backoffBase = 50 * time.Millisecond
	defer func() { backoffBase = prev }()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	t.Cleanup(s.Close)
	c := newClientFor(t, s.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond) // let first attempt fail
		cancel()
	}()

	start := time.Now()
	_, err := c.execWithRetry(ctx, "{}", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after cancellation")
	}
	// Cancel should land during the first backoff (~50ms ± 25%).
	// Total elapsed must be well under the full schedule (~200ms+).
	if elapsed > 150*time.Millisecond {
		t.Errorf("elapsed = %v, expected cancellation to interrupt backoff promptly", elapsed)
	}
}

// ─── Public-method wiring ─────────────────────────────────────

// Each public method in client.go that previously called c.exec()
// directly is now expected to route through execWithRetry. We assert
// this by pointing the client at a server that 503s once then succeeds
// and verifying we see exactly two HTTP calls — one would mean retry
// was bypassed.

func TestPublicMethods_RouteThroughRetry(t *testing.T) {
	withFastBackoff(t)

	cases := []struct {
		name string
		// minimal JSON payload returned on the success call so the
		// method can unmarshal without error
		successBody string
		call        func(c *Client) error
	}{
		{
			name:        "LookupByExternalID",
			successBody: `{"data":{"customerByExternalId":null}}`,
			call: func(c *Client) error {
				_, err := c.LookupByExternalID(context.Background(), "ext-1")
				return err
			},
		},
		{
			name:        "GetCustomer",
			successBody: `{"data":{"customer":null}}`,
			call: func(c *Client) error {
				_, err := c.GetCustomer(context.Background(), "id-1")
				return err
			},
		},
		{
			name:        "CreateCheckIn",
			successBody: `{"data":{"createCheckIn":{"__typename":"CreateCheckInResult","recordId":"r1","record":{"id":"r1","status":"OK"}}}}`,
			call: func(c *Client) error {
				_, err := c.CreateCheckIn(context.Background(), "gate-1", "cust-1", "")
				return err
			},
		},
		{
			name:        "ListGates",
			successBody: `{"data":{"gates":{"edges":[]}}}`,
			call: func(c *Client) error {
				_, err := c.ListGates(context.Background())
				return err
			},
		},
		{
			name:        "ListRecentCheckIns",
			successBody: `{"data":{"checkIns":{"edges":[],"total":0}}}`,
			call: func(c *Client) error {
				_, err := c.ListRecentCheckIns(context.Background(), 10)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := atomic.AddInt64(&calls, 1)
				if n == 1 {
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.successBody)
			}))
			t.Cleanup(s.Close)

			c := newClientFor(t, s.URL)
			if err := tc.call(c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := atomic.LoadInt64(&calls); got != 2 {
				t.Errorf("%s: calls = %d, want 2 (retry must engage)", tc.name, got)
			}
		})
	}
}

// TestExec_TypedErrors verifies the exec() layer wraps errors in the
// right typed wrapper so downstream classification works.
func TestExec_HTTPErrorIsTyped(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(418)
		io.WriteString(w, "I'm a teapot")
	}))
	t.Cleanup(s.Close)

	c := newClientFor(t, s.URL)
	_, err := c.exec(context.Background(), "{}", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *httpError
	if !errors.As(err, &herr) {
		t.Fatalf("err is %T, want *httpError", err)
	}
	if herr.Status != 418 || !strings.Contains(herr.Body, "teapot") {
		t.Errorf("got %+v", herr)
	}
}

func TestExec_TransportErrorIsTyped(t *testing.T) {
	// Point at a closed server so Do() fails immediately.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := s.URL
	s.Close() // close before making the request

	c := newClientFor(t, url)
	_, err := c.exec(context.Background(), "{}", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var terr *transportError
	if !errors.As(err, &terr) {
		t.Fatalf("err is %T, want *transportError", err)
	}
}
