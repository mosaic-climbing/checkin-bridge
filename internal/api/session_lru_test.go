package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// withMaxEntries temporarily lowers the tracker cap for a test and restores it
// on cleanup. Lets us exercise eviction without minting 10k Authenticate
// calls (bcrypt would dominate the test runtime).
func withMaxEntries(t *testing.T, n int) {
	t.Helper()
	prev := maxLoginAttemptEntries
	maxLoginAttemptEntries = n
	t.Cleanup(func() { maxLoginAttemptEntries = prev })
}

// insertTracker is a test-only helper that forces a tracker into place at the
// front of the LRU with a specified lastTry / lockedUntil. Bypasses bcrypt
// entirely; exists only in tests.
func insertTracker(sm *SessionManager, ip string, lastTry, lockedUntil time.Time, failures int) {
	sm.loginMu.Lock()
	defer sm.loginMu.Unlock()
	if elem, ok := sm.loginAttempts[ip]; ok {
		t := elem.Value.(*loginTracker)
		t.lastTry = lastTry
		t.lockedUntil = lockedUntil
		t.failures = failures
		sm.loginLRU.MoveToFront(elem)
		return
	}
	tr := &loginTracker{ip: ip, lastTry: lastTry, lockedUntil: lockedUntil, failures: failures}
	elem := sm.loginLRU.PushFront(tr)
	sm.loginAttempts[ip] = elem
}

func TestLoginLRU_EvictsOldestNonLocked(t *testing.T) {
	withMaxEntries(t, 3)
	sm := NewSessionManager("test-pass")

	// Fill to capacity with three distinct IPs, oldest to newest.
	base := time.Now().Add(-time.Hour)
	insertTracker(sm, "1.1.1.1", base, time.Time{}, 0)            // oldest
	insertTracker(sm, "2.2.2.2", base.Add(time.Minute), time.Time{}, 0)
	insertTracker(sm, "3.3.3.3", base.Add(2*time.Minute), time.Time{}, 0) // newest

	if got := sm.loginLRU.Len(); got != 3 {
		t.Fatalf("precondition: LRU len = %d, want 3", got)
	}

	// touchTrackerLocked for a brand new IP must evict 1.1.1.1 (oldest).
	sm.loginMu.Lock()
	sm.touchTrackerLocked("4.4.4.4")
	sm.loginMu.Unlock()

	if _, ok := sm.loginAttempts["1.1.1.1"]; ok {
		t.Error("oldest entry 1.1.1.1 should have been evicted")
	}
	if _, ok := sm.loginAttempts["4.4.4.4"]; !ok {
		t.Error("newest entry 4.4.4.4 should be present")
	}
	if got := sm.loginLRU.Len(); got != 3 {
		t.Errorf("LRU len after eviction = %d, want 3", got)
	}
}

func TestLoginLRU_PreservesLockedEntriesDuringEviction(t *testing.T) {
	withMaxEntries(t, 3)
	sm := NewSessionManager("test-pass")

	now := time.Now()
	// 1.1.1.1 is the oldest BUT is currently locked out — eviction must
	// skip it and take the next oldest non-locked entry instead.
	insertTracker(sm, "1.1.1.1", now.Add(-time.Hour), now.Add(time.Minute), maxLoginFailures)
	insertTracker(sm, "2.2.2.2", now.Add(-30*time.Minute), time.Time{}, 0)
	insertTracker(sm, "3.3.3.3", now.Add(-20*time.Minute), time.Time{}, 0)

	sm.loginMu.Lock()
	sm.touchTrackerLocked("4.4.4.4")
	sm.loginMu.Unlock()

	if _, ok := sm.loginAttempts["1.1.1.1"]; !ok {
		t.Error("locked entry 1.1.1.1 must survive eviction")
	}
	if _, ok := sm.loginAttempts["2.2.2.2"]; ok {
		t.Error("oldest non-locked entry 2.2.2.2 should have been evicted")
	}
	if _, ok := sm.loginAttempts["4.4.4.4"]; !ok {
		t.Error("newest entry 4.4.4.4 should be present")
	}

	// And the lock still actually rejects a real auth attempt.
	if sm.Authenticate("test-pass", "1.1.1.1:12345") {
		t.Error("preserved lockout should still reject correct password")
	}
}

func TestLoginLRU_AllLockedEntriesGrowsPastCap(t *testing.T) {
	// Pathological case: every slot currently holds a live lockout. Rather
	// than evict a live rate-limit (which would let an attacker reset their
	// own lockout by flooding unique source IPs), we allow the map to grow
	// past cap and rely on the janitor to recover once locks expire.
	withMaxEntries(t, 2)
	sm := NewSessionManager("test-pass")

	now := time.Now()
	insertTracker(sm, "1.1.1.1", now, now.Add(time.Minute), maxLoginFailures)
	insertTracker(sm, "2.2.2.2", now, now.Add(time.Minute), maxLoginFailures)

	sm.loginMu.Lock()
	sm.touchTrackerLocked("3.3.3.3")
	sm.loginMu.Unlock()

	if got := sm.loginLRU.Len(); got != 3 {
		t.Errorf("LRU len = %d, want 3 (grew past cap because all were locked)", got)
	}
	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		if _, ok := sm.loginAttempts[ip]; !ok {
			t.Errorf("entry %s should still be present", ip)
		}
	}
}

func TestSweepLoginAttempts_RemovesStaleUnlocked(t *testing.T) {
	sm := NewSessionManager("test-pass")

	now := time.Now()
	stale := now.Add(-(loginStaleAge + time.Minute)) // definitively old
	fresh := now.Add(-time.Minute)                   // definitively young

	// Populate back-to-front: staleA is oldest, fresh is newest.
	insertTracker(sm, "staleA", stale, time.Time{}, 0)
	insertTracker(sm, "staleB", stale.Add(time.Second), time.Time{}, 0)
	insertTracker(sm, "fresh", fresh, time.Time{}, 0)

	removed := sm.sweepLoginAttempts(now)
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if _, ok := sm.loginAttempts["staleA"]; ok {
		t.Error("staleA should have been swept")
	}
	if _, ok := sm.loginAttempts["staleB"]; ok {
		t.Error("staleB should have been swept")
	}
	if _, ok := sm.loginAttempts["fresh"]; !ok {
		t.Error("fresh entry must survive")
	}
}

func TestSweepLoginAttempts_PreservesCurrentLockouts(t *testing.T) {
	sm := NewSessionManager("test-pass")

	now := time.Now()
	// Tracker whose lastTry is ancient but whose lockout is still active —
	// represents a persistent attacker we locked out a while ago who hasn't
	// tried again. We want to keep them locked until the lockout expires,
	// not silently drop the rate limit via sweep.
	insertTracker(sm, "attacker", now.Add(-time.Hour), now.Add(5*time.Minute), maxLoginFailures)
	insertTracker(sm, "collateral", now.Add(-(loginStaleAge + time.Minute)), time.Time{}, 0)

	removed := sm.sweepLoginAttempts(now)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, ok := sm.loginAttempts["attacker"]; !ok {
		t.Error("active lockout must not be swept")
	}
	if sm.Authenticate("test-pass", "attacker:12345") {
		t.Error("preserved lockout should still reject correct password after sweep")
	}
}

func TestSweepLoginAttempts_ReclaimsExpiredLockouts(t *testing.T) {
	sm := NewSessionManager("test-pass")

	now := time.Now()
	// Lockout field is set but the deadline is in the past AND the entry
	// is otherwise stale — sweep should treat it like any other stale entry.
	insertTracker(sm, "expired", now.Add(-(loginStaleAge + time.Hour)), now.Add(-time.Hour), maxLoginFailures)

	removed := sm.sweepLoginAttempts(now)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, ok := sm.loginAttempts["expired"]; ok {
		t.Error("expired lockout that is also stale should be swept")
	}
}

func TestSweepLoginAttempts_EmptyMapIsNoop(t *testing.T) {
	sm := NewSessionManager("test-pass")
	if got := sm.sweepLoginAttempts(time.Now()); got != 0 {
		t.Errorf("empty sweep removed = %d, want 0", got)
	}
}

func TestJanitor_ShutdownDrainsCleanly(t *testing.T) {
	// Tighten the interval so the janitor actually ticks during the test.
	prev := loginJanitorInterval
	loginJanitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { loginJanitorInterval = prev })

	sm := NewSessionManager("test-pass")
	ctx, cancel := context.WithCancel(context.Background())
	sm.StartJanitor(ctx)

	// Let at least one tick fire.
	time.Sleep(25 * time.Millisecond)

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := sm.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown error = %v, want nil (janitor should exit promptly)", err)
	}
}

func TestJanitor_ActuallySweepsViaTick(t *testing.T) {
	// End-to-end: pre-populate a stale tracker, start the janitor with a
	// fast tick, verify the entry is gone shortly after.
	prev := loginJanitorInterval
	loginJanitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { loginJanitorInterval = prev })

	sm := NewSessionManager("test-pass")
	insertTracker(sm, "stale", time.Now().Add(-(loginStaleAge + time.Hour)), time.Time{}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sm.StartJanitor(ctx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sm.loginMu.Lock()
		_, present := sm.loginAttempts["stale"]
		sm.loginMu.Unlock()
		if !present {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("janitor did not sweep stale entry within deadline")
}

func TestAuthenticate_LRUOrdering(t *testing.T) {
	// Three IPs attempt in order A, B, C then A again. After the second A
	// attempt, A should be at the Front (newest) and B at the Back (oldest).
	sm := NewSessionManager("test-pass")
	for _, ip := range []string{"A:1", "B:1", "C:1", "A:1"} {
		// Wrong password — we only care about LRU ordering, not the auth result.
		sm.Authenticate("nope", ip)
	}

	front := sm.loginLRU.Front().Value.(*loginTracker).ip
	back := sm.loginLRU.Back().Value.(*loginTracker).ip
	if front != "A" {
		t.Errorf("front = %q, want A (most recently touched)", front)
	}
	if back != "B" {
		t.Errorf("back = %q, want B (oldest untouched)", back)
	}
}

func TestTouchTrackerLocked_DoesNotLeakBetweenIPs(t *testing.T) {
	// Regression guard: map stores per-IP trackers; touching IP X must not
	// mutate IP Y's tracker.
	sm := NewSessionManager("test-pass")

	now := time.Now()
	insertTracker(sm, "x", now.Add(-time.Minute), time.Time{}, 3)
	insertTracker(sm, "y", now.Add(-2*time.Minute), time.Time{}, 0)

	sm.loginMu.Lock()
	ty := sm.touchTrackerLocked("y")
	tx := sm.loginAttempts["x"].Value.(*loginTracker)
	sm.loginMu.Unlock()

	if ty.failures != 0 {
		t.Errorf("y.failures = %d, want 0", ty.failures)
	}
	if tx.failures != 3 {
		t.Errorf("x.failures = %d, want 3 (untouched)", tx.failures)
	}
}

// Demonstrate that the map count stays bounded under the configured cap even
// when an attacker mints many unique source IPs. This is the whole point of S2.
func TestAuthenticate_BoundsTrackerMap(t *testing.T) {
	withMaxEntries(t, 5)
	sm := NewSessionManager("test-pass")

	// Use insertTracker so we don't pay bcrypt cost per iteration. This is
	// valid because touchTrackerLocked / eviction run in insertTracker's
	// bypass path too — wait, insertTracker skips the cap check. Use the
	// real entry path via touchTrackerLocked directly.
	for i := 0; i < 50; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i)
		sm.loginMu.Lock()
		sm.touchTrackerLocked(ip)
		sm.loginMu.Unlock()
	}

	if got := sm.loginLRU.Len(); got != 5 {
		t.Errorf("LRU len = %d, want cap of 5", got)
	}
	if got := len(sm.loginAttempts); got != 5 {
		t.Errorf("map len = %d, want cap of 5", got)
	}
}
