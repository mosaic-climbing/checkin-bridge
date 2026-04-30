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
