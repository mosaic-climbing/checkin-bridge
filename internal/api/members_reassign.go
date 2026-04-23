package api

// NFC reassignment handlers (v0.5.9 #10).
//
// When an NFC card ends up bound to the wrong UA-Hub user (mis-scan
// during enrolment, card handed to the wrong person at the front
// desk, etc.), staff need a way to move it to the correct user
// without deleting the card from UA-Hub and re-enrolling. The v0.5.9
// Reassign flow uses the UA-Hub Developer API's §3.7 AssignNFCCard
// with force_add=true, which the API treats as a three-step atomic:
//   1. remove token from the current owner
//   2. add token to the target user
//   3. return the updated target user row
//
// Two MatchAudit rows are written per reassign — one under the old
// owner's UAUserID (field "nfc_card", before=token, after="") and
// one under the new owner's (before="", after=token) — so either
// user's forensic view surfaces the hand-off.
//
// The flow is a 3-handler sequence:
//   1. GET  /ui/frag/member/{nfcUid}/reassign          — initial panel
//   2. POST /ui/frag/member/{nfcUid}/reassign/search   — find target
//   3. POST /ui/frag/member/{nfcUid}/reassign/confirm  — commit swap

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
)

// resolveReassignContext loads the member + current mapping + current
// UA-Hub user shared by all three reassign handlers. Returns
// (renderedErrorFragment, nil) when a short-circuit is required — the
// caller writes it to w and stops; otherwise the tuple is populated and
// err==nil means "keep going".
//
// Factored out so the three handlers don't each re-implement the
// lookup chain and the "which error fragment to render" logic.
type reassignContext struct {
	member  *store.Member
	mapping *store.Mapping
	uaUser  *store.UAUser
}

func (s *Server) resolveReassignContext(r *http.Request) (*reassignContext, string) {
	if s.store == nil || s.unifi == nil {
		return nil, "Store or UniFi client not configured"
	}
	nfcUID := r.PathValue("nfcUid")
	if nfcUID == "" {
		return nil, "Missing NFC UID"
	}
	ctx := r.Context()

	m, err := s.store.GetMemberByNFC(ctx, nfcUID)
	if err != nil {
		return nil, "Member lookup failed: " + err.Error()
	}
	if m == nil {
		return nil, "Member not found"
	}
	mapping, err := s.store.GetMappingByCustomerID(ctx, m.CustomerID)
	if err != nil {
		return nil, "Mapping lookup failed: " + err.Error()
	}
	if mapping == nil {
		return nil, "Cannot reassign — this member has no UA-Hub mapping. Use Unbind or Remove instead."
	}
	uaUser, err := s.store.GetUAUser(ctx, mapping.UAUserID)
	if err != nil {
		return nil, "UA-Hub user lookup failed: " + err.Error()
	}
	if uaUser == nil {
		return nil, "Cannot reassign — the UA-Hub mirror row is missing for this member. Run a UA-Hub sync first."
	}
	return &reassignContext{member: m, mapping: mapping, uaUser: uaUser}, ""
}

// handleFragMemberReassign — GET: render the reassign target picker
// with an empty search state. The operator types into the picker's
// search box and the form POSTs to the search handler below.
func (s *Server) handleFragMemberReassign(w http.ResponseWriter, r *http.Request) {
	rc, errMsg := s.resolveReassignContext(r)
	if rc == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, errMsg))
		return
	}
	ui.RenderFragment(w, ui.MemberReassignFragment(ui.MemberReassignData{
		NfcUID:        rc.member.NfcUID,
		CurrentUserID: rc.uaUser.ID,
		CurrentName:   rc.uaUser.FullName(),
		CurrentMember: rc.member.FullName(),
	}))
}

// handleFragMemberReassignSearch — POST /ui/frag/member/{nfcUid}/reassign/search.
// Reads `q` from the form, runs store.SearchUAUsers, filters the
// current owner out of the results (no point reassigning to self), and
// re-renders the picker with the candidates.
//
// HasExistingCard on each candidate is set from the mirror's nfc_tokens
// JSON column so the UI can surface a ⚠ hint when a reassign will
// also unbind the target's current card.
func (s *Server) handleFragMemberReassignSearch(w http.ResponseWriter, r *http.Request) {
	rc, errMsg := s.resolveReassignContext(r)
	if rc == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, errMsg))
		return
	}
	if err := r.ParseForm(); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Bad form: "+err.Error()))
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))

	data := ui.MemberReassignData{
		NfcUID:        rc.member.NfcUID,
		CurrentUserID: rc.uaUser.ID,
		CurrentName:   rc.uaUser.FullName(),
		CurrentMember: rc.member.FullName(),
		Query:         q,
	}

	if q != "" {
		hits, err := s.store.SearchUAUsers(r.Context(), q, 50)
		if err != nil {
			s.logger.Warn("reassign search failed", "q", q, "error", err)
			data.ErrorMessage = "Search failed: " + err.Error()
		}
		for _, u := range hits {
			if u.ID == rc.uaUser.ID {
				continue // don't offer a self-reassign
			}
			data.Candidates = append(data.Candidates, ui.MemberReassignCandidate{
				UAUserID:        u.ID,
				Name:            u.FullName(),
				Email:           u.Email,
				Status:          u.Status,
				HasExistingCard: len(u.NfcTokens()) > 0,
			})
		}
	}

	ui.RenderFragment(w, ui.MemberReassignFragment(data))
}

// handleFragMemberReassignConfirm — POST /ui/frag/member/{nfcUid}/reassign/confirm.
// Commits the swap:
//
//  1. Call unifi.AssignNFCCard(targetUserID, token, forceAdd=true).
//     UA-Hub performs the three-step atomic (remove from old, add to
//     new, return the new row).
//  2. Write the old-side audit row: UAUserID=oldUser, field=nfc_card,
//     before=token, after="", source=staff:reassign.
//  3. Write the new-side audit row: UAUserID=newUser, field=nfc_card,
//     before="", after=token, source=staff:reassign.
//  4. If the new UA-Hub user has a mapping row, rewrite the member row
//     to point at their Redpoint customer (identity stays correct on the
//     Members page). If they don't, leave the member row alone — the
//     next statusync pass will sort it out and the operator will see
//     an Orphaned hint in the detail panel in the meantime.
//
// HX-Trigger=member-updated on the response so the member table
// refreshes once the swap lands.
func (s *Server) handleFragMemberReassignConfirm(w http.ResponseWriter, r *http.Request) {
	rc, errMsg := s.resolveReassignContext(r)
	if rc == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, errMsg))
		return
	}
	if err := r.ParseForm(); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Bad form: "+err.Error()))
		return
	}
	targetUAUserID := strings.TrimSpace(r.FormValue("targetUaUserId"))
	if targetUAUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing target UA-Hub user ID"))
		return
	}
	if targetUAUserID == rc.uaUser.ID {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"Target UA-Hub user is the same as the current owner — nothing to do."))
		return
	}

	ctx := r.Context()

	// Verify the target mirror row exists. We allow a missing mapping
	// (the target may be un-bound and sitting in Needs Match), but we
	// require the mirror row so the audit trail has a real UA user ID
	// to key on.
	target, err := s.store.GetUAUser(ctx, targetUAUserID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Target user lookup failed: "+err.Error()))
		return
	}
	if target == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"Target UA-Hub user not found in the mirror. Run a UA-Hub sync and try again."))
		return
	}

	// ── Step 1: UA-Hub reassignment ─────────────────────────────────
	// force_add=true asks UA-Hub to unbind the card from the current
	// owner (and from the target, if they already had one) as part of
	// the same request. If this fails we haven't touched local state,
	// so the operator can retry.
	if err := s.unifi.AssignNFCCard(ctx, targetUAUserID, rc.member.NfcUID, true); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"UA-Hub reassign failed: "+err.Error()+
				" (no local changes made — safe to retry)"))
		return
	}

	// ── Step 2+3: two-sided audit trail ─────────────────────────────
	// Ordering: we write both audit rows even if the local bridge
	// mutation below fails. The UA-Hub side is already committed at
	// this point, so the forensic log must reflect that regardless.
	if err := s.store.AppendMatchAudit(ctx, &store.MatchAudit{
		UAUserID:  rc.uaUser.ID,
		Field:     "nfc_card",
		BeforeVal: rc.member.NfcUID,
		AfterVal:  "",
		Source:    statusync.MatchSourceStaffReassign,
	}); err != nil {
		s.logger.Error("reassign audit (old side) failed",
			"uaUserId", rc.uaUser.ID, "error", err)
	}
	if err := s.store.AppendMatchAudit(ctx, &store.MatchAudit{
		UAUserID:  targetUAUserID,
		Field:     "nfc_card",
		BeforeVal: "",
		AfterVal:  rc.member.NfcUID,
		Source:    statusync.MatchSourceStaffReassign,
	}); err != nil {
		s.logger.Error("reassign audit (new side) failed",
			"uaUserId", targetUAUserID, "error", err)
	}

	// ── Step 4: re-point the bridge member row ──────────────────────
	// If the new UA-Hub user has an existing mapping, update the member
	// row's customer_id to match — this keeps the Members page showing
	// the correct identity for the card. The member row is keyed on
	// nfc_uid, so no insert/delete juggling is needed.
	//
	// If the new user has no mapping (unusual — reassigning to a
	// Needs-Match user), leave the member row pointing at the old
	// Redpoint customer. The next sync pass will reconcile it; in the
	// meantime the detail panel will show a stale identity, which
	// we accept as a known transient state.
	targetMapping, err := s.store.GetMapping(ctx, targetUAUserID)
	if err != nil {
		s.logger.Warn("reassign: target mapping lookup failed",
			"uaUserId", targetUAUserID, "error", err)
	}
	var newCustomerName string
	if targetMapping != nil {
		cust, err := s.store.GetCustomerByID(ctx, targetMapping.RedpointCustomer)
		if err != nil {
			s.logger.Warn("reassign: target customer lookup failed",
				"customerId", targetMapping.RedpointCustomer, "error", err)
		}
		if cust != nil {
			updated := *rc.member
			updated.CustomerID = cust.RedpointID
			updated.FirstName = cust.FirstName
			updated.LastName = cust.LastName
			if err := s.store.UpsertMember(ctx, &updated); err != nil {
				s.logger.Error("reassign: member row update failed",
					"nfcUid", rc.member.NfcUID, "error", err)
			}
			newCustomerName = strings.TrimSpace(cust.FirstName + " " + cust.LastName)
		}
	}

	s.audit.Log("staff_reassign", r.RemoteAddr, map[string]any{
		"nfcUid":    rc.member.NfcUID,
		"fromUaUid": rc.uaUser.ID,
		"toUaUid":   targetUAUserID,
	})
	s.htmlCache.Invalidate()

	successMsg := fmt.Sprintf("Reassigned NFC card %s from %s to %s.",
		rc.member.NfcUID, rc.uaUser.FullName(), target.FullName())
	if newCustomerName != "" {
		successMsg += fmt.Sprintf(" Member row now reflects Redpoint customer %s.", newCustomerName)
	} else {
		successMsg += " Target has no Redpoint mapping yet — next sync will reconcile the member row."
	}

	w.Header().Set("HX-Trigger", "member-updated")
	ui.RenderFragment(w, ui.AlertFragment(true, successMsg))
}
