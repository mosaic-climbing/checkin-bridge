package api

// Handler coverage for the v0.5.9 member-detail panel + recovery actions
// (internal/api/members_detail.go) and the refactored DELETE /members
// response shape.
//
// Why a dedicated file: members_detail.go introduces three new HTMX
// mutations (unbind, reactivate, and the DELETE contract refactor) and
// each has a specific side-effect footprint — audit row written, mapping
// deleted, UA-Hub PUT dispatched, HX-Trigger header set. The needs_match
// tests cover staff:match/skip/defer but don't touch these code paths.
//
// Server scaffolding reuses buildNeedsMatchTestServer: same cardmap +
// FakeUniFi + FakeRedpoint wiring, so UpdateUserStatus calls land on the
// fake (verifiable via StatusUpdateCount) and DB assertions read through
// the same *store.Store.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// seedMember is the member-side sibling of needs_match_test's seedPending.
// Writes a cache.members row with the usual ACTIVE defaults so tests can
// say "given a member bound to ua-X" in one line.
func seedMember(t *testing.T, db *store.Store, nfcUID, customerID, firstName, lastName string) {
	t.Helper()
	if err := db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      strings.ToUpper(nfcUID),
		CustomerID:  customerID,
		FirstName:   firstName,
		LastName:    lastName,
		BadgeStatus: "ACTIVE",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}
}

// seedMapping writes a ua_user_mappings row. matched_at is stamped to now
// so per-test sort-order assertions have a predictable timestamp.
func seedMapping(t *testing.T, db *store.Store, uaUserID, customerID, source string) {
	t.Helper()
	if err := db.UpsertMapping(context.Background(), &store.Mapping{
		UAUserID:         uaUserID,
		RedpointCustomer: customerID,
		MatchedBy:        source,
	}); err != nil {
		t.Fatalf("UpsertMapping: %v", err)
	}
}

// seedUAUser writes a ua_users mirror row. The Reactivate handler reads
// this to snapshot the prior status for the audit trail, and the detail
// fragment keys the Reassign/Reactivate button enabling rules off it.
func seedUAUser(t *testing.T, db *store.Store, id, firstName, lastName, email, status string) {
	t.Helper()
	if err := db.UpsertUAUser(context.Background(), &store.UAUser{
		ID:        id,
		FirstName: firstName,
		LastName:  lastName,
		Email:     email,
		Status:    status,
	}, nil); err != nil {
		t.Fatalf("UpsertUAUser: %v", err)
	}
}

// ─── GET /ui/frag/member/{nfcUid}/detail ───────────────────────────────

// TestFragMemberDetail_HappyPath — member + mapping + mirror + one audit
// row all present. Fragment should render every section (identity,
// mapping, UA user, audit) and enable every action button (Unbind,
// Reassign, Remove). Reactivate stays disabled because status=ACTIVE.
func TestFragMemberDetail_HappyPath(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	seedMember(t, db, "04DEADBEEF", "rp-happy", "Happy", "Path")
	seedMapping(t, db, "ua-happy", "rp-happy", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-happy", "Happy", "Path", "happy@example.com", "ACTIVE")
	_ = db.AppendMatchAudit(ctx, &store.MatchAudit{
		UAUserID:  "ua-happy",
		Field:     "mapping",
		BeforeVal: "",
		AfterVal:  "rp-happy",
		Source:    statusync.MatchSourceEmail,
	})

	req := httptest.NewRequest("GET", "/ui/frag/member/04DEADBEEF/detail", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Identity header
	if !strings.Contains(body, "Happy Path") {
		t.Errorf("member name missing from panel; body = %q", body)
	}
	// UA-Hub identity row
	if !strings.Contains(body, "happy@example.com") {
		t.Errorf("UA-Hub email missing; body = %q", body)
	}
	if !strings.Contains(body, "ua-happy") {
		t.Errorf("UA user ID missing; body = %q", body)
	}
	// Unbind button wired to the right endpoint
	if !strings.Contains(body, `hx-post="/ui/frag/member/04DEADBEEF/unbind"`) {
		t.Errorf("Unbind URL wrong; body = %q", body)
	}
	// Audit row renders (source surfaces in the per-row <code>)
	if !strings.Contains(body, "auto:email") {
		t.Errorf("audit source missing; body = %q", body)
	}
}

// TestFragMemberDetail_Orphan — member in cache, no mapping row. Panel
// must render the "No UA-Hub mapping" warning and show the "no audit —
// keyed on UA-Hub user ID" copy. Unbind/Reactivate/Reassign all land as
// disabled; Remove stays live (verified in fragments_test).
func TestFragMemberDetail_Orphan(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)
	seedMember(t, db, "04ORPHAN01", "rp-orphan", "Orphan", "One")

	req := httptest.NewRequest("GET", "/ui/frag/member/04ORPHAN01/detail", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No UA-Hub mapping") {
		t.Errorf("orphan banner missing; body = %q", body)
	}
	if !strings.Contains(body, "audit is keyed on UA-Hub user ID") {
		t.Errorf("orphan audit copy missing; body = %q", body)
	}
}

// TestFragMemberDetail_MissingMember — NFC UID that isn't in cache
// should render a friendly alert, not 404. HTMX fragments always return
// 200 with the alert in the body so the swap target updates cleanly.
func TestFragMemberDetail_MissingMember(t *testing.T) {
	srv, _, _, _ := buildNeedsMatchTestServer(t)

	req := httptest.NewRequest("GET", "/ui/frag/member/04GHOST999/detail", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "alert-error") {
		t.Errorf("missing member should produce an error alert; body = %q", body)
	}
	if !strings.Contains(body, "04GHOST999") {
		t.Errorf("alert should cite the NFC UID that was looked up; body = %q", body)
	}
}

// ─── POST /ui/frag/member/{nfcUid}/unbind ──────────────────────────────

// TestFragMemberUnbind_HappyPath — with a live mapping, Unbind should:
//  1. write a match_audit row (field=mapping, source=staff:unbind)
//  2. DELETE the mapping
//  3. UPSERT a pending row with reason=no_match and ~7d grace
//  4. emit HX-Trigger: member-updated
//  5. render a success alert that names the member + customer
func TestFragMemberUnbind_HappyPath(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	seedMember(t, db, "04UNBIND01", "rp-unbind", "Un", "Bind")
	seedMapping(t, db, "ua-unbind", "rp-unbind", statusync.MatchSourceEmail)

	req := httptest.NewRequest("POST", "/ui/frag/member/04UNBIND01/unbind", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Trigger"); got != "member-updated" {
		t.Errorf("HX-Trigger = %q, want member-updated", got)
	}

	// (2) mapping gone
	if m, _ := db.GetMapping(ctx, "ua-unbind"); m != nil {
		t.Errorf("mapping should be deleted; got %+v", m)
	}
	// (1) audit row written
	audits, _ := db.ListMatchAudit(ctx, "ua-unbind", 0)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a.Field != "mapping" || a.Source != statusync.MatchSourceStaffUnbind {
		t.Errorf("audit = %+v; want field=mapping, source=staff:unbind", a)
	}
	if a.BeforeVal != "rp-unbind" || a.AfterVal != "" {
		t.Errorf("audit values wrong: before=%q after=%q; want before=rp-unbind after=\"\"",
			a.BeforeVal, a.AfterVal)
	}
	// (3) pending row created
	p, _ := db.GetPending(ctx, "ua-unbind")
	if p == nil {
		t.Fatal("pending row should be created after unbind")
	}
	if p.Reason != store.PendingReasonNoMatch {
		t.Errorf("pending reason = %q, want %q", p.Reason, store.PendingReasonNoMatch)
	}
	parsed, err := time.Parse(time.RFC3339, p.GraceUntil)
	if err != nil {
		t.Fatalf("GraceUntil should be RFC3339: %q (%v)", p.GraceUntil, err)
	}
	// Window is 7d ±; anything less than 6d means the handler regressed
	// to a short window and staff won't have time to act.
	if parsed.Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("GraceUntil = %v, want ~7 days out", parsed)
	}
	// (5) success alert names both sides of the unbind
	body := w.Body.String()
	if !strings.Contains(body, "Un Bind") || !strings.Contains(body, "rp-unbind") {
		t.Errorf("success alert should name member + customer; body = %q", body)
	}
}

// TestFragMemberUnbind_NoMapping — nothing to unbind, render a friendly
// alert (not a 500). The member cache row survives — Unbind doesn't
// imply Remove.
func TestFragMemberUnbind_NoMapping(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)
	seedMember(t, db, "04NOMAP0001", "rp-nomap", "No", "Map")

	req := httptest.NewRequest("POST", "/ui/frag/member/04NOMAP0001/unbind", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Trigger"); got != "" {
		t.Errorf("HX-Trigger should NOT fire on a no-op unbind; got %q", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nothing to unbind") {
		t.Errorf("expected 'Nothing to unbind' alert; body = %q", body)
	}
	// Member cache row survives — the operator can still open the detail panel.
	if m, _ := db.GetMemberByNFC(context.Background(), "04NOMAP0001"); m == nil {
		t.Error("member cache row should survive a no-op unbind")
	}
}

// TestFragMemberUnbind_MissingMember — unknown NFC returns an error
// alert, writes no audit, makes no UA-Hub call.
func TestFragMemberUnbind_MissingMember(t *testing.T) {
	srv, _, fakeUA, _ := buildNeedsMatchTestServer(t)

	req := httptest.NewRequest("POST", "/ui/frag/member/04GHOST999/unbind", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Member not found") {
		t.Errorf("expected 'Member not found' alert; body = %q", body)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("ghost unbind must not call UA-Hub; got %d status updates", got)
	}
}

// ─── POST /ui/frag/member/{nfcUid}/reactivate ──────────────────────────

// TestFragMemberReactivate_HappyPath — DEACTIVATED user gets flipped to
// ACTIVE. Assertions:
//  1. FakeUniFi.StatusUpdateCount() == 1 with status=ACTIVE
//  2. audit row field=user_status, before=DEACTIVATED, after=ACTIVE,
//     source=staff:reactivate
//  3. HX-Trigger: member-updated set
//  4. alert names the member
func TestFragMemberReactivate_HappyPath(t *testing.T) {
	srv, db, fakeUA, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	seedMember(t, db, "04REACT0001", "rp-react", "Re", "Act")
	seedMapping(t, db, "ua-react", "rp-react", statusync.MatchSourceStaffSkip)
	seedUAUser(t, db, "ua-react", "Re", "Act", "react@example.com", "DEACTIVATED")

	req := httptest.NewRequest("POST", "/ui/frag/member/04REACT0001/reactivate", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Trigger"); got != "member-updated" {
		t.Errorf("HX-Trigger = %q, want member-updated", got)
	}
	if got := fakeUA.StatusUpdateCount(); got != 1 {
		t.Errorf("UA-Hub status updates = %d, want 1 (ACTIVE PUT)", got)
	}
	// Also verify the recorded PUT body carried the right status —
	// guards against a handler regression that set something other
	// than "ACTIVE" on the wire.
	if len(fakeUA.StatusUpdates) != 1 || fakeUA.StatusUpdates[0].Status != "ACTIVE" {
		t.Errorf("UA-Hub PUT = %+v; want UserID=ua-react Status=ACTIVE", fakeUA.StatusUpdates)
	}

	audits, _ := db.ListMatchAudit(ctx, "ua-react", 0)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a.Field != "user_status" || a.BeforeVal != "DEACTIVATED" || a.AfterVal != "ACTIVE" ||
		a.Source != statusync.MatchSourceStaffReactivate {
		t.Errorf("audit = %+v; want user_status DEACTIVATED→ACTIVE source=staff:reactivate", a)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Re Act") {
		t.Errorf("success alert should name the member; body = %q", body)
	}
}

// TestFragMemberReactivate_NoMapping — reactivating an orphan should
// error out rather than try to call UA-Hub with an empty ID. No UA-Hub
// call, no audit row.
func TestFragMemberReactivate_NoMapping(t *testing.T) {
	srv, db, fakeUA, _ := buildNeedsMatchTestServer(t)
	seedMember(t, db, "04NOMAP0002", "rp-nomap2", "No", "Map")

	req := httptest.NewRequest("POST", "/ui/frag/member/04NOMAP0002/reactivate", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "no UA-Hub mapping") {
		t.Errorf("orphan reactivate should surface mapping error; body = %q", body)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("orphan reactivate must not call UA-Hub; got %d updates", got)
	}
	if got := w.Header().Get("HX-Trigger"); got != "" {
		t.Errorf("no-op reactivate must not fire HX-Trigger; got %q", got)
	}
}

// TestFragMemberReactivate_MissingUAUser — mapping exists but the mirror
// row hasn't been written yet (fresh auto-match, pre-mirror-walk). The
// handler tolerates the missing mirror, records "unknown" as the prior
// status, and still issues the UA-Hub PUT — the point is recovery, not
// pedantic state capture.
func TestFragMemberReactivate_MissingUAUser(t *testing.T) {
	srv, db, fakeUA, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	seedMember(t, db, "04NOMIRROR1", "rp-nomirror", "No", "Mirror")
	seedMapping(t, db, "ua-nomirror", "rp-nomirror", statusync.MatchSourceEmail)
	// NOTE: no seedUAUser call.

	req := httptest.NewRequest("POST", "/ui/frag/member/04NOMIRROR1/reactivate", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := fakeUA.StatusUpdateCount(); got != 1 {
		t.Errorf("UA-Hub status updates = %d, want 1 even without mirror", got)
	}
	audits, _ := db.ListMatchAudit(ctx, "ua-nomirror", 0)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	if audits[0].BeforeVal != "unknown" {
		t.Errorf("missing-mirror before_val = %q, want \"unknown\"", audits[0].BeforeVal)
	}
}

// ─── DELETE /members/{externalId} (v0.5.9 response shape) ──────────────

// TestRemoveMember_NewShape — DELETE /members on a real row should
// return:
//   - 200 with empty body (HTMX swaps empty into the row/panel)
//   - HX-Trigger: member-updated header so the member table refreshes
//
// The old JSON response was the v0.5.8 bug #118 regression (JSON landed
// as text inside the row <td>); pin both halves so nobody "helpfully"
// adds content back to the response.
func TestRemoveMember_NewShape(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)
	seedMember(t, db, "04DELETE001", "rp-delete", "De", "Lete")

	req := httptest.NewRequest("DELETE", "/members/04DELETE001", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Trigger"); got != "member-updated" {
		t.Errorf("HX-Trigger = %q, want member-updated", got)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "" {
		t.Errorf("body should be empty for HTMX swap; got %q", body)
	}
	// Row is gone.
	if m, _ := db.GetMemberByNFC(context.Background(), "04DELETE001"); m != nil {
		t.Errorf("member should be gone after DELETE; got %+v", m)
	}
}

// TestRemoveMember_MissingMember — DELETE on an unknown NFC returns an
// HTMX alert fragment (not a 404) so the row-level swap gets an error
// banner instead of an empty frame. Verifies the error path also
// withholds the HX-Trigger (we don't want the table to refresh on a
// no-op).
func TestRemoveMember_MissingMember(t *testing.T) {
	srv, _, _, _ := buildNeedsMatchTestServer(t)

	req := httptest.NewRequest("DELETE", "/members/04GHOST999", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Trigger"); got != "" {
		t.Errorf("missing-member DELETE should NOT fire HX-Trigger; got %q", got)
	}
	if !strings.Contains(w.Body.String(), "Member not found") {
		t.Errorf("expected alert body; got %q", w.Body.String())
	}
}

// ─── GET /ui/frag/member/{nfcUid}/reassign ─────────────────────────────

// TestFragMemberReassign_HappyPath — GET on a fully-bound member renders
// the reassign picker. The fragment should include the NFC UID, the
// current owner's identity, a search form, and the placeholder copy
// ("Type a name…") since no query has been submitted yet.
func TestFragMemberReassign_HappyPath(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)

	seedMember(t, db, "04CARDAA01", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")

	req := httptest.NewRequest("GET", "/ui/frag/member/04CARDAA01/reassign", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "04CARDAA01") {
		t.Errorf("NFC UID missing from panel; body = %q", body)
	}
	if !strings.Contains(body, "ua-alice") {
		t.Errorf("current owner UA-Hub ID missing; body = %q", body)
	}
	if !strings.Contains(body, "Alice Alpha") {
		t.Errorf("current owner name missing; body = %q", body)
	}
	// Search form is POST to the search endpoint.
	if !strings.Contains(body, `hx-post="/ui/frag/member/04CARDAA01/reassign/search"`) {
		t.Errorf("search form URL wrong; body = %q", body)
	}
	// Placeholder copy surfaces when Query is empty.
	if !strings.Contains(body, "Type a name or email") {
		t.Errorf("placeholder copy missing; body = %q", body)
	}
	// Cancel button wires back to detail.
	if !strings.Contains(body, `hx-get="/ui/frag/member/04CARDAA01/detail"`) {
		t.Errorf("cancel button URL wrong; body = %q", body)
	}
}

// TestFragMemberReassign_MissingMapping — member exists but no mapping.
// Reassign requires both sides for the audit trail, so the handler
// should render an error alert rather than the picker.
func TestFragMemberReassign_MissingMapping(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)

	seedMember(t, db, "04ORPHAN01", "rp-orphan", "Orphan", "Row")

	req := httptest.NewRequest("GET", "/ui/frag/member/04ORPHAN01/reassign", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "alert-error") {
		t.Errorf("expected an alert-error; body = %q", body)
	}
	if !strings.Contains(body, "no UA-Hub mapping") {
		t.Errorf("expected 'no UA-Hub mapping' hint; body = %q", body)
	}
}

// TestFragMemberReassign_MissingMember — handler returns an error alert
// for an unknown NFC UID.
func TestFragMemberReassign_MissingMember(t *testing.T) {
	srv, _, _, _ := buildNeedsMatchTestServer(t)

	req := httptest.NewRequest("GET", "/ui/frag/member/04NOPE9999/reassign", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Member not found") {
		t.Errorf("expected Member not found; body = %q", w.Body.String())
	}
}

// ─── POST /ui/frag/member/{nfcUid}/reassign/search ─────────────────────

// TestFragMemberReassignSearch_FindsTarget — three UA users (the current
// owner plus two candidates); a query on "bob" should surface Bob and
// exclude the current owner by ID even if their name would match too.
func TestFragMemberReassignSearch_FindsTarget(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)

	seedMember(t, db, "04CARDBB02", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")

	seedUAUser(t, db, "ua-bob", "Bob", "Brava", "bob@example.com", "ACTIVE")
	seedUAUser(t, db, "ua-charlie", "Charlie", "Cee", "charlie@example.com", "ACTIVE")

	form := strings.NewReader("q=bob")
	req := httptest.NewRequest("POST", "/ui/frag/member/04CARDBB02/reassign/search", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Candidate table surfaces Bob.
	if !strings.Contains(body, "ua-bob") {
		t.Errorf("Bob missing from candidates; body = %q", body)
	}
	if !strings.Contains(body, "Bob Brava") {
		t.Errorf("Bob's name missing; body = %q", body)
	}
	// Charlie should NOT match a "bob" query.
	if strings.Contains(body, "ua-charlie") {
		t.Errorf("Charlie unexpectedly surfaced on a 'bob' query; body = %q", body)
	}
	// Confirm button wires to the confirm endpoint with the target ID.
	if !strings.Contains(body, `hx-post="/ui/frag/member/04CARDBB02/reassign/confirm"`) {
		t.Errorf("confirm URL wrong; body = %q", body)
	}
	if !strings.Contains(body, `value="ua-bob"`) {
		t.Errorf("hidden targetUaUserId field missing; body = %q", body)
	}
}

// TestFragMemberReassignSearch_ExcludesCurrentOwner — a query that would
// otherwise match the current owner (say, search for "alice" when alice
// is the card holder) must NOT surface her in the picker. No self-
// reassigns.
func TestFragMemberReassignSearch_ExcludesCurrentOwner(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)

	seedMember(t, db, "04CARDCC03", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")
	// Different UA user with an overlapping name, same first-letter
	// query target — only this user should appear.
	seedUAUser(t, db, "ua-alice2", "Alice", "Other", "alice2@example.com", "ACTIVE")

	form := strings.NewReader("q=alice")
	req := httptest.NewRequest("POST", "/ui/frag/member/04CARDCC03/reassign/search", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The other Alice must show up.
	if !strings.Contains(body, "ua-alice2") {
		t.Errorf("ua-alice2 missing from candidates; body = %q", body)
	}
	// Current owner's UA-Hub ID must NOT appear inside the candidate
	// table rows. She's referenced once in the header ("bound to
	// Alice Alpha (ua-alice)") but a hidden input carrying her ID
	// shouldn't exist anywhere.
	if strings.Contains(body, `value="ua-alice"`) {
		t.Errorf("current owner surfaced as a candidate; body = %q", body)
	}
}

// TestFragMemberReassignSearch_NoMatches — the empty-results path
// renders the "No UA-Hub users matched" hint rather than a bare table.
func TestFragMemberReassignSearch_NoMatches(t *testing.T) {
	srv, db, _, _ := buildNeedsMatchTestServer(t)

	seedMember(t, db, "04CARDDD04", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")

	form := strings.NewReader("q=zzzzzzzz")
	req := httptest.NewRequest("POST", "/ui/frag/member/04CARDDD04/reassign/search", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No UA-Hub users matched") {
		t.Errorf("expected empty-match hint; body = %q", w.Body.String())
	}
}

// ─── POST /ui/frag/member/{nfcUid}/reassign/confirm ────────────────────

// TestFragMemberReassignConfirm_HappyPath — the full swap:
//   - UA-Hub PUT /users/ua-bob/nfc_cards fires with force_add=true
//   - two MatchAudit rows land (one under ua-alice, one under ua-bob)
//   - the member row rewrites its customer_id + first/last_name to match
//     the new UA-Hub user's Redpoint mapping
//   - HX-Trigger: member-updated fires so the table refreshes
func TestFragMemberReassignConfirm_HappyPath(t *testing.T) {
	srv, db, fakeUA, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	// Old side: Alice holds the card.
	seedMember(t, db, "04SWAP9001", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")

	// New side: Bob exists, has a mapping to his own Redpoint customer,
	// and that Redpoint customer is in cache.customers.
	if err := db.UpsertCustomerBatch(ctx, []store.Customer{
		{RedpointID: "rp-bob", FirstName: "Bob", LastName: "Brava",
			Email: "bob@example.com", Active: true},
	}); err != nil {
		t.Fatalf("UpsertCustomerBatch: %v", err)
	}
	seedMapping(t, db, "ua-bob", "rp-bob", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-bob", "Bob", "Brava", "bob@example.com", "ACTIVE")

	form := strings.NewReader("targetUaUserId=ua-bob")
	req := httptest.NewRequest("POST", "/ui/frag/member/04SWAP9001/reassign/confirm", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Trigger"); got != "member-updated" {
		t.Errorf("HX-Trigger = %q, want member-updated", got)
	}

	// UA-Hub AssignNFCCard was called with force_add=true.
	if len(fakeUA.NFCCardAssignments) != 1 {
		t.Fatalf("NFCCardAssignments = %d, want 1", len(fakeUA.NFCCardAssignments))
	}
	got := fakeUA.NFCCardAssignments[0]
	if got.UserID != "ua-bob" || got.Token != "04SWAP9001" || !got.ForceAdd {
		t.Errorf("AssignNFCCard = %+v; want UserID=ua-bob Token=04SWAP9001 ForceAdd=true", got)
	}

	// Two audit rows — one keyed on ua-alice (old, after=""), one keyed
	// on ua-bob (new, before=""). Both source=staff:reassign.
	oldRows, err := db.ListMatchAudit(ctx, "ua-alice", 10)
	if err != nil {
		t.Fatalf("ListMatchAudit old: %v", err)
	}
	if len(oldRows) != 1 {
		t.Fatalf("old-side audit rows = %d, want 1: %+v", len(oldRows), oldRows)
	}
	if oldRows[0].Field != "nfc_card" || oldRows[0].BeforeVal != "04SWAP9001" || oldRows[0].AfterVal != "" {
		t.Errorf("old audit = %+v; want field=nfc_card before=04SWAP9001 after=\"\"", oldRows[0])
	}
	if oldRows[0].Source != statusync.MatchSourceStaffReassign {
		t.Errorf("old audit source = %q, want %q", oldRows[0].Source, statusync.MatchSourceStaffReassign)
	}

	newRows, err := db.ListMatchAudit(ctx, "ua-bob", 10)
	if err != nil {
		t.Fatalf("ListMatchAudit new: %v", err)
	}
	if len(newRows) != 1 {
		t.Fatalf("new-side audit rows = %d, want 1: %+v", len(newRows), newRows)
	}
	if newRows[0].Field != "nfc_card" || newRows[0].BeforeVal != "" || newRows[0].AfterVal != "04SWAP9001" {
		t.Errorf("new audit = %+v; want field=nfc_card before=\"\" after=04SWAP9001", newRows[0])
	}

	// Member row should now point at Bob's Redpoint customer.
	m, err := db.GetMemberByNFC(ctx, "04SWAP9001")
	if err != nil {
		t.Fatalf("GetMemberByNFC: %v", err)
	}
	if m == nil {
		t.Fatal("member row missing after reassign")
	}
	if m.CustomerID != "rp-bob" {
		t.Errorf("member.CustomerID = %q, want rp-bob", m.CustomerID)
	}
	if m.FirstName != "Bob" || m.LastName != "Brava" {
		t.Errorf("member identity = %s %s, want Bob Brava", m.FirstName, m.LastName)
	}
}

// TestFragMemberReassignConfirm_NoTargetMapping — Bob has a mirror row
// but no mapping (i.e. he's still in Needs Match). The reassign must
// still succeed at UA-Hub and in the audit trail, but the member row
// is left pointing at Alice's Redpoint customer — the success alert
// calls this out so the operator doesn't wonder why the Members page
// still shows Alice's name.
func TestFragMemberReassignConfirm_NoTargetMapping(t *testing.T) {
	srv, db, fakeUA, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	seedMember(t, db, "04SWAP9002", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")

	// Bob exists in the mirror but has no mapping yet.
	seedUAUser(t, db, "ua-bob", "Bob", "Brava", "bob@example.com", "ACTIVE")

	form := strings.NewReader("targetUaUserId=ua-bob")
	req := httptest.NewRequest("POST", "/ui/frag/member/04SWAP9002/reassign/confirm", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if len(fakeUA.NFCCardAssignments) != 1 {
		t.Fatalf("NFCCardAssignments = %d, want 1", len(fakeUA.NFCCardAssignments))
	}
	// Member row stays pointing at Alice's Redpoint customer.
	m, err := db.GetMemberByNFC(ctx, "04SWAP9002")
	if err != nil {
		t.Fatalf("GetMemberByNFC: %v", err)
	}
	if m.CustomerID != "rp-alice" {
		t.Errorf("member.CustomerID = %q; expected unchanged (rp-alice) when target has no mapping", m.CustomerID)
	}
	// Success alert should acknowledge the transient-state caveat.
	body := w.Body.String()
	if !strings.Contains(body, "no Redpoint mapping yet") {
		t.Errorf("expected next-sync caveat in success alert; body = %q", body)
	}
}

// TestFragMemberReassignConfirm_SelfReassign — sending the current
// owner as the target should no-op with an error alert rather than
// double-writing audit rows.
func TestFragMemberReassignConfirm_SelfReassign(t *testing.T) {
	srv, db, fakeUA, _ := buildNeedsMatchTestServer(t)

	seedMember(t, db, "04SWAP9003", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")

	form := strings.NewReader("targetUaUserId=ua-alice")
	req := httptest.NewRequest("POST", "/ui/frag/member/04SWAP9003/reassign/confirm", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if len(fakeUA.NFCCardAssignments) != 0 {
		t.Errorf("self-reassign should NOT call UA-Hub; got %d calls", len(fakeUA.NFCCardAssignments))
	}
	if !strings.Contains(w.Body.String(), "same as the current owner") {
		t.Errorf("expected self-reassign error; body = %q", w.Body.String())
	}
}

// TestFragMemberReassignConfirm_MissingTarget — target UA user has no
// mirror row. Handler should refuse (the audit trail can't key on a
// non-existent user ID) without touching UA-Hub.
func TestFragMemberReassignConfirm_MissingTarget(t *testing.T) {
	srv, db, fakeUA, _ := buildNeedsMatchTestServer(t)

	seedMember(t, db, "04SWAP9004", "rp-alice", "Alice", "Alpha")
	seedMapping(t, db, "ua-alice", "rp-alice", statusync.MatchSourceEmail)
	seedUAUser(t, db, "ua-alice", "Alice", "Alpha", "alice@example.com", "ACTIVE")

	form := strings.NewReader("targetUaUserId=ua-ghost")
	req := httptest.NewRequest("POST", "/ui/frag/member/04SWAP9004/reassign/confirm", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if len(fakeUA.NFCCardAssignments) != 0 {
		t.Errorf("missing target should NOT call UA-Hub; got %d calls", len(fakeUA.NFCCardAssignments))
	}
	if !strings.Contains(w.Body.String(), "not found in the mirror") {
		t.Errorf("expected missing-target error; body = %q", w.Body.String())
	}
}
