package statusync

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// buildTestSyncer assembles a Syncer wired to real SQLite + fake Redpoint.
// UA client isn't exercised by matchOne/persistDecision, so a nil-safe stub
// URL is fine — we'll never call its methods here.
func buildTestSyncer(t *testing.T, fake *testutil.FakeRedpoint, graceDays int) (*Syncer, *store.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rp := redpoint.NewClient(fake.GraphQLURL(), "test-api-key", "TEST", logger)
	// unifi client is constructed but never called here; ListAllUsersWithStatus
	// etc. aren't part of the matchOne path.
	ua := unifi.NewClient("wss://unused", "http://unused", "test-token", 500, "", logger)
	s := New(ua, rp, db, Config{UnmatchedGraceDays: graceDays}, false /* shadowMode */, nil /* metrics */, logger)
	return s, db
}

// TestMatchOne_EmailSingleHit — the happy path: UA user, one Redpoint row
// with that email, mapping persisted, no pending, no audit-bypass.
func TestMatchOne_EmailSingleHit(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-1", ExternalID: "ext-1",
		FirstName: "Alex", LastName: "Smith",
		Email: "alex@example.com", Active: true, Badge: "ACTIVE",
	})
	s, db := buildTestSyncer(t, fake, 7)
	ua := unifi.UniFiUser{ID: "ua-1", FirstName: "Alex", LastName: "Smith", Email: "alex@example.com"}

	d, err := s.matchOne(context.Background(), ua)
	if err != nil {
		t.Fatalf("matchOne: %v", err)
	}
	if d.Matched == nil || d.Matched.ID != "rp-1" {
		t.Fatalf("Matched = %+v, want rp-1", d.Matched)
	}
	if d.Source != MatchSourceEmail {
		t.Errorf("Source = %q, want %q", d.Source, MatchSourceEmail)
	}

	m, err := db.GetMapping(context.Background(), "ua-1")
	if err != nil || m == nil {
		t.Fatalf("GetMapping = (%+v, %v)", m, err)
	}
	if m.RedpointCustomer != "rp-1" || m.MatchedBy != MatchSourceEmail {
		t.Errorf("mapping = %+v", m)
	}
	p, _ := db.GetPending(context.Background(), "ua-1")
	if p != nil {
		t.Errorf("GetPending = %+v; a matched user must not be pending", p)
	}

	// Audit row: new mapping, before_val empty, after_val rp-1, source auto:email.
	audits, err := db.ListMatchAudit(context.Background(), "ua-1", 0)
	if err != nil || len(audits) != 1 {
		t.Fatalf("audits = %+v, err=%v; want exactly 1", audits, err)
	}
	a := audits[0]
	if a.Field != "mapping" || a.BeforeVal != "" || a.AfterVal != "rp-1" || a.Source != MatchSourceEmail {
		t.Errorf("audit row = %+v", a)
	}
}

// TestMatchOne_HouseholdCollision — two customers share the email, name
// disambiguates. This is the regression test the whole C2 work exists to
// protect: a parent's email on a child's account must not silently bind
// the child's UA user to the parent's Redpoint row.
func TestMatchOne_HouseholdCollision(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-parent", ExternalID: "ext-parent",
		FirstName: "Jamie", LastName: "Lee",
		Email: "jamie@example.com", Active: true, Badge: "ACTIVE",
	})
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-child", ExternalID: "ext-child",
		FirstName: "Robin", LastName: "Lee",
		Email: "jamie@example.com", Active: true, Badge: "ACTIVE",
	})
	s, db := buildTestSyncer(t, fake, 7)

	// UA user is the child. Email matches both; name disambiguates to child.
	d, err := s.matchOne(context.Background(), unifi.UniFiUser{
		ID: "ua-child", FirstName: "Robin", LastName: "Lee", Email: "jamie@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Matched == nil || d.Matched.ID != "rp-child" {
		t.Fatalf("Matched = %+v, want rp-child", d.Matched)
	}
	if d.Source != MatchSourceEmailAndName {
		t.Errorf("Source = %q, want %q", d.Source, MatchSourceEmailAndName)
	}
	m, _ := db.GetMapping(context.Background(), "ua-child")
	if m == nil || m.RedpointCustomer != "rp-child" {
		t.Errorf("mapping = %+v, want bound to rp-child", m)
	}
}

// TestMatchOne_AmbiguousEmailPending — two customers share the email
// but neither matches the UA user's name. Persists pending with all
// candidate ids so the staff UI can render the shortlist.
func TestMatchOne_AmbiguousEmailPending(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-a", ExternalID: "ext-a",
		FirstName: "Alex", LastName: "Smith",
		Email: "shared@example.com", Active: true, Badge: "ACTIVE",
	})
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-b", ExternalID: "ext-b",
		FirstName: "Jamie", LastName: "Lee",
		Email: "shared@example.com", Active: true, Badge: "ACTIVE",
	})
	s, db := buildTestSyncer(t, fake, 7)

	d, err := s.matchOne(context.Background(), unifi.UniFiUser{
		ID: "ua-unknown", FirstName: "Chris", LastName: "Evans", Email: "shared@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Matched != nil {
		t.Errorf("Matched = %+v; want nil (ambiguous)", d.Matched)
	}
	if d.PendingReason != store.PendingReasonAmbiguousEmail {
		t.Errorf("PendingReason = %q, want %q", d.PendingReason, store.PendingReasonAmbiguousEmail)
	}
	p, _ := db.GetPending(context.Background(), "ua-unknown")
	if p == nil {
		t.Fatal("GetPending = nil; want pending row written")
	}
	if p.Reason != store.PendingReasonAmbiguousEmail {
		t.Errorf("pending.Reason = %q", p.Reason)
	}
	if !strings.Contains(p.Candidates, "rp-a") || !strings.Contains(p.Candidates, "rp-b") {
		t.Errorf("pending.Candidates = %q; want both ids", p.Candidates)
	}
	// No mapping should exist.
	if m, _ := db.GetMapping(context.Background(), "ua-unknown"); m != nil {
		t.Errorf("GetMapping = %+v; want nil (pending user must not hold mapping row)", m)
	}
}

// TestMatchOne_NoSignalSkipsUpstream — UA user with no email, no first
// name, no last name. matchOne must persist pending(no_email) WITHOUT
// calling Redpoint. The FakeRedpoint count confirms zero calls.
func TestMatchOne_NoSignalSkipsUpstream(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	s, db := buildTestSyncer(t, fake, 7)

	d, err := s.matchOne(context.Background(), unifi.UniFiUser{ID: "ua-blank"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Matched != nil {
		t.Errorf("Matched = %+v; want nil", d.Matched)
	}
	if d.PendingReason != store.PendingReasonNoEmail {
		t.Errorf("PendingReason = %q", d.PendingReason)
	}
	p, _ := db.GetPending(context.Background(), "ua-blank")
	if p == nil || p.Reason != store.PendingReasonNoEmail {
		t.Errorf("pending = %+v", p)
	}
}

// TestPersistDecision_PreservesGraceUntilAcrossReobservation — the clock
// must NOT restart every sync. If a user has been pending for 3 days and
// the sync re-observes them, grace_until stays anchored to the original
// (first-seen + graceDays) value.
func TestPersistDecision_PreservesGraceUntilAcrossReobservation(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	s, db := buildTestSyncer(t, fake, 7)

	ua := unifi.UniFiUser{ID: "ua-X"} // no signal → pending(no_email)

	// First observation: grace_until set to now + 7d.
	if _, err := s.matchOne(context.Background(), ua); err != nil {
		t.Fatal(err)
	}
	p1, _ := db.GetPending(context.Background(), "ua-X")
	if p1 == nil {
		t.Fatal("first pending row missing")
	}
	grace1 := p1.GraceUntil

	// Simulate the next day's sync by calling matchOne again.
	if _, err := s.matchOne(context.Background(), ua); err != nil {
		t.Fatal(err)
	}
	p2, _ := db.GetPending(context.Background(), "ua-X")
	if p2 == nil {
		t.Fatal("second pending row missing")
	}
	if p2.GraceUntil != grace1 {
		t.Errorf("GraceUntil drifted: %q → %q (should be stable)", grace1, p2.GraceUntil)
	}
	// LastSeen, though, should advance (or at least not regress).
	t1, _ := time.Parse(time.RFC3339, p1.LastSeen)
	t2, _ := time.Parse(time.RFC3339, p2.LastSeen)
	if t2.Before(t1) {
		t.Errorf("LastSeen went backward: %q → %q", p1.LastSeen, p2.LastSeen)
	}
}

// TestPersistDecision_ReMatchClearsStalePending — UA user was pending
// yesterday, today's sync finds a match. Mapping gets written and the
// stale pending row must be gone.
func TestPersistDecision_ReMatchClearsStalePending(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	s, db := buildTestSyncer(t, fake, 7)
	ctx := context.Background()

	// Day 1: UA user has no email, lands in pending.
	ua := unifi.UniFiUser{ID: "ua-Y"}
	if _, err := s.matchOne(ctx, ua); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetPending(ctx, "ua-Y"); p == nil {
		t.Fatal("expected pending row after day 1")
	}

	// Day 2: staff updated UA-Hub to include the user's email, and a
	// Redpoint customer exists with a matching address. Sync finds them.
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-Y", ExternalID: "ext-Y",
		FirstName: "Test", LastName: "User",
		Email: "test@example.com", Active: true, Badge: "ACTIVE",
	})
	ua.FirstName = "Test"
	ua.LastName = "User"
	ua.Email = "test@example.com"
	if _, err := s.matchOne(ctx, ua); err != nil {
		t.Fatal(err)
	}
	m, _ := db.GetMapping(ctx, "ua-Y")
	if m == nil || m.RedpointCustomer != "rp-Y" {
		t.Errorf("mapping = %+v, want bound to rp-Y", m)
	}
	p, _ := db.GetPending(ctx, "ua-Y")
	if p != nil {
		t.Errorf("GetPending = %+v; stale pending row was not cleared", p)
	}
}

// TestPersistDecision_ReboundCustomerEmitsAudit — a UA user was bound
// to rp-A yesterday; today the email points to rp-B. Mapping row is
// overwritten AND a new match_audit row records the before/after.
// (This can happen if a Redpoint customer's externalID changes, or
// staff manually rebinds.)
func TestPersistDecision_ReboundCustomerEmitsAudit(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	s, db := buildTestSyncer(t, fake, 7)
	ctx := context.Background()

	// Day 1: bound to rp-A.
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-A", ExternalID: "ext-A",
		FirstName: "A", LastName: "User",
		Email: "rebind@example.com", Active: true, Badge: "ACTIVE",
	})
	ua := unifi.UniFiUser{ID: "ua-Z", FirstName: "A", LastName: "User", Email: "rebind@example.com"}
	if _, err := s.matchOne(ctx, ua); err != nil {
		t.Fatal(err)
	}
	audits, _ := db.ListMatchAudit(ctx, "ua-Z", 0)
	if len(audits) != 1 {
		t.Fatalf("day-1 audits = %d, want 1", len(audits))
	}

	// Day 2: the rp-A customer is removed from Redpoint, rp-B appears
	// with the same email. Sync rebinds.
	delete(fake.Customers, "ext-A")
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-B", ExternalID: "ext-B",
		FirstName: "A", LastName: "User",
		Email: "rebind@example.com", Active: true, Badge: "ACTIVE",
	})
	if _, err := s.matchOne(ctx, ua); err != nil {
		t.Fatal(err)
	}
	m, _ := db.GetMapping(ctx, "ua-Z")
	if m == nil || m.RedpointCustomer != "rp-B" {
		t.Errorf("mapping = %+v, want rebound to rp-B", m)
	}
	audits, _ = db.ListMatchAudit(ctx, "ua-Z", 0)
	if len(audits) != 2 {
		t.Fatalf("day-2 audits = %d, want 2 (rebind emits a new audit row)", len(audits))
	}
	// Newest-first — audits[0] is the rebind event.
	if audits[0].BeforeVal != "rp-A" || audits[0].AfterVal != "rp-B" {
		t.Errorf("rebind audit = %+v", audits[0])
	}
}

// TestPersistDecision_UnchangedMappingNoExtraAudit — same UA user, same
// customer, run sync again. The second pass must NOT write a duplicate
// match_audit row; audit noise was a known risk.
func TestPersistDecision_UnchangedMappingNoExtraAudit(t *testing.T) {
	fake := testutil.NewFakeRedpoint()
	defer fake.Close()
	fake.AddCustomer(testutil.FakeCustomer{
		ID: "rp-S", ExternalID: "ext-S",
		FirstName: "Stable", LastName: "User",
		Email: "stable@example.com", Active: true, Badge: "ACTIVE",
	})
	s, db := buildTestSyncer(t, fake, 7)
	ctx := context.Background()

	ua := unifi.UniFiUser{ID: "ua-S", FirstName: "Stable", LastName: "User", Email: "stable@example.com"}
	for i := 0; i < 3; i++ {
		if _, err := s.matchOne(ctx, ua); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	audits, _ := db.ListMatchAudit(ctx, "ua-S", 0)
	if len(audits) != 1 {
		t.Errorf("audits = %d, want 1 (three runs of the same mapping = one audit row)", len(audits))
	}
}
