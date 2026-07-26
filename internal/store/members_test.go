package store

import (
	"context"
	"testing"
)

// TestAllMembersPaged_Sorting pins the UI-overhaul sort contract:
//
//	ORDER BY last_name COLLATE NOCASE, first_name COLLATE NOCASE, nfc_uid
//
// Plain alphabetical — the order front-desk staff can predict and scan.
// The previous "most-recently-bound first" order (mapping.matched_at
// DESC) keyed on a column the table doesn't display, so to staff it
// read as random. Mappings are seeded with matched_at stamps that would
// reorder the list under the OLD contract, proving matched_at no longer
// influences position; the lowercase "baker" row pins case-insensitivity.
func TestAllMembersPaged_Sorting(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Four members with stable NFC UIDs so assertion failures are legible.
	// "baker" is deliberately lowercase: byte order would sort it after
	// the capitalised names, NOCASE keeps it in the B slot.
	for _, m := range []Member{
		{NfcUID: "CC03", CustomerID: "rp-cooper", FirstName: "Cate", LastName: "Cooper",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
		{NfcUID: "AA01", CustomerID: "rp-aardvark", FirstName: "Amy", LastName: "Aardvark",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
		{NfcUID: "BB02", CustomerID: "rp-baker", FirstName: "Ben", LastName: "baker",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
		{NfcUID: "ZZ99", CustomerID: "rp-orphan", FirstName: "Owen", LastName: "Orphan",
			BadgeStatus: "ACTIVE", Active: true, CachedAt: "2026-01-01T00:00:00Z"},
	} {
		if err := s.UpsertMember(ctx, &m); err != nil {
			t.Fatalf("UpsertMember %s: %v", m.NfcUID, err)
		}
	}

	// Mappings whose matched_at stamps would produce Baker, Cooper,
	// Aardvark under the retired recently-bound-first order. Orphan has
	// no mapping row — under the old contract it sank to the bottom;
	// now it slots alphabetically like everyone else.
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

	want := []string{"AA01", "BB02", "CC03", "ZZ99"} // Aardvark, baker, Cooper, Orphan
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

// TestAllMembersPaged_FirstNameTiebreak — identical last names fall back
// to first_name, then nfc_uid, so pagination is stable.
func TestAllMembersPaged_FirstNameTiebreak(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, m := range []Member{
		{NfcUID: "ZZ01", CustomerID: "rp-zeta", FirstName: "Zara", LastName: "Smith",
			BadgeStatus: "ACTIVE", Active: true},
		{NfcUID: "AA01", CustomerID: "rp-alpha", FirstName: "Alex", LastName: "Smith",
			BadgeStatus: "ACTIVE", Active: true},
	} {
		if err := s.UpsertMember(ctx, &m); err != nil {
			t.Fatalf("UpsertMember %s: %v", m.NfcUID, err)
		}
	}

	got, _, err := s.AllMembersPaged(ctx, 10, 0)
	if err != nil {
		t.Fatalf("AllMembersPaged: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	// Same last name → first_name order: Alex before Zara.
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
			NfcUID:      string(rune('A'+i)) + string(rune('A'+i)),
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
