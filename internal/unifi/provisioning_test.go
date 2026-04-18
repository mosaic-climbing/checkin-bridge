package unifi

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
)

func newTestClient(t *testing.T, fake *testutil.FakeUniFi) *Client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewClient("wss://unused", fake.BaseURL(), "test-token", 500, "", logger)
}

func TestCreateUser_RoundTrip(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	c := newTestClient(t, fake)

	id, err := c.CreateUser(context.Background(), "Alex", "Smith", "alex@example.com")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id == "" {
		t.Error("CreateUser returned empty id")
	}
	if len(fake.UsersCreated) != 1 {
		t.Fatalf("UsersCreated = %d, want 1", len(fake.UsersCreated))
	}
	created := fake.UsersCreated[0]
	if created.FirstName != "Alex" || created.LastName != "Smith" || created.Email != "alex@example.com" {
		t.Errorf("body sent to UA-Hub = %+v", created)
	}
	if created.ID != id {
		t.Errorf("returned id %q doesn't match fake's recorded id %q", id, created.ID)
	}
}

func TestUpdateUser_PartialPatch(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	c := newTestClient(t, fake)

	// Only email set — UA-Hub must receive only user_email, not an empty
	// first_name that'd overwrite the real one.
	if err := c.UpdateUser(context.Background(), "ua-1", UserPatch{Email: "new@example.com"}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if len(fake.UserPatches) != 1 {
		t.Fatalf("UserPatches = %d, want 1", len(fake.UserPatches))
	}
	p := fake.UserPatches[0]
	if p.UserID != "ua-1" || p.Email != "new@example.com" {
		t.Errorf("patch = %+v", p)
	}
	if p.FirstName != "" || p.LastName != "" || p.Status != "" {
		t.Errorf("partial patch leaked extra fields: %+v", p)
	}

	// Combined email + status in one call — the bridge's matching path
	// mirrors the email while flipping the user back to ACTIVE for a
	// returning renewed member. Should produce one PUT, not two.
	if err := c.UpdateUser(context.Background(), "ua-2", UserPatch{
		Email: "b@example.com", Status: "ACTIVE",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.UserPatches) != 2 {
		t.Fatalf("UserPatches after second call = %d, want 2", len(fake.UserPatches))
	}
	if fake.UserPatches[1].Email != "b@example.com" || fake.UserPatches[1].Status != "ACTIVE" {
		t.Errorf("combined patch = %+v", fake.UserPatches[1])
	}
	// Legacy StatusUpdates slice stays in sync so the shadow-mode test
	// keeps its existing count assertion.
	if len(fake.StatusUpdates) != 1 {
		t.Errorf("StatusUpdates = %d, want 1 (only the second call sets status)", len(fake.StatusUpdates))
	}
}

func TestUpdateUser_NoFieldsIsNoOp(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	c := newTestClient(t, fake)

	// Zero-value patch: no HTTP call should be issued.
	if err := c.UpdateUser(context.Background(), "ua-1", UserPatch{}); err != nil {
		t.Errorf("UpdateUser(empty) = %v, want nil", err)
	}
	if len(fake.UserPatches) != 0 {
		t.Errorf("UserPatches = %d, want 0 (empty patch should not hit UA-Hub)", len(fake.UserPatches))
	}
}

func TestAssignAccessPolicies(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	c := newTestClient(t, fake)

	ids := []string{"pol-members", "pol-after-hours"}
	if err := c.AssignAccessPolicies(context.Background(), "ua-1", ids); err != nil {
		t.Fatalf("AssignAccessPolicies: %v", err)
	}
	if len(fake.AccessPolicyAssignments) != 1 {
		t.Fatalf("AccessPolicyAssignments = %d, want 1", len(fake.AccessPolicyAssignments))
	}
	got := fake.AccessPolicyAssignments[0]
	if got.UserID != "ua-1" || len(got.PolicyIDs) != 2 {
		t.Errorf("assignment = %+v", got)
	}
	if got.PolicyIDs[0] != "pol-members" || got.PolicyIDs[1] != "pol-after-hours" {
		t.Errorf("policy IDs = %v", got.PolicyIDs)
	}
}

func TestAssignNFCCard_ForceAddFlag(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	c := newTestClient(t, fake)

	if err := c.AssignNFCCard(context.Background(), "ua-1", "TOK123", false); err != nil {
		t.Fatal(err)
	}
	if err := c.AssignNFCCard(context.Background(), "ua-1", "TOK456", true); err != nil {
		t.Fatal(err)
	}
	if len(fake.NFCCardAssignments) != 2 {
		t.Fatalf("NFCCardAssignments = %d, want 2", len(fake.NFCCardAssignments))
	}
	if fake.NFCCardAssignments[0].ForceAdd != false {
		t.Error("first assignment should have ForceAdd=false")
	}
	if fake.NFCCardAssignments[1].ForceAdd != true {
		t.Error("second assignment should have ForceAdd=true")
	}
}

func TestStartNFCEnrollment_ReturnsSessionID(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	fake.NextSessionID = "sess-X"
	c := newTestClient(t, fake)

	sid, err := c.StartNFCEnrollment(context.Background(), "reader-1")
	if err != nil {
		t.Fatalf("StartNFCEnrollment: %v", err)
	}
	if sid != "sess-X" {
		t.Errorf("session id = %q, want sess-X", sid)
	}
	if _, ok := fake.Sessions["sess-X"]; !ok {
		t.Error("fake did not record the session")
	}
}

func TestGetNFCEnrollmentStatus_PendingThenCompleted(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	fake.NextSessionID = "sess-poll"
	c := newTestClient(t, fake)

	sid, err := c.StartNFCEnrollment(context.Background(), "reader-1")
	if err != nil {
		t.Fatal(err)
	}

	// Poll before the tap: Token is empty, Status is pending.
	st, err := c.GetNFCEnrollmentStatus(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Token != "" {
		t.Errorf("before tap: token = %q, want empty", st.Token)
	}
	if st.Status != "pending" {
		t.Errorf("before tap: status = %q, want pending", st.Status)
	}

	// Simulate the tap.
	fake.CompleteSession(sid, "TOKEN-TAPPED", "card-42")

	st, err = c.GetNFCEnrollmentStatus(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Token != "TOKEN-TAPPED" {
		t.Errorf("after tap: token = %q", st.Token)
	}
	if st.CardID != "card-42" {
		t.Errorf("after tap: cardID = %q", st.CardID)
	}
	if st.Status != "completed" {
		t.Errorf("after tap: status = %q, want completed", st.Status)
	}
}

func TestDeleteNFCEnrollmentSession(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	fake.NextSessionID = "sess-dead"
	c := newTestClient(t, fake)

	sid, err := c.StartNFCEnrollment(context.Background(), "reader-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNFCEnrollmentSession(context.Background(), sid); err != nil {
		t.Fatalf("DeleteNFCEnrollmentSession: %v", err)
	}
	if _, ok := fake.Sessions[sid]; ok {
		t.Error("session not removed from fake")
	}
	if len(fake.DeletedSessions) != 1 {
		t.Errorf("DeletedSessions = %d, want 1", len(fake.DeletedSessions))
	}
}

func TestFetchNFCCardByToken_Unknown404(t *testing.T) {
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	c := newTestClient(t, fake)

	// No CardOwners preloaded → fake returns 404 → client returns (nil, nil).
	// The caller (provisioning UI) interprets nil as "card not enrolled yet"
	// and proceeds to the enrollment step.
	owner, err := c.FetchNFCCardByToken(context.Background(), "TOK-UNKNOWN")
	if err != nil {
		t.Errorf("404 should surface as (nil, nil); got err %v", err)
	}
	if owner != nil {
		t.Errorf("expected nil owner, got %+v", owner)
	}
}

func TestFetchNFCCardByToken_AlreadyBound(t *testing.T) {
	// The "this card is already bound to someone else" path. The UI uses
	// this to refuse silent reassignment and surface a confirmation prompt.
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	fake.AddCardOwner("TOK-OURS", testutil.CardOwner{
		CardID: "card-1", UserID: "ua-parent", UserName: "Jamie Lee",
	})
	c := newTestClient(t, fake)

	owner, err := c.FetchNFCCardByToken(context.Background(), "TOK-OURS")
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		t.Fatal("owner nil for an enrolled-and-bound token")
	}
	if owner.UserID != "ua-parent" {
		t.Errorf("UserID = %q, want ua-parent", owner.UserID)
	}
	if owner.UserName != "Jamie Lee" {
		t.Errorf("UserName = %q", owner.UserName)
	}
}

func TestFetchNFCCardByToken_EnrolledButUnbound(t *testing.T) {
	// A card that's enrolled (AddCardOwner with empty UserID) but not yet
	// bound to a user — the expected state immediately after §6.3 reports
	// completion and before §3.7 runs.
	fake := testutil.NewFakeUniFi()
	defer fake.Close()
	fake.AddCardOwner("TOK-FRESH", testutil.CardOwner{CardID: "card-new"})
	c := newTestClient(t, fake)

	owner, err := c.FetchNFCCardByToken(context.Background(), "TOK-FRESH")
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		t.Fatal("owner nil for an enrolled-but-unbound token")
	}
	if owner.UserID != "" {
		t.Errorf("UserID = %q, want empty for unbound card", owner.UserID)
	}
}
