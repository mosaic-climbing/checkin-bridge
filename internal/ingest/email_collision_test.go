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

// Regression tests for the household email-collision bug: the ingest
// email match used a first-row-wins single lookup, so a UA user whose
// email matched several customers (parent + kid on one address) was
// silently bound to whichever row SQLite returned first — bypassing
// the ambiguity handling the statusync matcher applies to the same
// case. The fix fetches the whole collision set, binds only on a
// unique name match, and otherwise leaves the user unbound with a
// warning for the Needs Match flow.

func collisionHarness(t *testing.T) (*store.Store, *Ingester) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)
	rpClient := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)

	ctx := context.Background()
	// A household: parent and kid share the email address.
	for _, c := range []store.Customer{
		{RedpointID: "rp-parent", FirstName: "Elena", LastName: "Petrov", Email: "petrov@example.com", Active: true, BadgeStatus: "ACTIVE"},
		{RedpointID: "rp-kid", FirstName: "Milo", LastName: "Petrov", Email: "petrov@example.com", Active: true, BadgeStatus: "ACTIVE"},
	} {
		cc := c
		if err := db.UpsertCustomer(ctx, &cc); err != nil {
			t.Fatal(err)
		}
	}
	return db, NewIngester(rpClient, db, logger)
}

func TestRun_EmailCollisionWithUniqueNameMatch_BindsTheRightPerson(t *testing.T) {
	ctx := context.Background()
	db, ing := collisionHarness(t)

	// The UA user carries the shared email but the KID's name — must
	// bind rp-kid, never first-row rp-parent.
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID: "ua-kid", FirstName: "Milo", LastName: "Petrov",
		Email: "petrov@example.com", Status: "ACTIVE",
	}, []string{"KID123"}); err != nil {
		t.Fatal(err)
	}

	result, err := ing.Run(ctx, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Matched != 1 {
		t.Fatalf("Matched = %d, want 1; result=%+v", result.Matched, result)
	}
	if got := result.Mappings[0].RedpointID; got != "rp-kid" {
		t.Errorf("bound to %q, want rp-kid (unique name match within the collision set)", got)
	}
	mem, _ := db.GetMemberByNFC(ctx, "KID123")
	if mem == nil || mem.CustomerID != "rp-kid" {
		t.Errorf("members row = %+v, want bound to rp-kid", mem)
	}
}

func TestRun_EmailCollisionWithoutNameMatch_LeavesUnbound(t *testing.T) {
	ctx := context.Background()
	db, ing := collisionHarness(t)

	// UA user shares the email but matches NEITHER name (e.g. UA-Hub
	// display name "Petrov Kid" typed by staff). Must stay unbound.
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID: "ua-ambiguous", FirstName: "Petrov", LastName: "Kid",
		Email: "petrov@example.com", Status: "ACTIVE",
	}, []string{"AMB123"}); err != nil {
		t.Fatal(err)
	}

	result, err := ing.Run(ctx, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Matched != 0 {
		t.Fatalf("Matched = %d, want 0 (ambiguous email must not bind); mappings=%+v", result.Matched, result.Mappings)
	}
	if len(result.Mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(result.Mappings))
	}
	w := result.Mappings[0].Warning
	if !strings.Contains(w, "share this email") {
		t.Errorf("warning = %q, want email-collision explanation", w)
	}
	if !strings.Contains(w, "rp-parent") || !strings.Contains(w, "rp-kid") {
		t.Errorf("warning = %q, want both candidate IDs listed", w)
	}
	// No members row may exist for the ambiguous user's token.
	mem, _ := db.GetMemberByNFC(ctx, "AMB123")
	if mem != nil {
		t.Errorf("members row written for ambiguous user: %+v — must stay unbound for Needs Match", mem)
	}
}

func TestRun_SingleEmailMatch_StillBinds(t *testing.T) {
	ctx := context.Background()
	db, ing := collisionHarness(t)

	// A third customer with a unique email — the common case must keep
	// working exactly as before.
	if err := db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "rp-solo", FirstName: "Grace", LastName: "Lin",
		Email: "grace@example.com", Active: true, BadgeStatus: "ACTIVE",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID: "ua-solo", FirstName: "Grace", LastName: "Lin",
		Email: "grace@example.com", Status: "ACTIVE",
	}, []string{"SOLO123"}); err != nil {
		t.Fatal(err)
	}

	result, err := ing.Run(ctx, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Matched != 1 || result.Mappings[0].RedpointID != "rp-solo" {
		t.Errorf("single-email match broken: %+v", result.Mappings)
	}
}
