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
