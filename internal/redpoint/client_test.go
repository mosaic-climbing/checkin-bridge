package redpoint

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
)

func newTestClient(t *testing.T, f *testutil.FakeRedpoint) *Client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewClient(f.GraphQLURL(), "test-api-key", "TEST", logger)
}

// TestCustomersByEmail_SingleMatch is the happy path: one Redpoint
// customer has the requested email, so matching lands directly on that
// row with no name-disambiguation needed.
func TestCustomersByEmail_SingleMatch(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()

	f.AddCustomer(testutil.FakeCustomer{
		ID: "rp-1", ExternalID: "ext-1",
		FirstName: "Alex", LastName: "Smith",
		Email: "alex@example.com",
		Active: true, Badge: "ACTIVE",
	})

	c := newTestClient(t, f)
	got, err := c.CustomersByEmail(context.Background(), "alex@example.com", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d customers, want 1", len(got))
	}
	if got[0].ID != "rp-1" {
		t.Errorf("ID = %q, want rp-1", got[0].ID)
	}
	if got[0].Email != "alex@example.com" {
		t.Errorf("Email = %q", got[0].Email)
	}
}

// TestCustomersByEmail_HouseholdCollision is the core reason C2 fetches
// up to N customers per email instead of first: 1 — a parent's email is
// commonly on a child's Redpoint account too. Both rows must come back
// so the bridge can do the name-disambiguation check locally.
func TestCustomersByEmail_HouseholdCollision(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()

	f.AddCustomer(testutil.FakeCustomer{
		ID: "rp-parent", ExternalID: "ext-parent",
		FirstName: "Jamie", LastName: "Lee",
		Email: "jamie@example.com",
		Active: true, Badge: "ACTIVE",
	})
	f.AddCustomer(testutil.FakeCustomer{
		ID: "rp-child", ExternalID: "ext-child",
		FirstName: "Robin", LastName: "Lee",
		Email: "jamie@example.com", // parent's email on the child's account
		Active: true, Badge: "ACTIVE",
	})
	// An unrelated customer with a different email must NOT surface.
	f.AddCustomer(testutil.FakeCustomer{
		ID: "rp-stranger", ExternalID: "ext-stranger",
		FirstName: "Unrelated", LastName: "Person",
		Email: "stranger@example.com",
		Active: true, Badge: "ACTIVE",
	})

	c := newTestClient(t, f)
	got, err := c.CustomersByEmail(context.Background(), "jamie@example.com", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d customers, want 2 (parent + child)", len(got))
	}
	seen := map[string]bool{}
	for _, cust := range got {
		seen[cust.ID] = true
	}
	if !seen["rp-parent"] || !seen["rp-child"] {
		t.Errorf("expected both rp-parent and rp-child; got %v", seen)
	}
	if seen["rp-stranger"] {
		t.Error("rp-stranger leaked into email match")
	}
}

func TestCustomersByEmail_ZeroMatches(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()

	c := newTestClient(t, f)
	got, err := c.CustomersByEmail(context.Background(), "nobody@example.com", 10)
	if err != nil {
		t.Fatalf("zero-match query should not error; got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

// TestCustomersByEmail_EmptyEmailReturnsNilWithoutRequest verifies the
// caller-side safety net: if the statusync loop ever routes a user with
// an empty email through here, we return (nil, nil) rather than hitting
// the upstream with a filter that'd likely return *every* customer
// (Redpoint's email filter semantics for an empty string are
// unspecified). The guard is cheap and makes the contract obvious.
func TestCustomersByEmail_EmptyEmailReturnsNilWithoutRequest(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()

	c := newTestClient(t, f)
	got, err := c.CustomersByEmail(context.Background(), "", 10)
	if err != nil {
		t.Errorf("got error %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
	got, err = c.CustomersByEmail(context.Background(), "   ", 10)
	if err != nil || got != nil {
		t.Errorf("whitespace-only email: got (%v, %v), want (nil, nil)", got, err)
	}
}

// TestCustomersByEmail_CaseInsensitive — matching email is typically
// case-insensitive at the RFC level for the local-part and definitely so
// for the domain. The upstream may or may not normalise; the client
// lowercases the request so two equivalent addresses always produce the
// same match set.
func TestCustomersByEmail_CaseInsensitive(t *testing.T) {
	f := testutil.NewFakeRedpoint()
	defer f.Close()

	f.AddCustomer(testutil.FakeCustomer{
		ID: "rp-1", ExternalID: "ext-1",
		FirstName: "Alex", LastName: "Smith",
		Email:  "Alex@Example.COM", // stored with mixed case
		Active: true, Badge: "ACTIVE",
	})

	c := newTestClient(t, f)
	got, err := c.CustomersByEmail(context.Background(), "ALEX@example.com", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (case-insensitive match)", len(got))
	}
}
