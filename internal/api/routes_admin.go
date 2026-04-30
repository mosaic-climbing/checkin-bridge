// Public-data and admin-sync handlers split out of server.go in PR5.
// Spans three coherent groups: read-mostly public routes (/health,
// /stats, /doors, /gates, /checkins, /customer/{externalId},
// /export/checkins), card-mapping CRUD (/cards), and the admin sync
// cluster (/cache/*, /ua-hub/sync, /directory/*, /ingest/unifi). The
// status-sync and debug endpoints live in routes_status_and_ui.go.

package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
)
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var totalMembers, activeMembers int
	if s.store != nil {
		if stats, err := s.store.MemberStats(r.Context()); err == nil && stats != nil {
			totalMembers = stats.Total
			activeMembers = stats.Active
		}
	}
	// instance defaults to "prod" in the response when unset, mirroring
	// config.defaults() — keeps probes that don't bother reading
	// BRIDGE_INSTANCE_NAME from seeing an empty string and assuming
	// it means stage.
	instance := s.instanceName
	if instance == "" {
		instance = "prod"
	}
	writeJSON(w, map[string]any{
		"status":             "ok",
		"service":            "mosaic-checkin-bridge",
		"instance":           instance,
		"mode":               "store-first",
		"unifiConnected":     s.unifi.Connected(),
		"cacheMembers":       totalMembers,
		"cacheActiveMembers": activeMembers,
		"cardOverrides":      len(s.cardMapper.AllOverrides()),
		"redpointGateId":     s.gateID,
		"uptime":             time.Since(startTime).String(),
		"timestamp":          time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.handler.GetStats())
}

// ─── UniFi ───────────────────────────────────────────────────

func (s *Server) handleDoors(w http.ResponseWriter, r *http.Request) {
	doors, err := s.unifi.ListDoors(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"doors": doors})
}

// ─── Redpoint ────────────────────────────────────────────────

func (s *Server) handleGates(w http.ResponseWriter, r *http.Request) {
	gates, err := s.redpoint.ListGates(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"gates": gates,
		"hint":  "Set REDPOINT_GATE_ID in .env to the id of your entrance gate",
	})
}

// handleCheckins returns the last N check-ins. Source is controlled by the
// `source` query parameter:
//
//   - `source=local` (default): reads from the bridge's own sqlite `checkins`
//     table. Free (no outbound calls), returns denied/shadow events too, and
//     is what the UI fragments use. This is the right choice for polling.
//   - `source=redpoint`: proxies live to Redpoint's GraphQL API. Returns only
//     what Redpoint has recorded; each call costs Redpoint quota. Use only
//     when callers specifically want the authoritative Redpoint view.
//
// Response envelope is identical for both sources:
//
//	{"checkIns": [...source-native items...], "total": N, "source": "local|redpoint"}
//
// Item shape DIFFERS between sources — local events are flat (timestamp,
// customerId, customerName, doorId, doorName, result, unifiResult,
// redpointRecorded); Redpoint items have nested customer/gate/facility
// objects. Clients should branch on the `source` field if they need to
// interpret individual items.
//
// See P2 in docs/architecture-review.md — before this fix, the default was
// `redpoint`, which meant a single UI tab polling every few seconds cost
// ~28k Redpoint calls/day.
func (s *Server) handleCheckins(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500 // hard cap to prevent accidental fanout
	}

	source := r.URL.Query().Get("source")
	if source == "" {
		source = "local"
	}

	switch source {
	case "local":
		if s.store == nil {
			writeError(w, http.StatusServiceUnavailable, "local store not configured")
			return
		}
		events, err := s.store.RecentCheckIns(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"checkIns": events,
			"total":    len(events),
			"source":   "local",
		})
	case "redpoint":
		list, err := s.redpoint.ListRecentCheckIns(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		checkIns := list.CheckIns
		if checkIns == nil {
			checkIns = []redpoint.CheckIn{}
		}
		writeJSON(w, map[string]any{
			"checkIns": checkIns,
			"total":    list.Total,
			"source":   "redpoint",
		})
	default:
		writeError(w, http.StatusBadRequest, "source must be 'local' or 'redpoint'")
	}
}

// handleExportCheckins streams the local store's check-in events for a given
// date range as either CSV (default) or JSON. Sources from the bridge's own
// sqlite database — not Redpoint — so it includes denied events, shadow-mode
// decisions, and the UniFi result column that live in our store only.
//
// Query params:
//   from=YYYY-MM-DD or RFC3339    (optional — unbounded if empty)
//   to=YYYY-MM-DD or RFC3339      (optional — unbounded if empty, bare dates
//                                  are expanded to end-of-day inside the store)
//   format=csv|json               (default: csv)
//
// Admin-auth only: this route is not in the public middleware allowlist, so
// the security middleware requires admin API key or a staff session.
func (s *Server) handleExportCheckins(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}

	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "csv"
	}

	events, err := s.store.CheckInsBetween(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}

	// Filename label: use date range if provided, else "all".
	label := "all"
	if from != "" || to != "" {
		label = strings.TrimSpace(from + "_to_" + to)
	}

	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="checkins_%s.json"`, label))
		writeJSON(w, map[string]any{
			"from":   from,
			"to":     to,
			"count":  len(events),
			"events": events,
		})
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="checkins_%s.csv"`, label))
		cw := csv.NewWriter(w)
		// Header row — keep column order stable so downstream parsers don't
		// need to rediscover schema between exports.
		_ = cw.Write([]string{
			"id", "timestamp", "nfc_uid", "customer_id", "customer_name",
			"door_id", "door_name", "result", "deny_reason",
			"redpoint_recorded", "redpoint_checkin_id", "unifi_result",
		})
		for _, e := range events {
			_ = cw.Write([]string{
				strconv.Itoa(e.ID),
				e.Timestamp,
				e.NfcUID,
				e.CustomerID,
				e.CustomerName,
				e.DoorID,
				e.DoorName,
				e.Result,
				e.DenyReason,
				strconv.FormatBool(e.RedpointRecorded),
				e.RedpointCheckInID,
				e.UnifiResult,
			})
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			s.logger.Error("csv export flush failed", "error", err)
		}
	default:
		writeError(w, http.StatusBadRequest, "format must be csv or json")
	}
}

func (s *Server) handleCustomerLookup(w http.ResponseWriter, r *http.Request) {
	extID := r.PathValue("externalId")

	// Show both live Redpoint data and cached data for comparison
	resp := map[string]any{}

	// Check local store first (always available)
	if s.store != nil {
		if member, err := s.store.GetMemberByNFC(r.Context(), extID); err == nil && member != nil {
			resp["cached"] = member
			resp["cachedAllowed"] = member.IsAllowed()
		}
	}

	// Also try live Redpoint lookup
	cust, err := s.redpoint.LookupByExternalID(r.Context(), extID)
	if err != nil {
		resp["redpointError"] = err.Error()
	} else if cust == nil {
		resp["redpointCustomer"] = nil
	} else {
		validation := s.redpoint.ValidateCheckIn(cust)
		resp["redpointCustomer"] = cust
		resp["redpointValidation"] = validation
	}

	if len(resp) == 0 {
		writeError(w, http.StatusNotFound, "not found in cache or Redpoint")
		return
	}
	writeJSON(w, resp)
}

// ─── Card Override Mappings ──────────────────────────────────

func (s *Server) handleListCards(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"overrides": s.cardMapper.AllOverrides(),
	})
}

func (s *Server) handleAddCard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CardUID    string `json:"cardUid"`
		CustomerID string `json:"customerId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.CardUID == "" || body.CustomerID == "" {
		writeError(w, http.StatusBadRequest, "cardUid and customerId are required")
		return
	}
	if err := s.cardMapper.SetOverride(body.CardUID, body.CustomerID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Log("card_override_add", r.RemoteAddr, map[string]any{
		"cardUid": body.CardUID, "customerId": body.CustomerID,
	})
	s.htmlCache.Invalidate()
	writeJSON(w, map[string]any{"success": true, "cardUid": body.CardUID, "customerId": body.CustomerID})
}

func (s *Server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	cardUID := r.PathValue("cardUid")
	if err := s.cardMapper.DeleteOverride(cardUID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Log("card_override_delete", r.RemoteAddr, map[string]any{"cardUid": cardUID})
	s.htmlCache.Invalidate()
	writeJSON(w, map[string]any{"success": true})
}

// ─── Cache Management ────────────────────────────────────────

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, map[string]any{})
		return
	}
	stats, err := s.store.MemberStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, stats)
}

func (s *Server) handleCacheMembers(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, map[string]any{"members": []any{}})
		return
	}
	members, err := s.store.AllMembers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"members": members})
}

// handleCacheSync refreshes every cached member's status against Redpoint
// in a single synchronous pass. The call is wrapped in a jobs-table
// lifecycle (running → completed / failed) so the /ui/sync page's "Last
// run" pill and Recent Jobs list can show staff that a sync fired. The
// HTMX response is a rich confirmation fragment; API callers still get
// the original JSON body.
func (s *Server) handleCacheSync(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("manual membership status refresh triggered via API")
	s.audit.Log("cache_sync", r.RemoteAddr, nil)

	jobID := s.startSyncJob(r.Context(), jobTypeCacheSync)

	started := time.Now()
	refreshErr := s.syncer.RefreshAllStatuses(r.Context())

	var stats *store.MemberStats
	if s.store != nil {
		stats, _ = s.store.MemberStats(r.Context())
	}
	duration := time.Since(started).Round(100 * time.Millisecond)

	if refreshErr != nil {
		s.finishSyncJob(r.Context(), jobID, nil, refreshErr)
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeCacheSync, http.StatusOK, false,
				"Cache sync failed",
				"Refresh against Redpoint returned an error. Leaving member cache as it was.",
				[]ui.SyncStat{
					{Label: "Error", Value: refreshErr.Error()},
					{Label: "Duration", Value: duration.String()},
				}, nil)
			return
		}
		writeError(w, http.StatusBadGateway, "status refresh failed: "+refreshErr.Error())
		return
	}

	s.finishSyncJob(r.Context(), jobID, map[string]any{
		"cache":    stats,
		"duration": duration.String(),
	}, nil)

	s.writeSyncResult(w, r, jobTypeCacheSync, http.StatusOK, true,
		"Cache sync complete",
		"Refreshed every cached member's status from Redpoint.",
		syncStatsFromMemberStats(stats, duration),
		map[string]any{
			"success": true,
			"cache":   stats,
		})
}

// handleUAHubSync refreshes the local UA-Hub directory mirror (ua_users)
// synchronously. Added in v0.5.2 alongside the nightly unifimirror
// Syncer — the Syncer owns the daily cadence; this handler lets staff
// force an immediate refresh after a UA-Hub-side edit without waiting
// for the next tick.
//
// Shape mirrors handleCacheSync on purpose: jobs-table lifecycle for
// the /ui/sync page's "last run" pill, HTMX-aware response via
// writeSyncResult, plain JSON fallback for API callers. The refresher
// callback is wired via SetUAHubMirrorRefresher from cmd/bridge;
// when unset, we 503 with a clear message rather than silently
// succeed, so operators notice the wiring gap in dev builds.
func (s *Server) handleUAHubSync(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("manual UA-Hub directory mirror refresh triggered via API")
	s.audit.Log("ua_hub_sync", r.RemoteAddr, nil)

	if s.uaHubMirrorRefresh == nil {
		writeError(w, http.StatusServiceUnavailable, "UA-Hub mirror refresher not wired")
		return
	}

	jobID := s.startSyncJob(r.Context(), jobTypeUAHubSync)
	started := time.Now()

	// progress writes survive request abandonment (HTMX/browser
	// cancel after a few minutes) for the same reason finishSyncJob
	// detaches its ctx — the request is gone but the refresh isn't,
	// and the staff pill should still tick. See sync_ux.go's
	// finishSyncJob comment for the full lifetime story.
	progress := s.makeJobProgressFn(jobID)
	stats, refreshErr := s.uaHubMirrorRefresh(r.Context(), progress)
	duration := time.Since(started).Round(100 * time.Millisecond)

	if refreshErr != nil {
		s.finishSyncJob(r.Context(), jobID, nil, refreshErr)
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeUAHubSync, http.StatusOK, false,
				"UA-Hub sync failed",
				"Couldn't complete the UA-Hub directory refresh. The local mirror is unchanged.",
				[]ui.SyncStat{
					{Label: "Error", Value: refreshErr.Error()},
					{Label: "Duration", Value: duration.String()},
				}, nil)
			return
		}
		writeError(w, http.StatusBadGateway, "UA-Hub sync failed: "+refreshErr.Error())
		return
	}

	s.finishSyncJob(r.Context(), jobID, map[string]any{
		"observed":    stats.Observed,
		"upserted":    stats.Upserted,
		"hydrated":    stats.Hydrated,
		"rechecked":   stats.Rechecked,
		"mirrorTotal": stats.MirrorTotal,
		"duration":    duration.String(),
	}, nil)

	s.writeSyncResult(w, r, jobTypeUAHubSync, http.StatusOK, true,
		"UA-Hub sync complete",
		"Refreshed the local UA-Hub user directory mirror. The Needs Match page and ingest matcher now read from this cache instead of hitting UA-Hub live.",
		[]ui.SyncStat{
			{Label: "Observed", Value: fmt.Sprintf("%d", stats.Observed)},
			{Label: "Upserted", Value: fmt.Sprintf("%d", stats.Upserted)},
			{Label: "Hydrated", Value: fmt.Sprintf("%d", stats.Hydrated)},
			{Label: "Rechecked", Value: fmt.Sprintf("%d", stats.Rechecked)},
			{Label: "Mirror total", Value: fmt.Sprintf("%d", stats.MirrorTotal)},
			{Label: "Duration", Value: duration.String()},
		},
		map[string]any{
			"success":     true,
			"observed":    stats.Observed,
			"upserted":    stats.Upserted,
			"hydrated":    stats.Hydrated,
			"rechecked":   stats.Rechecked,
			"mirrorTotal": stats.MirrorTotal,
		})
}

// syncStatsFromMemberStats unpacks *store.MemberStats into the uniform
// []ui.SyncStat rows the fragment renders. Nil stats (store absent or
// read failure) degrades to just the duration row rather than blowing
// up the response.
func syncStatsFromMemberStats(stats *store.MemberStats, duration time.Duration) []ui.SyncStat {
	rows := []ui.SyncStat{{Label: "Duration", Value: duration.String()}}
	if stats == nil {
		return rows
	}
	rows = append(rows,
		ui.SyncStat{Label: "Members total", Value: fmt.Sprintf("%d", stats.Total)},
		ui.SyncStat{Label: "Active", Value: fmt.Sprintf("%d", stats.Active)},
	)
	if stats.Frozen > 0 {
		rows = append(rows, ui.SyncStat{Label: "Frozen", Value: fmt.Sprintf("%d", stats.Frozen)})
	}
	if stats.Expired > 0 {
		rows = append(rows, ui.SyncStat{Label: "Expired", Value: fmt.Sprintf("%d", stats.Expired)})
	}
	return rows
}

// ─── Customer Directory (SQLite) ─────────────────────────────

// GET /directory/status — check the Redpoint → SQLite sync status.
func (s *Server) handleDirectoryStatus(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, map[string]any{"customers": 0})
		return
	}
	count, _ := s.store.CustomerCount(r.Context())
	state, _ := s.store.GetSyncState(r.Context())
	writeJSON(w, map[string]any{
		"customers": count,
		"sync":      state,
	})
}

// POST /directory/sync — start the Redpoint → SQLite bulk load.
// Runs in the background; poll GET /directory/status to monitor progress.
func (s *Server) handleDirectorySync(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	state, _ := s.store.GetSyncState(r.Context())
	if state != nil && state.Status == "running" {
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeDirectorySync, http.StatusOK, true,
				"Directory sync already running",
				"A bulk customer load kicked off earlier is still in progress. Wait for the Last-run pill to flip to ✓, or check /directory/status for a row count.",
				nil, nil)
			return
		}
		writeJSON(w, map[string]any{
			"message": "sync already in progress",
			"sync":    state,
		})
		return
	}

	s.logger.Info("Redpoint → SQLite directory sync triggered via API")

	jobID := s.startSyncJob(r.Context(), jobTypeDirectorySync)

	// Run in background via the supervised group. The jobID is captured
	// by value so the goroutine can stamp the terminal status even after
	// the request context has long returned. Delegates to
	// RunDirectorySync so the scheduled directory-syncer in cmd/bridge
	// shares the same code path as this manual button.
	s.bg.Go("directory-sync", func(ctx context.Context) error {
		res, err := s.RunDirectorySync(ctx)
		if err != nil {
			s.finishSyncJob(ctx, jobID, nil, err)
			return nil
		}
		s.finishSyncJob(ctx, jobID, map[string]any{
			"duration":     res.Duration.String(),
			"totalFetched": res.TotalFetched,
			"completedAt":  res.CompletedAt,
		}, nil)
		return nil
	})

	if wantsHTMX(r) {
		s.writeSyncResult(w, r, jobTypeDirectorySync, http.StatusAccepted, true,
			"Directory sync started",
			"Bulk-loading every active Redpoint customer into the local mirror. This can take several minutes for large directories; the Last-run pill will flip to ✓ when done.",
			nil, nil)
		return
	}

	writeJSON(w, map[string]any{
		"message": "sync started — poll GET /directory/status to monitor",
	})
}

// DirectorySyncResult is the outcome of one full RunDirectorySync pass.
// Returned by both the manual POST /directory/sync handler and the
// scheduled directory-syncer goroutine in cmd/bridge so callers can
// build a uniform jobs.result body without round-tripping through
// sync_state. Duration is rounded to whole seconds because the walk
// is minutes-long and sub-second precision is noise.
type DirectorySyncResult struct {
	TotalFetched int
	Duration     time.Duration
	CompletedAt  string
}

// RunDirectorySync performs one full Redpoint → SQLite directory walk
// and returns the outcome. The walker writes its progress through
// sync_state as it goes (so /directory/status keeps reporting the
// in-flight cursor), and on completion the final sync_state row drives
// the returned result. An "error" status in sync_state surfaces as a
// non-nil error so callers can fail their job row.
//
// Used by:
//   - POST /directory/sync (handleDirectorySync) — manual button.
//   - The scheduled directory-syncer goroutine (cmd/bridge), which
//     wraps each call in jobs.Track so the run lands in the jobs
//     table the same way manual triggers do.
//
// Both call sites read the same sync_state guard via GetSyncState
// before calling — RunDirectorySync itself does NOT check for an
// already-running sync, since the existing-walk-in-progress case is
// a UX concern (different messaging for manual vs scheduled callers).
func (s *Server) RunDirectorySync(ctx context.Context) (DirectorySyncResult, error) {
	if s.store == nil {
		return DirectorySyncResult{}, fmt.Errorf("store not available")
	}
	started := time.Now()
	s.bulkLoadCustomers(ctx)
	finalState, _ := s.store.GetSyncState(ctx)
	if finalState != nil && finalState.Status == "error" {
		return DirectorySyncResult{}, fmt.Errorf("%s", finalState.LastError)
	}
	res := DirectorySyncResult{Duration: time.Since(started).Round(time.Second)}
	if finalState != nil {
		res.TotalFetched = finalState.TotalFetched
		res.CompletedAt = finalState.CompletedAt
	}
	return res, nil
}

// bulkLoadCustomers pages through all Redpoint customers and upserts them into the store.
func (s *Server) bulkLoadCustomers(ctx context.Context) {
	s.store.UpdateSyncState(ctx, &store.SyncState{
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})

	var cursor *string
	totalFetched := 0
	pageSize := 100

	for {
		vars := map[string]any{
			"filter": map[string]any{"active": "ACTIVE"},
			"first":  pageSize,
		}
		if cursor != nil {
			vars["after"] = *cursor
		}

		data, err := s.redpoint.ExecQuery(ctx, `
			query Customers($filter: CustomerFilter!, $first: Int, $after: String) {
				customers(filter: $filter, first: $first, after: $after) {
					pageInfo { hasNextPage endCursor }
					edges {
						node {
							id active firstName lastName email barcode externalId
						}
					}
				}
			}
		`, vars)
		if err != nil {
			s.logger.Error("directory sync page fetch failed", "error", err)
			s.store.UpdateSyncState(ctx, &store.SyncState{
				Status:    "error",
				LastError: err.Error(),
			})
			return
		}

		var result struct {
			Customers struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Edges []struct {
					Node struct {
						ID         string `json:"id"`
						Active     bool   `json:"active"`
						FirstName  string `json:"firstName"`
						LastName   string `json:"lastName"`
						Email      string `json:"email"`
						Barcode    string `json:"barcode"`
						ExternalID string `json:"externalId"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"customers"`
		}

		if err := json.Unmarshal(data, &result); err != nil {
			s.logger.Error("directory sync parse failed", "error", err)
			s.store.UpdateSyncState(ctx, &store.SyncState{
				Status:    "error",
				LastError: err.Error(),
			})
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		batch := make([]store.Customer, len(result.Customers.Edges))
		for i, e := range result.Customers.Edges {
			batch[i] = store.Customer{
				RedpointID: e.Node.ID,
				FirstName:  e.Node.FirstName,
				LastName:   e.Node.LastName,
				Email:      e.Node.Email,
				Barcode:    e.Node.Barcode,
				ExternalID: e.Node.ExternalID,
				Active:     e.Node.Active,
				UpdatedAt:  now,
			}
		}

		if err := s.store.UpsertCustomerBatch(ctx, batch); err != nil {
			s.logger.Error("directory sync batch upsert failed", "error", err)
			s.store.UpdateSyncState(ctx, &store.SyncState{
				Status:    "error",
				LastError: err.Error(),
			})
			return
		}

		totalFetched += len(batch)
		s.logger.Info("directory sync progress", "fetched", totalFetched)

		if !result.Customers.PageInfo.HasNextPage {
			break
		}
		endCursor := result.Customers.PageInfo.EndCursor
		cursor = &endCursor

		s.store.UpdateSyncState(ctx, &store.SyncState{
			Status:       "running",
			TotalFetched: totalFetched,
			LastCursor:   endCursor,
		})
	}

	s.store.UpdateSyncState(ctx, &store.SyncState{
		Status:       "complete",
		TotalFetched: totalFetched,
		CompletedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	s.logger.Info("directory sync complete", "total", totalFetched)
}

// ─── UniFi Ingest ────────────────────────────────────────────

// GET /unifi/users — list all UniFi Access users with NFC credentials.
// Useful for seeing who has NFC tags before running the ingest.
func (s *Server) handleUniFiUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.unifi.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch UniFi users: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"users": users,
		"count": len(users),
	})
}

// POST /ingest/unifi — run the UniFi → Redpoint user mapping pipeline.
//
// Query params:
//
//	?dry_run=true  (default) — preview mappings without writing to cache
//	?dry_run=false           — match users and write to cache
//
// Flow:
//  1. Fetches all UniFi users with NFC credentials
//  2. Fetches all active Redpoint customers
//  3. Matches by email (primary), then by name (fallback)
//  4. Returns full mapping table for review
//  5. If dry_run=false, writes matched+active members to cache
func (s *Server) handleIngestUniFi(w http.ResponseWriter, r *http.Request) {
	dryRun := true
	if r.URL.Query().Get("dry_run") == "false" {
		dryRun = false
	}

	s.logger.Info("UniFi ingest triggered", "dryRun", dryRun)
	s.audit.Log("ingest_start", r.RemoteAddr, map[string]any{"dryRun": dryRun})

	jobID := s.startSyncJob(r.Context(), jobTypeUniFiIngest)
	started := time.Now()

	result, err := s.ingester.Run(r.Context(), dryRun)
	if err != nil {
		s.finishSyncJob(r.Context(), jobID, nil, err)
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeUniFiIngest, http.StatusOK, false,
				"UniFi ingest failed",
				"Couldn't complete the UniFi → Redpoint match pass. Members table unchanged.",
				[]ui.SyncStat{
					{Label: "Error", Value: err.Error()},
					{Label: "Duration", Value: time.Since(started).Round(100 * time.Millisecond).String()},
				}, nil)
			return
		}
		writeError(w, http.StatusBadGateway, "ingest failed: "+err.Error())
		return
	}
	s.audit.Log("ingest_complete", r.RemoteAddr, map[string]any{
		"dryRun": dryRun, "matched": result.Matched,
		"unmatched": result.Unmatched, "applied": result.Applied,
	})
	duration := time.Since(started).Round(100 * time.Millisecond)
	s.finishSyncJob(r.Context(), jobID, map[string]any{
		"dryRun":     dryRun,
		"unifiUsers": result.UniFiUsers,
		"withNfc":    result.WithNFC,
		"matched":    result.Matched,
		"unmatched":  result.Unmatched,
		"applied":    result.Applied,
		"duration":   duration.String(),
	}, nil)

	if wantsHTMX(r) {
		title := "UniFi ingest complete"
		body := fmt.Sprintf("Scanned %d UniFi users (%d with NFC tags) and resolved them against Redpoint. Wrote %d members to the cache.",
			result.UniFiUsers, result.WithNFC, result.Applied)
		if dryRun {
			title = "UniFi ingest — dry run"
			body = fmt.Sprintf("Previewed %d UniFi users (%d with NFC tags) against Redpoint. No writes to the members table. Click \"Run (writes)\" to apply.",
				result.UniFiUsers, result.WithNFC)
		}
		s.writeSyncResult(w, r, jobTypeUniFiIngest, http.StatusOK, true,
			title, body,
			[]ui.SyncStat{
				{Label: "UniFi users", Value: fmt.Sprintf("%d", result.UniFiUsers)},
				{Label: "With NFC", Value: fmt.Sprintf("%d", result.WithNFC)},
				{Label: "Matched", Value: fmt.Sprintf("%d", result.Matched)},
				{Label: "Unmatched", Value: fmt.Sprintf("%d", result.Unmatched)},
				{Label: "Applied", Value: fmt.Sprintf("%d", result.Applied)},
				{Label: "Duration", Value: duration.String()},
			}, nil)
		return
	}

	// ?summary=true returns counts + unmatched/warning list only (no full mappings)
	if r.URL.Query().Get("summary") == "true" {
		type problemEntry struct {
			UniFiName string `json:"unifiName"`
			Warning   string `json:"warning"`
		}
		var unmatched []problemEntry
		var warnings []problemEntry
		for _, m := range result.Mappings {
			if m.Method == ingest.MatchNone {
				unmatched = append(unmatched, problemEntry{m.UniFiName, m.Warning})
			} else if m.Warning != "" {
				warnings = append(warnings, problemEntry{m.UniFiName, m.Warning})
			}
		}
		writeJSON(w, map[string]any{
			"timestamp":  result.Timestamp,
			"dryRun":     result.DryRun,
			"unifiUsers": result.UniFiUsers,
			"withNfc":    result.WithNFC,
			"matched":    result.Matched,
			"unmatched":  result.Unmatched,
			"skipped":    result.Skipped,
			"applied":    result.Applied,
			"unmatchedUsers": unmatched,
			"warningUsers":   warnings,
		})
		return
	}

	writeJSON(w, result)
}

// ─── Testing & Manual Control ────────────────────────────────
//
// handleTestCheckin lives in testhooks_on.go (build tag: devhooks). The
// default production build compiles testhooks_off.go instead, which
// registers no routes. See registerTestHooks above and S5 in the review.
