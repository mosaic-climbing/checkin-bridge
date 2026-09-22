package ingest

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
)

// The directory walk only fetches ACTIVE customers, so "customer is
// active" says nothing about whether their badge is. Ingest must write
// the directory's badge status, not assume ACTIVE — otherwise every
// matched member is door-allowed from the daily ingest until the next
// cache refresh corrects them.
func TestIngest_WritesDirectoryBadgeStatusNotActiveFlag(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)
	rpClient := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)

	cases := []struct {
		id, badge, wantBadge string
		wantAllowed          bool
	}{
		{"rp-expired", "EXPIRED", "EXPIRED", false},
		{"rp-frozen", "FROZEN", "FROZEN", false},
		{"rp-active", "ACTIVE", "ACTIVE", true},
		{"rp-nobadge", "", "PENDING_SYNC", false},
	}
	for _, tc := range cases {
		// Seed through the directory walk's own upsert — the one path
		// that writes badge columns (plain UpsertCustomer does not).
		if err := db.UpsertCustomerWithBadgeBatch(ctx, []store.Customer{{
			RedpointID: tc.id, FirstName: "T", LastName: tc.id,
			Email: tc.id + "@example.com", Active: true, // active customer, badge varies
			BadgeStatus: tc.badge, BadgeName: "Member",
		}}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMapping(ctx, &store.Mapping{
			UAUserID: "ua-" + tc.id, RedpointCustomer: tc.id, MatchedBy: "staff",
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertUAUser(ctx, &store.UAUser{
			ID: "ua-" + tc.id, Email: tc.id + "@example.com", Status: "ACTIVE",
		}, []string{"tok-" + tc.id}); err != nil {
			t.Fatal(err)
		}
	}

	ing := NewIngester(rpClient, db, logger)
	if _, err := ing.Run(ctx, false); err != nil {
		t.Fatalf("ingest Run: %v", err)
	}

	for _, tc := range cases {
		mem, err := db.GetMemberByNFC(ctx, "TOK-"+tc.id)
		if err != nil || mem == nil {
			t.Fatalf("%s: member lookup: %v %v", tc.id, mem, err)
		}
		if mem.BadgeStatus != tc.wantBadge {
			t.Errorf("%s: BadgeStatus = %q, want %q", tc.id, mem.BadgeStatus, tc.wantBadge)
		}
		if mem.IsAllowed() != tc.wantAllowed {
			t.Errorf("%s: IsAllowed = %v, want %v", tc.id, mem.IsAllowed(), tc.wantAllowed)
		}
	}
}
