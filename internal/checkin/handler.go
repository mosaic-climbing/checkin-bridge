// Package checkin orchestrates the check-in flow.
//
// STORE-FIRST ARCHITECTURE:
//   The door unlock decision uses the unified store for member data and access policies.
//   Member lookups and check-in recording happen through the store.
//   No network call to Redpoint happens in the critical path.
//
//   Fast path (NFC tap → door unlock):
//     1. Resolve card UID → nfcUID (via card mapper overrides or passthrough)
//     2. Look up nfcUID in store (~0ms)
//     3. Evaluate door policy: policy.EvaluateAccess(member) → unlock, deny, or recheck
//     4. Unlock the door via UniFi REST API
//
//   Background (async, does not block the door):
//     5. Record the check-in event in store
//     6. Record the check-in in Redpoint via GraphQL (if applicable)
//
//   Store integration (separate goroutines):
//     - Member sync from Redpoint on startup + periodic updates
//     - Every successful live check-in records a CheckInEvent in store
//     - Door policy evaluation is decoupled from member data
package checkin

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/recheck"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

type Stats struct {
	TotalEvents        int64      `json:"totalEvents"`
	SuccessfulCheckins int64      `json:"successfulCheckins"`
	DeniedCheckins     int64      `json:"deniedCheckins"`
	DuplicateCheckins  int64      `json:"duplicateCheckins"`
	Errors             int64      `json:"errors"`
	LastEvent          *time.Time `json:"lastEvent"`
	LastCheckin        *LastCI    `json:"lastCheckin"`
}

type LastCI struct {
	Member string `json:"member"`
	Badge  string `json:"badge"`
	Door   string `json:"door"`
	Time   string `json:"time"`
}

type Handler struct {
	unifiClient    *unifi.Client
	redpointClient *redpoint.Client
	cardMapper     *cardmap.Mapper
	store          *store.Store
	// rechecker performs the live "did this denied member just renew?"
	// query against Redpoint. nil disables the recheck path entirely
	// (denials short-circuit straight to recordDeniedEvent). Pre-A3 this
	// was a *statusync.Syncer; the dependency is now a narrow interface
	// (recheck.Rechecker) so tests can stub it without spinning up
	// statusync, and the implementation lives in its own package
	// (internal/recheck) with its own breaker and config knobs.
	rechecker recheck.Rechecker
	gateID    string
	logger    *slog.Logger

	// shadowMode and metrics are settable post-construction (SetShadowMode /
	// SetMetrics) and read on the tap hot path. Atomic so the operator
	// toggling shadow mode from the UI can't race against an in-flight
	// HandleEvent — a torn read here is exactly the safety failure shadow
	// mode is meant to prevent.
	shadowMode atomic.Bool
	metrics    atomic.Pointer[metrics.Registry]

	// asyncWG tracks goroutines spawned for background Redpoint writes. The
	// main shutdown path waits on this so we don't lose in-flight check-ins
	// when the process is told to exit — the door has already unlocked, and
	// losing the Redpoint record means a paying member shows as "no
	// check-in today" in HQ reports.
	asyncWG sync.WaitGroup

	mu    sync.Mutex
	stats Stats
}

func NewHandler(
	unifiClient *unifi.Client,
	redpointClient *redpoint.Client,
	cardMapper *cardmap.Mapper,
	s *store.Store,
	gateID string,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		unifiClient:    unifiClient,
		redpointClient: redpointClient,
		cardMapper:     cardMapper,
		store:          s,
		gateID:         gateID,
		logger:         logger,
	}
}

// SetShadowMode toggles shadow mode. When on, the handler performs all lookups
// and logs every decision but never calls UniFi unlock or Redpoint createCheckIn.
// Intended for parallel-run deployments that mirror production traffic without
// mutating either system.
func (h *Handler) SetShadowMode(on bool) {
	h.shadowMode.Store(on)
}

// SetRechecker attaches the denied-tap recheck implementation. Called
// after both the handler and the recheck.Service are initialized in
// cmd/bridge. nil disables the recheck path (denials are immediate).
//
// Renamed from SetStatusSyncer in A3 — the recheck is no longer
// owned by the status syncer.
func (h *Handler) SetRechecker(r recheck.Rechecker) {
	h.rechecker = r
}

// SetMetrics attaches the metrics registry for disagreement alerting.
// Optional — if Load returns nil, the handler silently skips metric
// updates. Call sites snapshot the pointer with Load() so a registry
// swap mid-flight cannot tear an Inc/Dec pair across two registries.
func (h *Handler) SetMetrics(m *metrics.Registry) {
	h.metrics.Store(m)
}

// noteDisagreement compares the bridge's decision (bridgeResult: "allowed",
// "denied", or "recheck_allowed") against the UA-Hub's own verdict
// (event.Result: "ACCESS" or "BLOCKED") and, in live mode, emits a warn log
// and bumps the disagreement counter so operators notice drift.
//
// In shadow mode the same disagreements are recorded in the store and shown
// in the shadow-decisions panel; we don't double-log from here to keep
// shadow-mode output quiet.
func (h *Handler) noteDisagreement(event unifi.AccessEvent, member *store.Member, bridgeResult, denyReason string) {
	if h.shadowMode.Load() {
		return
	}
	unifiResult := strings.ToUpper(event.Result)
	if unifiResult == "" {
		return
	}
	bridgeAllowed := bridgeResult == "allowed" || bridgeResult == "recheck_allowed"
	unifiAllowed := unifiResult == "ACCESS"
	if bridgeAllowed == unifiAllowed {
		return // agree
	}

	memberName := "Unknown"
	customerID := ""
	if member != nil {
		memberName = member.FullName()
		customerID = member.CustomerID
	}

	if bridgeAllowed && !unifiAllowed {
		h.logger.Warn("LIVE DISAGREEMENT: bridge allowed, UniFi would have BLOCKED",
			"member", memberName,
			"customerId", customerID,
			"door", event.DoorName,
			"bridgeResult", bridgeResult,
		)
	} else {
		h.logger.Warn("LIVE DISAGREEMENT: bridge denied, UniFi would have ALLOWED",
			"member", memberName,
			"customerId", customerID,
			"door", event.DoorName,
			"bridgeResult", bridgeResult,
			"denyReason", denyReason,
		)
	}

	if m := h.metrics.Load(); m != nil {
		m.Counter("decision_disagreement_total").Inc()
	}
}

func (h *Handler) Start() {
	h.unifiClient.OnEvent(func(event unifi.AccessEvent) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		h.HandleEvent(ctx, event)
	})
	h.logger.Info("check-in handler started (store-first mode)", "gateId", h.gateID)
}

func (h *Handler) GetStats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stats
}

func (h *Handler) HandleEvent(ctx context.Context, event unifi.AccessEvent) {
	h.mu.Lock()
	h.stats.TotalEvents++
	now := time.Now()
	h.stats.LastEvent = &now
	h.mu.Unlock()

	// Only process NFC events
	if strings.ToUpper(event.AuthType) != "NFC" {
		h.logger.Debug("skipping non-NFC event", "authType", event.AuthType)
		return
	}

	if event.CredentialID == "" {
		h.logger.Warn("access event missing credential ID")
		h.incErrors()
		return
	}

	h.logger.Info("── NFC tap ──",
		"credential", event.CredentialID,
		"actorId", event.ActorID,
		"door", event.DoorName,
	)

	// ── FAST PATH: all local, no network ─────────────────────

	// Step 1: Resolve the tap to a member row.
	//
	// Lookup order:
	//
	//   (1) event.ActorID → ua_user_mappings → members.customer_id
	//       Primary path for firmware that hashes NFC tokens server-side
	//       (UA-Hub 1.x): the WebSocket event carries the UA-Hub user_id
	//       in actor.id, and we stored that id verbatim in ua_users /
	//       ua_user_mappings during ingest. The card UID itself is
	//       irrelevant for this branch.
	//
	//   (2) cardMapper.HasOverride(CredentialID) → explicit customer_id
	//       Manual override table (data/card_overrides.json). Empty in
	//       production today; kept so staff can still pin a loaner tag
	//       to a specific customer without waiting on the daily sync.
	//
	//   (3) GetMemberByNFC(CredentialID)
	//       Legacy / raw-UID path. Never matches on hashed-token firmware,
	//       but the code path is kept so (a) older firmware that exposes
	//       the raw UID in credentialId continues to work, and (b) the
	//       checkin unit tests — which use plain-text nfc_uid values —
	//       don't require a full mappings harness.
	//
	// Any branch that returns a real error (not sql.ErrNoRows) aborts the
	// tap with "lookup_error". A miss in one branch falls through to the
	// next; exhausting all branches records a "not_found" denial.
	lookupKey := h.cardMapper.Resolve(event.CredentialID)
	hasOverride := h.cardMapper.HasOverride(event.CredentialID)

	var member *store.Member
	var err error
	var matchedBy string

	// (1) Actor-id primary path.
	if event.ActorID != "" {
		member, err = h.store.GetMemberByUAUserID(ctx, event.ActorID)
		if err != nil {
			h.logger.Error("store lookup by actor id failed",
				"actorId", event.ActorID, "error", err)
			h.recordDeniedEvent(ctx, event, "lookup_error", "Database lookup failed")
			h.incErrors()
			return
		}
		if member != nil {
			matchedBy = "actor_id"
		}
	}

	// (2) Card-override fallback.
	if member == nil && hasOverride {
		member, err = h.store.GetMemberByCustomerID(ctx, lookupKey)
		if err != nil {
			h.logger.Error("store lookup by customer ID failed",
				"lookupKey", lookupKey, "error", err)
			h.recordDeniedEvent(ctx, event, "lookup_error", "Database lookup failed")
			h.incErrors()
			return
		}
		if member == nil {
			// Override value might itself be an NFC UID (legacy override shape).
			member, err = h.store.GetMemberByNFC(ctx, lookupKey)
			if err != nil {
				h.logger.Error("store lookup by NFC (override) failed",
					"lookupKey", lookupKey, "error", err)
				h.recordDeniedEvent(ctx, event, "lookup_error", "Database lookup failed")
				h.incErrors()
				return
			}
		}
		if member != nil {
			matchedBy = "override"
		}
	}

	// (3) Raw nfc_uid fallback.
	if member == nil && !hasOverride {
		member, err = h.store.GetMemberByNFC(ctx, lookupKey)
		if err != nil {
			h.logger.Error("store lookup by NFC failed",
				"lookupKey", lookupKey, "error", err)
			h.recordDeniedEvent(ctx, event, "lookup_error", "Database lookup failed")
			h.incErrors()
			return
		}
		if member != nil {
			matchedBy = "nfc_uid"
		}
	}

	if member == nil {
		h.logger.Warn("DENIED: not in store",
			"credential", event.CredentialID,
			"actorId", event.ActorID,
			"lookupKey", lookupKey,
			"hasOverride", hasOverride,
		)
		h.recordDeniedEvent(ctx, event, "not_found", "Member not found")
		h.incDenied()
		return
	}

	h.logger.Debug("member resolved",
		"matchedBy", matchedBy,
		"customerId", member.CustomerID,
		"actorId", event.ActorID,
	)

	// Step 3: Evaluate door policy for access control
	var policy *store.DoorPolicy
	if event.DoorID != "" {
		policy, err = h.store.GetDoorPolicy(ctx, event.DoorID)
		if err != nil {
			h.logger.Error("failed to get door policy", "doorId", event.DoorID, "error", err)
			h.recordDeniedEvent(ctx, event, "policy_error", "Policy evaluation failed")
			h.incErrors()
			return
		}
	}

	// Determine access: use door policy if available, otherwise use member's general allowed status
	allowed := true
	denyReason := ""

	if policy != nil {
		allowed, denyReason = policy.EvaluateAccess(member)
	} else if !member.IsAllowed() {
		allowed = false
		denyReason = member.DenyReason()
	}

	if !allowed {
		h.logger.Warn("DENIED: "+denyReason,
			"name", member.FullName(),
			"badgeStatus", member.BadgeStatus,
			"active", member.Active,
		)

		// ── Live recheck: maybe they renewed since last sync ────
		if h.rechecker != nil {
			recheckCtx, recheckCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer recheckCancel()

			result, err := h.rechecker.RecheckDeniedTap(recheckCtx, member.NfcUID)
			if err != nil {
				h.logger.Error("denied-tap recheck failed", "error", err)
			} else if result != nil && result.Reactivated {
				h.logger.Info("DENIED→ALLOWED: member renewed, recheck passed!",
					"name", result.Name,
					"reason", result.Reason,
				)
				// Re-read the updated member and evaluate policy again
				updated, err := h.store.GetMemberByNFC(ctx, member.NfcUID)
				if err != nil {
					h.logger.Error("failed to re-read member after recheck", "error", err)
				} else if updated != nil {
					recheckAllowed := true
					recheckDeny := ""
					if policy != nil {
						recheckAllowed, recheckDeny = policy.EvaluateAccess(updated)
					} else if !updated.IsAllowed() {
						recheckAllowed = false
						recheckDeny = updated.DenyReason()
					}
					if recheckAllowed {
						h.unlockAndRecord(ctx, event, updated)
						return
					}
					h.logger.Info("recheck passed but policy still denies", "reason", recheckDeny)
				}
			} else if result != nil {
				h.logger.Info("denied-tap recheck: still denied",
					"name", result.Name,
					"reason", result.Reason,
				)
			}
		}

		h.recordDeniedEvent(ctx, event, "denied", denyReason)
		h.incDenied()
		return
	}

	h.unlockAndRecord(ctx, event, member)
}

// unlockAndRecord performs the "approved tap" tail of HandleEvent: unlock the
// door (skipped in shadow mode), bump success stats, record the allowed check-in
// event in store, and queue async Redpoint recording. Called both from the
// main approval path and from the denied → recheck → reactivated branch.
//
// This replaced a `goto unlock` label that previously jumped from the recheck
// branch back into the approval tail. Extracting the shared tail as a method
// makes both callers express intent directly and removes the forward jump.
func (h *Handler) unlockAndRecord(ctx context.Context, event unifi.AccessEvent, member *store.Member) {
	// Step 4: Unlock the door (with actor info for UniFi system logs).
	// Shadow mode skips the REST call but logs what would have happened so
	// operators can diff the would-be behaviour against the UA-Hub's native
	// enforcement in the UniFi system log.
	//
	// Backfilled events (replayed after a WS reconnect) never unlock — the
	// door already had its chance during the outage and the member is long
	// gone. We still record the event so the audit trail is complete.
	shadow := h.shadowMode.Load()
	if event.DoorID != "" {
		switch {
		case event.IsBackfill:
			h.logger.Info("BACKFILL: skipping door unlock for replayed event",
				"doorId", event.DoorID,
				"doorName", event.DoorName,
				"member", member.FullName(),
				"customerId", member.CustomerID,
				"timestamp", event.Timestamp,
			)
		case shadow:
			h.logger.Info("SHADOW: would unlock door",
				"doorId", event.DoorID,
				"doorName", event.DoorName,
				"member", member.FullName(),
				"customerId", member.CustomerID,
			)
		default:
			if err := h.unifiClient.UnlockDoorForMember(ctx, event.DoorID, member.FullName(), member.CustomerID); err != nil {
				h.logger.Error("door unlock FAILED", "doorId", event.DoorID, "error", err)
				h.incErrors()
				return
			}
		}
	}

	// Mark success
	memberName := member.FullName()
	badgeName := member.BadgeName

	h.logger.Info("CHECK-IN SUCCESS",
		"member", memberName,
		"badge", badgeName,
		"door", event.DoorName,
	)

	// Record the successful check-in event in store
	h.mu.Lock()
	h.stats.SuccessfulCheckins++
	h.stats.LastCheckin = &LastCI{
		Member: memberName,
		Badge:  badgeName,
		Door:   event.DoorName,
		Time:   time.Now().UTC().Format(time.RFC3339),
	}
	h.mu.Unlock()

	// Update member's last check-in timestamp
	if err := h.store.RecordMemberCheckIn(ctx, member.NfcUID); err != nil {
		h.logger.Error("failed to update member last check-in", "error", err)
	}

	h.recordAllowedEvent(ctx, event, member)

	// ── BACKGROUND: record in Redpoint asynchronously ────────
	// Shadow mode logs the would-be call and skips it so we never double-record.

	if shadow {
		if h.gateID != "" {
			h.logger.Info("SHADOW: would record check-in in Redpoint",
				"gateId", h.gateID,
				"customerId", member.CustomerID,
				"member", member.FullName(),
			)
		}
		return
	}

	// Backfilled events never push to Redpoint: if the tap was ACCESS the
	// UA-Hub's webhook (or a prior bridge run) already recorded it; if it
	// was BLOCKED we have no business creating a check-in now. Either way
	// replaying to Redpoint risks duplicates.
	if event.IsBackfill {
		h.logger.Debug("BACKFILL: skipping Redpoint async record",
			"member", member.FullName(),
			"timestamp", event.Timestamp,
		)
		return
	}

	// A5 observability: track in-flight async-Redpoint-write goroutines
	// as a gauge so the operator can see when this depth climbs (sustained
	// >5 means Redpoint is backing up — see architecture-review §A5).
	// The increment is paired with the WG.Add(1) so the gauge value
	// always equals the WG counter; the decrement runs in the same defer
	// stack as WG.Done() so panics inside recordInRedpoint still keep the
	// gauge honest.
	//
	// Snapshot the metrics registry once so Inc/Dec target the same
	// registry — without this, a SetMetrics swap between the two
	// emissions would leave the gauge out of step with the WaitGroup.
	h.asyncWG.Add(1)
	mr := h.metrics.Load()
	if mr != nil {
		mr.Gauge("redpoint_async_writes_in_flight").Inc()
	}
	go func() {
		defer h.asyncWG.Done()
		if mr != nil {
			defer mr.Gauge("redpoint_async_writes_in_flight").Dec()
		}
		h.recordInRedpoint(ctx, member, event)
	}()
}

// Shutdown blocks until all background Redpoint-recording goroutines have
// returned, or ctx is cancelled. Call from main.go's graceful-shutdown
// sequence after the HTTP server and WebSocket have stopped accepting new
// events, so this drain isn't racing new arrivals.
//
// Returns ctx.Err() if the drain didn't complete inside the deadline. The
// caller logs that and continues; we'd rather crash-log a few missing
// Redpoint records than hang the process on a slow GraphQL endpoint.
func (h *Handler) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		h.asyncWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recordInRedpoint sends the check-in to Redpoint without blocking the door,
// and marks the check-in event as recorded in the store.
func (h *Handler) recordInRedpoint(ctx context.Context, member *store.Member, event unifi.AccessEvent) {
	if h.gateID == "" {
		return
	}

	rCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.redpointClient.CreateCheckIn(rCtx, h.gateID, member.CustomerID, "")
	if err != nil {
		h.logger.Warn("async Redpoint check-in failed (door already unlocked)",
			"name", member.FullName(),
			"error", err,
		)
		h.incErrors()
		return
	}

	if !result.Success {
		h.logger.Warn("async Redpoint check-in rejected",
			"name", member.FullName(),
			"error", result.Error,
		)
		return
	}

	if result.Duplicate {
		h.mu.Lock()
		h.stats.DuplicateCheckins++
		h.mu.Unlock()
	}

	h.logger.Debug("Redpoint check-in recorded async",
		"name", member.FullName(),
		"recordId", result.RecordID,
	)

	// Mark the check-in event as recorded in Redpoint
	if result.RecordID != "" {
		// Find the corresponding check-in event ID (latest for this member/door)
		// For now, we update based on the record ID
		markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer markCancel()

		// Note: This assumes store tracks the event ID from the recent allowed event.
		// You may need to adjust this based on your actual store implementation.
		// The store should provide a way to look up the event ID by member/door/timestamp.
		if err := h.store.MarkRedpointRecorded(markCtx, 0, result.RecordID); err != nil {
			h.logger.Warn("failed to mark check-in event as recorded in store",
				"recordId", result.RecordID,
				"error", err,
			)
		}
	}
}

// recordAllowedEvent records a successful check-in event in the store.
// We also capture the UA-Hub's own verdict (event.Result: ACCESS/BLOCKED) so
// the shadow-decisions panel can flag disagreements — a BLOCKED here means
// UniFi's rules would have stopped this tap even though the bridge allowed it.
func (h *Handler) recordAllowedEvent(ctx context.Context, event unifi.AccessEvent, member *store.Member) {
	evt := &store.CheckInEvent{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		NfcUID:       member.NfcUID,
		CustomerID:   member.CustomerID,
		CustomerName: member.FullName(),
		DoorID:       event.DoorID,
		DoorName:     event.DoorName,
		Result:       "allowed",
		DenyReason:   "",
		UnifiResult:  strings.ToUpper(event.Result),
		UnifiLogID:   event.LogID,
	}

	eventID, err := h.store.RecordCheckIn(ctx, evt)
	if err != nil {
		h.logger.Error("failed to record allowed check-in event in store",
			"member", member.FullName(),
			"door", event.DoorName,
			"error", err,
		)
		return
	}

	h.logger.Debug("check-in event recorded in store",
		"eventId", eventID,
		"member", member.FullName(),
		"door", event.DoorName,
	)

	h.noteDisagreement(event, member, "allowed", "")
}

// recordDeniedEvent records a denied check-in event in the store.
// event.Result carries UniFi's native decision — if UniFi said ACCESS but we
// denied, that's a would-be-missed paying member, which the shadow-decisions
// panel surfaces.
func (h *Handler) recordDeniedEvent(ctx context.Context, event unifi.AccessEvent, result, denyReason string) {
	// For denied events, we may not have member info, so use minimal details
	evt := &store.CheckInEvent{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		NfcUID:       event.CredentialID, // Use credential ID as NFC UID
		CustomerID:   "",
		CustomerName: "Unknown",
		DoorID:       event.DoorID,
		DoorName:     event.DoorName,
		Result:       result,
		DenyReason:   denyReason,
		UnifiResult:  strings.ToUpper(event.Result),
		UnifiLogID:   event.LogID,
	}

	_, err := h.store.RecordCheckIn(ctx, evt)
	if err != nil {
		h.logger.Error("failed to record denied check-in event in store",
			"credential", event.CredentialID,
			"door", event.DoorName,
			"error", err,
		)
		return
	}

	h.logger.Debug("denied check-in event recorded in store",
		"credential", event.CredentialID,
		"door", event.DoorName,
		"reason", denyReason,
	)

	h.noteDisagreement(event, nil, result, denyReason)
}

func (h *Handler) incDenied() {
	h.mu.Lock()
	h.stats.DeniedCheckins++
	h.mu.Unlock()
}

func (h *Handler) incErrors() {
	h.mu.Lock()
	h.stats.Errors++
	h.mu.Unlock()
}
