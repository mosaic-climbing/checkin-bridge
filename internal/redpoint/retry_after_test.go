package redpoint

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ─── parseRetryAfter ─────────────────────────────────────────

// TestParseRetryAfter_DeltaSeconds covers the numeric form —
// "Retry-After: 30" means wait 30 seconds.
func TestParseRetryAfter_DeltaSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"0", 0},                  // "retry immediately" → 0; loop floors with its own backoff
		{"1", 1 * time.Second},    // smallest useful hint
		{"30", 30 * time.Second},  // typical
		{"120", 120 * time.Second},
		{"3600", time.Hour},       // long hint (Redpoint daily cap?)
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseRetryAfter(tc.in)
			if got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseRetryAfter_NegativeIsZero guards against servers sending
// "-1" or similar. We refuse to interpret negative as "retry immediately";
// the loop's own backoff floor handles that correctly.
func TestParseRetryAfter_NegativeIsZero(t *testing.T) {
	for _, s := range []string{"-1", "-60", "-9999"} {
		if got := parseRetryAfter(s); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", s, got)
		}
	}
}

// TestParseRetryAfter_EmptyIsZero — a missing header should parse to 0,
// which the caller treats as "no hint; use your own backoff".
func TestParseRetryAfter_EmptyIsZero(t *testing.T) {
	for _, s := range []string{"", "   ", "\t"} {
		if got := parseRetryAfter(s); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", s, got)
		}
	}
}

// TestParseRetryAfter_GarbageIsZero — malformed values must not be
// misinterpreted. A server bug is not worth aborting over; fall back
// to the loop's own backoff.
func TestParseRetryAfter_GarbageIsZero(t *testing.T) {
	cases := []string{
		"later",
		"5m",              // we don't parse Go-style durations
		"1.5",             // no fractional seconds in the spec
		"30s",             // the header spec is bare integer, not "30s"
		"tomorrow",
		"{}",
	}
	for _, s := range cases {
		if got := parseRetryAfter(s); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", s, got)
		}
	}
}

// TestParseRetryAfter_HTTPDate_Future — an HTTP-date in the future
// should produce a positive duration roughly equal to now→then.
func TestParseRetryAfter_HTTPDate_Future(t *testing.T) {
	future := time.Now().UTC().Add(2 * time.Minute)
	header := future.Format(http.TimeFormat) // IMF-fixdate

	got := parseRetryAfter(header)
	// Allow a wide margin — the test's "now" is a few microseconds
	// earlier than parseRetryAfter's "now", and the header has
	// second-precision so up to a full second can disappear in
	// truncation. Be generous: anywhere in [90s, 130s] is fine.
	if got < 90*time.Second || got > 130*time.Second {
		t.Errorf("parseRetryAfter(%q) = %v, want ~120s", header, got)
	}
}

// TestParseRetryAfter_HTTPDate_Past — a date in the past has already
// elapsed, so there's nothing to wait for. Zero is the right answer.
func TestParseRetryAfter_HTTPDate_Past(t *testing.T) {
	past := time.Now().UTC().Add(-10 * time.Minute)
	header := past.Format(http.TimeFormat)

	if got := parseRetryAfter(header); got != 0 {
		t.Errorf("parseRetryAfter(%q) = %v, want 0 (already elapsed)", header, got)
	}
}

// TestParseRetryAfter_HTTPDate_RFC850 — http.ParseTime accepts the
// legacy RFC 850 format too. Not common in the wild but spec-allowed.
func TestParseRetryAfter_HTTPDate_RFC850(t *testing.T) {
	future := time.Now().UTC().Add(3 * time.Minute)
	header := future.Format(time.RFC850)

	got := parseRetryAfter(header)
	if got < 150*time.Second || got > 200*time.Second {
		t.Errorf("parseRetryAfter(%q) = %v, want ~180s", header, got)
	}
}

// ─── Retry loop honours Retry-After ────────────────────────────

// TestExecWithRetry_HonoursRetryAfter verifies that when a 429 arrives
// with a Retry-After hint larger than our exponential backoff, the
// loop waits at least the hint before retrying. Without this, a gym
// with aggressive 429s would retry in 200ms and earn another 429, ad
// nauseam.
func TestExecWithRetry_HonoursRetryAfter(t *testing.T) {
	withFastBackoff(t) // collapse the exp base to ~100µs so only the
	// Retry-After hint could possibly be responsible for a >=1s wait.

	// The header spec only supports integer seconds, so the smallest
	// hint we can send is 1s. That's long enough to be unambiguously
	// distinguishable from the fast-backoff floor (~100µs).
	var calls int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			io.WriteString(w, "rate limited")
			return
		}
		graphqlOK(w)
	}))
	t.Cleanup(s.Close)

	c := newClientFor(t, s.URL)
	start := time.Now()
	if _, err := c.execWithRetry(context.Background(), "{}", nil); err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	elapsed := time.Since(start)

	// Must have waited ≥ ~1s (the Retry-After hint) despite the
	// fast-backoff being in the sub-millisecond range. Allow a
	// small scheduler slop below 1s for the timer implementation.
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~1s (Retry-After honoured)", elapsed)
	}
	// And not orders-of-magnitude more — we didn't mistakenly
	// multiply the hint or sleep per-attempt.
	if elapsed > 3*time.Second {
		t.Errorf("elapsed = %v, want roughly the hint, not much more", elapsed)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

// TestExecWithRetry_NoRetryAfterFallsBackToBackoff verifies that when
// a 429 arrives WITHOUT a Retry-After, we use our own exponential
// backoff and don't wait any longer than that. Guards against a bug
// where we misparse "absent" as "huge" and block for minutes.
func TestExecWithRetry_NoRetryAfterFallsBackToBackoff(t *testing.T) {
	withFastBackoff(t)

	fs, s := newFlakingServer(t, 429, 200)
	c := newClientFor(t, s.URL)

	start := time.Now()
	if _, err := c.execWithRetry(context.Background(), "{}", nil); err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	elapsed := time.Since(start)

	// With fast-backoff (~100µs base), two attempts should complete
	// in single-digit milliseconds. If we somehow mishandled the
	// absent header as a huge wait, we'd see seconds.
	if elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %v, want single-digit ms with fast-backoff + no Retry-After", elapsed)
	}
	if got := fs.attempts(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

// TestExecWithRetry_RetryAfterZeroUsesBackoffFloor verifies that
// "Retry-After: 0" doesn't produce a zero-wait tight loop — the
// exponential backoff is still used as a floor. This is a defensive
// property: a server that sends "0" is either confused or hostile,
// and we don't want to help either case.
func TestExecWithRetry_RetryAfterZeroUsesBackoffFloor(t *testing.T) {
	withFastBackoff(t)

	var calls int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		graphqlOK(w)
	}))
	t.Cleanup(s.Close)

	c := newClientFor(t, s.URL)
	if _, err := c.execWithRetry(context.Background(), "{}", nil); err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	// We can't measure the floor precisely without racing the
	// scheduler, but we can verify we at least reached the second
	// attempt — proves the loop didn't bail on a zero wait, and the
	// RetryAfter=0 path didn't propagate garbage into the timer.
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

// TestHTTPError_CarriesRetryAfter verifies that the exec() layer
// populates httpError.RetryAfter from the response header, so callers
// that want to log or inspect the hint can do so via errors.As.
func TestHTTPError_CarriesRetryAfter(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(429)
		io.WriteString(w, "slow down")
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
	if herr.Status != 429 {
		t.Errorf("status = %d, want 429", herr.Status)
	}
	if herr.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %v, want 42s", herr.RetryAfter)
	}
}

// TestHTTPError_NoRetryAfterIsZero — a non-2xx response without a
// Retry-After header must produce RetryAfter=0, not a spurious value.
func TestHTTPError_NoRetryAfterIsZero(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	t.Cleanup(s.Close)

	c := newClientFor(t, s.URL)
	_, err := c.exec(context.Background(), "{}", nil)
	var herr *httpError
	if !errors.As(err, &herr) {
		t.Fatalf("err is %T, want *httpError", err)
	}
	if herr.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 (header absent)", herr.RetryAfter)
	}
}

// TestHTTPError_RetryAfterGarbageIsZero — a malformed Retry-After
// must not panic or produce a huge wait; 0 is the fallback.
func TestHTTPError_RetryAfterGarbageIsZero(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "banana")
		w.WriteHeader(429)
	}))
	t.Cleanup(s.Close)

	c := newClientFor(t, s.URL)
	_, err := c.exec(context.Background(), "{}", nil)
	var herr *httpError
	if !errors.As(err, &herr) {
		t.Fatalf("err is %T, want *httpError", err)
	}
	if herr.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 (malformed header)", herr.RetryAfter)
	}
}

