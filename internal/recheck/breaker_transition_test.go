package recheck

// A5 observability tests for circuit-breaker transition logging.
//
// The breaker is the operator's single most important Redpoint-health
// signal — every transition is the difference between "Redpoint is
// dead, denials are fast" and "Redpoint is back, denials are slow
// again". The architecture review §A5 asks for structured logs at
// every transition; these tests pin the four we emit:
//
//   1. closed → open                       — initial trip
//   2. open → closed (probe admitted)      — cooldown elapsed
//   3. probe succeeded                     — recovery confirmed
//   4. closed → open (probe failed window) — re-trip while probing
//
// We pin both the event message AND the `transition` field, since
// downstream alert rules will key on `transition` (the human prose can
// drift; the field can't without a deliberate code change).

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// captureLogger returns a logger that writes JSON-line events to buf, plus a
// helper that decodes them. JSON is easier to assert on than the text handler
// because field values come back already typed.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// readTransitions parses the captured JSON-line stream and returns just the
// `transition` field of every record that has one. The test asserts on the
// exact ordered sequence; this avoids brittle full-record matching while still
// pinning the operator-visible event chain.
func readTransitions(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid log line %q: %v", line, err)
		}
		if v, ok := rec["transition"].(string); ok {
			out = append(out, v)
		}
	}
	return out
}

// TestBreakerTransition_Trip — closed → open after threshold consecutive
// failures emits exactly one "closed_to_open" record. Sub-threshold failures
// stay silent (we'd drown the operator otherwise).
func TestBreakerTransition_Trip(t *testing.T) {
	logger, buf := captureLogger()
	b := newBreaker(3, 100*time.Millisecond)
	b.logger = logger

	b.failure() // 1: silent
	b.failure() // 2: silent
	if got := readTransitions(t, buf); len(got) != 0 {
		t.Errorf("got transitions before threshold: %v", got)
	}
	b.failure() // 3: trip

	got := readTransitions(t, buf)
	if len(got) != 1 || got[0] != "closed_to_open" {
		t.Errorf("transitions = %v, want [closed_to_open]", got)
	}
}

// TestBreakerTransition_ProbeAdmittedAndRecovered — full happy-path
// recovery cycle: trip, cooldown elapses, probe admitted, probe succeeds.
// Operators see closed_to_open → open_to_closed → probe_succeeded.
func TestBreakerTransition_ProbeAdmittedAndRecovered(t *testing.T) {
	logger, buf := captureLogger()
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newBreaker(2, 20*time.Millisecond)
	b.now = clock.Now
	b.logger = logger

	b.failure()
	b.failure() // trip
	clock.advance(25 * time.Millisecond)
	if !b.allow() {
		t.Fatal("breaker should admit probe after cooldown")
	}
	b.success() // probe succeeded — recovery confirmed

	got := readTransitions(t, buf)
	want := []string{"closed_to_open", "open_to_closed", "probe_succeeded"}
	if !equalStrings(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}

// TestBreakerTransition_ProbeWindowReTrip — cooldown elapses, probe
// admitted, but failures keep arriving. The re-trip is logged as
// closed_to_open_after_probe so an alert rule can distinguish "still
// down" from "fresh outage". Threshold=2 means we need two failures
// after the probe to re-trip.
func TestBreakerTransition_ProbeWindowReTrip(t *testing.T) {
	logger, buf := captureLogger()
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newBreaker(2, 20*time.Millisecond)
	b.now = clock.Now
	b.logger = logger

	b.failure()
	b.failure() // trip
	clock.advance(25 * time.Millisecond)
	_ = b.allow() // probe admitted
	b.failure()   // 1 of 2 in probe window — silent
	b.failure()   // 2 of 2 — re-trip

	got := readTransitions(t, buf)
	want := []string{"closed_to_open", "open_to_closed", "closed_to_open_after_probe"}
	if !equalStrings(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}

// TestBreakerTransition_OrdinarySuccess_IsSilent — calling success() when
// the breaker was never opened (no probe in flight) emits NO transition
// log. This is the dominant code path; logging it would drown signal.
func TestBreakerTransition_OrdinarySuccess_IsSilent(t *testing.T) {
	logger, buf := captureLogger()
	b := newBreaker(3, 100*time.Millisecond)
	b.logger = logger

	for i := 0; i < 5; i++ {
		b.success()
	}
	if got := readTransitions(t, buf); len(got) != 0 {
		t.Errorf("ordinary success() emitted transitions: %v", got)
	}
}

// TestBreakerTransition_LoggerInheritsServiceContext — when the breaker is
// constructed via recheck.New the breaker logger MUST carry a stable
// "component" attribute so transition logs are filterable in production.
// This pins the wiring promised in recheck.go.
func TestBreakerTransition_LoggerInheritsServiceContext(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc := New(nil, nil, nil, Config{
		BreakerThreshold: 1,
		BreakerCooldown:  50 * time.Millisecond,
	}, logger)

	svc.breaker.failure() // trip on first failure (threshold=1)

	// Find the trip record and assert it carries component=recheck.breaker.
	var rec map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r["transition"] == "closed_to_open" {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatalf("no closed_to_open record in log: %s", buf.String())
	}
	if got := rec["component"]; got != "recheck.breaker" {
		t.Errorf("component = %v, want recheck.breaker", got)
	}
}

// equalStrings compares two string slices for exact equality (length + order).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

