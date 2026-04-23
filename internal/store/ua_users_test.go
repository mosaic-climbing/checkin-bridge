package store

// Coverage for the audit-side ua_users table introduced in v0.5.2.
// The table mirrors the UA-Hub user directory so code paths that used
// to walk unifi.Client.ListUsers live (Needs Match render, recheck
// pre-filter) can answer from SQLite.
//
// The tests assert four properties that the nightly unifimirror sync
// and the handler code rely on:
//
//   1. Upsert roundtrip — every observed column is readable back, and
//      NfcTokens decodes the stored JSON array.
//   2. first_seen is preserved across re-upserts — we want a stable
//      "when did we first see this user" timestamp even as the row is
//      refreshed nightly.
//   3. Token JSON is well-formed — a nil slice stores as "[]" (so the
//      column's NOT NULL constraint is satisfied) and a populated
//      slice roundtrips via json.Marshal / NfcTokens().
//   4. AllUAUsers ordering is last_synced_at DESC, id ASC — matches
//      the GoDoc contract and the handler's expectations.

import (
	"context"
	"testing"
)

// TestUpsertUAUser_Roundtrip writes a fully-populated row and reads it
// back. Exercises the basic INSERT path and every column.
func TestUpsertUAUser_Roundtrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	u := &UAUser{
		ID:        "ua-1",
		FirstName: "Alex",
		LastName:  "Honnold",
		Name:      "Alex Honnold",
		Email:     "alex@example.com",
		Status:    "active",
	}
	if err := s.UpsertUAUser(ctx, u, []string{"NFC-A", "NFC-B"}); err != nil {
		t.Fatalf("UpsertUAUser: %v", err)
	}
	got, err := s.GetUAUser(ctx, "ua-1")
	if err != nil || got == nil {
		t.Fatalf("GetUAUser: %+v err=%v", got, err)
	}
	if got.FirstName != "Alex" || got.LastName != "Honnold" ||
		got.Name != "Alex Honnold" || got.Email != "alex@example.com" ||
		got.Status != "active" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.FirstSeen == "" {
		t.Error("first_seen should be populated by the default")
	}
	if got.LastSyncedAt == "" {
		t.Error("last_synced_at should be populated")
	}
	toks := got.NfcTokens()
	if len(toks) != 2 || toks[0] != "NFC-A" || toks[1] != "NFC-B" {
		t.Errorf("NfcTokens mismatch: %v", toks)
	}
	if got.FullName() != "Alex Honnold" {
		t.Errorf("FullName = %q, want %q", got.FullName(), "Alex Honnold")
	}
}

// TestUpsertUAUser_MissingRow_NilResult confirms GetUAUser returns nil
// on miss rather than a sql.ErrNoRows leak. The rest of the store
// package follows the same convention; the Needs Match handler relies
// on it to distinguish "not mirrored yet" from "lookup errored".
func TestUpsertUAUser_MissingRow_NilResult(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	got, err := s.GetUAUser(ctx, "nope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on miss, got %+v", got)
	}
}

// TestUpsertUAUser_PreservesFirstSeen is the main invariant we want to
// protect: the nightly sync re-upserts every user every 24h; the audit
// trail value of first_seen depends on not being stomped by those
// refreshes. The store does this with COALESCE(NULLIF(?, ''), (SELECT
// first_seen FROM ua_users WHERE id = ?), ?); this test fails loudly
// if that clause is ever simplified back to excluded.first_seen.
func TestUpsertUAUser_PreservesFirstSeen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Stamp explicit, distinct LastSyncedAt values on the two upserts
	// so the assertions don't depend on wall-clock sub-second
	// granularity (RFC3339 is second-precision and the default
	// `now` stamp can alias under fast test runs).
	u := &UAUser{
		ID:           "ua-2",
		FirstName:    "First",
		Email:        "e@example.com",
		LastSyncedAt: "2025-01-01T00:00:00Z",
	}
	if err := s.UpsertUAUser(ctx, u, nil); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	first, err := s.GetUAUser(ctx, "ua-2")
	if err != nil || first == nil {
		t.Fatalf("read after initial upsert: %+v err=%v", first, err)
	}
	originalFirstSeen := first.FirstSeen
	if originalFirstSeen == "" {
		t.Fatal("first_seen was empty after initial upsert")
	}

	// Caller doesn't re-populate FirstSeen; the store should preserve
	// it from the existing row via the COALESCE subquery.
	refresh := &UAUser{
		ID:           "ua-2",
		FirstName:    "Updated",
		Email:        "e@example.com",
		LastSyncedAt: "2026-01-01T00:00:00Z",
	}
	if err := s.UpsertUAUser(ctx, refresh, nil); err != nil {
		t.Fatalf("refresh upsert: %v", err)
	}
	again, err := s.GetUAUser(ctx, "ua-2")
	if err != nil || again == nil {
		t.Fatalf("read after refresh: %+v err=%v", again, err)
	}
	if again.FirstSeen != originalFirstSeen {
		t.Errorf("first_seen was overwritten: original=%q after=%q",
			originalFirstSeen, again.FirstSeen)
	}
	if again.FirstName != "Updated" {
		t.Errorf("first_name should reflect the refreshed payload, got %q", again.FirstName)
	}
	// The handler UI uses last_synced_at as the "last seen" pill —
	// make sure caller-supplied values replace the older timestamp.
	if again.LastSyncedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("last_synced_at should reflect the refreshed value, got %q", again.LastSyncedAt)
	}
}

// TestUpsertUAUser_EmptyTokenSliceStoresAsJSONArray confirms a nil or
// empty []string lands in the column as "[]" rather than an empty
// string. Otherwise the NOT NULL DEFAULT '[]' would silently paper
// over what should be a well-formed JSON array.
func TestUpsertUAUser_EmptyTokenSliceStoresAsJSONArray(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	u := &UAUser{ID: "ua-3"}
	if err := s.UpsertUAUser(ctx, u, nil); err != nil {
		t.Fatalf("upsert nil tokens: %v", err)
	}
	got, err := s.GetUAUser(ctx, "ua-3")
	if err != nil || got == nil {
		t.Fatalf("GetUAUser: %+v err=%v", got, err)
	}
	if got.NfcTokensJSON != "[]" {
		t.Errorf("expected nfc_tokens = %q, got %q", "[]", got.NfcTokensJSON)
	}
	if toks := got.NfcTokens(); len(toks) != 0 {
		t.Errorf("NfcTokens on empty payload should be nil, got %v", toks)
	}
}

// TestAllUAUsers_Ordering proves AllUAUsers is sorted by
// last_synced_at DESC, id ASC. The ingest matcher doesn't care about
// order, but the operator diagnostics page does — stale rows should
// sink to the bottom.
func TestAllUAUsers_Ordering(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Explicitly stamp LastSyncedAt so the ordering is deterministic
	// regardless of how fast the test runs.
	write := func(id, sync string) {
		t.Helper()
		u := &UAUser{ID: id, LastSyncedAt: sync}
		if err := s.UpsertUAUser(ctx, u, nil); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	write("id-old", "2025-01-01T00:00:00Z")
	write("id-new", "2026-01-01T00:00:00Z")
	write("id-mid", "2025-06-01T00:00:00Z")

	all, err := s.AllUAUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 rows, got %d", len(all))
	}
	order := []string{all[0].ID, all[1].ID, all[2].ID}
	want := []string{"id-new", "id-mid", "id-old"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("ordering wrong: got %v, want %v", order, want)
			break
		}
	}

	if n, err := s.UAUserCount(ctx); err != nil || n != 3 {
		t.Errorf("UAUserCount = %d (err=%v), want 3", n, err)
	}
}

// TestSearchUAUsers pins the v0.5.9 #10 contract used by the NFC
// reassign picker: case-insensitive LIKE across first_name, last_name,
// name, and email; whitespace-split tokens AND-ed together; empty query
// returns nil (not a scan of every row).
func TestSearchUAUsers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	users := []*UAUser{
		{ID: "ua-alice", FirstName: "Alice", LastName: "Alpha", Email: "alice@example.com"},
		{ID: "ua-bob", FirstName: "Bob", LastName: "Brava", Email: "bob@example.com"},
		{ID: "ua-alice2", FirstName: "Alice", LastName: "Zebra", Email: "alice2@example.com"},
		{ID: "ua-c", FirstName: "Charlie", LastName: "Chaplin", Email: "c@chaplin.com"},
	}
	for _, u := range users {
		if err := s.UpsertUAUser(ctx, u, nil); err != nil {
			t.Fatalf("upsert %s: %v", u.ID, err)
		}
	}

	t.Run("empty query returns nil", func(t *testing.T) {
		got, err := s.SearchUAUsers(ctx, "", 10)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != nil {
			t.Errorf("empty query should return nil slice, got %+v", got)
		}
	})

	t.Run("case-insensitive substring on name", func(t *testing.T) {
		got, err := s.SearchUAUsers(ctx, "ALICE", 10)
		if err != nil {
			t.Fatalf("SearchUAUsers: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 alices, got %d: %+v", len(got), got)
		}
		// Results ordered by last_name, first_name — Alice Alpha
		// precedes Alice Zebra.
		if got[0].ID != "ua-alice" || got[1].ID != "ua-alice2" {
			t.Errorf("order wrong: got [%s, %s]", got[0].ID, got[1].ID)
		}
	})

	t.Run("email substring match", func(t *testing.T) {
		got, err := s.SearchUAUsers(ctx, "chaplin", 10)
		if err != nil {
			t.Fatalf("SearchUAUsers: %v", err)
		}
		if len(got) != 1 || got[0].ID != "ua-c" {
			t.Fatalf("email search miss: %+v", got)
		}
	})

	t.Run("AND semantics across tokens", func(t *testing.T) {
		// Matches "Alice" AND "Zebra" — only Alice Zebra qualifies even
		// though both words appear individually in the fixture.
		got, err := s.SearchUAUsers(ctx, "alice zebra", 10)
		if err != nil {
			t.Fatalf("SearchUAUsers: %v", err)
		}
		if len(got) != 1 || got[0].ID != "ua-alice2" {
			t.Fatalf("AND-across-tokens wrong: %+v", got)
		}
	})

	t.Run("limit is respected", func(t *testing.T) {
		got, err := s.SearchUAUsers(ctx, "a", 1)
		if err != nil {
			t.Fatalf("SearchUAUsers: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("limit=1 ignored, got %d rows", len(got))
		}
	})
}

// TestUAUser_FullName_Fallback pins the FullName() precedence the
// Needs Match handler relies on: UA-Hub-provided display Name wins,
// otherwise "First Last", with single-field fallbacks for each side.
func TestUAUser_FullName_Fallback(t *testing.T) {
	cases := []struct {
		in   UAUser
		want string
	}{
		{UAUser{Name: "Display Name", FirstName: "a", LastName: "b"}, "Display Name"},
		{UAUser{FirstName: "Lynn", LastName: "Hill"}, "Lynn Hill"},
		{UAUser{FirstName: "Solo"}, "Solo"},
		{UAUser{LastName: "Lastonly"}, "Lastonly"},
		{UAUser{}, ""},
	}
	for _, tc := range cases {
		if got := tc.in.FullName(); got != tc.want {
			t.Errorf("FullName(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
