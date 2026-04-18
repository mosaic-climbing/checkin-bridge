package api

// C2 Layer 4d — staff-UI provisioning flow tests.
//
// Coverage matrix (mapped to architecture-review.md C2 §"New-user
// provisioning flow"):
//
//   Gate         AllowNewMembers=false → 403 + alert on every route
//   Lookup       empty / no-match / ambiguous-no-name / disambig / single
//   Create       happy path: §3.2 user created + §3.6 policy attached +
//                mapping written + audit row landed
//   Create       no-match in Redpoint → error fragment, NO UA-Hub user
//   Create       collision: Redpoint customer already mapped → blocked
//   Enroll       POST /enroll → §6.2 session opened, polling fragment
//   Poll         pending session → polling fragment re-rendered
//   Poll         tap completed → §3.7 bind + complete fragment
//   Poll         tap completed but card belongs to other UA user →
//                §6.4 deletes session, failed fragment
//   Cancel       DELETE /enroll → §6.4 deletes session, retry fragment
//
// Pushover notification is intentionally not asserted: there's no
// notify package wired into the bridge yet (confirmed by `grep -rn
// "Pushover\|notify\." internal/`). When the notification path lands
// it should add an assertion here.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/cache"
	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// buildMembersNewTestServer wires a real store + cardmap + FakeUniFi +
// FakeRedpoint into an http-testable Server. The allowNewMembers flag
// is parameterised so the gate test can construct a disabled server
// without duplicating the whole rig.
//
// Note on policy IDs: tests pass a single fixed policy id "pol-members"
// which both proves boot validation didn't refuse the config (non-empty
// slice when AllowNewMembers=true) and gives the §3.6 assertion an
// exact slice to compare against.
func buildMembersNewTestServer(t *testing.T, allowNewMembers bool) (*Server, *store.Store, *testutil.FakeUniFi, *testutil.FakeRedpoint) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	db, err := store.Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cm, err := cardmap.New(dir, logger)
	if err != nil {
		t.Fatal(err)
	}

	fakeUA := testutil.NewFakeUniFi()
	t.Cleanup(fakeUA.Close)
	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)

	uaClient := unifi.NewClient("wss://unused", fakeUA.BaseURL(), "test-token", 500, "", logger)
	rpClient := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)

	syncer := cache.NewSyncer(db, rpClient, cache.SyncConfig{
		SyncInterval: 24 * time.Hour, PageSize: 100,
	}, logger)
	statusSyncer := statusync.New(uaClient, rpClient, db, statusync.Config{
		SyncInterval:       24 * time.Hour,
		RateLimitDelay:     time.Millisecond,
		UnmatchedGraceDays: 7,
	}, logger)
	handler := checkin.NewHandler(uaClient, rpClient, cm, db, "gate-1", logger)
	ingester := ingest.NewIngester(uaClient, rpClient, db, logger)
	sessionMgr := NewSessionManager("test-password")

	bgGroup := bg.New(context.Background(), logger)
	t.Cleanup(func() {
		bgGroup.Shutdown(context.Background())
	})

	var policyIDs []string
	if allowNewMembers {
		policyIDs = []string{"pol-members"}
	}

	srv := NewServer(
		handler, uaClient, rpClient, cm, syncer, statusSyncer, ingester,
		sessionMgr, nil /* audit */, "gate-1", logger, db, nil /* ui */, nil, nil, /* trustedProxies */
		bgGroup, false /* enableTestHooks */, allowNewMembers, policyIDs,
	)
	return srv, db, fakeUA, fakeRP
}

// seedRedpointCustomer is a one-line helper for the lookup tests.
// Each customer needs a unique ExternalID because FakeRedpoint keys
// the Customers map by ExternalID — household tests give two customers
// with the same Email but distinct ExternalIDs.
func seedRedpointCustomer(t *testing.T, fakeRP *testutil.FakeRedpoint, c testutil.FakeCustomer) {
	t.Helper()
	if c.ExternalID == "" {
		c.ExternalID = c.ID
	}
	fakeRP.AddCustomer(c)
}

// doRequest is a tiny helper to keep the test bodies focused on the
// invariants. Always sets the htmx header — the fragment endpoints
// require it via SecurityMiddleware in production, but here we drive
// the mux directly so it's just consistency with needs_match_test.
func doRequest(srv *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

// ─── Gate ─────────────────────────────────────────────────────────────

// TestMembersNew_GateBlocksWhenDisabled — when AllowNewMembers=false the
// page route returns 403 with a friendly inline alert (so staff sees a
// reason rather than the browser's default 403 page). The same gate
// guards the POST/GET fragment endpoints; spot-checking the page route
// is enough — they all share the requireProvisioning() helper.
func TestMembersNew_GateBlocksWhenDisabled(t *testing.T) {
	srv, _, fakeUA, _ := buildMembersNewTestServer(t, false)

	w := doRequest(srv, http.MethodGet, "/ui/members/new", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("page status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-error") {
		t.Errorf("403 body should be an alert-error fragment; got %q", w.Body.String())
	}

	// Also verify the POST is gated — the most consequential endpoint.
	form := strings.NewReader("first_name=A&last_name=B&email=a@b.com")
	w = doRequest(srv, http.MethodPost, "/ui/members/new", form)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST status = %d, want 403", w.Code)
	}
	if got := len(fakeUA.UsersCreated); got != 0 {
		t.Errorf("UA-Hub users created = %d, want 0 (gate must block before §3.2)", got)
	}
}

// ─── Email lookup ─────────────────────────────────────────────────────

// TestMembersNew_LookupEmpty — the live-validation endpoint debounces
// while the email field is empty; we shouldn't paint either a success
// or an error chip until the staff member starts typing.
func TestMembersNew_LookupEmpty(t *testing.T) {
	srv, _, _, _ := buildMembersNewTestServer(t, true)

	w := doRequest(srv, http.MethodGet, "/ui/members/new/lookup?email=", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := strings.TrimSpace(w.Body.String())
	if body != "" {
		t.Errorf("empty email should return empty body; got %q", body)
	}
}

// TestMembersNew_LookupNoMatch — a clean miss surfaces the "only paying
// members" message so staff can quickly diagnose typos.
func TestMembersNew_LookupNoMatch(t *testing.T) {
	srv, _, _, _ := buildMembersNewTestServer(t, true)

	w := doRequest(srv, http.MethodGet, "/ui/members/new/lookup?email=ghost@example.com", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No Redpoint customer") {
		t.Errorf(`"No Redpoint customer" missing; body = %q`, body)
	}
	if !strings.Contains(body, `data-lookup="error"`) {
		t.Errorf("error chip missing data-lookup=error attribute; body = %q", body)
	}
}

// TestMembersNew_LookupAmbiguousNoName — household sharing: two
// customers, same email, no first/last → the lookup should refuse and
// tell staff to add the name.
func TestMembersNew_LookupAmbiguousNoName(t *testing.T) {
	srv, _, _, fakeRP := buildMembersNewTestServer(t, true)
	seedRedpointCustomer(t, fakeRP, testutil.FakeCustomer{
		ID: "rp-A", ExternalID: "ext-A", FirstName: "Alice", LastName: "Smith",
		Email: "smith@example.com", Active: true, Badge: "ACTIVE",
	})
	seedRedpointCustomer(t, fakeRP, testutil.FakeCustomer{
		ID: "rp-B", ExternalID: "ext-B", FirstName: "Bob", LastName: "Smith",
		Email: "smith@example.com", Active: true, Badge: "ACTIVE",
	})

	w := doRequest(srv, http.MethodGet, "/ui/members/new/lookup?email=smith@example.com", nil)
	body := w.Body.String()
	if !strings.Contains(body, "matches 2 Redpoint customers") {
		t.Errorf("expected ambiguous-2-customers copy; body = %q", body)
	}
	if !strings.Contains(body, `data-lookup="error"`) {
		t.Errorf("ambiguous lookup should be an error chip; body = %q", body)
	}
}

// TestMembersNew_LookupAmbiguousWithName — same household, but staff has
// typed first+last so name disambiguation lands on a unique customer:
// the chip should be green with the right RedpointCustomerID.
func TestMembersNew_LookupAmbiguousWithName(t *testing.T) {
	srv, _, _, fakeRP := buildMembersNewTestServer(t, true)
	seedRedpointCustomer(t, fakeRP, testutil.FakeCustomer{
		ID: "rp-A", ExternalID: "ext-A", FirstName: "Alice", LastName: "Smith",
		Email: "smith@example.com", Active: true, Badge: "ACTIVE", BadgeName: "Adult",
	})
	seedRedpointCustomer(t, fakeRP, testutil.FakeCustomer{
		ID: "rp-B", ExternalID: "ext-B", FirstName: "Bob", LastName: "Smith",
		Email: "smith@example.com", Active: true, Badge: "ACTIVE", BadgeName: "Adult",
	})

	w := doRequest(srv, http.MethodGet,
		"/ui/members/new/lookup?email=smith@example.com&first_name=Bob&last_name=Smith", nil)
	body := w.Body.String()
	if !strings.Contains(body, `data-lookup="ok"`) {
		t.Errorf("disambiguated lookup should be a success chip; body = %q", body)
	}
	if !strings.Contains(body, `data-redpoint-customer-id="rp-B"`) {
		t.Errorf("expected rp-B in success chip; body = %q", body)
	}
}

// TestMembersNew_LookupSingleHit — one customer, one match: green chip,
// no name needed.
func TestMembersNew_LookupSingleHit(t *testing.T) {
	srv, _, _, fakeRP := buildMembersNewTestServer(t, true)
	seedRedpointCustomer(t, fakeRP, testutil.FakeCustomer{
		ID: "rp-1", ExternalID: "ext-1", FirstName: "Solo", LastName: "Climber",
		Email: "solo@example.com", Active: true, Badge: "ACTIVE",
	})

	w := doRequest(srv, http.MethodGet, "/ui/members/new/lookup?email=solo@example.com", nil)
	body := w.Body.String()
	if !strings.Contains(body, `data-lookup="ok"`) {
		t.Errorf("single-hit lookup should be a success chip; body = %q", body)
	}
	if !strings.Contains(body, `data-redpoint-customer-id="rp-1"`) {
		t.Errorf("expected rp-1 in success chip; body = %q", body)
	}
}

// ─── POST /ui/members/new (orchestration) ─────────────────────────────

// TestMembersNew_CreateHappyPath — the most important test: a clean
// create should hit §3.2 once, §3.6 once with the configured policy
// slice, write the mapping, and write a match-audit row. We assert each
// of those four invariants individually so a regression in one doesn't
// hide behind a red signal in another.
func TestMembersNew_CreateHappyPath(t *testing.T) {
	srv, db, fakeUA, fakeRP := buildMembersNewTestServer(t, true)
	seedRedpointCustomer(t, fakeRP, testutil.FakeCustomer{
		ID: "rp-happy", ExternalID: "ext-happy", FirstName: "Happy", LastName: "Path",
		Email: "happy@example.com", Active: true, Badge: "ACTIVE",
	})

	form := strings.NewReader("first_name=Happy&last_name=Path&email=happy@example.com")
	w := doRequest(srv, http.MethodPost, "/ui/members/new", form)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="members-new-result"`) {
		t.Errorf("response should embed members-new-result swap target; body = %q", body)
	}
	if !strings.Contains(body, "Created UA-Hub user") {
		t.Errorf("post-create copy missing; body = %q", body)
	}

	// §3.2: exactly one user created with the form's first/last/email.
	if got := len(fakeUA.UsersCreated); got != 1 {
		t.Fatalf("UsersCreated = %d, want 1", got)
	}
	uaUser := fakeUA.UsersCreated[0]
	if uaUser.FirstName != "Happy" || uaUser.LastName != "Path" || uaUser.Email != "happy@example.com" {
		t.Errorf("§3.2 body = %+v; want Happy/Path/happy@example.com", uaUser)
	}

	// §3.6: exactly one policy attach for that user, with the configured slice.
	if got := len(fakeUA.AccessPolicyAssignments); got != 1 {
		t.Fatalf("AccessPolicyAssignments = %d, want 1", got)
	}
	assignment := fakeUA.AccessPolicyAssignments[0]
	if assignment.UserID != uaUser.ID {
		t.Errorf("policy attach UserID = %q, want %q", assignment.UserID, uaUser.ID)
	}
	if len(assignment.PolicyIDs) != 1 || assignment.PolicyIDs[0] != "pol-members" {
		t.Errorf("policy IDs = %v, want [pol-members]", assignment.PolicyIDs)
	}

	// Mapping row written.
	mapping, err := db.GetMapping(context.Background(), uaUser.ID)
	if err != nil || mapping == nil {
		t.Fatalf("mapping for %s missing; err=%v mapping=%+v", uaUser.ID, err, mapping)
	}
	if mapping.RedpointCustomer != "rp-happy" {
		t.Errorf("mapping.RedpointCustomer = %q, want rp-happy", mapping.RedpointCustomer)
	}
	if mapping.MatchedBy != statusync.MatchSourceEmail {
		t.Errorf("mapping.MatchedBy = %q, want %q", mapping.MatchedBy, statusync.MatchSourceEmail)
	}

	// Audit row written.
	audits, err := db.ListMatchAudit(context.Background(), uaUser.ID, 0)
	if err != nil {
		t.Fatalf("ListMatchAudit: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a.Field != "mapping" || a.AfterVal != "rp-happy" || a.Source != statusync.MatchSourceEmail {
		t.Errorf("audit = %+v; want field=mapping after=rp-happy source=%s", a, statusync.MatchSourceEmail)
	}
}

// TestMembersNew_CreateNoMatch — POSTing with an email that doesn't
// exist in Redpoint must not call §3.2. The bridge only provisions
// UA-Hub users for paying members, and that policy is enforced
// server-side (the form's live lookup is a UX hint).
func TestMembersNew_CreateNoMatch(t *testing.T) {
	srv, db, fakeUA, _ := buildMembersNewTestServer(t, true)

	form := strings.NewReader("first_name=Ghost&last_name=Member&email=ghost@example.com")
	w := doRequest(srv, http.MethodPost, "/ui/members/new", form)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (alert in body); body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "alert-error") || !strings.Contains(body, "No clean Redpoint match") {
		t.Errorf("expected no-match alert; body = %q", body)
	}
	if got := len(fakeUA.UsersCreated); got != 0 {
		t.Errorf("§3.2 must not run on no-match: UsersCreated = %d", got)
	}
	if got := len(fakeUA.AccessPolicyAssignments); got != 0 {
		t.Errorf("§3.6 must not run on no-match: AccessPolicyAssignments = %d", got)
	}
	// No mapping landed.
	mappings, _ := db.AllMappings(context.Background())
	if len(mappings) != 0 {
		t.Errorf("mappings = %d, want 0", len(mappings))
	}
}

// TestMembersNew_CreateCollision — the Redpoint customer is already
// bound to a different UA-Hub user. The handler must refuse, leave
// state untouched, and surface the collision so staff can pick a
// different customer or un-match the existing one. Same contract as
// the staff:match collision handled in needs_match_test.
func TestMembersNew_CreateCollision(t *testing.T) {
	srv, db, fakeUA, fakeRP := buildMembersNewTestServer(t, true)
	seedRedpointCustomer(t, fakeRP, testutil.FakeCustomer{
		ID: "rp-taken", ExternalID: "ext-taken", FirstName: "Taken", LastName: "Twice",
		Email: "taken@example.com", Active: true, Badge: "ACTIVE",
	})
	// Pre-existing mapping that owns rp-taken.
	if err := db.UpsertMapping(context.Background(), &store.Mapping{
		UAUserID:         "ua-existing",
		RedpointCustomer: "rp-taken",
		MatchedBy:        statusync.MatchSourceEmail,
	}); err != nil {
		t.Fatal(err)
	}

	form := strings.NewReader("first_name=Taken&last_name=Twice&email=taken@example.com")
	w := doRequest(srv, http.MethodPost, "/ui/members/new", form)
	body := w.Body.String()
	if !strings.Contains(body, "already bound") {
		t.Errorf("expected collision alert; body = %q", body)
	}
	if got := len(fakeUA.UsersCreated); got != 0 {
		t.Errorf("§3.2 must not run on collision: UsersCreated = %d", got)
	}
	// Existing mapping survives.
	existing, _ := db.GetMapping(context.Background(), "ua-existing")
	if existing == nil || existing.RedpointCustomer != "rp-taken" {
		t.Errorf("existing mapping should be unchanged: %+v", existing)
	}
}

// ─── Enrollment lifecycle ─────────────────────────────────────────────

// TestMembersNew_EnrollStart — POST /enroll opens a §6.2 session and
// returns the polling fragment. The polling fragment is identifiable by
// its `every 500ms` HTMX trigger; that attribute is what keeps the
// client-side loop alive until a terminal fragment swaps it out.
func TestMembersNew_EnrollStart(t *testing.T) {
	srv, _, fakeUA, _ := buildMembersNewTestServer(t, true)

	form := strings.NewReader("device_id=reader-1")
	w := doRequest(srv, http.MethodPost, "/ui/members/new/ua-test/enroll", form)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "every 500ms") {
		t.Errorf("polling fragment must contain hx-trigger=every 500ms; body = %q", body)
	}
	if !strings.Contains(body, "waiting for card tap") {
		t.Errorf("polling copy missing; body = %q", body)
	}
	if got := len(fakeUA.Sessions); got != 1 {
		t.Errorf("Sessions = %d, want 1", got)
	}
}

// TestMembersNew_EnrollStartMissingDevice — the form must include a
// device_id; without it the handler refuses without opening a session.
func TestMembersNew_EnrollStartMissingDevice(t *testing.T) {
	srv, _, fakeUA, _ := buildMembersNewTestServer(t, true)

	w := doRequest(srv, http.MethodPost, "/ui/members/new/ua-test/enroll", strings.NewReader(""))
	body := w.Body.String()
	if !strings.Contains(body, "Pick a reader") {
		t.Errorf("expected pick-a-reader alert; body = %q", body)
	}
	if got := len(fakeUA.Sessions); got != 0 {
		t.Errorf("Sessions = %d, want 0 (no §6.2 call without device)", got)
	}
}

// TestMembersNew_PollPending — the session has no token yet (still
// waiting for a tap). The poll endpoint must re-render the polling
// fragment so HTMX continues the every-500ms loop. The §3.7 bind must
// NOT fire.
func TestMembersNew_PollPending(t *testing.T) {
	srv, _, fakeUA, _ := buildMembersNewTestServer(t, true)

	// Open a session via the start handler so we get a real session id.
	startForm := strings.NewReader("device_id=reader-1")
	doRequest(srv, http.MethodPost, "/ui/members/new/ua-poll/enroll", startForm)
	var sid string
	for k := range fakeUA.Sessions {
		sid = k
	}
	if sid == "" {
		t.Fatal("no session opened")
	}

	w := doRequest(srv, http.MethodGet, "/ui/members/new/ua-poll/enroll/"+sid+"/poll", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "every 500ms") {
		t.Errorf("pending poll should re-render polling fragment; body = %q", body)
	}
	if got := len(fakeUA.NFCCardAssignments); got != 0 {
		t.Errorf("§3.7 bind must not fire on pending: NFCCardAssignments = %d", got)
	}
}

// TestMembersNew_PollComplete — the session has a token: §3.7 must
// fire with forceAdd=false (we just verified ownership via §6.7 above
// it), and the response must be the terminal complete fragment (no
// every-500ms trigger so the polling loop terminates).
func TestMembersNew_PollComplete(t *testing.T) {
	srv, _, fakeUA, _ := buildMembersNewTestServer(t, true)

	startForm := strings.NewReader("device_id=reader-1")
	doRequest(srv, http.MethodPost, "/ui/members/new/ua-complete/enroll", startForm)
	var sid string
	for k := range fakeUA.Sessions {
		sid = k
	}
	// Simulate the card tap landing: token now present.
	fakeUA.CompleteSession(sid, "tok-abc", "card-1")

	w := doRequest(srv, http.MethodGet, "/ui/members/new/ua-complete/enroll/"+sid+"/poll", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "is enrolled") {
		t.Errorf("expected complete fragment copy; body = %q", body)
	}
	if strings.Contains(body, "every 500ms") {
		t.Error("complete fragment must not contain hx-trigger=every 500ms (would loop forever)")
	}

	if got := len(fakeUA.NFCCardAssignments); got != 1 {
		t.Fatalf("NFCCardAssignments = %d, want 1", got)
	}
	bind := fakeUA.NFCCardAssignments[0]
	if bind.UserID != "ua-complete" || bind.Token != "tok-abc" {
		t.Errorf("§3.7 = %+v; want user=ua-complete token=tok-abc", bind)
	}
	if bind.ForceAdd {
		t.Error("§3.7 forceAdd must be false (we explicitly checked §6.7 first)")
	}
}

// TestMembersNew_PollCardOwnedByOtherUser — §6.7 reports the card is
// already bound to a different UA user. The handler must NOT call §3.7
// (would silently steal the card), must delete the in-flight session
// (§6.4) so the reader exits enrollment mode, and must surface an
// error fragment so staff can fix the misclick.
func TestMembersNew_PollCardOwnedByOtherUser(t *testing.T) {
	srv, _, fakeUA, _ := buildMembersNewTestServer(t, true)

	startForm := strings.NewReader("device_id=reader-1")
	doRequest(srv, http.MethodPost, "/ui/members/new/ua-victim/enroll", startForm)
	var sid string
	for k := range fakeUA.Sessions {
		sid = k
	}
	fakeUA.CompleteSession(sid, "tok-stolen", "card-1")
	fakeUA.AddCardOwner("tok-stolen", testutil.CardOwner{
		Token: "tok-stolen", CardID: "card-1",
		UserID: "ua-someone-else", UserName: "Existing Owner",
	})

	w := doRequest(srv, http.MethodGet, "/ui/members/new/ua-victim/enroll/"+sid+"/poll", nil)
	body := w.Body.String()
	if !strings.Contains(body, "alert-error") || !strings.Contains(body, "already bound") {
		t.Errorf("expected card-already-bound error; body = %q", body)
	}
	if got := len(fakeUA.NFCCardAssignments); got != 0 {
		t.Errorf("§3.7 must NOT fire on collision: NFCCardAssignments = %d", got)
	}
	if got := len(fakeUA.DeletedSessions); got != 1 {
		t.Errorf("§6.4 must run to drop the orphan session: DeletedSessions = %d", got)
	}
}

// TestMembersNew_EnrollCancel — DELETE /enroll calls §6.4 and re-renders
// the post-create reader picker so staff can retry the tap without
// losing the just-created UA-Hub user.
func TestMembersNew_EnrollCancel(t *testing.T) {
	srv, _, fakeUA, _ := buildMembersNewTestServer(t, true)

	startForm := strings.NewReader("device_id=reader-1")
	doRequest(srv, http.MethodPost, "/ui/members/new/ua-cancel/enroll", startForm)
	var sid string
	for k := range fakeUA.Sessions {
		sid = k
	}

	w := doRequest(srv, http.MethodDelete, "/ui/members/new/ua-cancel/enroll/"+sid, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Start enrollment") {
		t.Errorf("cancel should re-render the start-enrollment form; body = %q", body)
	}
	if got := len(fakeUA.DeletedSessions); got != 1 {
		t.Errorf("§6.4 should fire once: DeletedSessions = %d", got)
	}
}
