package recheck

import (
	"log/slog"
	"sync"
	"time"
)

// breakerState is the lifecycle of a circuit breaker. We track only the two
// states we actually act on: closed (requests pass through) and open (requests
// short-circuit). A half-open probe state isn't needed here because the first
// request after cooldown automatically acts as the probe — if it succeeds we
// reset, if it fails we re-open.
type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
)

// breaker is a minimal consecutive-failure circuit breaker.
//
// Why: RecheckDeniedTap blocks a denied NFC tap on a live Redpoint GraphQL
// call with a 10-second context. If Redpoint is unreachable (outage, LAN
// partition, expired token) every denied tap stacks another 10-second
// goroutine and every single tap a denied member makes feels like a hang.
// Once we've seen N consecutive failures in a short window we assume the
// upstream is down and return immediately for the cooldown period, so denials
// stay denials but we stop piling on broken requests.
//
// One breaker per Service is fine because Redpoint is the only upstream it
// guards; no per-endpoint sharding needed. Mutex over atomic because the
// transitions inspect multiple fields together.
//
// Moved from internal/statusync as part of A3 (the breaker is only
// meaningful to the recheck path; statusync's daily loop has its own
// retry/backoff strategy).
//
// A5 observability: every state transition (trip, probe admit, probe
// succeed, probe re-trip) emits a structured slog at the logger we hold.
// "transition" is a stable label-free key the operator can grep for; the
// "from"/"to" fields surface the actual state change. We intentionally do
// NOT emit on "ordinary" success() calls (state was already closed,
// counter at zero) because those would drown the signal in noise — only
// the four operator-actionable transitions are logged.
type breaker struct {
	mu        sync.Mutex
	state     breakerState
	failures  int
	openedAt  time.Time
	threshold int           // consecutive failures to trip
	cooldown  time.Duration // how long open state lasts

	// probing is true between "cooldown elapsed → admitted a probe" and the
	// next outcome (success or failure). It exists ONLY to drive the
	// recovery/re-trip logs — without it, success() can't distinguish
	// "ordinary close-state success" from "probe succeeded after we were
	// open". The state field can't carry that signal because we deliberately
	// don't model a half-open state (see package comment).
	probing bool

	// now is injectable so tests can drive cooldown transitions
	// deterministically without sleeps. Defaults to time.Now.
	now func() time.Time

	// logger receives structured transition events. Never nil after
	// newBreaker — the constructor falls back to slog.Default() so the
	// breaker is safe to use without a wired logger (matches the
	// nil-safety pattern in cmd/bridge wiring).
	logger *slog.Logger
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	return &breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		logger:    slog.Default(),
	}
}

// allow reports whether the next call is permitted. When the breaker is open
// but the cooldown has elapsed, it transitions back to closed so the next call
// gets a fresh attempt; success/failure on that attempt then resets or re-trips.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == breakerOpen {
		if b.now().Sub(b.openedAt) >= b.cooldown {
			// Cooldown elapsed — give one attempt through. Stay in closed
			// state so a second caller during that attempt isn't blocked;
			// worst case we make two near-simultaneous probe calls, which
			// is cheap compared to the complexity of a half-open gate.
			openFor := b.now().Sub(b.openedAt)
			b.state = breakerClosed
			b.failures = 0
			b.probing = true
			b.logger.Info("circuit breaker transition: probe admitted",
				"transition", "open_to_closed",
				"from", "open",
				"to", "closed",
				"reason", "cooldown_elapsed",
				"openForSeconds", openFor.Seconds(),
				"cooldownSeconds", b.cooldown.Seconds(),
			)
			return true
		}
		return false
	}
	return true
}

// success resets the failure counter. Safe to call even if the breaker was
// already closed — this is the normal path. When `probing` is set (meaning
// the previous allow() admitted a probe after cooldown), we additionally
// log a confirmed-recovery transition so the operator sees the breaker
// fully recover, not just the probe attempt.
func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasProbing := b.probing
	b.failures = 0
	b.state = breakerClosed
	b.probing = false
	if wasProbing {
		b.logger.Info("circuit breaker transition: probe succeeded — recovered",
			"transition", "probe_succeeded",
			"from", "closed",
			"to", "closed",
			"reason", "probe_call_succeeded",
		)
	}
}

// failure increments the consecutive-failure counter and trips the breaker
// once the threshold is hit. Callers should only report failures that look
// like upstream unavailability (network errors, 5xx) — do NOT count
// application-level "not found" or "still denied" as failures, or we'll trip
// the breaker whenever a single card isn't in Redpoint.
//
// The trip transition (closed → open) is the only state change here, but
// we differentiate two flavours in the log so operators can distinguish a
// fresh outage from a flapping upstream:
//   - "closed_to_open": first time we trip, or a trip that follows a
//     successful recovery cycle.
//   - "closed_to_open_after_probe": this trip happened while still
//     inside a probe window (we admitted a probe after cooldown and
//     accumulated `threshold` failures before any success). Operators
//     should treat this as "upstream still down" rather than a new fault.
//
// Note: a single probe failure does NOT re-trip immediately — it counts
// as one fresh failure toward the threshold. This preserves the original
// semantics covered by TestBreaker_ReopensAfterCooldownOnFailure.
func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++

	if b.failures >= b.threshold && b.state == breakerClosed {
		b.state = breakerOpen
		b.openedAt = b.now()
		transition := "closed_to_open"
		reason := "consecutive_failures_exceeded_threshold"
		if b.probing {
			transition = "closed_to_open_after_probe"
			reason = "probe_window_failures_exceeded_threshold"
		}
		b.probing = false
		b.logger.Warn("circuit breaker transition: tripped",
			"transition", transition,
			"from", "closed",
			"to", "open",
			"reason", reason,
			"failures", b.failures,
			"threshold", b.threshold,
			"cooldownSeconds", b.cooldown.Seconds(),
		)
	}
}

// isOpen returns true if the breaker is currently tripped and hasn't cooled
// down yet. Used by tests and (potentially) by debug logging.
func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == breakerOpen && b.now().Sub(b.openedAt) < b.cooldown {
		return true
	}
	return false
}

// forceReset force-closes the breaker out-of-band: clears the failure
// counter, drops the probing flag, and puts the state back to closed.
//
// The return value reports whether the breaker was actually open at the
// moment of the reset — operators use this to distinguish "reset was
// needed" from "breaker was already closed" in /debug/reset-breakers
// responses and audit logs. A no-op reset is still a safe operation and
// the caller may proceed either way.
//
// This is the manual-override path behind POST /debug/reset-breakers
// (P3 in docs/architecture-review.md). Prior to this, recovering from a
// false-positive trip required a full bridge restart; having a surgical
// reset means the on-call engineer can recover the recheck path without
// dropping the check-in queue or WebSocket connections.
//
// Logs at Info level with transition="manual_reset" so the audit trail
// shows when and how the breaker was forced closed. We deliberately log
// even when the breaker was already closed (as a no-op confirmation) —
// operators running the endpoint want feedback that their action landed.
func (b *breaker) forceReset() (wasOpen bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasOpen = b.state == breakerOpen
	b.state = breakerClosed
	b.failures = 0
	b.probing = false
	b.logger.Info("circuit breaker transition: manual reset",
		"transition", "manual_reset",
		"from", func() string {
			if wasOpen {
				return "open"
			}
			return "closed"
		}(),
		"to", "closed",
		"reason", "operator_reset",
		"wasOpen", wasOpen,
	)
	return wasOpen
}
