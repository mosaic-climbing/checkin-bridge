package api

import (
	"context"
	"net/http"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// ─── Mirror admin endpoints ──────────────────────────────────────
//
// These two routes ride the control plane (see s.controlMux registration
// in routes()) and together provide the operator surface for the
// mirror.Walker:
//
//	POST /admin/mirror/resync  — kick off a run in the background
//	GET  /admin/mirror/stats   — inspect current sync_state + counts
//
// The Walker itself lives in internal/mirror; this file is a thin HTTP
// façade. Keeping the wiring thin is deliberate: if the mirror grows
// extra endpoints later (pause, cancel, tail-logs), they slot in here
// without touching the Walker's logic.
//
// Concurrency control:
//
//   - /resync first reads sync_state. If the previous run is still
//     "running" AND started_at is within the stale window, we reject
//     with 409 Conflict. Stale runs (process crashed mid-walk and a
//     newer one is requested) must be explicitly resume-able, so we
//     let them through — the Walker's own GetSyncState check then
//     picks up from LastCursor instead of restarting.
//
//   - Multiple concurrent POSTs are defended by the 409 check itself.
//     If two arrive in the same instant before either has updated
//     sync_state, the second one will still see the first's StartSync
//     transition because both go through s.store's single-writer lock.
//     Not a perfect mutex, but tight enough for an admin endpoint the
//     operator invokes manually or via cron.

// staleWindow is how long a "running" sync_state entry can sit before
// we consider the run dead and allow a new one to start. 30 minutes
// fits a single 900-row walk comfortably (9 pages × 2s inter-page
// delay = <30s in the happy path) while leaving headroom for a
// retry-heavy run hitting Retry-After hints.
const mirrorStaleWindow = 30 * time.Minute

// handleMirrorResync starts a mirror walk in the background.
//
// Response shapes:
//
//	202 Accepted  — walk dispatched; body reports the state we
//	                transitioned from (idle/error/resume).
//	409 Conflict  — a fresh "running" state is present; operator
//	                should wait or wait past the stale window.
//	503 Unavailable — the walker callback wasn't wired (mis-boot).
//	500           — sync_state read failure; the underlying error
//	                surfaces in the response body.
func (s *Server) handleMirrorResync(w http.ResponseWriter, r *http.Request) {
	if s.mirrorWalk == nil {
		writeError(w, http.StatusServiceUnavailable, "mirror walker not configured")
		return
	}

	state, err := s.store.GetSyncState(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 409 if a running sync was started recently. "Recently" here is
	// a wall-clock window — we can't get a precise liveness signal
	// from a crashed bridge, so the stale window is a pragmatic
	// upper bound.
	if state != nil && state.Status == "running" {
		if fresh, err := startedWithin(state.StartedAt, mirrorStaleWindow); err != nil {
			// Malformed started_at — treat as non-fresh and let the
			// walk claim. Log for operator follow-up.
			s.logger.Warn("mirror resync: could not parse started_at, treating as stale",
				"started_at", state.StartedAt, "error", err)
		} else if fresh {
			writeError(w, http.StatusConflict, "mirror walk already in progress")
			return
		}
	}

	// Background-dispatch the walk under bg.Group so its lifetime
	// ties to shutdown and it shows up in /stats' goroutine gauge.
	// We ignore the return value from bg.Go — the walk's outcome is
	// reflected in sync_state, not the HTTP response.
	peer := s.clientIP(r)
	s.logger.Info("mirror resync requested",
		"peer", peer,
		"previous_status", statusOrEmpty(state))
	s.bg.Go("mirror-walk", func(ctx context.Context) error {
		if err := s.mirrorWalk(ctx); err != nil {
			// Walker already persists its own error state; we just
			// log so the operator sees "walk ended badly" in the
			// service log even if they never poll /stats.
			s.logger.Warn("mirror walk ended with error", "error", err)
			return err
		}
		return nil
	})

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{
		"status":         "dispatched",
		"previousStatus": statusOrEmpty(state),
		"resumed":        state != nil && state.LastCursor != "" && state.Status != "complete",
	})
}

// handleMirrorStats returns the current sync_state plus a breakdown
// of customers by badge_status. Used by operators to answer "how far
// through the walk are we?" and "what does the mirror think our
// active-member count is?" in one request.
//
// Shape is deliberately flat JSON — operators hit this from curl more
// often than from a web UI. Tools like `jq .counts.ACTIVE` should be
// trivial.
func (s *Server) handleMirrorStats(w http.ResponseWriter, r *http.Request) {
	state, err := s.store.GetSyncState(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	counts, err := s.store.CountByBadgeStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	total, err := s.store.CustomerCount(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, map[string]any{
		"syncState":    state,
		"counts":       counts,
		"totalRows":    total,
		"staleWindowS": int(mirrorStaleWindow.Seconds()),
	})
}

// startedWithin reports whether startedAt (RFC3339) is within d of now.
// An empty startedAt returns (false, nil) — treat as stale.
func startedWithin(startedAt string, d time.Duration) (bool, error) {
	if startedAt == "" {
		return false, nil
	}
	t, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return false, err
	}
	return time.Since(t) <= d, nil
}

// statusOrEmpty is a nil-safe accessor for state.Status so JSON
// responses don't blow up when sync_state has never been touched.
func statusOrEmpty(s *store.SyncState) string {
	if s == nil {
		return ""
	}
	return s.Status
}
