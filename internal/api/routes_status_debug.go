// Status-sync and debug endpoints split out of server.go in PR5:
//
//   POST /status/sync            — kick a UniFi status sync (admin)
//   GET  /status/sync/status     — read the in-flight result
//   POST /debug/reset-breakers   — manually close the recheck breaker
//
// The status-sync handlers use the same jobs.Track bracketing as the
// other admin sync routes; the breaker-reset endpoint exists so an
// operator can recover from a wedged-open recheck circuit without
// waiting for the cooldown.

package api

import (
	"context"
	"encoding/json"
	"net/http"
)
func (s *Server) handleStatusSync(w http.ResponseWriter, r *http.Request) {
	if s.statusSyncer == nil {
		writeError(w, http.StatusServiceUnavailable, "status syncer not configured")
		return
	}
	if s.statusSyncer.IsRunning() {
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeStatusSync, http.StatusOK, true,
				"Status sync already running",
				"A status sync kicked off earlier hasn't finished yet — watch the Last-run pill or the Recent Jobs list; it'll flip to ✓ when done.",
				nil, nil)
			return
		}
		writeJSON(w, map[string]any{
			"message": "sync already in progress — poll GET /status-sync to monitor",
			"running": true,
		})
		return
	}

	s.logger.Info("manual UniFi status sync triggered via API")
	s.audit.Log("status_sync_start", r.RemoteAddr, nil)

	// Snapshot the caller's IP so the completion audit event has the same
	// attribution as the start event. The background goroutine runs with
	// its own context, so r.RemoteAddr is not safe to read there.
	peer := r.RemoteAddr

	// Create the jobs-table row before dispatching so the initial pill
	// flips to "running" immediately on the next /ui/frag/sync-last-run
	// poll. The bg goroutine owns the completion write since the sync
	// outlives the HTTP request.
	jobID := s.startSyncJob(r.Context(), jobTypeStatusSync)

	// Run in background via the supervised group. ctx is the bridge
	// shutdown context, not the request context — so a client disconnect
	// mid-sync no longer cancels in-flight Redpoint or DB work.
	s.bg.Go("status-sync", func(ctx context.Context) error {
		result, err := s.statusSyncer.RunSync(ctx)
		if err != nil {
			s.logger.Error("background status sync failed", "error", err)
			s.audit.Log("status_sync_error", peer, map[string]any{"error": err.Error()})
			s.finishSyncJob(ctx, jobID, nil, err)
			return nil // swallow — bg.Go's error is logged but we've already handled it
		}
		s.audit.Log("status_sync_complete", peer, map[string]any{
			"activated":    result.Activated,
			"deactivated":  result.Deactivated,
			"unchanged":    result.Unchanged,
			"errors":       result.Errors,
			"newlyMatched": result.NewlyMatched,
			"newlyPending": result.NewlyPending,
			"duration":     result.Duration,
		})
		s.finishSyncJob(ctx, jobID, map[string]any{
			"activated":    result.Activated,
			"deactivated":  result.Deactivated,
			"unchanged":    result.Unchanged,
			"errors":       result.Errors,
			"newlyMatched": result.NewlyMatched,
			"newlyPending": result.NewlyPending,
			"duration":     result.Duration,
		}, nil)
		return nil
	})

	if wantsHTMX(r) {
		s.writeSyncResult(w, r, jobTypeStatusSync, http.StatusAccepted, true,
			"Status sync started",
			"Running in the background. The Last-run pill will flip to ✓ when done; you can leave this page and come back.",
			nil, nil)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message": "sync started — poll GET /status-sync to monitor",
	})
}

// GET /status-sync — check last sync result and whether a sync is running.
func (s *Server) handleStatusSyncStatus(w http.ResponseWriter, r *http.Request) {
	if s.statusSyncer == nil {
		writeError(w, http.StatusServiceUnavailable, "status syncer not configured")
		return
	}

	writeJSON(w, map[string]any{
		"running":    s.statusSyncer.IsRunning(),
		"lastResult": s.statusSyncer.LastResult(),
	})
}

// POST /debug/reset-breakers — force-close the recheck circuit breaker.
//
// Operator-invoked recovery endpoint for the case where the breaker has
// tripped (e.g. during a brief Redpoint outage) and the operator wants
// to short-circuit the 60s cooldown because they've already confirmed
// upstream is healthy. Without this, the only remediation was a bridge
// restart — which drops the UA-Hub WebSocket, pauses the check-in queue,
// and takes ~5-10s to come back.
//
// Auth: SecurityMiddleware gates this under the admin-key-OR-session
// branch (it's not /health, not /ui/*, not /ui/login). Listed in the
// audit log for both success and no-op cases so the forensic trail shows
// when an on-call operator manually overrode the breaker.
//
// Response shape is deliberately small so a curl one-liner from a
// runbook prints cleanly:
//
//	{ "ok": true, "wasOpen": true, "breaker": "recheck" }
//
// `wasOpen` distinguishes a meaningful recovery from a no-op press.
func (s *Server) handleDebugResetBreakers(w http.ResponseWriter, r *http.Request) {
	if s.breakerResetter == nil {
		writeError(w, http.StatusServiceUnavailable, "breaker resetter not configured")
		return
	}
	wasOpen := s.breakerResetter()
	peer := s.clientIP(r)
	s.logger.Info("manual breaker reset via /debug/reset-breakers",
		"breaker", "recheck",
		"wasOpen", wasOpen,
		"peer", peer,
	)
	if s.audit != nil {
		s.audit.Log("breaker_reset", peer, map[string]any{
			"breaker": "recheck",
			"wasOpen": wasOpen,
		})
	}
	writeJSON(w, map[string]any{
		"ok":      true,
		"breaker": "recheck",
		"wasOpen": wasOpen,
	})
}



// ─── Helpers ─────────────────────────────────────────────────
