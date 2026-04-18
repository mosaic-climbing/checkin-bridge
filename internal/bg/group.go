// Package bg provides a supervised goroutine group with a unified shutdown.
// Each goroutine receives the group's context, which is cancelled when
// Shutdown is called. Panics are recovered and logged; non-nil errors
// (other than context.Canceled) are logged at Warn.
//
// The group also publishes two operability signals (A2 in
// docs/architecture-review.md): an optional per-name gauge of currently
// running goroutines via the Metrics hook, and — on Shutdown deadline
// expiry — a per-name Warn log naming each goroutine that did not exit
// in time. Both pin failure modes that are otherwise invisible: a long-
// running task that never exits its loop, or a goroutine leak that grows
// unboundedly across reconnects.
package bg

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
)

// Metrics is an optional hook the Group calls to publish a per-name
// "goroutines currently running" gauge. The bg package keeps this
// interface tiny so it doesn't take a hard dependency on the metrics
// package; the adapter that wires it to internal/metrics.Registry lives
// in cmd/bridge.
//
// Implementations MUST be safe for concurrent use: Inc/Dec may run on
// many goroutines simultaneously (every Go() call → Inc on the calling
// goroutine, Dec on the spawned one).
type Metrics interface {
	Inc(name string)
	Dec(name string)
}

type Group struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	logger *slog.Logger

	// metrics is optional; nil disables the gauge but stuck-goroutine
	// logging still works (it uses the alive map below).
	metrics Metrics

	// alive tracks the currently-running goroutine count keyed by the
	// name passed to Go(). Used by Shutdown to log which goroutines
	// failed to exit before the deadline. Indexed by name (not by
	// goroutine ID) because the operationally meaningful thing is "the
	// directory-sync didn't drain", not "goroutine 47 didn't drain".
	mu    sync.Mutex
	alive map[string]int
}

// New returns a Group whose goroutines inherit a cancellable child of
// parent. No metrics gauge is published; use NewWithMetrics to enable
// it. Stuck-goroutine logging at Shutdown deadline still applies.
func New(parent context.Context, logger *slog.Logger) *Group {
	return NewWithMetrics(parent, logger, nil)
}

// NewWithMetrics is like New but also publishes a per-name goroutine
// count via the Metrics adapter. Pass nil for metrics to get the same
// behaviour as New. Useful for tests that want to assert gauge updates
// without spinning up the full metrics.Registry.
func NewWithMetrics(parent context.Context, logger *slog.Logger, metrics Metrics) *Group {
	ctx, cancel := context.WithCancel(parent)
	return &Group{
		ctx:     ctx,
		cancel:  cancel,
		logger:  logger,
		metrics: metrics,
		alive:   make(map[string]int),
	}
}

// Go runs fn as a managed goroutine. name is used in logs and as the
// gauge label, so it should be a short, stable identifier (e.g.
// "directory-sync", "cache-syncer") rather than a per-call value. fn
// receives the group's context, which is cancelled on Shutdown. If fn
// returns a non-nil error that is not context.Canceled, it is logged at
// Warn. Panics are recovered, logged, and counted as a normal exit so
// the gauge does not leak.
func (g *Group) Go(name string, fn func(ctx context.Context) error) {
	g.wg.Add(1)
	g.trackStart(name)

	go func() {
		defer g.wg.Done()
		// trackEnd MUST run after the panic recover — order matters
		// here. Defers run in LIFO order, so the recover() defer is
		// scheduled second and runs first; trackEnd then runs after
		// the panic is swallowed, ensuring a panicking goroutine is
		// still removed from the alive map and Dec'd on the gauge.
		defer g.trackEnd(name)
		defer func() {
			if r := recover(); r != nil {
				g.logger.Error("bg goroutine panic",
					"name", name,
					"recover", r,
					"stack", string(debug.Stack()),
				)
			}
		}()
		if err := fn(g.ctx); err != nil && !errors.Is(err, context.Canceled) {
			g.logger.Warn("bg goroutine exited with error", "name", name, "error", err)
		}
	}()
}

// trackStart bumps the alive map and the metrics gauge. Called on the
// caller's goroutine before the spawned one runs, so a tight Go() loop
// reflects in the gauge before any of those goroutines finish.
func (g *Group) trackStart(name string) {
	g.mu.Lock()
	g.alive[name]++
	g.mu.Unlock()
	if g.metrics != nil {
		g.metrics.Inc(name)
	}
}

// trackEnd is the inverse of trackStart. Always runs (deferred), even
// on panic — see ordering note in Go().
func (g *Group) trackEnd(name string) {
	g.mu.Lock()
	g.alive[name]--
	if g.alive[name] <= 0 {
		// Drop the key once the count hits zero so logStuck doesn't
		// have to filter zero entries on the slow path.
		delete(g.alive, name)
	}
	g.mu.Unlock()
	if g.metrics != nil {
		g.metrics.Dec(name)
	}
}

// Shutdown cancels the group's context and waits for all goroutines, up to
// the parent context's deadline. Returns ctx.Err() if the deadline is hit.
// On deadline expiry, logs the names (and counts, when > 1) of the
// goroutines that did not exit, so an operator can see exactly which
// background task is blocking shutdown.
//
// After Shutdown returns, calling Go is a programming error: the fn will
// receive an already-cancelled context.
func (g *Group) Shutdown(ctx context.Context) error {
	g.cancel()
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		g.logStuck()
		return ctx.Err()
	}
}

// logStuck snapshots the alive map under the lock and emits one Warn
// per still-running name. Called only on Shutdown deadline expiry, so
// the cost of the snapshot is irrelevant. Sort by name so the log
// output is stable across runs.
func (g *Group) logStuck() {
	g.mu.Lock()
	pairs := make([]struct {
		name  string
		count int
	}, 0, len(g.alive))
	for n, c := range g.alive {
		pairs = append(pairs, struct {
			name  string
			count int
		}{n, c})
	}
	g.mu.Unlock()

	if len(pairs) == 0 {
		// Race: every goroutine finished between cancel() and the
		// snapshot. Note it but don't pretend a stuck goroutine
		// exists.
		g.logger.Warn("bg shutdown deadline reached but no goroutines tracked as live (likely racy exit)")
		return
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })
	for _, p := range pairs {
		g.logger.Warn("bg goroutine did not exit before shutdown deadline",
			"name", p.name,
			"count", p.count,
		)
	}
}
