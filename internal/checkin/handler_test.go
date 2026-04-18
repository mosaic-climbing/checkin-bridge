package checkin

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// setupHandler creates a Handler with a real store and cardmap but nil unifi/redpoint
// (those methods won't be called in unit tests since we control the event flow).
func setupHandler(t *testing.T) (*Handler, *store.Store, *cardmap.Mapper) {
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

	h := &Handler{
		unifiClient:    nil, // won't call UnlockDoor in these tests (no doorID)
		redpointClient: nil, // check-in recording is async and we skip it
		cardMapper:     cm,
		store:          db,
		gateID:         "", // empty = skip Redpoint recording
		logger:         logger,
	}

	return h, db, cm
}

func TestHandleEvent_HappyPath(t *testing.T) {
	h, db, _ := setupHandler(t)

	ctx := context.Background()

	// Pre-populate store
	db.UpsertMember(ctx, &store.Member{
		NfcUID:      "AABB1122",
		CustomerID:  "cust-1",
		FirstName:   "Alice",
		LastName:    "Smith",
		BadgeStatus: "ACTIVE",
		Active:      true,
		BadgeName:   "Monthly",
	})

	event := unifi.AccessEvent{
		EventType:    "access.logs.add",
		CredentialID: "AABB1122",
		DoorID:       "", // empty = skip unlock call
		DoorName:     "Front Door",
		AuthType:     "NFC",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	}

	h.HandleEvent(ctx, event)

	stats := h.GetStats()
	if stats.TotalEvents != 1 {
		t.Errorf("TotalEvents = %d, want 1", stats.TotalEvents)
	}
	if stats.SuccessfulCheckins != 1 {
		t.Errorf("SuccessfulCheckins = %d, want 1", stats.SuccessfulCheckins)
	}
	if stats.LastCheckin == nil {
		t.Fatal("LastCheckin is nil")
	}
	if stats.LastCheckin.Member != "Alice Smith" {
		t.Errorf("LastCheckin.Member = %q, want 'Alice Smith'", stats.LastCheckin.Member)
	}
	if stats.LastCheckin.Badge != "Monthly" {
		t.Errorf("LastCheckin.Badge = %q, want 'Monthly'", stats.LastCheckin.Badge)
	}

	// Verify RecordCheckIn was called
	member, err := db.GetMemberByNFC(ctx, "AABB1122")
	if err != nil {
		t.Fatal(err)
	}
	if member.LastCheckIn == "" {
		t.Error("LastCheckIn should be set after successful check-in")
	}
}

func TestHandleEvent_NotInCache(t *testing.T) {
	h, _, _ := setupHandler(t)

	event := unifi.AccessEvent{
		CredentialID: "UNKNOWN_TAG",
		AuthType:     "NFC",
		DoorName:     "Front Door",
	}

	h.HandleEvent(context.Background(), event)

	stats := h.GetStats()
	if stats.DeniedCheckins != 1 {
		t.Errorf("DeniedCheckins = %d, want 1", stats.DeniedCheckins)
	}
	if stats.SuccessfulCheckins != 0 {
		t.Errorf("SuccessfulCheckins = %d, want 0", stats.SuccessfulCheckins)
	}
}

func TestHandleEvent_FrozenMembership(t *testing.T) {
	h, db, _ := setupHandler(t)

	ctx := context.Background()

	db.UpsertMember(ctx, &store.Member{
		NfcUID:      "FROZEN_TAG",
		CustomerID:  "cust-2",
		FirstName:   "Bob",
		LastName:    "Frost",
		BadgeStatus: "FROZEN",
		Active:      true,
	})

	event := unifi.AccessEvent{
		CredentialID: "FROZEN_TAG",
		AuthType:     "NFC",
	}

	h.HandleEvent(ctx, event)

	stats := h.GetStats()
	if stats.DeniedCheckins != 1 {
		t.Errorf("DeniedCheckins = %d, want 1", stats.DeniedCheckins)
	}
}

func TestHandleEvent_ExpiredMembership(t *testing.T) {
	h, db, _ := setupHandler(t)

	ctx := context.Background()

	db.UpsertMember(ctx, &store.Member{
		NfcUID:      "EXPIRED_TAG",
		CustomerID:  "cust-3",
		BadgeStatus: "EXPIRED",
		Active:      true,
	})

	event := unifi.AccessEvent{
		CredentialID: "EXPIRED_TAG",
		AuthType:     "NFC",
	}

	h.HandleEvent(ctx, event)
	if h.GetStats().DeniedCheckins != 1 {
		t.Error("expired membership should be denied")
	}
}

func TestHandleEvent_InactiveAccount(t *testing.T) {
	h, db, _ := setupHandler(t)

	ctx := context.Background()

	db.UpsertMember(ctx, &store.Member{
		NfcUID:      "INACTIVE_TAG",
		CustomerID:  "cust-4",
		BadgeStatus: "ACTIVE",
		Active:      false,
	})

	event := unifi.AccessEvent{
		CredentialID: "INACTIVE_TAG",
		AuthType:     "NFC",
	}

	h.HandleEvent(ctx, event)
	if h.GetStats().DeniedCheckins != 1 {
		t.Error("inactive account should be denied")
	}
}

func TestHandleEvent_NonNFCSkipped(t *testing.T) {
	h, _, _ := setupHandler(t)

	event := unifi.AccessEvent{
		CredentialID: "AABB1122",
		AuthType:     "PIN_CODE",
	}

	h.HandleEvent(context.Background(), event)

	stats := h.GetStats()
	if stats.TotalEvents != 1 {
		t.Errorf("TotalEvents = %d, want 1 (event counted)", stats.TotalEvents)
	}
	if stats.DeniedCheckins != 0 {
		t.Errorf("DeniedCheckins = %d, want 0 (non-NFC not denied, just skipped)", stats.DeniedCheckins)
	}
}

func TestHandleEvent_EmptyCredentialID(t *testing.T) {
	h, _, _ := setupHandler(t)

	event := unifi.AccessEvent{
		CredentialID: "",
		AuthType:     "NFC",
	}

	h.HandleEvent(context.Background(), event)

	stats := h.GetStats()
	if stats.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (missing credential)", stats.Errors)
	}
}

func TestHandleEvent_WithCardOverride(t *testing.T) {
	h, db, cm := setupHandler(t)

	ctx := context.Background()

	// Store member by customer ID
	db.UpsertMember(ctx, &store.Member{
		NfcUID:      "ORIGINAL_TAG",
		CustomerID:  "cust-override",
		FirstName:   "Carol",
		LastName:    "Override",
		BadgeStatus: "ACTIVE",
		Active:      true,
	})

	// Set override: NEW_TAG → cust-override
	cm.SetOverride("NEW_TAG", "cust-override")

	event := unifi.AccessEvent{
		CredentialID: "NEW_TAG",
		AuthType:     "NFC",
	}

	h.HandleEvent(ctx, event)

	stats := h.GetStats()
	if stats.SuccessfulCheckins != 1 {
		t.Errorf("SuccessfulCheckins = %d, want 1 (override should work)", stats.SuccessfulCheckins)
	}
}

func TestHandleEvent_ConcurrentEvents(t *testing.T) {
	h, db, _ := setupHandler(t)

	ctx := context.Background()

	// Pre-populate several members
	for i := 0; i < 10; i++ {
		db.UpsertMember(ctx, &store.Member{
			NfcUID:      fmt.Sprintf("TAG%02d", i),
			CustomerID:  fmt.Sprintf("cust-%d", i),
			FirstName:   "User",
			LastName:    fmt.Sprintf("%d", i),
			BadgeStatus: "ACTIVE",
			Active:      true,
		})
	}

	// Fire 100 events concurrently
	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func(n int) {
			event := unifi.AccessEvent{
				CredentialID: fmt.Sprintf("TAG%02d", n%10),
				AuthType:     "NFC",
			}
			h.HandleEvent(ctx, event)
			done <- struct{}{}
		}(i)
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	stats := h.GetStats()
	if stats.TotalEvents != 100 {
		t.Errorf("TotalEvents = %d, want 100", stats.TotalEvents)
	}
	if stats.SuccessfulCheckins != 100 {
		t.Errorf("SuccessfulCheckins = %d, want 100", stats.SuccessfulCheckins)
	}
}
