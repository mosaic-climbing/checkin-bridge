package api

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

// buildNeedsMatchTestServer wires a real store + cardmap + FakeUniFi +
// FakeRedpoint into an http-testable Server. Returns the server, the
// store (for seeding pending rows and reading audit back), and the
// FakeUniFi (for asserting that skip sent a DEACTIVATED PUT).
//
// A shared helper factored out from setupTestServer because these tests
// want a *live* fake UA-Hub (so UpdateUserStatus actually lands) rather
// than the unreachable "fake:12445" URL the existing server_test uses.
func buildNeedsMatchTestServer(t *testing.T) (*Server, *store.Store, *testutil.FakeUniFi) {
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

	// Create a supervised group for background tasks
	bgGroup := bg.New(context.Background(), logger)
	t.Cleanup(func() {
		bgGroup.Shutdown(context.Background())
	})

	srv := NewServer(
		handler, uaClient, rpClient, cm, syncer, statusSyncer, ingester,
		sessionMgr, nil /* audit */, "gate-1", logger, db, nil /* ui */, nil, nil, /* trustedProxies */
		bgGroup, false /* enableTestHooks */, false /* allowNewMembers */, nil, /* defaultAccessPolicyIDs */
	)
	return srv, db, fakeUA
}

// seedPending is a tiny helper — every test needs at least one pending
// row and the UpsertPending call is noisy.
func seedPending(t *testing.T, db *store.Store, uaUserID, reason, candidates string, graceOffset time.Duration) {
	t.Helper()
	grace := time.Now().Add(graceOffset).UTC().Format(time.RFC3339)
	if err := db.UpsertPending(context.Background(), &store.Pending{
		UAUserID:   uaUserID,
		Reason:     reason,
		GraceUntil: grace,
		Candidates: candidates,
	}); err != nil {
		t.Fatal(err)
	}
}

// seedPendingWithIdentity seeds a pending row that also carries a cached
// UA-Hub display name + email, matching what statusync.persistDecision
// writes at observation time (v0.5.2). Drives the list/detail renderers
// without any live UA-Hub dependency.
func seedPendingWithIdentity(t *testing.T, db *store.Store,
	uaUserID, reason, candidates, uaName, uaEmail string, graceOffset time.Duration,
) {
	t.Helper()
	grace := time.Now().Add(graceOffset).UTC().Format(time.RFC3339)
	if err := db.UpsertPending(context.Background(), &store.Pending{
		UAUserID:   uaUserID,
		Reason:     reason,
		GraceUntil: grace,
		Candidates: candidates,
		UAName:     uaName,
		UAEmail:    uaEmail,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestNeedsMatchList_EmptyState — zero pending rows renders the "nothing
// to match" copy plus a 0 badge; the absence of a <table> is the invariant.
func TestNeedsMatchList_EmptyState(t *testing.T) {
	srv, _, _ := buildNeedsMatchTestServer(t)

	req := httptest.NewRequest("GET", "/ui/frag/unmatched-list", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req) // bypass SecurityMiddleware — the test focuses on handler logic

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nothing to match") {
		t.Errorf("empty-state text missing; body = %q", body)
	}
	if strings.Contains(body, "<table>") {
		t.Errorf("table shouldn't render in empty state; body = %q", body)
	}
}

// TestNeedsMatchList_RendersPendingRows — two pending rows get one <tr>
// each, and the headline count shows "2".
func TestNeedsMatchList_RendersPendingRows(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)
	seedPending(t, db, "ua-A", store.PendingReasonNoMatch, "", 24*time.Hour)
	seedPending(t, db, "ua-B", store.PendingReasonAmbiguousEmail, "rp-1|rp-2", 24*time.Hour)

	req := httptest.NewRequest("GET", "/ui/frag/unmatched-list", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "ua-A") || !strings.Contains(body, "ua-B") {
		t.Errorf("both UA IDs should appear in the table; body = %q", body)
	}
	if !strings.Contains(body, "ambiguous email (2)") {
		t.Errorf("ambiguous-email reason should render candidate count; body = %q", body)
	}
	// Headline stat-card shows the pending count.
	if !strings.Contains(body, `<div class="stat-value">2</div>`) {
		t.Errorf("headline count should be 2; body = %q", body)
	}
}

// TestNeedsMatchList_RendersCachedIdentity pins the v0.5.2 fix: the list
// fragment must render the ua_name + ua_email cached on the pending row
// WITHOUT making any live UA-Hub ListUsers call. buildNeedsMatchTestServer
// wires a FakeUniFi with zero users, so if the handler fell back to the
// old "walk UA-Hub, match by id, enrich" path the name/email would be
// absent from the rendered body. Asserting their presence is the
// regression pin.
func TestNeedsMatchList_RendersCachedIdentity(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)
	seedPendingWithIdentity(t, db,
		"ua-cached", store.PendingReasonNoMatch, "",
		"Dana Cached", "dana.cached@example.com",
		24*time.Hour,
	)

	req := httptest.NewRequest("GET", "/ui/frag/unmatched-list", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Dana Cached") {
		t.Errorf("cached ua_name missing from list; body = %q", body)
	}
	if !strings.Contains(body, "dana.cached@example.com") {
		t.Errorf("cached ua_email missing from list; body = %q", body)
	}
}

// TestNeedsMatchDefer_ExtendsGraceAndAudits — grace_until should move
// roughly 7 days forward, and a staff:defer audit row should land.
func TestNeedsMatchDefer_ExtendsGraceAndAudits(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)
	// Seed with a grace window that's almost gone.
	seedPending(t, db, "ua-defer", store.PendingReasonNoMatch, "", 1*time.Hour)

	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-defer/defer", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	p, _ := db.GetPending(context.Background(), "ua-defer")
	if p == nil {
		t.Fatal("pending row should still exist after defer")
	}
	parsed, err := time.Parse(time.RFC3339, p.GraceUntil)
	if err != nil {
		t.Fatalf("GraceUntil should be RFC3339: %q (%v)", p.GraceUntil, err)
	}
	minAcceptable := time.Now().Add(6 * 24 * time.Hour)
	if parsed.Before(minAcceptable) {
		t.Errorf("GraceUntil = %v; want >= ~%v (+7 days)", parsed, minAcceptable)
	}

	audits, _ := db.ListMatchAudit(context.Background(), "ua-defer", 0)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	if audits[0].Source != statusync.MatchSourceStaffDefer || audits[0].Field != "grace_until" {
		t.Errorf("audit = %+v; want source=staff:defer, field=grace_until", audits[0])
	}
}

// TestNeedsMatchDefer_MissingRow — defer against a uaUserID that isn't
// in pending should return a user-facing error, not panic or 500.
func TestNeedsMatchDefer_MissingRow(t *testing.T) {
	srv, _, _ := buildNeedsMatchTestServer(t)

	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-ghost/defer", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		// We still return 200 for HTMX fragments — the error is in the
		// body as an .alert-error. Verify both.
		t.Errorf("status = %d, want 200 with alert-error body", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-error") {
		t.Errorf("missing row should render an error alert; body = %q", w.Body.String())
	}
}

// TestNeedsMatchSkip_DeactivatesAndAudits — skip should hit UA-Hub with
// a DEACTIVATED PUT, drop the pending row, and write a staff:skip audit.
func TestNeedsMatchSkip_DeactivatesAndAudits(t *testing.T) {
	srv, db, fakeUA := buildNeedsMatchTestServer(t)
	seedPending(t, db, "ua-skip", store.PendingReasonNoMatch, "", 24*time.Hour)

	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-skip/skip", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if got := fakeUA.StatusUpdateCount(); got != 1 {
		t.Errorf("UA status-update count = %d, want 1 (DEACTIVATED PUT)", got)
	}
	if p, _ := db.GetPending(context.Background(), "ua-skip"); p != nil {
		t.Errorf("pending row should be gone after skip: %+v", p)
	}
	audits, _ := db.ListMatchAudit(context.Background(), "ua-skip", 0)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a.Field != "user_status" || a.BeforeVal != "ACTIVE" || a.AfterVal != "DEACTIVATED" || a.Source != statusync.MatchSourceStaffSkip {
		t.Errorf("audit = %+v; want user_status ACTIVE→DEACTIVATED source=staff:skip", a)
	}
}

// TestNeedsMatchMatch_CollisionBlocked — if the selected Redpoint
// customer is already bound to a different UA user, the handler must
// refuse and leave state untouched. This is a contract invariant of
// the mapping table's UNIQUE(redpoint_customer_id) constraint —
// surfacing it in the UI rather than letting the SQL error through.
func TestNeedsMatchMatch_CollisionBlocked(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)

	// Pre-existing mapping for someone else.
	if err := db.UpsertMapping(context.Background(), &store.Mapping{
		UAUserID:         "ua-other",
		RedpointCustomer: "rp-conflict",
		MatchedBy:        statusync.MatchSourceEmail,
	}); err != nil {
		t.Fatal(err)
	}
	seedPending(t, db, "ua-new", store.PendingReasonNoMatch, "", 24*time.Hour)

	form := strings.NewReader("redpointCustomerId=rp-conflict")
	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-new/match", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "already bound") {
		t.Errorf("expected collision error, got body = %q", body)
	}
	// Pending row must still be present — staff needs to try again.
	if p, _ := db.GetPending(context.Background(), "ua-new"); p == nil {
		t.Error("pending row should be preserved when match is refused")
	}
	// Other user's mapping untouched.
	if m, _ := db.GetMapping(context.Background(), "ua-other"); m == nil || m.RedpointCustomer != "rp-conflict" {
		t.Errorf("existing mapping should survive refused match: %+v", m)
	}
}

// TestNeedsMatchSearch_EmptyQuery — POSTing /search with an empty "q"
// just rerenders the detail panel (candidates come from the pending
// row's Candidates field, not the search). Guards against a nil-deref
// on empty form.
func TestNeedsMatchSearch_EmptyQuery(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)
	seedPending(t, db, "ua-search", store.PendingReasonNoMatch, "", 24*time.Hour)

	form := strings.NewReader("q=")
	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-search/search", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "ua-search") {
		t.Errorf("detail panel should show UA ID; body = %q", body)
	}
	// Empty-query panel should offer the search box (so staff can
	// actually enter a name) and the skip/defer buttons.
	if !strings.Contains(body, "Skip (deactivate now)") {
		t.Errorf("skip button missing from empty-query panel; body = %q", body)
	}
	// v0.5.7 — the placeholder tells staff they can search by email too.
	if !strings.Contains(body, "Search Redpoint by name or email") {
		t.Errorf("updated placeholder should advertise email search; body = %q", body)
	}
}

// TestNeedsMatchSearch_ByEmail_HitsLocalCache — the v0.5.7 fix: search
// should resolve a customer out of cache.customers via FTS5 using an
// email query. Previously this called the live Redpoint name-only
// search, which had two failure modes: (1) no email path at all, so
// staff who knew a member's email had to translate it to a name
// guess; (2) flaky during Redpoint 429 storms (we saw this with the
// tap-poller overload on Apr 20).
//
// Seeds the cache directly (bypassing FakeRedpoint) because the test
// concern is "the handler goes to the local mirror", not "the mirror
// was populated correctly earlier" — the latter is covered by
// customers_fts_test.go.
func TestNeedsMatchSearch_ByEmail_HitsLocalCache(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	// Two customers in the local mirror; only one matches the email.
	for _, c := range []store.Customer{
		{RedpointID: "rp-target", FirstName: "Ainsley", LastName: "Lightcap",
			Email: "ainsley@example.com", Active: true},
		{RedpointID: "rp-other", FirstName: "Someone", LastName: "Else",
			Email: "else@example.com", Active: true},
	} {
		if err := db.UpsertCustomer(ctx, &c); err != nil {
			t.Fatalf("UpsertCustomer %s: %v", c.RedpointID, err)
		}
	}
	seedPending(t, db, "ua-search", store.PendingReasonNoMatch, "", 24*time.Hour)

	form := strings.NewReader("q=ainsley@example.com")
	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-search/search", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "rp-target") {
		t.Errorf("email search should surface the matching customer; body = %q", body)
	}
	if strings.Contains(body, "rp-other") {
		t.Errorf("email search should not surface the non-matching customer; body = %q", body)
	}
	// The search box echoes the last query so the staff member keeps
	// context after the fragment swap — pin it.
	if !strings.Contains(body, `value="ainsley@example.com"`) {
		t.Errorf("search box should echo the query; body = %q", body)
	}
}

// TestNeedsMatchSearch_ByName_HitsLocalCache — companion to the email
// test: verify the name path still works after the swap. This is the
// "it doesn't always work" failure mode Chris reported — under the
// old code, live Redpoint search with "Last, First" format missed for
// single-token queries, hyphenated names, and during any upstream
// flakiness. FTS5 prefix-AND handles all of these.
func TestNeedsMatchSearch_ByName_HitsLocalCache(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	if err := db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "rp-dana",
		FirstName:  "Dana",
		LastName:   "Skoglund-Jones", // hyphenated last: trips the old splitName + "Last, First" path
		Email:      "dana@example.com",
		Active:     true,
	}); err != nil {
		t.Fatalf("UpsertCustomer: %v", err)
	}
	seedPending(t, db, "ua-search", store.PendingReasonNoMatch, "", 24*time.Hour)

	// Single-token first-name query — the old path sent this as
	// "Last: , First: Dana" which missed on Redpoint's filter. FTS5
	// prefix-AND over all indexed columns finds it.
	form := strings.NewReader("q=dana")
	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-search/search", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rp-dana") {
		t.Errorf("single-token name search should hit; body = %q", w.Body.String())
	}
}

// TestNeedsMatchSearch_PrefixAND verifies the multi-word query shape:
// "dana skog" should AND both tokens as prefixes and still find
// "Dana Skoglund-Jones". Guards against a regression where someone
// "simplifies" the FTS query builder to an OR.
func TestNeedsMatchSearch_PrefixAND(t *testing.T) {
	srv, db, _ := buildNeedsMatchTestServer(t)
	ctx := context.Background()

	for _, c := range []store.Customer{
		{RedpointID: "rp-dana", FirstName: "Dana", LastName: "Skoglund-Jones",
			Email: "dana@example.com", Active: true},
		{RedpointID: "rp-other-dana", FirstName: "Dana", LastName: "Carter",
			Email: "carter@example.com", Active: true},
	} {
		if err := db.UpsertCustomer(ctx, &c); err != nil {
			t.Fatalf("UpsertCustomer: %v", err)
		}
	}
	seedPending(t, db, "ua-search", store.PendingReasonNoMatch, "", 24*time.Hour)

	form := strings.NewReader("q=dana+skog")
	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-search/search", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "rp-dana") {
		t.Errorf("prefix-AND should find Dana Skoglund-Jones on 'dana skog'; body = %q", body)
	}
	// The other Dana shouldn't show — her last name doesn't have the
	// 'skog' prefix, so the AND excludes her.
	if strings.Contains(body, "rp-other-dana") {
		t.Errorf("prefix-AND should exclude the other Dana; body = %q", body)
	}
}
