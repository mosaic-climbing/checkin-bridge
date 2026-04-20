package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMappingUpsertAndLookup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	m := &Mapping{
		UAUserID:         "ua-user-1",
		RedpointCustomer: "rp-cust-1",
		MatchedBy:        "auto:email",
	}
	if err := s.UpsertMapping(ctx, m); err != nil {
		t.Fatalf("UpsertMapping: %v", err)
	}
	if m.MatchedAt == "" {
		t.Error("UpsertMapping should populate MatchedAt when empty")
	}

	got, err := s.GetMapping(ctx, "ua-user-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.RedpointCustomer != "rp-cust-1" {
		t.Fatalf("GetMapping: %+v", got)
	}
	if got.MatchedBy != "auto:email" {
		t.Errorf("MatchedBy = %q", got.MatchedBy)
	}

	// Reverse lookup (drives the "customer already bound" refusal path).
	rev, err := s.GetMappingByCustomerID(ctx, "rp-cust-1")
	if err != nil {
		t.Fatal(err)
	}
	if rev == nil || rev.UAUserID != "ua-user-1" {
		t.Fatalf("GetMappingByCustomerID: %+v", rev)
	}

	// Absent keys must return (nil, nil), never sql.ErrNoRows (store convention).
	nope, err := s.GetMapping(ctx, "does-not-exist")
	if err != nil || nope != nil {
		t.Errorf("GetMapping(missing) = (%v, %v), want (nil, nil)", nope, err)
	}
	nope2, err := s.GetMappingByCustomerID(ctx, "does-not-exist")
	if err != nil || nope2 != nil {
		t.Errorf("GetMappingByCustomerID(missing) = (%v, %v), want (nil, nil)", nope2, err)
	}
}

func TestMappingUniqueCustomerBinding(t *testing.T) {
	// The UNIQUE (redpoint_customer_id) constraint is what enforces
	// "one UA-Hub user per Redpoint customer" — a concurrent match would
	// otherwise leave two UA-Hub users both claiming the same person.
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-A", RedpointCustomer: "rp-X", MatchedBy: "auto:email",
	}); err != nil {
		t.Fatal(err)
	}
	err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-B", RedpointCustomer: "rp-X", MatchedBy: "auto:email",
	})
	if err == nil {
		t.Fatal("expected UNIQUE violation when two UA users claim the same customer")
	}
}

func TestMappingUpsertPreservesEmailSyncedOnReupsert(t *testing.T) {
	// The ON CONFLICT clause preserves last_email_synced_at unless the new
	// row carries a non-empty value. This matters because the match-path
	// writes the mapping but not the email-synced timestamp; only a
	// successful UpdateUser(email=...) call should advance it.
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-1", RedpointCustomer: "rp-1", MatchedBy: "auto:email",
		LastEmailSyncedAt: "2026-04-01T03:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	// A subsequent match-path upsert does NOT include a synced-at value.
	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-1", RedpointCustomer: "rp-1", MatchedBy: "staff:chris",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMapping(ctx, "ua-1")
	if got.LastEmailSyncedAt != "2026-04-01T03:00:00Z" {
		t.Errorf("LastEmailSyncedAt = %q, want preserved", got.LastEmailSyncedAt)
	}
	if got.MatchedBy != "staff:chris" {
		t.Errorf("MatchedBy = %q, want staff:chris", got.MatchedBy)
	}

	// TouchMappingEmailSynced advances the timestamp explicitly.
	newTime := time.Date(2026, 4, 16, 3, 0, 0, 0, time.UTC)
	if err := s.TouchMappingEmailSynced(ctx, "ua-1", newTime); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMapping(ctx, "ua-1")
	if got.LastEmailSyncedAt != "2026-04-16T03:00:00Z" {
		t.Errorf("LastEmailSyncedAt = %q, want advanced", got.LastEmailSyncedAt)
	}
}

func TestMappingDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-1", RedpointCustomer: "rp-1", MatchedBy: "auto:email",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMapping(ctx, "ua-1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMapping(ctx, "ua-1")
	if got != nil {
		t.Errorf("GetMapping after delete = %+v, want nil", got)
	}
	// Deleting again must be a no-op (idempotent).
	if err := s.DeleteMapping(ctx, "ua-1"); err != nil {
		t.Errorf("second DeleteMapping returned %v, want nil", err)
	}
}

func TestGetMemberByUAUserID(t *testing.T) {
	// Validates the v0.5.3 tap-resolution path:
	//   event.ActorID → ua_user_mappings.ua_user_id
	//                 → redpoint_customer_id
	//                 → members.customer_id  → *Member
	// Both tables live in different SQLite files (audit.db vs cache.db via
	// ATTACH), so this doubles as coverage that the cross-DB join is wired
	// correctly at Open().
	s := testStore(t)
	ctx := context.Background()

	// Seed a mapping (audit.db).
	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID:         "ua-user-42",
		RedpointCustomer: "rp-cust-42",
		MatchedBy:        "auto:email",
	}); err != nil {
		t.Fatalf("UpsertMapping: %v", err)
	}

	// Seed the matching member (cache.db). The NFC UID is a 64-char
	// hash-shaped string — deliberately *not* something the tap event
	// could ever carry — so any accidental fallthrough to the nfc_uid
	// lookup branch would leave member == nil.
	if err := s.UpsertMember(ctx, &Member{
		NfcUID:      strings.Repeat("A", 64),
		CustomerID:  "rp-cust-42",
		FirstName:   "Alice",
		LastName:    "Mapping",
		BadgeStatus: "ACTIVE",
		Active:      true,
	}); err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}

	// Happy path: resolves through the join.
	got, err := s.GetMemberByUAUserID(ctx, "ua-user-42")
	if err != nil {
		t.Fatalf("GetMemberByUAUserID: %v", err)
	}
	if got == nil {
		t.Fatal("GetMemberByUAUserID returned nil for mapped user")
	}
	if got.CustomerID != "rp-cust-42" {
		t.Errorf("CustomerID = %q, want rp-cust-42", got.CustomerID)
	}
	if got.FullName() != "Alice Mapping" {
		t.Errorf("FullName = %q, want Alice Mapping", got.FullName())
	}

	// No mapping for this UA user — must be (nil, nil).
	nope, err := s.GetMemberByUAUserID(ctx, "ua-user-unknown")
	if err != nil || nope != nil {
		t.Errorf("missing UA user: (%v, %v), want (nil, nil)", nope, err)
	}

	// Mapping exists but member row hasn't landed yet (sync-lag corner).
	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-user-orphan", RedpointCustomer: "rp-cust-missing", MatchedBy: "auto:email",
	}); err != nil {
		t.Fatal(err)
	}
	orphan, err := s.GetMemberByUAUserID(ctx, "ua-user-orphan")
	if err != nil || orphan != nil {
		t.Errorf("orphan mapping: (%v, %v), want (nil, nil)", orphan, err)
	}

	// Empty input short-circuits — must not scan the table.
	empty, err := s.GetMemberByUAUserID(ctx, "")
	if err != nil || empty != nil {
		t.Errorf("empty uaUserID: (%v, %v), want (nil, nil)", empty, err)
	}
}

func TestPendingLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	grace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	p := &Pending{
		UAUserID:   "ua-pending-1",
		Reason:     PendingReasonNoMatch,
		GraceUntil: grace,
		Candidates: "",
		UAName:     "Alex First",
		UAEmail:    "alex@example.com",
	}
	if err := s.UpsertPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.FirstSeen == "" || p.LastSeen == "" {
		t.Error("UpsertPending should populate FirstSeen/LastSeen on first insert")
	}
	firstSeen := p.FirstSeen

	// Re-upserting must preserve first_seen (the grace window's anchor) but
	// refresh reason/grace_until/candidates/ua_name/ua_email. This is the
	// per-sync update path: every time the syncer walks unmatched users it
	// re-asserts the pending row, without resetting how long they've been
	// waiting — and the cached UA-Hub identity tracks whatever staff has
	// typed into UA-Hub since the last observation (v0.5.2 migration 5).
	p2 := &Pending{
		UAUserID:   "ua-pending-1",
		Reason:     PendingReasonAmbiguousEmail,
		FirstSeen:  "1999-01-01T00:00:00Z", // deliberately wrong — must be ignored by SQL
		GraceUntil: grace,
		Candidates: "rp-X|rp-Y",
		UAName:     "Alex Renamed",
		UAEmail:    "alex.renamed@example.com",
	}
	if err := s.UpsertPending(ctx, p2); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPending(ctx, "ua-pending-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("GetPending returned nil for existing row")
	}
	if got.FirstSeen != firstSeen {
		t.Errorf("FirstSeen drifted: got %q, want %q (preserved from first insert)", got.FirstSeen, firstSeen)
	}
	if got.Reason != PendingReasonAmbiguousEmail {
		t.Errorf("Reason = %q, want %q", got.Reason, PendingReasonAmbiguousEmail)
	}
	if got.Candidates != "rp-X|rp-Y" {
		t.Errorf("Candidates = %q, want rp-X|rp-Y", got.Candidates)
	}
	if got.UAName != "Alex Renamed" || got.UAEmail != "alex.renamed@example.com" {
		t.Errorf("cached UA identity not refreshed on upsert: got name=%q email=%q; want name=%q email=%q",
			got.UAName, got.UAEmail, "Alex Renamed", "alex.renamed@example.com")
	}

	// PendingCount and AllPending.
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID: "ua-pending-2", Reason: PendingReasonNoEmail, GraceUntil: grace,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PendingCount(ctx)
	if err != nil || n != 2 {
		t.Errorf("PendingCount = %d, err=%v; want 2", n, err)
	}
	all, err := s.AllPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("AllPending len = %d, want 2", len(all))
	}

	// Delete should be idempotent.
	if err := s.DeletePending(ctx, "ua-pending-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePending(ctx, "ua-pending-1"); err != nil {
		t.Errorf("second DeletePending returned %v, want nil", err)
	}
}

// TestUpsertPendingSelfHealsIdentityFromUAUsers — if the caller passes
// blank UAName/UAEmail (the v0.5.2/v0.5.3 production failure mode),
// UpsertPending must fall back to the ua_users mirror row so the Needs
// Match page still renders a real display identity. Also checks that a
// non-blank value passed by the caller wins over the mirror (so a
// UA-Hub-side rename propagates on the next sync).
func TestUpsertPendingSelfHealsIdentityFromUAUsers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Seed ua_users with a complete identity — this represents the
	// state after a healthy unifimirror.Refresh pass.
	if err := s.UpsertUAUser(ctx, &UAUser{
		ID:        "ua-user-selfheal",
		FirstName: "Chase",
		LastName:  "", // Chase has no last name in UA-Hub
		Name:      "",
		Email:     "chase@mosaicclimbing.com",
		Status:    "ACTIVE",
	}, nil); err != nil {
		t.Fatalf("seed ua_users: %v", err)
	}

	grace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)

	// Caller passes blanks — the failure mode observed on LEF prod.
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID:   "ua-user-selfheal",
		Reason:     PendingReasonAmbiguousEmail,
		GraceUntil: grace,
		Candidates: "rp-1|rp-2",
		UAName:     "",
		UAEmail:    "",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPending(ctx, "ua-user-selfheal")
	if err != nil || got == nil {
		t.Fatalf("GetPending: (%v, %v)", got, err)
	}
	if got.UAName != "Chase" {
		t.Errorf("self-heal UAName = %q, want %q", got.UAName, "Chase")
	}
	if got.UAEmail != "chase@mosaicclimbing.com" {
		t.Errorf("self-heal UAEmail = %q, want %q", got.UAEmail, "chase@mosaicclimbing.com")
	}

	// Non-blank caller value wins over the mirror — a subsequent
	// statusync pass that saw fresh UA-Hub data must be able to
	// update the cached identity (e.g. after an operator rename).
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID:   "ua-user-selfheal",
		Reason:     PendingReasonAmbiguousEmail,
		GraceUntil: grace,
		Candidates: "rp-1|rp-2",
		UAName:     "Chase Renamed",
		UAEmail:    "chase.renamed@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetPending(ctx, "ua-user-selfheal")
	if err != nil || got == nil {
		t.Fatalf("GetPending after rename: (%v, %v)", got, err)
	}
	if got.UAName != "Chase Renamed" || got.UAEmail != "chase.renamed@example.com" {
		t.Errorf("caller value must override mirror: got name=%q email=%q",
			got.UAName, got.UAEmail)
	}
}

// TestUpsertPendingSelfHealDerivesFullNameFromParts — ua_users rows
// populated by the nightly mirror typically have first_name/last_name
// set but name blank (UA-Hub returns the fields separately). Exercises
// the CASE ladder that assembles "first last" when name alone is empty.
func TestUpsertPendingSelfHealDerivesFullNameFromParts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertUAUser(ctx, &UAUser{
		ID:        "ua-user-ainsley",
		FirstName: "Ainsley Rae",
		LastName:  "Lightcap",
		Name:      "", // UA-Hub leaves this blank when first+last are used
		Email:     "",
	}, nil); err != nil {
		t.Fatalf("seed ua_users: %v", err)
	}
	grace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID:   "ua-user-ainsley",
		Reason:     PendingReasonAmbiguousName,
		GraceUntil: grace,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPending(ctx, "ua-user-ainsley")
	if err != nil || got == nil {
		t.Fatalf("GetPending: (%v, %v)", got, err)
	}
	if got.UAName != "Ainsley Rae Lightcap" {
		t.Errorf("derived UAName = %q, want %q", got.UAName, "Ainsley Rae Lightcap")
	}
	if got.UAEmail != "" {
		t.Errorf("UAEmail should stay blank when mirror row has no email; got %q", got.UAEmail)
	}
}

// TestUpsertPendingSelfHealNoMirrorRow — if the ua_users mirror has no
// row for this UA user (e.g. the matcher observed them before the
// nightly mirror has ever run, or they were deleted upstream), the
// pending row should persist with blank identity fields rather than
// erroring. The next sync pass will either refresh from live UA-Hub
// data or re-run the self-heal after the mirror populates.
func TestUpsertPendingSelfHealNoMirrorRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	grace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID:   "ua-user-no-mirror",
		Reason:     PendingReasonNoEmail,
		GraceUntil: grace,
	}); err != nil {
		t.Fatalf("UpsertPending with no mirror row should not error: %v", err)
	}
	got, err := s.GetPending(ctx, "ua-user-no-mirror")
	if err != nil || got == nil {
		t.Fatalf("GetPending: (%v, %v)", got, err)
	}
	if got.UAName != "" || got.UAEmail != "" {
		t.Errorf("no-mirror pending should have blank identity; got name=%q email=%q",
			got.UAName, got.UAEmail)
	}
}

// TestMigration7BackfillsExistingBlankPendingRows — represents the
// v0.5.2/v0.5.3 production state: pending rows already on disk with
// blank ua_name/ua_email and a fully-populated ua_users mirror. The
// v0.5.4 migration must repair them in place so operators see real
// identities on the Needs Match page without having to wait for the
// next statusync pass.
//
// This test leans on the fact that testStore runs all migrations in
// Open, including migration 7. We simulate the pre-fix state by
// writing the pending row with blank identity (via a direct DB exec
// that bypasses the new self-heal), seeding ua_users, and then
// re-running migration 7 manually — the backfill is idempotent.
func TestMigration7BackfillsExistingBlankPendingRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Seed ua_users with identities the backfill should copy.
	for _, u := range []UAUser{
		{ID: "ua-backfill-1", FirstName: "Abigail", LastName: "Smith"},
		{ID: "ua-backfill-2", Name: "Chase", Email: "chase@example.com"},
		{ID: "ua-backfill-3", FirstName: "Aaron", LastName: "Hart"},
	} {
		u := u
		if err := s.UpsertUAUser(ctx, &u, nil); err != nil {
			t.Fatalf("seed ua_users %s: %v", u.ID, err)
		}
	}

	// Write pending rows with blank identity, bypassing UpsertPending's
	// self-heal (reproducing the production state before v0.5.4). We
	// do this via a direct exec rather than calling UpsertPending to
	// guarantee the rows hit disk blank.
	grace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, id := range []string{"ua-backfill-1", "ua-backfill-2", "ua-backfill-3"} {
		if _, err := s.db.ExecContext(ctx, `
            INSERT INTO ua_user_mappings_pending
                (ua_user_id, reason, grace_until, candidates, ua_name, ua_email)
            VALUES (?, ?, ?, '', '', '')
        `, id, PendingReasonAmbiguousName, grace); err != nil {
			t.Fatalf("seed blank pending %s: %v", id, err)
		}
	}

	// Re-run migration 7 as a one-shot backfill. Idempotent: the WHERE
	// clause only touches rows whose identity is still blank.
	if _, err := s.db.ExecContext(ctx, auditMigration7_pending_identity_backfill); err != nil {
		t.Fatalf("replay migration 7: %v", err)
	}

	got1, _ := s.GetPending(ctx, "ua-backfill-1")
	if got1 == nil || got1.UAName != "Abigail Smith" {
		t.Errorf("first_name+last_name backfill: %+v", got1)
	}
	got2, _ := s.GetPending(ctx, "ua-backfill-2")
	if got2 == nil || got2.UAName != "Chase" || got2.UAEmail != "chase@example.com" {
		t.Errorf("name+email backfill: %+v", got2)
	}
	got3, _ := s.GetPending(ctx, "ua-backfill-3")
	if got3 == nil || got3.UAName != "Aaron Hart" {
		t.Errorf("Aaron backfill: %+v", got3)
	}
}

func TestPendingExpired(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	if err := s.UpsertPending(ctx, &Pending{
		UAUserID: "ua-expired", Reason: PendingReasonNoMatch, GraceUntil: past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID: "ua-still-waiting", Reason: PendingReasonNoMatch, GraceUntil: future,
	}); err != nil {
		t.Fatal(err)
	}

	expired, err := s.ExpiredPending(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].UAUserID != "ua-expired" {
		t.Errorf("ExpiredPending returned %+v, want exactly [ua-expired]", expired)
	}
}

func TestMatchAuditAppendAndList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.AppendMatchAudit(ctx, &MatchAudit{
			UAUserID: "ua-X",
			Field:    "mapping",
			AfterVal: "rp-Y",
			Source:   "auto:email",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A different user's row should not leak into the list.
	if err := s.AppendMatchAudit(ctx, &MatchAudit{
		UAUserID: "ua-Z",
		Field:    "mapping",
		Source:   "staff:chris",
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListMatchAudit(ctx, "ua-X", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("ListMatchAudit len = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.UAUserID != "ua-X" {
			t.Errorf("leaked row for %q", r.UAUserID)
		}
		if !strings.HasPrefix(r.Source, "auto:") {
			t.Errorf("Source = %q, want auto:*", r.Source)
		}
	}
	// Newest-first ordering: ids descend.
	if rows[0].ID <= rows[1].ID || rows[1].ID <= rows[2].ID {
		t.Errorf("ListMatchAudit not newest-first: ids %d, %d, %d", rows[0].ID, rows[1].ID, rows[2].ID)
	}

	// Limit respected.
	rows, _ = s.ListMatchAudit(ctx, "ua-X", 2)
	if len(rows) != 2 {
		t.Errorf("ListMatchAudit(limit=2) len = %d, want 2", len(rows))
	}
}
