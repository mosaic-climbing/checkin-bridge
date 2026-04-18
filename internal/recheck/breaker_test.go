package recheck

// Tests for the consecutive-failure circuit breaker. Moved from
// internal/statusync as part of A3. The breaker behaviour is unchanged;
// these tests pin the same five invariants with the same threshold/
// cooldown shapes the recheck path actually uses.

import (
	"testing"
	"time"
)

// TestBreaker_ClosedByDefault verifies a fresh breaker permits traffic.
func TestBreaker_ClosedByDefault(t *testing.T) {
	b := newBreaker(3, 10*time.Millisecond)
	if !b.allow() {
		t.Fatal("new breaker should allow traffic")
	}
	if b.isOpen() {
		t.Fatal("new breaker should not be open")
	}
}

// TestBreaker_TripsAfterThreshold verifies N consecutive failures open it.
func TestBreaker_TripsAfterThreshold(t *testing.T) {
	b := newBreaker(3, 100*time.Millisecond)
	b.failure()
	b.failure()
	if b.isOpen() {
		t.Fatal("breaker should not be open after 2 failures (threshold=3)")
	}
	b.failure() // third failure — trip
	if !b.isOpen() {
		t.Fatal("breaker should be open after 3 failures")
	}
	if b.allow() {
		t.Fatal("breaker should not allow traffic when open")
	}
}

// TestBreaker_ResetOnSuccess verifies one success clears the failure counter.
func TestBreaker_ResetOnSuccess(t *testing.T) {
	b := newBreaker(3, 100*time.Millisecond)
	b.failure()
	b.failure()
	b.success()
	b.failure()
	b.failure() // should still be two, not four
	if b.isOpen() {
		t.Fatal("breaker should not be open — counter was reset by success")
	}
}

// TestBreaker_RecoversAfterCooldown verifies cooldown elapses and closes it.
// Uses an injected clock so the test doesn't sleep.
func TestBreaker_RecoversAfterCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newBreaker(2, 20*time.Millisecond)
	b.now = clock.Now

	b.failure()
	b.failure()
	if !b.isOpen() {
		t.Fatal("breaker should be open")
	}
	clock.advance(25 * time.Millisecond)
	if !b.allow() {
		t.Fatal("breaker should allow after cooldown elapsed")
	}
	if b.isOpen() {
		t.Fatal("breaker should not report open after allow() cleared it")
	}
}

// TestBreaker_ReopensAfterCooldownOnFailure verifies the first failure post-
// cooldown doesn't leak into the previous failure count — it starts fresh,
// so we need `threshold` new failures to trip again.
func TestBreaker_ReopensAfterCooldownOnFailure(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newBreaker(2, 20*time.Millisecond)
	b.now = clock.Now

	b.failure()
	b.failure()
	clock.advance(25 * time.Millisecond)
	_ = b.allow() // probe permitted, resets counter
	b.failure()
	if b.isOpen() {
		t.Fatal("breaker should not be open after one fresh failure (threshold=2)")
	}
	b.failure()
	if !b.isOpen() {
		t.Fatal("breaker should be open after two fresh failures")
	}
}

// fakeClock is a hand-rolled monotonic clock for the cooldown tests.
// time.Now-based tests would either need real sleeps (slow + flaky) or
// would race the cooldown threshold. Keeping the clock injected on the
// breaker via the now field is invasive enough to be obvious in the
// implementation but cheap enough that production code stays unchanged.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// TestBreaker_ForceReset_WhileOpen pins the P3 manual-override contract:
// forceReset on a tripped breaker reports wasOpen=true and returns the
// breaker to the closed state so allow() starts permitting traffic
// immediately, even though the cooldown hasn't elapsed. The test uses a
// fakeClock to make it obvious we're not relying on wall-clock time —
// the reset must work purely on state, not on the passage of time.
func TestBreaker_ForceReset_WhileOpen(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
	b := newBreaker(3, 60*time.Second)
	b.now = clk.Now

	// Trip the breaker.
	b.failure()
	b.failure()
	b.failure()
	if !b.isOpen() {
		t.Fatal("precondition: breaker should be open after threshold failures")
	}

	// Force reset before the cooldown elapses.
	wasOpen := b.forceReset()
	if !wasOpen {
		t.Errorf("forceReset returned wasOpen=false; want true")
	}
	if b.isOpen() {
		t.Error("breaker still open after forceReset")
	}
	if !b.allow() {
		t.Error("breaker did not permit traffic after forceReset")
	}

	// Fresh failures must accumulate from zero — the reset must have
	// cleared the counter, not just flipped the state. If the counter
	// persisted, a single post-reset failure would re-open the breaker,
	// which would defeat the whole point of the manual override.
	b.failure()
	if b.isOpen() {
		t.Error("breaker re-tripped after a single post-reset failure; counter was not cleared")
	}
}

// TestBreaker_ForceReset_WhileClosed documents the no-op contract: a
// reset on an already-closed breaker is safe, reports wasOpen=false,
// and leaves behaviour unchanged. /debug/reset-breakers relies on this
// return value to render a useful response when the operator presses
// the button at the wrong time.
func TestBreaker_ForceReset_WhileClosed(t *testing.T) {
	b := newBreaker(3, 60*time.Second)
	if b.isOpen() {
		t.Fatal("fresh breaker should not be open")
	}
	wasOpen := b.forceReset()
	if wasOpen {
		t.Errorf("forceReset on closed breaker returned wasOpen=true; want false")
	}
	if !b.allow() {
		t.Error("breaker should still allow traffic after no-op reset")
	}
}

// TestService_ResetBreaker verifies the exported wrapper on *Service
// threads through to the inner breaker and preserves the wasOpen flag.
// This is the method cmd/bridge hands to the API server as the
// BreakerResetter callback, so the surface contract it exposes matters.
func TestService_ResetBreaker(t *testing.T) {
	s := &Service{breaker: newBreaker(2, 60*time.Second)}
	s.breaker.failure()
	s.breaker.failure()
	if !s.breaker.isOpen() {
		t.Fatal("precondition: inner breaker should be open after two failures")
	}
	if wasOpen := s.ResetBreaker(); !wasOpen {
		t.Errorf("Service.ResetBreaker returned wasOpen=false; want true")
	}
	if s.breaker.isOpen() {
		t.Error("inner breaker still open after Service.ResetBreaker")
	}
	if wasOpen := s.ResetBreaker(); wasOpen {
		t.Error("second Service.ResetBreaker returned wasOpen=true; want false")
	}
}
