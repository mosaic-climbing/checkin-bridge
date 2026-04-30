package ingest

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
)

func TestFirstName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Alice Smith", "Alice"},
		{"Bob", "Bob"},
		{"", ""},
		{"Carol Ann Jones", "Carol"},
	}
	for _, tt := range tests {
		if got := firstName(tt.input); got != tt.want {
			t.Errorf("firstName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLastName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Alice Smith", "Smith"},
		{"Bob", ""},
		{"", ""},
		{"Carol Ann Jones", "Ann Jones"},
	}
	for _, tt := range tests {
		if got := lastName(tt.input); got != tt.want {
			t.Errorf("lastName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestRun_PrefersExistingMappingOverFuzzyMatch pins the v0.5.10 fix for
// the "hundreds of manually-matched users won't backfill" problem. A UA
// user with a ua_user_mappings row but an email/name that doesn't auto-
// match the local Redpoint directory MUST be resolved via the mapping
// (Method == MatchByMapping) and a members row written. Without this
// the next /ingest/unifi run would re-skip them, and they'd stay
// visually "Not enrolled" forever.
func TestRun_PrefersExistingMappingOverFuzzyMatch(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// Real store + fake UniFi + fake Redpoint.
	dir := t.TempDir()
	db, err := store.Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)

	rpClient := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)

	// Seed local directory with the matched customer. Email is
	// deliberately different from the UA-Hub user's email so the
	// auto-match heuristics WOULD miss without the mapping check.
	if err := db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "rp-garibay",
		FirstName:  "Sean",
		LastName:   "Garibay",
		Email:      "sean.garibay@redpoint.example", // canonical
		Active:     true,
		BadgeStatus: "ACTIVE",
		BadgeName:   "Member",
	}); err != nil {
		t.Fatalf("UpsertCustomer: %v", err)
	}

	// Pre-existing manual match — e.g. staff resolved "Sean Garibay"
	// via Needs Match yesterday because their UA-Hub email had a typo.
	if err := db.UpsertMapping(ctx, &store.Mapping{
		UAUserID:         "ua-garibay",
		RedpointCustomer: "rp-garibay",
		MatchedBy:        "staff",
	}); err != nil {
		t.Fatalf("UpsertMapping: %v", err)
	}

	// Seed the UA-Hub mirror as if ua-hub-mirror had just refreshed:
	// email is the typo'd version that won't SearchCustomersByEmail-hit
	// the directory, name fields are blank so SearchCustomersByName
	// won't rescue it, and there's an NFC token so AllUAUsersWithNFC
	// returns this row. Only the mapping path can resolve this user.
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID:     "ua-garibay",
		Email:  "sean.garbiay@example.invalid", // typo + invalid TLD
		Status: "ACTIVE",
	}, []string{"abc123"}); err != nil {
		t.Fatalf("UpsertUAUser: %v", err)
	}

	ing := NewIngester(rpClient, db, logger)
	result, err := ing.Run(ctx, false /* dryRun */)
	if err != nil {
		t.Fatalf("ingest Run: %v", err)
	}

	if result.Matched != 1 {
		t.Fatalf("Matched = %d, want 1; result = %+v", result.Matched, result)
	}
	if result.Unmatched != 0 {
		t.Errorf("Unmatched = %d, want 0 (mapping should rescue this user)", result.Unmatched)
	}
	if len(result.Mappings) != 1 || result.Mappings[0].Method != MatchByMapping {
		t.Errorf("Method = %v, want %v", result.Mappings[0].Method, MatchByMapping)
	}
	if got := result.Mappings[0].RedpointID; got != "rp-garibay" {
		t.Errorf("resolved RedpointID = %q, want rp-garibay", got)
	}

	// Members row landed under the upper-cased token — the directory
	// search will now render "Enrolled (ABC123)" for rp-garibay.
	mem, err := db.GetMemberByNFC(ctx, "ABC123")
	if err != nil {
		t.Fatalf("GetMemberByNFC: %v", err)
	}
	if mem == nil {
		t.Fatal("members row missing — re-ingest didn't backfill the manually-matched user")
	}
	if mem.CustomerID != "rp-garibay" {
		t.Errorf("member.CustomerID = %q, want rp-garibay", mem.CustomerID)
	}
	if !strings.EqualFold(mem.LastName, "Garibay") {
		t.Errorf("member.LastName = %q, want Garibay", mem.LastName)
	}
}

// TestRun_ReadsFromUAMirror_NotLiveUniFi pins the v0.5.10 swap:
// ingest no longer holds a unifi.Client and Step 1 walks the local
// ua_users mirror. This test seeds the mirror, runs ingest, and
// asserts the user got matched and a members row was written —
// proving the read source is the mirror end-to-end. Without this
// test, a future "let's re-add the live client for X" change could
// silently revert the durability fix.
func TestRun_ReadsFromUAMirror_NotLiveUniFi(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	dir := t.TempDir()
	db, err := store.Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)
	rpClient := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)

	if err := db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "rp-cordes",
		FirstName:  "Tommy",
		LastName:   "Caldwell",
		Email:      "tommy@example.com",
		Active:     true,
	}); err != nil {
		t.Fatalf("UpsertCustomer: %v", err)
	}

	// Seed the mirror with the UA-Hub user that ingest should pick up.
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID:        "ua-tommy",
		FirstName: "Tommy",
		LastName:  "Caldwell",
		Email:     "tommy@example.com",
		Status:    "ACTIVE",
	}, []string{"deadbeef"}); err != nil {
		t.Fatalf("UpsertUAUser: %v", err)
	}

	// Also seed a tokenless mirror row to confirm AllUAUsersWithNFC's
	// filter actually kicks — if ingest read AllUAUsers instead, this
	// row would inflate result.UniFiUsers and fail the assertion below.
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID:    "ua-staff-no-card",
		Email: "staff@example.com",
	}, nil); err != nil {
		t.Fatalf("UpsertUAUser (no tokens): %v", err)
	}

	ing := NewIngester(rpClient, db, logger)
	res, err := ing.Run(ctx, false)
	if err != nil {
		t.Fatalf("ingest Run: %v", err)
	}
	if res.UniFiUsers != 1 {
		t.Errorf("UniFiUsers = %d, want 1 (tokenless row should be filtered)", res.UniFiUsers)
	}
	if res.Matched != 1 {
		t.Errorf("Matched = %d, want 1", res.Matched)
	}
	mem, err := db.GetMemberByNFC(ctx, "DEADBEEF")
	if err != nil {
		t.Fatalf("GetMemberByNFC: %v", err)
	}
	if mem == nil {
		t.Fatal("members row missing — ingest didn't write through the mirror-read path")
	}
	if mem.CustomerID != "rp-cordes" {
		t.Errorf("member.CustomerID = %q, want rp-cordes", mem.CustomerID)
	}
}

// TestRun_EmptyMirrorReturnsClearError covers the fresh-deploy case:
// ua-hub-mirror hasn't populated yet, so AllUAUsersWithNFC returns
// zero rows. Ingest should fail loud with a message that points
// operators at the right next step (run /ua-hub/sync) rather than
// silently succeeding with zero applied — the latter would let a
// scheduled ingest tick "succeed" while doing nothing useful.
func TestRun_EmptyMirrorReturnsClearError(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	dir := t.TempDir()
	db, err := store.Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)
	rpClient := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)

	// Customers populated (so the empty-customers guard doesn't trip
	// first), but no ua_users.
	if err := db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "rp-1",
		FirstName:  "X",
		Active:     true,
	}); err != nil {
		t.Fatalf("UpsertCustomer: %v", err)
	}

	ing := NewIngester(rpClient, db, logger)
	_, err = ing.Run(ctx, false)
	if err == nil {
		t.Fatal("Run on empty mirror should error, got nil")
	}
	if !strings.Contains(err.Error(), "UA-Hub mirror is empty") {
		t.Errorf("error message should name the empty mirror; got %q", err.Error())
	}
}

func TestParseUAUserName(t *testing.T) {
	tests := []struct {
		user      store.UAUser
		wantFirst string
		wantLast  string
	}{
		{store.UAUser{FirstName: "Alice", LastName: "Smith"}, "Alice", "Smith"},
		{store.UAUser{Name: "Bob Jones"}, "Bob", "Jones"},
		{store.UAUser{Name: "Carol"}, "Carol", ""},
		{store.UAUser{Name: "Carol Ann Jones"}, "Carol", "Jones"},
		{store.UAUser{}, "", ""},
	}
	for _, tt := range tests {
		first, last := parseUAUserName(tt.user)
		if first != tt.wantFirst || last != tt.wantLast {
			t.Errorf("parseUAUserName(%+v) = (%q, %q), want (%q, %q)",
				tt.user, first, last, tt.wantFirst, tt.wantLast)
		}
	}
}
