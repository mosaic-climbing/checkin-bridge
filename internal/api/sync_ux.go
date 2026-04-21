package api

// v0.5.1 sync-page UX support. Split out from server.go to keep the
// scope of the staff-UI polish visible without ballooning the main
// routing file.
//
// Wire-up:
//
//   - wantsHTMX + writeSyncResult let the four sync handlers
//     (/cache/sync, /status-sync, /directory/sync, /ingest/unifi)
//     respond with a rich HTML fragment for HTMX callers and keep
//     their plain JSON body for curl/API callers.
//
//   - startSyncJob + finishSyncJob bracket the lifetime of each run
//     in the jobs table so the /ui/sync page can surface "last run"
//     pills, and the existing /ui/frag/job-table Recent Jobs list
//     populates for the first time.
//
//   - handleFragSyncLastRun backs the hx-get target on each sync
//     card so the pill advances after a click (via hx-swap-oob in
//     SyncResultFragment) and on page load.
//
// Job-type string constants are deliberately matched to the comments
// on store.Job.Type ("ingest", "cache_sync", "status_sync",
// "directory_sync") so a later telemetry/aggregation pass can key
// on them without surprises.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
)

// Sync job type labels. These are stored verbatim in jobs.type and
// also form the path segment on GET /ui/frag/sync-last-run/{type}.
const (
	jobTypeCacheSync     = "cache_sync"
	jobTypeStatusSync    = "status_sync"
	jobTypeDirectorySync = "directory_sync"
	jobTypeUniFiIngest   = "unifi_ingest"
	// jobTypeUAHubSync is the nightly (and manually-triggered) UA-Hub
	// directory mirror refresh. Parallels jobTypeDirectorySync for the
	// Redpoint side: ListAllUsersWithStatus → ua_users upsert. Added
	// in v0.5.2.
	jobTypeUAHubSync = "ua_hub_sync"
)

// wantsHTMX returns true when the request is coming from the staff UI's
// HTMX-driven fetch rather than a plain API client. The /ui/sync page
// sets X-Requested-With="XMLHttpRequest" on every hx-post (see the
// hx-headers attribute in pages/sync.html); htmx itself also sets
// HX-Request="true" by default, so accepting either header keeps the
// branch robust against future wiring changes.
func wantsHTMX(r *http.Request) bool {
	if r.Header.Get("HX-Request") == "true" {
		return true
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		return true
	}
	return false
}

// newJobID produces a human-readable, sortable job id of the form
// "<type>-<utcTimestamp>". Using a timestamp (rather than a UUID)
// keeps the jobs-table listing self-sorting in created_at order even
// without the ORDER BY, and makes grep-debugging the audit log easier
// when staff report "this ran at 14:30 and didn't finish".
func newJobID(jobType string) string {
	return fmt.Sprintf("%s-%s", jobType, time.Now().UTC().Format("20060102T150405.000Z"))
}

// startSyncJob inserts a "running" row into the jobs table and returns
// the id. Errors are logged and swallowed — job tracking is strictly
// observability and must not fail a sync. The caller gets a valid id
// either way; a silent failure just means no row is later found when
// finishSyncJob tries to update it.
func (s *Server) startSyncJob(ctx context.Context, jobType string) string {
	id := newJobID(jobType)
	if s.store == nil {
		return id
	}
	if err := s.store.CreateJob(ctx, id, jobType); err != nil {
		s.logger.Warn("startSyncJob: CreateJob failed",
			"type", jobType, "id", id, "error", err)
	}
	return id
}

// finishSyncJob transitions the given job to a terminal state. On
// success, result is marshalled into jobs.result (it's stored as JSON
// text). On error, the message lands in jobs.error and the row is
// marked failed. Both calls swallow errors — same observability-only
// rationale as startSyncJob.
//
// The incoming ctx is detached before the DB write. Rationale:
// sync refreshes routinely take 3–5 minutes (UA-Hub mirror walks
// 1600+ users), which is long enough for HTMX and/or the browser to
// give up on the POST and for the request context to be canceled.
// If we passed r.Context() straight through, FailJob/CompleteJob
// would inherit the cancellation and the SQLite UPDATE would
// silently no-op — leaving jobs.status pinned at "running" forever
// and the staff-facing "Last run" pill stuck on a non-existent
// in-flight job. Using context.WithoutCancel preserves any values
// (trace ids, request-scoped loggers) while severing the lifetime
// link, and a short fresh deadline bounds the detached write.
//
// Observed at LEF on v0.5.7: three stale "running" rows accumulated
// across two deploys, all while the actual mirror refresh completed
// cleanly in ~4–5 min. See ops/v0.5.7.1-pr-body.md for the log
// trace.
func (s *Server) finishSyncJob(ctx context.Context, id string, result any, fnErr error) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if fnErr != nil {
		if err := s.store.FailJob(ctx, id, fnErr.Error()); err != nil {
			s.logger.Warn("finishSyncJob: FailJob failed",
				"id", id, "error", err)
		}
		return
	}
	if err := s.store.CompleteJob(ctx, id, result); err != nil {
		s.logger.Warn("finishSyncJob: CompleteJob failed",
			"id", id, "error", err)
	}
}

// writeSyncResult is the single response-writing path for all four
// sync handlers. HTMX callers get a rich confirmation fragment that
// swaps into #sync-result on the /ui/sync page and OOB-refreshes the
// per-card "Last run" pill. API callers get the plain JSON body
// they've always gotten, so nothing downstream breaks. status lets
// API callers keep their existing 200/202 code.
//
// Passing success=false also flips the fragment to the red alert
// styling. For non-HTMX errors, callers should use writeError
// directly — this helper is for the success+summary case and for
// "started, come back for status" acks on async handlers.
func (s *Server) writeSyncResult(w http.ResponseWriter, r *http.Request,
	jobType string, status int, success bool,
	title, body string, stats []ui.SyncStat, apiJSON any,
) {
	if wantsHTMX(r) {
		ui.RenderFragment(w, ui.SyncResultFragment(success, title, body, stats, jobType))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if apiJSON != nil {
		_ = json.NewEncoder(w).Encode(apiJSON)
	}
}

// handleFragSyncLastRun renders a single "Last run: ..." pill for a
// given job type. The pill is keyed on type (not job id) so the
// OOB swap in SyncResultFragment can overwrite the stale copy on
// the page. Reads the most-recent row — running, completed, or
// failed — so an in-flight sync shows a spinner badge rather than
// flickering back to "completed 2m ago".
//
// Route: GET /ui/frag/sync-last-run/{type}. Registered in server.go
// alongside the other /ui/frag/* routes.
func (s *Server) handleFragSyncLastRun(w http.ResponseWriter, r *http.Request) {
	jobType := r.PathValue("type")
	if jobType == "" {
		ui.RenderFragment(w, ui.SyncLastRunPill("unknown", "", "", ""))
		return
	}
	if !isKnownSyncJobType(jobType) {
		// Defensive: don't let the client ask for arbitrary type
		// strings — we don't want /ui/frag/sync-last-run/../../etc
		// or similar quoting surprises even though PathValue
		// already strips slashes. Render the "never run" pill.
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "", "", ""))
		return
	}
	if s.store == nil {
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "", "", ""))
		return
	}
	job, err := s.store.LastJobByType(r.Context(), jobType)
	if err != nil {
		s.logger.Warn("handleFragSyncLastRun: LastJobByType failed",
			"type", jobType, "error", err)
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "failed", time.Now().UTC().Format(time.RFC3339), "lookup failed"))
		return
	}
	if job == nil {
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "", "", ""))
		return
	}
	// v0.5.7.1: pass job.Progress through so the running pill can
	// render a phase segment ("hydrating 450/1500") while the sync
	// is in flight. For completed/failed pills the progress field
	// is ignored by SyncLastRunPillFull — cheap enough to always
	// pass through rather than branching here.
	ui.RenderFragment(w, ui.SyncLastRunPillFull(jobType, job.Status, job.CreatedAt, job.Error, job.Progress))
}

// isKnownSyncJobType is the allowlist for the /ui/frag/sync-last-run
// endpoint. Keeping it centralised here means adding a new sync
// surface is a one-line change rather than a multi-file audit.
func isKnownSyncJobType(t string) bool {
	switch t {
	case jobTypeCacheSync, jobTypeStatusSync,
		jobTypeDirectorySync, jobTypeUniFiIngest,
		jobTypeUAHubSync:
		return true
	}
	return false
}

// makeJobProgressFn returns a closure that updates jobs.progress for
// the in-flight sync job. The returned func is safe to call from any
// goroutine — it detaches the parent context so progress writes
// survive HTMX/browser request abandonment (same lifetime concern
// that drove the finishSyncJob ctx detachment above), and bounds
// the SQLite UPDATE to a 5s deadline so a wedged store can't pin
// the refresh goroutine.
//
// Errors are logged at debug and dropped: progress is observability
// only and a mid-refresh write failure should not influence the
// success/failure of the underlying sync. If you need the progress
// write to be authoritative, use Store.UpdateJobProgress directly.
//
// A nil store yields a no-op closure rather than a nil func, so
// callers can always pass the result to a downstream consumer
// without a nil-check.
func (s *Server) makeJobProgressFn(jobID string) func(phase string) {
	if s.store == nil || jobID == "" {
		return func(string) {}
	}
	return func(phase string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.UpdateJobProgress(ctx, jobID, phase); err != nil {
			s.logger.Debug("makeJobProgressFn: UpdateJobProgress failed",
				"id", jobID, "phase", phase, "error", err)
		}
	}
}

// handleSyncUnstick fails the most-recent running row of a given job
// type so the staff /ui/sync page's pill clears out without staff
// having to ssh to the host and run sqlite. Defensive against the
// ctx-cancel bug that v0.5.7.1 fixes upstream — even after that fix
// ships, a daemon restart mid-refresh can still leave a row in
// 'running' forever, and operators need a one-click escape hatch.
//
// Idempotent: if there's no running row of that type, returns 204
// (or the equivalent fragment) without writing. Always responds
// with a fresh "Last run" pill via the same OOB mechanism the sync
// handlers use, so the click visibly clears the stuck state.
//
// Route: POST /ui/sync/unstick/{type}. Registered in server.go's
// routes() alongside the rest of /ui/frag/*.
func (s *Server) handleSyncUnstick(w http.ResponseWriter, r *http.Request) {
	jobType := r.PathValue("type")
	if jobType == "" || !isKnownSyncJobType(jobType) {
		// Same defensive posture as handleFragSyncLastRun: never
		// let an unknown type string reach the store layer. A
		// caller hitting /ui/sync/unstick/garbage gets the
		// "never run" pill and a 200, not a 400 — the staff page
		// shouldn't surface a debug-style error for a route the
		// user can't construct from the UI.
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "", "", ""))
		return
	}
	if s.store == nil {
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "", "", ""))
		return
	}
	job, err := s.store.ActiveJob(r.Context(), jobType)
	if err != nil {
		s.logger.Warn("handleSyncUnstick: ActiveJob failed",
			"type", jobType, "error", err)
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "failed",
			time.Now().UTC().Format(time.RFC3339), "lookup failed"))
		return
	}
	if job == nil {
		// No running row to clear — render whatever the latest
		// row is so the pill doesn't lie about state.
		last, lerr := s.store.LastJobByType(r.Context(), jobType)
		if lerr != nil || last == nil {
			ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "", "", ""))
			return
		}
		ui.RenderFragment(w, ui.SyncLastRunPillFull(jobType, last.Status,
			last.CreatedAt, last.Error, last.Progress))
		return
	}
	// Mark the running row failed with a clear human-readable
	// reason. Audit log captures the operator action for after-the-
	// fact attribution; the FailJob write itself is the substantive
	// state change.
	const reason = "manually cleared via /ui/sync (running row stuck)"
	s.audit.Log("sync_unstick", r.RemoteAddr, map[string]any{
		"jobType": jobType,
		"jobId":   job.ID,
	})
	if err := s.store.FailJob(r.Context(), job.ID, reason); err != nil {
		s.logger.Warn("handleSyncUnstick: FailJob failed",
			"id", job.ID, "type", jobType, "error", err)
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "running",
			job.CreatedAt, ""))
		return
	}
	s.logger.Info("sync job manually unstuck",
		"type", jobType, "id", job.ID,
		"createdAt", job.CreatedAt, "remoteAddr", r.RemoteAddr)
	// Re-render the pill — it should now show the failed badge
	// with the unstick reason as the tooltip. Staff can click
	// Run again immediately.
	updated, err := s.store.LastJobByType(r.Context(), jobType)
	if err != nil || updated == nil {
		ui.RenderFragment(w, ui.SyncLastRunPill(jobType, "failed",
			time.Now().UTC().Format(time.RFC3339), reason))
		return
	}
	ui.RenderFragment(w, ui.SyncLastRunPillFull(jobType, updated.Status,
		updated.CreatedAt, updated.Error, updated.Progress))
}

// Note on server-side pill pre-fetch:
//
// An earlier draft of this file carried a syncPageLastRuns struct and
// a loadSyncPageLastRuns helper, the idea being to stamp the four
// "Last run" pills into the /ui/sync HTML at render time so the page
// would arrive fully populated rather than with four in-flight
// hx-get's. We dropped it:
//
//   1. sync.html is served as static HTML by ui.Handler.ServePage —
//      no Go template substitution pass runs over it, so stamping
//      values in would mean teaching the UI handler a new
//      rendering mode just for this page.
//   2. The four pills fetch in parallel on page load and each call
//      hits a single indexed row in `jobs` — total cost is dominated
//      by the round-trip, not the query, and the page renders
//      "Loading…" pills for ~10ms at most.
//
// If we ever do want the pre-fetch, the right fix is to move
// sync.html onto html/template and inject a ui.SyncLastRunPill
// stamp per card rather than reintroducing the helper here.
