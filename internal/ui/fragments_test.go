package ui

// v0.5.9 coverage for the Members-page search results.
//
// History: v0.5.8 introduced a "Select" button that populated the inline
// Add Member form (#118 fixed an apostrophe-escaping bug in that button).
// v0.5.9 deleted the Add form entirely — staff now provision members in
// UniFi Access and the bridge auto-binds on sync. The Select button
// went with the form. The search itself stayed useful as a directory
// lookup; enrolled rows now expose a "View details" button that opens
// the member detail panel (hx-get → #member-detail).
//
// These tests pin the new rendering:
//   - not-enrolled rows have no button and render an "Add in UniFi
//     Access" hint (documents the new provisioning flow to the staff)
//   - enrolled rows expose a "View details" button wired to the
//     detail fragment endpoint
//   - no inline onclick anywhere (the v0.5.8 #118 regression must stay
//     dead — data attrs or nothing)

import (
	"strings"
	"testing"
)

func TestSearchResultsFragment_NotEnrolledHasNoSelectButton(t *testing.T) {
	out := SearchResultsFragment([]SearchResult{{
		RedpointID: "cust_abc123",
		Name:       "Alice Smith",
		Email:      "alice@example.com",
		InCache:    false,
	}})

	// The Select button is gone — provisioning happens in UniFi Access.
	if strings.Contains(out, ">Select</button>") {
		t.Errorf("not-enrolled row should not render a Select button; output:\n%s", out)
	}
	// And the row tells the operator where to go next.
	if !strings.Contains(out, "Add in UniFi Access") {
		t.Errorf("not-enrolled row should hint operator to add in UniFi; output:\n%s", out)
	}
	// Status badge still says "Not enrolled".
	if !strings.Contains(out, "Not enrolled") {
		t.Errorf("not-enrolled row missing status badge; output:\n%s", out)
	}
}

func TestSearchResultsFragment_EnrolledRowLinksToDetailPanel(t *testing.T) {
	out := SearchResultsFragment([]SearchResult{{
		RedpointID: "cust_abc123",
		Name:       "Alice Smith",
		Email:      "alice@example.com",
		InCache:    true,
		NfcUID:     "04AABB1122",
	}})

	if !strings.Contains(out, ">View details</button>") {
		t.Fatalf("enrolled row missing View details button; output:\n%s", out)
	}
	if !strings.Contains(out, `hx-get="/ui/frag/member/04AABB1122/detail"`) {
		t.Errorf("View details button wired to wrong URL; output:\n%s", out)
	}
	if !strings.Contains(out, `hx-target="#member-detail"`) {
		t.Errorf("View details button should target #member-detail; output:\n%s", out)
	}
	if !strings.Contains(out, "Enrolled (04AABB1122)") {
		t.Errorf("enrolled badge missing NFC UID; output:\n%s", out)
	}
}

func TestSearchResultsFragment_ApostropheInNameRendersSafely(t *testing.T) {
	// Regression guard for #118 (v0.5.8): "O'Brien" used to break the
	// old inline-onclick Select button. The button is gone, but we
	// keep a test that asserts names with apostrophes render through
	// HTMLEscape without any inline onclick re-appearing.
	out := SearchResultsFragment([]SearchResult{{
		RedpointID: "cust_xyz",
		Name:       "Sean O'Brien",
		Email:      "sean@example.com",
		InCache:    false,
	}})

	if !strings.Contains(out, "Sean O&#39;Brien") {
		t.Errorf("apostrophe in name not HTML-escaped; output:\n%s", out)
	}
	if strings.Contains(out, "onclick=") {
		t.Errorf("regressed to inline onclick — #118 bug would return; output:\n%s", out)
	}
}

func TestSearchResultsFragment_EmptyResults(t *testing.T) {
	out := SearchResultsFragment(nil)
	if !strings.Contains(out, "No results found") {
		t.Errorf("empty result set should show a friendly placeholder; got:\n%s", out)
	}
}

// ─── MemberDetailFragment (v0.5.9) ─────────────────────────────────────
//
// The detail panel is the heart of the v0.5.9 member management pivot —
// the Add form is gone, so every recovery action now has to work from
// this one card. The button-enabling rules (documented on
// MemberDetailData in fragments.go) are the invariant under test:
//
//	Unbind:     Mapping != nil
//	Reactivate: Mapping != nil && UAUser != nil && UAUser.Status == "DEACTIVATED"
//	Remove:     always
//	Reassign:   Mapping != nil && UAUser != nil
//
// Each test names the exact state shape it seeds so a future regression
// points at the rule that broke.

// memberDetailHappyPath returns a fully-populated MemberDetailData —
// ACTIVE UA user, non-empty mapping, one audit row. Tests cherry-pick
// and mutate from this base so the invariant under test is obvious.
func memberDetailHappyPath() MemberDetailData {
	return MemberDetailData{
		Member: MemberDetailMember{
			NfcUID:      "04AABB1122",
			Name:        "Dana Tester",
			CustomerID:  "rp-dana",
			BadgeStatus: "ACTIVE",
			BadgeName:   "Monthly",
			Active:      true,
			LastCheckIn: "2026-04-22T09:15:00Z",
			CachedAt:    "2026-04-01T00:00:00Z",
		},
		Mapping: &MemberDetailMapping{
			UAUserID:  "ua-dana",
			MatchedAt: "2026-04-20T12:00:00Z",
			MatchedBy: "auto:email",
		},
		UAUser: &MemberDetailUAUser{
			ID:     "ua-dana",
			Name:   "Dana Tester",
			Email:  "dana@example.com",
			Status: "ACTIVE",
		},
	}
}

// containsDisabledButton asserts the fragment contains a <button ...
// disabled ...>label</button> tag. Needed because the render pattern
// splits the disabled attribute and the label across template glue
// and a naive strings.Contains on the combined substring would miss.
func containsDisabledButton(body, label string) bool {
	// Look for a button tag that both has `disabled` and closes with
	// the expected label text. Cheap two-pass check keeps the test
	// readable without introducing a DOM parser dep.
	for _, chunk := range strings.Split(body, "<button") {
		if strings.Contains(chunk, "disabled") &&
			strings.Contains(chunk, ">"+label+"</button>") {
			return true
		}
	}
	return false
}

// containsEnabledButton asserts there's a <button ...>label</button>
// with no disabled attribute up to the closing tag.
func containsEnabledButton(body, label string) bool {
	for _, chunk := range strings.Split(body, "<button") {
		if !strings.Contains(chunk, ">"+label+"</button>") {
			continue
		}
		// Consider only the segment between the opening `<button` and
		// the closing `</button>` — any later button's disabled
		// attr must not contaminate this check.
		end := strings.Index(chunk, "</button>")
		if end < 0 {
			continue
		}
		if !strings.Contains(chunk[:end], "disabled") {
			return true
		}
	}
	return false
}

// TestMemberDetailFragment_OrphanedMember — Mapping == nil.
// The "member in cache, no mapping row" state: Unbind/Reactivate/
// Reassign must all be disabled with tooltips; Remove must be the
// one live action so staff can drop the orphan.
func TestMemberDetailFragment_OrphanedMember(t *testing.T) {
	d := MemberDetailData{
		Member: MemberDetailMember{
			NfcUID:     "04ORPHAN01",
			Name:       "Orphan Member",
			CustomerID: "rp-orphan",
			Active:     true,
		},
	}
	out := MemberDetailFragment(d)

	if !strings.Contains(out, "No UA-Hub mapping") {
		t.Errorf("orphan state should render the warning banner; got:\n%s", out)
	}
	if !containsDisabledButton(out, "Unbind (re-queue)") {
		t.Errorf("Unbind must be disabled for orphan; got:\n%s", out)
	}
	if !containsDisabledButton(out, "Reactivate UA-Hub user") {
		t.Errorf("Reactivate must be disabled for orphan; got:\n%s", out)
	}
	if !containsDisabledButton(out, "Reassign NFC card") {
		t.Errorf("Reassign must be disabled for orphan; got:\n%s", out)
	}
	if !containsEnabledButton(out, "Remove from cache") {
		t.Errorf("Remove must stay enabled for orphan (it's the escape hatch); got:\n%s", out)
	}
	// The audit-trail block should tell staff why the log is empty,
	// not just show a blank table.
	if !strings.Contains(out, "audit is keyed on UA-Hub user ID") {
		t.Errorf("audit-empty copy should explain why; got:\n%s", out)
	}
}

// TestMemberDetailFragment_HappyPath_ActiveMember — Mapping + UAUser
// with Status==ACTIVE. Unbind/Reassign/Remove enabled; Reactivate
// disabled because the user isn't deactivated.
func TestMemberDetailFragment_HappyPath_ActiveMember(t *testing.T) {
	out := MemberDetailFragment(memberDetailHappyPath())

	if !containsEnabledButton(out, "Unbind (re-queue)") {
		t.Errorf("Unbind should be enabled with a live mapping; got:\n%s", out)
	}
	if !containsEnabledButton(out, "Reassign NFC card") {
		t.Errorf("Reassign should be enabled with mapping+mirror; got:\n%s", out)
	}
	if !containsEnabledButton(out, "Remove from cache") {
		t.Errorf("Remove always enabled; got:\n%s", out)
	}
	if !containsDisabledButton(out, "Reactivate UA-Hub user") {
		t.Errorf("Reactivate should be disabled while user is ACTIVE; got:\n%s", out)
	}
	// ACTIVE badge should render in the UA-Hub status row.
	if !strings.Contains(out, `badge-active">ACTIVE</span>`) {
		t.Errorf("ACTIVE status badge missing; got:\n%s", out)
	}
	// The Unbind button must hit the NFC-parameterised endpoint.
	if !strings.Contains(out, `hx-post="/ui/frag/member/04AABB1122/unbind"`) {
		t.Errorf("Unbind wired to wrong URL; got:\n%s", out)
	}
	// And target the detail sink, not the table.
	if !strings.Contains(out, `hx-target="#member-detail"`) {
		t.Errorf("Unbind should target #member-detail; got:\n%s", out)
	}
}

// TestMemberDetailFragment_DeactivatedUser_EnablesReactivate — the one
// state where Reactivate lights up. Staff hit Skip in Needs Match,
// realised the mistake, opened the detail panel — Reactivate is the undo.
func TestMemberDetailFragment_DeactivatedUser_EnablesReactivate(t *testing.T) {
	d := memberDetailHappyPath()
	d.UAUser.Status = "DEACTIVATED"

	out := MemberDetailFragment(d)

	if !containsEnabledButton(out, "Reactivate UA-Hub user") {
		t.Errorf("Reactivate should be enabled for DEACTIVATED user; got:\n%s", out)
	}
	if !strings.Contains(out, `hx-post="/ui/frag/member/04AABB1122/reactivate"`) {
		t.Errorf("Reactivate wired to wrong URL; got:\n%s", out)
	}
	if !strings.Contains(out, `badge-denied">DEACTIVATED</span>`) {
		t.Errorf("DEACTIVATED status badge missing; got:\n%s", out)
	}
}

// TestMemberDetailFragment_MissingMirror_DisablesReassign — Mapping set
// but UAUser == nil (sync-lag: mapping landed before the mirror walk
// caught up). Reassign needs both sides to write its two audit rows,
// so it must disable; Reactivate also needs the mirror to know the
// prior status. Unbind survives — it only cares about the mapping.
func TestMemberDetailFragment_MissingMirror_DisablesReassign(t *testing.T) {
	d := memberDetailHappyPath()
	d.UAUser = nil

	out := MemberDetailFragment(d)

	if !containsEnabledButton(out, "Unbind (re-queue)") {
		t.Errorf("Unbind should survive a missing mirror row; got:\n%s", out)
	}
	if !containsDisabledButton(out, "Reassign NFC card") {
		t.Errorf("Reassign should disable without mirror row; got:\n%s", out)
	}
	if !containsDisabledButton(out, "Reactivate UA-Hub user") {
		t.Errorf("Reactivate should disable without mirror row; got:\n%s", out)
	}
	// The explanatory tooltip should mention running a sync — that's
	// the operator's path out of sync-lag state.
	if !strings.Contains(out, "mirror row missing") {
		t.Errorf("missing-mirror tooltip should explain; got:\n%s", out)
	}
}

// TestMemberDetailFragment_AuditTrailRenders — audit rows land in a
// table with the right columns. Empty-string before/after render as
// em-dash placeholders rather than blank cells (the reassign flow
// will write before="" on the new owner's row, so this matters).
func TestMemberDetailFragment_AuditTrailRenders(t *testing.T) {
	d := memberDetailHappyPath()
	d.Audit = []MemberAuditRow{
		{
			Timestamp: "2026-04-22T10:00:00Z",
			Field:     "mapping",
			BeforeVal: "rp-old",
			AfterVal:  "",
			Source:    "staff:unbind",
		},
		{
			Timestamp: "2026-04-22T10:05:00Z",
			Field:     "user_status",
			BeforeVal: "DEACTIVATED",
			AfterVal:  "ACTIVE",
			Source:    "staff:reactivate",
		},
	}

	out := MemberDetailFragment(d)

	if !strings.Contains(out, "<th>When</th>") ||
		!strings.Contains(out, "<th>Field</th>") ||
		!strings.Contains(out, "<th>Before</th>") ||
		!strings.Contains(out, "<th>After</th>") ||
		!strings.Contains(out, "<th>Source</th>") {
		t.Errorf("audit table header missing expected columns; got:\n%s", out)
	}
	// Each field name should land under a <code> tag so the forensic
	// view reads like a changelog.
	if !strings.Contains(out, "<code>staff:unbind</code>") {
		t.Errorf("audit source should render as <code>; got:\n%s", out)
	}
	// Empty after_val should fall back to the em-dash placeholder.
	if !strings.Contains(out, `<span style="color: var(--text-muted)">—</span>`) {
		t.Errorf("empty audit value should render as em-dash; got:\n%s", out)
	}
	// Both rows should appear.
	if !strings.Contains(out, "rp-old") || !strings.Contains(out, "DEACTIVATED") {
		t.Errorf("audit rows missing; got:\n%s", out)
	}
}

// TestMemberDetailFragment_CloseButton — the ✕ button exists so staff
// can dismiss the panel. It's plain JS (not HTMX) because clearing a
// div doesn't need a round-trip; pin that so nobody turns it into an
// hx-get that returns empty bytes.
func TestMemberDetailFragment_CloseButton(t *testing.T) {
	out := MemberDetailFragment(memberDetailHappyPath())
	if !strings.Contains(out, "document.getElementById('member-detail').innerHTML = ''") {
		t.Errorf("close button should use inline JS to clear the panel; got:\n%s", out)
	}
}

// ─── MemberReassignFragment (v0.5.9 #10) ──────────────────────────────

// reassignHappyPath builds a two-candidate reassign picker with the
// current owner set and a query submitted. Factored out so the branch
// tests below each only tweak one dimension.
func reassignHappyPath() MemberReassignData {
	return MemberReassignData{
		NfcUID:        "04DEADBEEF",
		CurrentUserID: "ua-alice",
		CurrentName:   "Alice Alpha",
		CurrentMember: "Alice Alpha",
		Query:         "bob",
		Candidates: []MemberReassignCandidate{
			{UAUserID: "ua-bob", Name: "Bob Brava", Email: "bob@example.com", Status: "ACTIVE"},
			{UAUserID: "ua-bob2", Name: "Bob Cee", Email: "bob2@example.com", Status: "ACTIVE",
				HasExistingCard: true},
		},
	}
}

// TestMemberReassignFragment_EmptyState — no query submitted yet. The
// picker should include the search form and the placeholder hint but
// NO candidate table (staff hasn't typed anything).
func TestMemberReassignFragment_EmptyState(t *testing.T) {
	d := reassignHappyPath()
	d.Query = ""
	d.Candidates = nil
	out := MemberReassignFragment(d)

	if !strings.Contains(out, "Type a name or email") {
		t.Errorf("expected empty-state placeholder; got:\n%s", out)
	}
	if strings.Contains(out, "<table") {
		t.Errorf("empty state should not render a candidate table; got:\n%s", out)
	}
	// Cancel button wires back to the detail fragment — the back-
	// button contract.
	if !strings.Contains(out, `hx-get="/ui/frag/member/04DEADBEEF/detail"`) {
		t.Errorf("cancel button URL wrong; got:\n%s", out)
	}
}

// TestMemberReassignFragment_NoMatches — query submitted, zero hits.
// Picker should render the "No UA-Hub users matched" hint rather than
// a bare empty table.
func TestMemberReassignFragment_NoMatches(t *testing.T) {
	d := reassignHappyPath()
	d.Query = "zzzz"
	d.Candidates = nil
	out := MemberReassignFragment(d)

	if !strings.Contains(out, "No UA-Hub users matched") {
		t.Errorf("expected no-matches hint; got:\n%s", out)
	}
	if !strings.Contains(out, "<code>zzzz</code>") {
		t.Errorf("query should be echoed back to the staff; got:\n%s", out)
	}
}

// TestMemberReassignFragment_CandidateRowWiring — each candidate row
// should include a form that POSTs to the confirm endpoint with the
// target's UA-Hub ID in a hidden field.
func TestMemberReassignFragment_CandidateRowWiring(t *testing.T) {
	out := MemberReassignFragment(reassignHappyPath())

	if !strings.Contains(out, `hx-post="/ui/frag/member/04DEADBEEF/reassign/confirm"`) {
		t.Errorf("confirm URL missing; got:\n%s", out)
	}
	if !strings.Contains(out, `value="ua-bob"`) {
		t.Errorf("first candidate's hidden input missing; got:\n%s", out)
	}
	if !strings.Contains(out, `value="ua-bob2"`) {
		t.Errorf("second candidate's hidden input missing; got:\n%s", out)
	}
	// hx-confirm surfaces old + new identity so staff doesn't reassign
	// by accident.
	if !strings.Contains(out, "Alice Alpha") || !strings.Contains(out, "Bob Brava") {
		t.Errorf("confirm prompt should include both names; got:\n%s", out)
	}
}

// TestMemberReassignFragment_SwapHintRendersForExistingCard — when a
// candidate already has an NFC card, the row should carry the "⚠ has
// existing card" hint (staff sees the three-way swap up-front).
func TestMemberReassignFragment_SwapHintRendersForExistingCard(t *testing.T) {
	out := MemberReassignFragment(reassignHappyPath())

	// ua-bob2 has HasExistingCard=true in the happy-path fixture; the
	// hint should appear exactly once (ua-bob does NOT have one).
	if strings.Count(out, "has existing card") != 1 {
		t.Errorf("expected exactly one swap hint (on ua-bob2); got:\n%s", out)
	}
	// The confirm prompt for ua-bob2 should also mention the unbind.
	if !strings.Contains(out, "existing card will be unbound") {
		t.Errorf("confirm prompt should mention the existing-card unbind; got:\n%s", out)
	}
}

// TestMemberReassignFragment_ErrorMessageRenders — the handler sets
// ErrorMessage on a search failure so staff sees the DB error rather
// than a silent empty result.
func TestMemberReassignFragment_ErrorMessageRenders(t *testing.T) {
	d := reassignHappyPath()
	d.ErrorMessage = "database unavailable"
	out := MemberReassignFragment(d)

	if !strings.Contains(out, "alert-error") {
		t.Errorf("expected alert-error class; got:\n%s", out)
	}
	if !strings.Contains(out, "database unavailable") {
		t.Errorf("expected error text; got:\n%s", out)
	}
}
