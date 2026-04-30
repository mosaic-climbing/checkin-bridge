// Package jobs centralises the jobs-table lifecycle so both the manual
// HTTP handlers in internal/api and the scheduled syncer goroutines
// (cache, statusync, unifimirror) record runs through a single path.
//
// Before this package existed, only the api handlers wrapped their work
// in store.CreateJob / CompleteJob / FailJob. The scheduled syncers ran
// their tickers without ever touching the jobs table, so the /ui/sync
// page's "Last run" pills and the Recent Jobs list silently dropped
// every scheduled run — the row simply never existed. Operators saw
// only manual triggers and assumed the schedulers had stopped firing.
//
// The constants here are the canonical job-type strings; internal/api
// and the schedulers reference them so the two sides can never drift
// apart and end up writing/reading different labels.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// Job-type strings are stored verbatim in jobs.type and form the path
// segment on GET /ui/frag/sync-last-run/{type}. Keep this set as the
// single source of truth — internal/api/sync_ux.go redeclares these
// to the same values.
const (
	TypeCacheSync     = "cache_sync"
	TypeStatusSync    = "status_sync"
	TypeDirectorySync = "directory_sync"
	TypeUniFiIngest   = "unifi_ingest"
	TypeUAHubSync     = "ua_hub_sync"
)

// writeTimeout bounds the detached INSERT/UPDATE that opens and
// closes a job row out. Matches the value used by
// internal/api/sync_ux.go's finishSyncJob — see that comment for
// the lifetime story (request-ctx cancellation stranding rows in
// 'running' forever).
const writeTimeout = 5 * time.Second

// NewID produces a human-readable, sortable job id of the form
// "<type>-<utcTimestamp>". Same shape as internal/api/sync_ux.go's
// newJobID so scheduled and manual rows interleave cleanly when staff
// scan the Recent Jobs list.
func NewID(jobType string) string {
	return fmt.Sprintf("%s-%s", jobType, time.Now().UTC().Format("20060102T150405.000Z"))
}

// Start inserts a "running" row and returns its id. Errors are logged
// and swallowed — job tracking is observability and must not abort the
// underlying sync. Callers always get a usable id back; a silent
// CreateJob failure just means Finish has nothing to update.
//
// The INSERT runs on a detached ctx with a short deadline so a
// caller whose parent ctx is already cancelled (e.g. a scheduler
// firing on the same tick the supervisor went Done) can still
// record the row's birth — symmetric with Finish.
func Start(ctx context.Context, s *store.Store, logger *slog.Logger, jobType string) string {
	id := NewID(jobType)
	if s == nil {
		return id
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := s.CreateJob(writeCtx, id, jobType); err != nil {
		if logger != nil {
			logger.Warn("jobs.Start: CreateJob failed",
				"type", jobType, "id", id, "error", err)
		}
	}
	return id
}

// Finish transitions the row to a terminal state. On success, result
// is marshalled into jobs.result; on error, the message lands in
// jobs.error and the row is marked failed. Both writes detach the
// parent ctx so a caller-cancelled HTTP request (or a syncer whose
// supervisor ctx just went Done) cannot strand the row in 'running'.
//
// The detach pattern is the same one in internal/api/sync_ux.go —
// see finishSyncJob there for the v0.5.7 incident that drove it.
func Finish(ctx context.Context, s *store.Store, logger *slog.Logger, id string, result any, fnErr error) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if fnErr != nil {
		if err := s.FailJob(ctx, id, fnErr.Error()); err != nil && logger != nil {
			logger.Warn("jobs.Finish: FailJob failed", "id", id, "error", err)
		}
		return
	}
	if err := s.CompleteJob(ctx, id, result); err != nil && logger != nil {
		logger.Warn("jobs.Finish: CompleteJob failed", "id", id, "error", err)
	}
}

// Track is the convenience wrapper for the common scheduled-loop
// shape: open a 'running' row, run fn, close it with whatever fn
// returned. The result body is whatever fn produces on success and
// is ignored on error (the error string lands in jobs.error instead).
//
// fn's returned error is propagated back to the caller verbatim —
// Track is purely an observability bracket and does not change error
// semantics.
func Track(
	ctx context.Context,
	s *store.Store,
	logger *slog.Logger,
	jobType string,
	fn func(ctx context.Context) (result any, err error),
) error {
	id := Start(ctx, s, logger, jobType)
	result, err := fn(ctx)
	Finish(ctx, s, logger, id, result, err)
	return err
}

// LoopConfig controls a scheduled-loop's cadence, jitter, and
// failure-retry behaviour. Zero values are safe and match the
// pre-hardening behaviour: fire once on entry, then every Interval
// with no jitter and no backoff (failures retry on the next regular
// tick). Production callers in cmd/bridge populate the rest.
//
// The hardening (jitter + InitialDelay + exponential backoff) exists
// because the bridge runs five schedulers off the same Sync.Interval.
// Without staggering they all fire at boot within milliseconds of
// each other, and again at every tick — a thunderherd against
// Redpoint. Without backoff, an upstream outage retries at full
// intensity every interval; with backoff, consecutive failures grow
// the wait until success or BackoffMax.
type LoopConfig struct {
	// Interval between regular ticks after the initial pass. Must
	// be positive; a zero or negative Interval means "run the
	// initial pass and then block on ctx without re-running" —
	// useful only for fixture/test setups that want a single fire.
	Interval time.Duration

	// InitialDelay is how long to wait before the first run. Set
	// per-scheduler in cmd/bridge to stagger boot-time fires across
	// the five schedulers (e.g. 0s, 30s, 60s, 90s, 120s) so they
	// don't all hit Redpoint simultaneously. Zero = run immediately.
	InitialDelay time.Duration

	// Jitter is the fraction of Interval to randomise each tick by
	// (uniform in ±Jitter*Interval). 0.0 disables jitter; 0.1 is
	// the production default. Capped at 0.5 — anything larger and
	// successive ticks start overlapping.
	Jitter float64

	// BackoffStart is the wait after the first consecutive failure.
	// Doubles on each subsequent failure, capped at BackoffMax. The
	// next regular tick resets the wait to Interval (with jitter)
	// once a run succeeds. Zero disables backoff entirely — failures
	// retry at the normal Interval cadence.
	BackoffStart time.Duration

	// BackoffMax caps the exponential growth. Zero is treated as
	// "no cap" (so callers who set BackoffStart should always set
	// this too). Production sets 5 minutes.
	BackoffMax time.Duration
}

// Loop runs fn on an InitialDelay-then-Interval schedule, wrapping
// each invocation in Track so the run lands in the jobs table. On
// failure it applies exponential backoff (BackoffStart → 2× → cap
// at BackoffMax); on success it resets and resumes the regular
// jittered Interval cadence. Errors are logged and otherwise
// swallowed — a transient upstream blip should not wedge the
// scheduler. Designed to be passed directly to bg.Group.Go so the
// lifetime is supervised.
//
// Blocks until ctx is cancelled; returns ctx.Err() on exit.
//
// Use this for jobs whose only scheduling concern is "fire on a
// stagger, then every N hours". Schedulers with wall-clock pinning
// (statusync's SyncTimeLocal) need their own loop bodies — Loop is
// the simple-cadence shape only.
func Loop(
	ctx context.Context,
	cfg LoopConfig,
	s *store.Store,
	logger *slog.Logger,
	jobType string,
	fn func(ctx context.Context) (result any, err error),
) error {
	jitterFrac := cfg.Jitter
	if jitterFrac < 0 {
		jitterFrac = 0
	}
	if jitterFrac > 0.5 {
		jitterFrac = 0.5
	}

	// Seed once per Loop invocation. We're fine with math/rand/v2's
	// default global generator — ticking schedules don't need crypto
	// randomness, just spread.
	jitter := func(base time.Duration) time.Duration {
		if jitterFrac == 0 || base <= 0 {
			return base
		}
		// Uniform in [-jitterFrac, +jitterFrac] of base.
		delta := (rand.Float64()*2 - 1) * jitterFrac * float64(base)
		out := base + time.Duration(delta)
		if out < 0 {
			out = 0
		}
		return out
	}

	// Initial wait. Zero-or-negative InitialDelay → fire immediately,
	// matching the prior LoopWithInterval contract. Jitter is applied
	// here too so two schedulers configured with the same delay
	// don't fire at the same instant.
	if cfg.InitialDelay > 0 {
		if !waitFor(ctx, jitter(cfg.InitialDelay)) {
			return ctx.Err()
		}
	}

	// Backoff state. backoff == 0 means "no backoff active" — next
	// wait is the regular jittered interval. After a failure backoff
	// is set to BackoffStart (or doubled). After a success it resets.
	// We seed it from the initial pass too so a startup-time failure
	// counts toward the backoff sequence.
	var backoff time.Duration
	if err := Track(ctx, s, jobLoopLogger(logger), jobType, fn); err != nil {
		if logger != nil {
			logger.Error("scheduled job failed (initial)", "type", jobType, "error", err)
		}
		backoff = nextBackoff(backoff, cfg.BackoffStart, cfg.BackoffMax)
	}

	if cfg.Interval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}

	for {
		var sleep time.Duration
		if backoff > 0 {
			sleep = jitter(backoff)
		} else {
			sleep = jitter(cfg.Interval)
		}
		if !waitFor(ctx, sleep) {
			return ctx.Err()
		}
		err := Track(ctx, s, jobLoopLogger(logger), jobType, fn)
		if err != nil {
			if logger != nil {
				logger.Error("scheduled job failed", "type", jobType, "error", err, "backoff", backoff)
			}
			backoff = nextBackoff(backoff, cfg.BackoffStart, cfg.BackoffMax)
		} else {
			backoff = 0
		}
	}
}

// waitFor blocks for d, returning false if ctx is cancelled first.
// Uses a NewTimer so the timer is properly stopped on cancellation
// and doesn't leak into the goroutine's GC roots.
func waitFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		// Still respect a cancelled ctx so the caller can bail
		// before the first run if shutdown is already in progress.
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextBackoff returns the next backoff duration. Zero start means
// backoff is disabled; the result is always 0 in that case so the
// loop falls back to Interval. Otherwise: start → start*2 → ... up
// to max (or unbounded if max is 0).
func nextBackoff(current, start, max time.Duration) time.Duration {
	if start <= 0 {
		return 0
	}
	if current <= 0 {
		return start
	}
	next := current * 2
	if max > 0 && next > max {
		next = max
	}
	return next
}

// jobLoopLogger keeps Track from panicking when the caller passes a
// nil logger (some tests do). Track itself tolerates nil but the
// inner Start/Finish helpers want a non-nil for warnings, and the
// loop's "scheduled job failed" line above already nil-checks. This
// helper centralises the policy so future Track callers don't have
// to remember.
func jobLoopLogger(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}
