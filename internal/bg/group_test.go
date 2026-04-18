package bg

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (d discardWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

// fakeMetrics is a thread-safe Metrics implementation used by the gauge
// tests. Tracks both the running count per name (for "is the gauge in
// the right state right now?" assertions) and a running total of Inc
// calls per name (for "did Inc fire even if the goroutine has already
// exited?" assertions).
type fakeMetrics struct {
	mu       sync.Mutex
	live     map[string]int
	incCalls map[string]int
	decCalls map[string]int
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{
		live:     map[string]int{},
		incCalls: map[string]int{},
		decCalls: map[string]int{},
	}
}

func (f *fakeMetrics) Inc(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[name]++
	f.incCalls[name]++
}

func (f *fakeMetrics) Dec(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[name]--
	f.decCalls[name]++
}

func (f *fakeMetrics) Live(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[name]
}

func (f *fakeMetrics) Calls(name string) (inc, dec int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.incCalls[name], f.decCalls[name]
}

// TestGo_RunsAndExits: Go schedules fn, fn returns, Shutdown completes quickly.
func TestGo_RunsAndExits(t *testing.T) {
	ctx := context.Background()
	g := New(ctx, discardLogger())

	ran := false
	g.Go("test", func(ctx context.Context) error {
		ran = true
		return nil
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := g.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run")
	}
}

// TestShutdown_CancelsContext: fn blocks on <-ctx.Done(); Shutdown cancels and returns nil.
func TestShutdown_CancelsContext(t *testing.T) {
	ctx := context.Background()
	g := New(ctx, discardLogger())

	completed := false
	g.Go("test", func(ctx context.Context) error {
		<-ctx.Done()
		completed = true
		return ctx.Err()
	})

	// Give the goroutine time to start
	time.Sleep(10 * time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := g.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if !completed {
		t.Fatal("fn did not complete after shutdown")
	}
}

// TestShutdown_RespectsDeadline: fn ignores ctx and sleeps 1s; Shutdown(ctx with 50ms deadline) returns context.DeadlineExceeded.
func TestShutdown_RespectsDeadline(t *testing.T) {
	ctx := context.Background()
	g := New(ctx, discardLogger())

	g.Go("test", func(ctx context.Context) error {
		time.Sleep(1 * time.Second)
		return nil
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := g.Shutdown(shutdownCtx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

// TestGo_PanicRecovered: fn panics; group does not crash; Shutdown returns nil.
func TestGo_PanicRecovered(t *testing.T) {
	ctx := context.Background()
	g := New(ctx, discardLogger())

	g.Go("test", func(ctx context.Context) error {
		panic("test panic")
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := g.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestGo_ErrorLogged: fn returns a non-Canceled error; verify it returns and doesn't panic.
func TestGo_ErrorLogged(t *testing.T) {
	ctx := context.Background()
	g := New(ctx, discardLogger())

	testErr := errors.New("test error")
	g.Go("test-error", func(ctx context.Context) error {
		return testErr
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := g.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	// If we get here without panic, the error was logged (even if we can't easily verify it).
}

// TestGo_MultipleGoroutines: 10 concurrent Go calls; Shutdown waits for all.
func TestGo_MultipleGoroutines(t *testing.T) {
	ctx := context.Background()
	g := New(ctx, discardLogger())

	const numGoroutines = 10
	completed := 0
	var mu sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		g.Go("test", func(ctx context.Context) error {
			<-ctx.Done()
			mu.Lock()
			completed++
			mu.Unlock()
			return nil
		})
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := g.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	if completed != numGoroutines {
		t.Fatalf("expected %d goroutines to complete, got %d", numGoroutines, completed)
	}
}

// TestShutdown_Idempotent_IsNotGuaranteed_But_DoubleCallDoesNotPanic: call Shutdown twice; second call should not panic.
// Note: The second call's behavior is undefined (may hang, may succeed, etc.). This test just ensures we don't panic.
func TestShutdown_Idempotent_IsNotGuaranteed_But_DoubleCallDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	g := New(ctx, discardLogger())

	g.Go("test", func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := g.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("first Shutdown failed: %v", err)
	}

	// Second call. We don't assert behavior since it's undefined. We just
	// make sure it doesn't panic (e.g. by trying to close a channel twice).
	shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	_ = g.Shutdown(shutdownCtx2)
	// If we get here without panic, test passes.
}

// ─── A2: Metrics gauge + stuck-goroutine logging ───────────────────────────

// TestMetrics_IncDecOnNormalExit pins the happy path: Inc fires once on
// Go(), Dec fires once when the goroutine returns nil. The live count
// must settle at 0 after Shutdown — a stuck +1 here would mean the
// gauge would grow without bound across reconnects, defeating the
// whole point of A2.
func TestMetrics_IncDecOnNormalExit(t *testing.T) {
	fm := newFakeMetrics()
	g := NewWithMetrics(context.Background(), discardLogger(), fm)

	done := make(chan struct{})
	g.Go("worker", func(ctx context.Context) error {
		close(done)
		return nil
	})
	<-done

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	inc, dec := fm.Calls("worker")
	if inc != 1 || dec != 1 {
		t.Fatalf("worker calls = (inc=%d, dec=%d), want (1, 1)", inc, dec)
	}
	if live := fm.Live("worker"); live != 0 {
		t.Errorf("worker live = %d after exit, want 0 (gauge leak)", live)
	}
}

// TestMetrics_IncDecOnPanic asserts the gauge is decremented even when
// the goroutine panics. Without the LIFO defer ordering in Go(), a
// panicking goroutine would skip trackEnd and the gauge would leak —
// which is exactly the failure mode A2 tries to make visible, so the
// gauge must not lie about it.
func TestMetrics_IncDecOnPanic(t *testing.T) {
	fm := newFakeMetrics()
	g := NewWithMetrics(context.Background(), discardLogger(), fm)

	g.Go("crashy", func(ctx context.Context) error {
		panic("boom")
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	inc, dec := fm.Calls("crashy")
	if inc != 1 || dec != 1 {
		t.Fatalf("crashy calls = (inc=%d, dec=%d), want (1, 1) — Dec must run even on panic", inc, dec)
	}
	if live := fm.Live("crashy"); live != 0 {
		t.Errorf("crashy live = %d after panic, want 0 (gauge leak on panic)", live)
	}
}

// TestMetrics_IncDecOnError pins the same Inc/Dec property when fn
// returns a non-nil error. The error path uses a different code path
// inside the goroutine (the err != nil branch), so it gets its own
// pin to avoid a regression where, say, an early-return short-circuits
// the trackEnd defer.
func TestMetrics_IncDecOnError(t *testing.T) {
	fm := newFakeMetrics()
	g := NewWithMetrics(context.Background(), discardLogger(), fm)

	g.Go("failing", func(ctx context.Context) error {
		return errors.New("intentional")
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	inc, dec := fm.Calls("failing")
	if inc != 1 || dec != 1 {
		t.Fatalf("failing calls = (inc=%d, dec=%d), want (1, 1)", inc, dec)
	}
}

// TestMetrics_PerName asserts Inc/Dec are keyed on the name passed to
// Go(), not coalesced across all goroutines. Operationally this is
// what makes the gauge useful — bg_goroutines_running{name="x"} per
// name, not a single total.
func TestMetrics_PerName(t *testing.T) {
	fm := newFakeMetrics()
	g := NewWithMetrics(context.Background(), discardLogger(), fm)

	const perName = 3
	var ready sync.WaitGroup
	ready.Add(perName * 2)
	block := func(ctx context.Context) error {
		ready.Done()
		<-ctx.Done()
		return nil
	}
	for i := 0; i < perName; i++ {
		g.Go("alpha", block)
		g.Go("beta", block)
	}
	ready.Wait()

	if live := fm.Live("alpha"); live != perName {
		t.Errorf("alpha live = %d, want %d", live, perName)
	}
	if live := fm.Live("beta"); live != perName {
		t.Errorf("beta live = %d, want %d", live, perName)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if live := fm.Live("alpha"); live != 0 {
		t.Errorf("alpha live after shutdown = %d, want 0", live)
	}
	if live := fm.Live("beta"); live != 0 {
		t.Errorf("beta live after shutdown = %d, want 0", live)
	}
}

// TestNew_NoMetricsHookIsSafe pins backwards compatibility: the
// existing New(parent, logger) constructor must still work — no
// adapter required, no nil-deref on Inc/Dec inside Go().
func TestNew_NoMetricsHookIsSafe(t *testing.T) {
	g := New(context.Background(), discardLogger())

	g.Go("nometrics", func(ctx context.Context) error { return nil })

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// captureLogger returns a slog.Logger that writes to buf. The tests
// below scrape buf for the stuck-goroutine Warn message — a separate
// fakeMetrics wouldn't help because the logging path doesn't go
// through Metrics.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestShutdown_LogsStuckGoroutinesByName is the operability pin: when
// Shutdown's deadline expires, every still-running goroutine must
// surface in the logs by name. Without this, a stuck shutdown looks
// like "context deadline exceeded" with no clue which task is to
// blame. We launch two named goroutines that ignore ctx and force the
// deadline to expire, then assert both names appear in the log.
func TestShutdown_LogsStuckGoroutinesByName(t *testing.T) {
	var buf bytes.Buffer
	g := New(context.Background(), captureLogger(&buf))

	// Use a channel to release the goroutines after the test asserts.
	// We don't want them to leak past the test even though they ignore
	// the group context.
	release := make(chan struct{})
	defer close(release)

	for _, name := range []string{"directory-sync", "cache-syncer"} {
		name := name
		g.Go(name, func(ctx context.Context) error {
			<-release // ignore ctx — we want to force the deadline
			_ = name
			return nil
		})
	}

	// Give both goroutines a moment to register in the alive map. The
	// trackStart call is on the caller's goroutine so this isn't
	// strictly necessary, but it ensures we don't race the goroutines
	// past their reads (which we don't want anyway).
	time.Sleep(20 * time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := g.Shutdown(shutdownCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want DeadlineExceeded", err)
	}

	logs := buf.String()
	for _, want := range []string{"directory-sync", "cache-syncer"} {
		if !strings.Contains(logs, want) {
			t.Errorf("stuck-goroutine log missing name %q; got:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, "did not exit before shutdown deadline") {
		t.Errorf("expected stuck-goroutine warning text in logs; got:\n%s", logs)
	}
}

// TestShutdown_NoStuckLogWhenAllExit asserts the inverse: a clean
// shutdown must not emit the stuck-goroutine Warn. A false positive
// here would train operators to ignore the warning, which is exactly
// the opposite of what we want.
func TestShutdown_NoStuckLogWhenAllExit(t *testing.T) {
	var buf bytes.Buffer
	g := New(context.Background(), captureLogger(&buf))

	g.Go("clean", func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if logs := buf.String(); strings.Contains(logs, "did not exit before shutdown deadline") {
		t.Errorf("clean shutdown emitted stuck-goroutine warning:\n%s", logs)
	}
}

// TestMetrics_Concurrent stresses the alive-map lock. A bug in the
// CAS-free Inc/Dec path on the gauge or the alive map (e.g. a missing
// lock) would surface here under -race. Uses an atomic counter to
// double-check Inc count vs. number of Go() calls.
func TestMetrics_Concurrent(t *testing.T) {
	fm := newFakeMetrics()
	g := NewWithMetrics(context.Background(), discardLogger(), fm)

	const n = 100
	var spawned atomic.Int64
	for i := 0; i < n; i++ {
		g.Go("worker", func(ctx context.Context) error {
			spawned.Add(1)
			return nil
		})
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := spawned.Load(); got != n {
		t.Fatalf("spawned = %d, want %d", got, n)
	}
	inc, dec := fm.Calls("worker")
	if inc != n || dec != n {
		t.Fatalf("worker calls = (inc=%d, dec=%d), want (%d, %d)", inc, dec, n, n)
	}
	if live := fm.Live("worker"); live != 0 {
		t.Errorf("worker live = %d after all exit, want 0", live)
	}
}
