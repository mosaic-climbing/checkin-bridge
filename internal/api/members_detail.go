package api

// Member detail panel handlers (v0.5.9).
//
// The panel replaces the pre-v0.5.9 inline "Add Member" form. The v0.5.8
// UI audit showed that once a misassigned member landed in the cache
// there was no recovery path short of dropping the database — Unbind,
// Reactivate, and Reassign are the escape hatches that make the
// member table trustworthy again.
//
// All three mutations emit `HX-Trigger: member-updated` on success so
// the member table on the Members page refreshes itself. The detail
// panel itself is swapped in place with an alert fragment, then the
// operator can close or take another action.
//
// Audit trail: every mutation writes a row to match_audit (keyed on
// ua_user_id) so `/ui/frag/member/{nfcUid}/detail` can surface a
// per-user forensic log the next time staff views the panel.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
)

// handleFragMemberDetail — GET /ui/frag/member/{nfcUid}/detail.
//
// Loads the member row, its mapping (if any), the UA-Hub mirror row
// (if the mapping points at a mirrored user), and the audit trail
// (keyed on ua_user_id so it's empty for orphaned members). Renders a
// MemberDetailFragment.
func (s *Server) handleFragMemberDetail(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	nfcUID := r.PathValue("nfcUid")
	if nfcUID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing NFC UID"))
		return
	}

	ctx := r.Context()
	m, err := s.store.GetMemberByNFC(ctx, nfcUID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Member lookup failed: "+err.Error()))
		return
	}
	if m == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"No member found with NFC UID "+nfcUID))
		return
	}

	data := ui.MemberDetailData{
		Member: ui.MemberDetailMember{
			NfcUID:      m.NfcUID,
			Name:        m.FullName(),
			CustomerID:  m.CustomerID,
			BadgeStatus: m.BadgeStatus,
			BadgeName:   m.BadgeName,
			Active:      m.Active,
			LastCheckIn: m.LastCheckIn,
			CachedAt:    m.CachedAt,
		},
	}

	// Resolve the mapping via customer_id. Members are keyed on NFC UID
	// but the mapping table is keyed on ua_user_id; the bridge between
	// them is redpoint_customer_id (both tables have it as a foreign
	// key into cache.customers).
	mapping, err := s.store.GetMappingByCustomerID(ctx, m.CustomerID)
	if err != nil {
		// Non-fatal: render the panel without mapping-side info. The
		// operator will see the orphan hint.
		s.logger.Warn("member detail: mapping lookup failed",
			"nfcUid", nfcUID, "customerId", m.CustomerID, "error", err)
	}
	if mapping != nil {
		data.Mapping = &ui.MemberDetailMapping{
			UAUserID:  mapping.UAUserID,
			MatchedAt: mapping.MatchedAt,
			MatchedBy: mapping.MatchedBy,
		}

		// UA-Hub mirror row — optional; missing row is expected on
		// freshly-ingested members before the first mirror walk.
		uaUser, err := s.store.GetUAUser(ctx, mapping.UAUserID)
		if err != nil {
			s.logger.Warn("member detail: UA user lookup failed",
				"uaUserId", mapping.UAUserID, "error", err)
		}
		if uaUser != nil {
			data.UAUser = &ui.MemberDetailUAUser{
				ID:     uaUser.ID,
				Name:   uaUser.FullName(),
				Email:  uaUser.Email,
				Status: uaUser.Status,
			}
		}

		// Audit trail — keyed on ua_user_id. Limit 50 is enough for
		// the panel; power users can hit the raw /audit endpoint.
		rows, err := s.store.ListMatchAudit(ctx, mapping.UAUserID, 50)
		if err != nil {
			s.logger.Warn("member detail: audit fetch failed",
				"uaUserId", mapping.UAUserID, "error", err)
		}
		for _, a := range rows {
			data.Audit = append(data.Audit, ui.MemberAuditRow{
				Timestamp: a.Timestamp,
				Field:     a.Field,
				BeforeVal: a.BeforeVal,
				AfterVal:  a.AfterVal,
				Source:    a.Source,
			})
		}
	}

	ui.RenderFragment(w, ui.MemberDetailFragment(data))
}

// handleFragMemberUnbind — POST /ui/frag/member/{nfcUid}/unbind.
//
// Clears the ua_user_mappings row for this member's UA-Hub user and
// re-queues them in Needs Match with PendingReasonNoMatch. The UA-Hub
// user survives; the bridge member cache row survives (the operator
// can re-bind them on the next sync).
//
// Typical use case: the auto-matcher bound a UA-Hub user to the wrong
// Redpoint customer (household email collision slipped through the
// name disambiguator) and the operator wants to pick a different
// candidate from the Needs Match panel.
func (s *Server) handleFragMemberUnbind(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	nfcUID := r.PathValue("nfcUid")
	if nfcUID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing NFC UID"))
		return
	}

	ctx := r.Context()
	m, err := s.store.GetMemberByNFC(ctx, nfcUID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Member lookup failed: "+err.Error()))
		return
	}
	if m == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Member not found"))
		return
	}

	mapping, err := s.store.GetMappingByCustomerID(ctx, m.CustomerID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Mapping lookup failed: "+err.Error()))
		return
	}
	if mapping == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"Nothing to unbind — this member has no UA-Hub mapping."))
		return
	}

	// Audit first — if the write fails, the mapping stays, and the
	// operator retries. Doing it the other way round risks a silent
	// "I unbound but nobody can tell why" gap.
	if err := s.store.AppendMatchAudit(ctx, &store.MatchAudit{
		UAUserID:  mapping.UAUserID,
		Field:     "mapping",
		BeforeVal: mapping.RedpointCustomer,
		AfterVal:  "",
		Source:    statusync.MatchSourceStaffUnbind,
	}); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Audit write failed: "+err.Error()))
		return
	}

	if err := s.store.DeleteMapping(ctx, mapping.UAUserID); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Unbind failed: "+err.Error()))
		return
	}

	// Re-queue in Needs Match so the operator can pick the correct
	// Redpoint customer. Best-effort: if this fails, the mapping is
	// already gone and the next sync will re-queue.
	pending := &store.Pending{
		UAUserID:   mapping.UAUserID,
		Reason:     store.PendingReasonNoMatch,
		GraceUntil: time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	if err := s.store.UpsertPending(ctx, pending); err != nil {
		s.logger.Warn("pending upsert after unbind failed",
			"uaUserId", mapping.UAUserID, "error", err)
	}

	s.audit.Log("staff_unbind", r.RemoteAddr, map[string]any{
		"uaUserId":   mapping.UAUserID,
		"customerId": mapping.RedpointCustomer,
		"nfcUid":     nfcUID,
	})
	s.htmlCache.Invalidate()

	// HX-Trigger tells the member table to refresh. The panel itself
	// swaps to an alert — the member stays in cache (this is Unbind,
	// not Remove) so re-opening the detail panel will show the
	// "orphaned" state until the operator re-binds via Needs Match.
	w.Header().Set("HX-Trigger", "member-updated")
	ui.RenderFragment(w, ui.AlertFragment(true, fmt.Sprintf(
		"Unbound %s from Redpoint %s. Head to Needs Match to pick the correct customer.",
		m.FullName(), mapping.RedpointCustomer)))
}

// handleFragMemberReactivate — POST /ui/frag/member/{nfcUid}/reactivate.
//
// Flips the UA-Hub user status from DEACTIVATED back to ACTIVE. Most
// common case: staff hit Skip in Needs Match (which deactivates) and
// later realised it was the wrong call — this is the undo button.
func (s *Server) handleFragMemberReactivate(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.unifi == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store or UniFi client not configured"))
		return
	}
	nfcUID := r.PathValue("nfcUid")
	if nfcUID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing NFC UID"))
		return
	}

	ctx := r.Context()
	m, err := s.store.GetMemberByNFC(ctx, nfcUID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Member lookup failed: "+err.Error()))
		return
	}
	if m == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Member not found"))
		return
	}

	mapping, err := s.store.GetMappingByCustomerID(ctx, m.CustomerID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Mapping lookup failed: "+err.Error()))
		return
	}
	if mapping == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"Cannot reactivate — this member has no UA-Hub mapping."))
		return
	}

	// Capture prior status for the audit trail. If the mirror row is
	// missing we still allow the reactivate and record "unknown" as
	// the before value — the UA-Hub side UpdateUserStatus call is
	// idempotent for already-active users.
	beforeStatus := "unknown"
	if uaUser, err := s.store.GetUAUser(ctx, mapping.UAUserID); err == nil && uaUser != nil {
		beforeStatus = uaUser.Status
	}

	if err := s.unifi.UpdateUserStatus(ctx, mapping.UAUserID, "ACTIVE"); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "UA-Hub reactivate failed: "+err.Error()))
		return
	}

	if err := s.store.AppendMatchAudit(ctx, &store.MatchAudit{
		UAUserID:  mapping.UAUserID,
		Field:     "user_status",
		BeforeVal: beforeStatus,
		AfterVal:  "ACTIVE",
		Source:    statusync.MatchSourceStaffReactivate,
	}); err != nil {
		s.logger.Error("reactivate audit write failed",
			"uaUserId", mapping.UAUserID, "error", err)
	}

	s.audit.Log("staff_reactivate", r.RemoteAddr, map[string]any{
		"uaUserId": mapping.UAUserID,
		"nfcUid":   nfcUID,
	})
	s.htmlCache.Invalidate()

	w.Header().Set("HX-Trigger", "member-updated")
	ui.RenderFragment(w, ui.AlertFragment(true, fmt.Sprintf(
		"Reactivated UA-Hub user %s — they can tap in again.", m.FullName())))
}
