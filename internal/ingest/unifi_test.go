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
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
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

	fakeUA := testutil.NewFakeUniFi()
	t.Cleanup(fakeUA.Close)
	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)

	uaClient := unifi.NewClient("wss://unused", fakeUA.BaseURL(), "test-token", 500, "", logger)
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

	// FakeUniFi user: email is the typo'd version that won't
	// SearchCustomersByEmail-hit the directory. Name is also missing
	// so SearchCustomersByName won't rescue it. Only the mapping path
	// can resolve this user.
	fakeUA.Users = []map[string]any{{
		"id":         "ua-garibay",
		"first_name": "",
		"last_name":  "",
		"name":       "",
		"user_email": "sean.garbiay@example.invalid", // typo + invalid TLD
		"status":     "ACTIVE",
		"nfc_token":  "abc123",
	}}

	ing := NewIngester(uaClient, rpClient, db, logger)
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

func TestParseUniFiName(t *testing.T) {
	tests := []struct {
		user        unifi.UniFiUser
		wantFirst   string
		wantLast    string
	}{
		{unifi.UniFiUser{FirstName: "Alice", LastName: "Smith"}, "Alice", "Smith"},
		{unifi.UniFiUser{Name: "Bob Jones"}, "Bob", "Jones"},
		{unifi.UniFiUser{Name: "Carol"}, "Carol", ""},
		{unifi.UniFiUser{Name: "Carol Ann Jones"}, "Carol", "Jones"},
		{unifi.UniFiUser{}, "", ""},
	}
	for _, tt := range tests {
		first, last := parseUniFiName(tt.user)
		if first != tt.wantFirst || last != tt.wantLast {
			t.Errorf("parseUniFiName(%+v) = (%q, %q), want (%q, %q)",
				tt.user, first, last, tt.wantFirst, tt.wantLast)
		}
	}
}
