package store

import (
	"context"
	"testing"
)

// TestAllMembersPaged_Sorting pins the v0.5.9 sort contract:
//
//	ORDER BY map.matched_at IS NULL, map.matched_at DESC, last_name, first_name
//
// Intent documented on AllMembersPaged: after a bulk ingest, newly-bound
// members rise to the top so operator misassignments are the first thing
// staff see — they're one scroll away from the Unbind button instead of
// needing to thumb through the alphabetical back-catalogue. Orphans
// (members with no ua_user_mappings row) sort to the bottom because the
// recovery action is "Remove" and position doesn't matter.
//
// The test seeds four members with controlled matched_at stamps:
//
//	Aardvark — matched_at 2026-04-01 (older bind)
//	Baker    — matched_at 2026-04-20 (newer bind — expect first)
//	Cooper   — matched_at 2026-04-10 (middle bind)
//	Orphan   — no mapping (expect last)
//
// Expected order: Baker, Cooper, Aardvark, Orphan.
// The alphabetical tiebreak is covered by the name choice — even
// though Aardvark's last name sorts first alphabetically, the newer
// matched_at pushes Baker ahead.
func TestAllMembersPaged_Sorting(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Four members with stable NFC UIDs so assertion failures are legible.
	for _, m := range []Member{
		{NfcUID: "AA01", CustomerID: "rp-aardvark", FirstName: "Amy", LastName: "Aardvark",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
		{NfcUID: "BB02", CustomerID: "rp-baker", FirstName: "Ben", LastName: "Baker",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
		{NfcUID: "CC03", CustomerID: "rp-cooper", FirstName: "Cate", LastName: "Cooper",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
		{NfcUID: "ZZ99", CustomerID: "rp-orphan", FirstName: "Owen", LastName: "Orphan",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
	} {
		if err := s.UpsertMember(ctx, &m); err != nil {
			t.Fatalf("UpsertMember %s: %v", m.NfcUID, err)
		}
	}

	// Three mappings with different matched_at stamps. Orphan
	// intentionally has no mapping row.
	mappings := []Mapping{
		{UAUserID: "ua-aardvark", RedpointCustomer: "rp-aardvark",
			MatchedAt: "2026-04-01T12:00:00Z", MatchedBy: "auto:email"},
		{UAUserID: "ua-baker", RedpointCustomer: "rp-baker",
			MatchedAt: "2026-04-20T12:00:00Z", MatchedBy: "auto:email"},
		{UAUserID: "ua-cooper", RedpointCustomer: "rp-cooper",
			MatchedAt: "2026-04-10T12:00:00Z", MatchedBy: "auto:email"},
	}
	for _, m := range mappings {
		if err := s.UpsertMapping(ctx, &m); err != nil {
			t.Fatalf("UpsertMapping %s: %v", m.UAUserID, err)
		}
	}

	got, total, err := s.AllMembersPaged(ctx, 10, 0)
	if err != nil {
		t.Fatalf("AllMembersPaged: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	if len(got) != 4 {
		t.Fatalf("len(got) = %d, want 4", len(got))
	}

	want := []string{"BB02", "CC03", "AA01", "ZZ99"} // Baker, Cooper, Aardvark, Orphan
	for i, nfc := range want {
		if got[i].NfcUID != nfc {
			names := make([]string, len(got))
			for j, m := range got {
				names[j] = m.NfcUID + "(" + m.LastName + ")"
			}
			t.Fatalf("position %d: got %s, want %s; full order: %v",
				i, got[i].NfcUID, nfc, names)
		}
	}
}

// TestAllMembersPaged_AlphabeticalTiebreak — when matched_at stamps are
// identical, the secondary sort is (last_name, first_name). Seeds two
// mapped members with the exact same matched_at to force the tie.
func TestAllMembersPaged_AlphabeticalTiebreak(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	stamp := "2026-04-22T12:00:00Z"
	for _, m := range []Member{
		{NfcUID: "ZZ01", CustomerID: "rp-zeta", FirstName: "Zara", LastName: "Zeta",
			BadgeStatus: "ACTIVE", Active: true},
		{NfcUID: "AA01", CustomerID: "rp-alpha", FirstName: "Alex", LastName: "Alpha",
			BadgeStatus: "ACTIVE", Active: true},
	} {
		if err := s.UpsertMember(ctx, &m); err != nil {
			t.Fatalf("UpsertMember %s: %v", m.NfcUID, err)
		}
	}
	for _, mp := range []Mapping{
		{UAUserID: "ua-zeta", RedpointCustomer: "rp-zeta",
			MatchedAt: stamp, MatchedBy: "auto:email"},
		{UAUserID: "ua-alpha", RedpointCustomer: "rp-alpha",
			MatchedAt: stamp, MatchedBy: "auto:email"},
	} {
		if err := s.UpsertMapping(ctx, &mp); err != nil {
			t.Fatalf("UpsertMapping %s: %v", mp.UAUserID, err)
		}
	}

	got, _, err := s.AllMembersPaged(ctx, 10, 0)
	if err != nil {
		t.Fatalf("AllMembersPaged: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	// Same matched_at → alphabetical by last_name: Alpha before Zeta.
	if got[0].NfcUID != "AA01" || got[1].NfcUID != "ZZ01" {
		t.Errorf("tiebreak order wrong: got [%s, %s], want [AA01, ZZ01]",
			got[0].NfcUID, got[1].NfcUID)
	}
}

// TestAllMembersPaged_PagingRespectsSort — limit/offset shouldn't
// silently reshuffle. Seeds 5 members and pages 2-at-a-time; the
// concatenation must match the full list.
func TestAllMembersPaged_PagingRespectsSort(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i, lastName := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} {
		if err := s.UpsertMember(ctx, &Member{
			NfcUID:      string(rune('A' + i)) + string(rune('A' + i)),
			CustomerID:  "rp-" + lastName,
			FirstName:   "F",
			LastName:    lastName,
			BadgeStatus: "ACTIVE",
			Active:      true,
		}); err != nil {
			t.Fatalf("UpsertMember: %v", err)
		}
	}

	full, total, err := s.AllMembersPaged(ctx, 10, 0)
	if err != nil {
		t.Fatalf("AllMembersPaged full: %v", err)
	}
	if total != 5 || len(full) != 5 {
		t.Fatalf("full: total=%d len=%d, want 5/5", total, len(full))
	}

	page1, _, _ := s.AllMembersPaged(ctx, 2, 0)
	page2, _, _ := s.AllMembersPaged(ctx, 2, 2)
	page3, _, _ := s.AllMembersPaged(ctx, 2, 4)
	concat := append(append(page1, page2...), page3...)
	if len(concat) != 5 {
		t.Fatalf("concat len = %d, want 5", len(concat))
	}
	for i := range full {
		if full[i].NfcUID != concat[i].NfcUID {
			t.Errorf("page concat[%d] = %s, full[%d] = %s — paging re-sorted",
				i, concat[i].NfcUID, i, full[i].NfcUID)
		}
	}
}
