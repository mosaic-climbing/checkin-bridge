package api

// Regression tests for the shadow-mode contract on staff-action handlers.
//
// Pre-fix, four api handlers wrote to UA-Hub regardless of shadowMode:
//
//   - POST /unlock/{doorId}                              (handleUnlock)
//   - POST /ui/frag/unmatched/{uaUserId}/skip            (handleFragUnmatchedSkip)
//   - POST /ui/frag/member/{nfcUid}/reactivate           (handleFragMemberReactivate)
//   - POST /ui/frag/member/{nfcUid}/reassign/confirm     (handleFragMemberReassignConfirm)
//
// The contract at internal/config/config.go:98 is explicit: shadowMode
// means no door unlocks, no Redpoint writes, no UniFi status writes.
// These tests pin the gates so a future refactor that drops the
// `if s.shadowMode` block fails CI instead of silently shipping a
// safety-valve regression.
//
// Strategy: construct an api.Server with shadowMode=true and a fake
// UniFi recorder, drive each handler, and assert (a) the response
// indicates shadow suppression and (b) the fake recorded zero
// outbound UA-Hub mutations.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/cache"
	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// setupTestServerShadow is a shadow-mode variant of setupTestServer.
// Returns the server, store, and a FakeUniFi the test can assert
// against to verify zero outbound UA-Hub mutations.
func setupTestServerShadow(t *testing.T) (*Server, *store.Store, *testutil.FakeUniFi) {
	t.Helper()
	dir := t.TempDir()
	logger := discardLogger()

	db, err := store.Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cm, err := cardmap.New(dir, logger)
	if err != nil {
		t.Fatal(err)
	}

	fakeUnifi := testutil.NewFakeUniFi()
	t.Cleanup(fakeUnifi.Close)

	unifiClient := unifi.NewClient("wss://unused", fakeUnifi.BaseURL(), "test-token", 500, "", logger)
	rpClient := redpoint.NewClient("https://fake.rphq.com/api/graphql", "fake-key", "TST", logger)

	syncer := cache.NewSyncer(db, rpClient, cache.SyncConfig{
		SyncInterval: 24 * time.Hour,
		PageSize:     100,
	}, logger)

	handler := checkin.NewHandler(checkin.HandlerDeps{
		UniFi: unifiClient, Redpoint: rpClient, CardMapper: cm,
		Store: db, GateID: "gate-1", Logger: logger,
		ShadowMode: true,
	})
	statusSyncer := statusync.New(unifiClient, rpClient, db, statusync.Config{
		SyncInterval: 24 * time.Hour,
	}, true /* shadowMode */, nil, logger)
	ingester := ingest.NewIngester(rpClient, db, logger)
	sessionMgr := NewSessionManager("test-password")

	bgGroup := bg.New(context.Background(), logger)
	t.Cleanup(func() { bgGroup.Shutdown(context.Background()) })

	br, mw, uahub := noopServerCallbacks()
	srv := NewServer(ServerDeps{
		Handler:              handler,
		Unifi:                unifiClient,
		Redpoint:             rpClient,
		CardMapper:           cm,
		Syncer:               syncer,
		StatusSyncer:         statusSyncer,
		Ingester:             ingester,
		Sessions:             sessionMgr,
		GateID:               "gate-1",
		Logger:               logger,
		Store:                db,
		BG:                   bgGroup,
		ShadowMode:           true,
		BreakerResetter:      br,
		MirrorWalker:         mw,
		UAHubMirrorRefresher: uahub,
	})
	return srv, db, fakeUnifi
}

// TestShadow_Unlock_NoUnifiCall verifies the manual unlock control
// endpoint suppresses the UA-Hub call in shadow mode. Door unlocks
// are the most safety-critical of the four side-effect classes —
// this gate was missing pre-fix.
func TestShadow_Unlock_NoUnifiCall(t *testing.T) {
	srv, _, fake := setupTestServerShadow(t)

	req := httptest.NewRequest("POST", "/unlock/door-1", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (shadow returns OK with success:false)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "shadow") {
		t.Errorf("body = %q, want it to mention shadow", w.Body.String())
	}
	if got := fake.UnlockCount(); got != 0 {
		t.Errorf("UnlockCount = %d, want 0 — shadow mode must not hit UA-Hub", got)
	}
}

// TestShadow_UnmatchedSkip_NoUnifiCall_PreservesPending verifies the
// skip handler in shadow mode:
//   - does not call UpdateUserStatus
//   - leaves the pending row in place so flipping to live mode
//     re-finds the staff intent (this was the exact behaviour the
//     user reported the bridge was getting wrong — the row was
//     being deleted, then re-created by the next matcher pass).
func TestShadow_UnmatchedSkip_NoUnifiCall_PreservesPending(t *testing.T) {
	srv, db, fake := setupTestServerShadow(t)
	ctx := context.Background()

	// Seed a pending row so we can assert it survives the call.
	pending := &store.Pending{
		UAUserID:   "ua-user-1",
		Reason:     store.PendingReasonNoMatch,
		LastSeen:   time.Now().UTC().Format(time.RFC3339),
		GraceUntil: time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	if err := db.UpsertPending(ctx, pending); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/ui/frag/unmatched/ua-user-1/skip", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "SHADOW MODE") {
		t.Errorf("body = %q, want it to flag SHADOW MODE", w.Body.String())
	}
	if got := fake.StatusUpdateCount(); got != 0 {
		t.Errorf("StatusUpdateCount = %d, want 0 — shadow mode must not hit UA-Hub", got)
	}

	got, err := db.GetPending(ctx, "ua-user-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("pending row was deleted; shadow mode must leave it in place so live-mode flip re-finds the staff intent")
	}
}

// TestShadow_MemberReactivate_NoUnifiCall pins the gate on the
// reactivate handler. The flow needs a member row + mapping row so
// the lookups succeed before the shadow check is reached.
func TestShadow_MemberReactivate_NoUnifiCall(t *testing.T) {
	srv, db, fake := setupTestServerShadow(t)
	ctx := context.Background()

	if err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC-RA-1",
		CustomerID:  "rp-cust-1",
		FirstName:   "Ada",
		LastName:    "Lovelace",
		BadgeStatus: "ACTIVE",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMapping(ctx, &store.Mapping{
		UAUserID:         "ua-user-ra-1",
		RedpointCustomer: "rp-cust-1",
		MatchedBy:        statusync.MatchSourceEmail,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/ui/frag/member/NFC-RA-1/reactivate", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "SHADOW MODE") {
		t.Errorf("body = %q, want it to flag SHADOW MODE", w.Body.String())
	}
	if got := fake.StatusUpdateCount(); got != 0 {
		t.Errorf("StatusUpdateCount = %d, want 0 — shadow mode must not hit UA-Hub", got)
	}
}

// TestShadow_MemberReassignConfirm_NoUnifiCall pins the gate on the
// reassign-confirm handler. Pre-fix, this was the most invasive
// violation: a credential rebind that survives across syncs.
func TestShadow_MemberReassignConfirm_NoUnifiCall(t *testing.T) {
	srv, db, fake := setupTestServerShadow(t)
	ctx := context.Background()

	if err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC-RX-1",
		CustomerID:  "rp-cust-rx-1",
		FirstName:   "Grace",
		LastName:    "Hopper",
		BadgeStatus: "ACTIVE",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMapping(ctx, &store.Mapping{
		UAUserID:         "ua-user-rx-from",
		RedpointCustomer: "rp-cust-rx-1",
		MatchedBy:        statusync.MatchSourceEmail,
	}); err != nil {
		t.Fatal(err)
	}
	// Both source and target UA-Hub mirror rows must exist
	// (resolveReassignContext + the target-existence check at
	// members_reassign.go:196 both consult the ua_users mirror).
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID:        "ua-user-rx-from",
		FirstName: "Grace",
		LastName:  "Hopper",
		Status:    "ACTIVE",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUAUser(ctx, &store.UAUser{
		ID:        "ua-user-rx-to",
		FirstName: "Margaret",
		LastName:  "Hamilton",
		Status:    "ACTIVE",
	}, nil); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader("targetUaUserId=ua-user-rx-to")
	req := httptest.NewRequest("POST", "/ui/frag/member/NFC-RX-1/reassign/confirm", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "SHADOW MODE") {
		t.Errorf("body = %q, want it to flag SHADOW MODE", w.Body.String())
	}
	if got := fake.NFCCardAssignmentCount(); got != 0 {
		t.Errorf("NFCCardAssignmentCount = %d, want 0 — shadow mode must not hit UA-Hub", got)
	}
}
