// Package internal integration tests using fake UniFi and Redpoint servers.
package internal

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// TestFullCheckInFlow simulates a complete NFC check-in through the bridge:
// NFC tap → cache lookup → door unlock → Redpoint recording
func TestFullCheckInFlow(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Set up fake servers
	fakeUnifi := testutil.NewFakeUniFi()
	defer fakeUnifi.Close()

	fakeRedpoint := testutil.NewFakeRedpoint()
	defer fakeRedpoint.Close()

	// Add a test customer to fake Redpoint
	fakeRedpoint.AddCustomer(testutil.FakeCustomer{
		ID:         "rp-cust-1",
		FirstName:  "Alice",
		LastName:   "Member",
		Email:      "alice@example.com",
		ExternalID: "04AABBCCDD",
		Active:     true,
		Badge:      "ACTIVE",
		BadgeName:  "Monthly",
	})

	// Create UniFi client pointing to fake server
	unifiClient := unifi.NewClient(
		"wss://fake-unifi/api/v1/developer/devices/notifications",
		fakeUnifi.BaseURL(),
		"test-token",
		500, // unlock duration ms
		"",  // no TLS fingerprint
		logger,
	)

	// Create Redpoint client pointing to fake server
	redpointClient := redpoint.NewClient(
		fakeRedpoint.GraphQLURL(),
		"test-api-key",
		"TEST",
		logger,
	)

	// Create store
	storeDir := t.TempDir()
	db, err := store.Open(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed store with the test member
	err = db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      "04AABBCCDD",
		CustomerID:  "rp-cust-1",
		FirstName:   "Alice",
		LastName:    "Member",
		BadgeStatus: "ACTIVE",
		BadgeName:   "Monthly",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create card mapper (no overrides for this test)
	cardMapper, err := cardmap.New(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Create check-in handler
	handler := checkin.NewHandler(
		unifiClient,
		redpointClient,
		cardMapper,
		db,
		"gate-1",
		logger,
	)

	// Verify initial state
	stats := handler.GetStats()
	if stats.TotalEvents != 0 {
		t.Errorf("expected no initial events, got %d", stats.TotalEvents)
	}

	// Simulate an NFC tap with allowed membership
	ctx := context.Background()
	accessEvent := unifi.AccessEvent{
		EventType:    "access.logs.add",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		DoorName:     "Front Door",
		DoorID:       "door-1",
		ActorName:    "Alice Member",
		ActorID:      "rp-cust-1",
		CredentialID: "04AABBCCDD",
		AuthType:     "NFC",
		Result:       "ACCESS",
	}

	// Call HandleEvent directly (normally called via WebSocket)
	handler.HandleEvent(ctx, accessEvent)

	// Give async operations time to complete
	time.Sleep(100 * time.Millisecond)

	// Verify check-in succeeded
	stats = handler.GetStats()
	if stats.TotalEvents != 1 {
		t.Errorf("expected 1 event, got %d", stats.TotalEvents)
	}
	if stats.SuccessfulCheckins != 1 {
		t.Errorf("expected 1 successful check-in, got %d", stats.SuccessfulCheckins)
	}
	if stats.DeniedCheckins != 0 {
		t.Errorf("expected 0 denied check-ins, got %d", stats.DeniedCheckins)
	}
	if stats.LastCheckin == nil {
		t.Error("expected last check-in to be recorded")
	} else {
		if stats.LastCheckin.Member != "Alice Member" {
			t.Errorf("expected member 'Alice Member', got %q", stats.LastCheckin.Member)
		}
		if stats.LastCheckin.Door != "Front Door" {
			t.Errorf("expected door 'Front Door', got %q", stats.LastCheckin.Door)
		}
	}

	// Verify Redpoint was updated asynchronously
	if fakeRedpoint.CheckInCount() != 1 {
		t.Errorf("expected 1 check-in in Redpoint, got %d", fakeRedpoint.CheckInCount())
	}
}

// TestDeniedCheckIn simulates a denied access (frozen membership)
func TestDeniedCheckIn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeUnifi := testutil.NewFakeUniFi()
	defer fakeUnifi.Close()

	fakeRedpoint := testutil.NewFakeRedpoint()
	defer fakeRedpoint.Close()

	// Add a frozen customer
	fakeRedpoint.AddCustomer(testutil.FakeCustomer{
		ID:         "rp-cust-2",
		FirstName:  "Bob",
		LastName:   "Frozen",
		ExternalID: "04DDEEFF00",
		Active:     true,
		Badge:      "FROZEN",
	})

	unifiClient := unifi.NewClient(
		"wss://fake-unifi/api/v1/developer/devices/notifications",
		fakeUnifi.BaseURL(),
		"test-token",
		500,
		"",
		logger,
	)

	redpointClient := redpoint.NewClient(
		fakeRedpoint.GraphQLURL(),
		"test-api-key",
		"TEST",
		logger,
	)

	storeDir := t.TempDir()
	db, err := store.Open(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed store with frozen member
	err = db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      "04DDEEFF00",
		CustomerID:  "rp-cust-2",
		FirstName:   "Bob",
		LastName:    "Frozen",
		BadgeStatus: "FROZEN",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}

	cardMapper, err := cardmap.New(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	handler := checkin.NewHandler(
		unifiClient,
		redpointClient,
		cardMapper,
		db,
		"gate-1",
		logger,
	)

	// Simulate NFC tap with frozen membership
	ctx := context.Background()
	accessEvent := unifi.AccessEvent{
		EventType:    "access.logs.add",
		DoorName:     "Bouldering",
		DoorID:       "door-2",
		ActorName:    "Bob Frozen",
		ActorID:      "rp-cust-2",
		CredentialID: "04DDEEFF00",
		AuthType:     "NFC",
		Result:       "BLOCKED",
	}

	handler.HandleEvent(ctx, accessEvent)
	time.Sleep(50 * time.Millisecond)

	stats := handler.GetStats()
	if stats.TotalEvents != 1 {
		t.Errorf("expected 1 event, got %d", stats.TotalEvents)
	}
	if stats.DeniedCheckins != 1 {
		t.Errorf("expected 1 denied check-in, got %d", stats.DeniedCheckins)
	}
	if stats.SuccessfulCheckins != 0 {
		t.Errorf("expected 0 successful check-ins, got %d", stats.SuccessfulCheckins)
	}

	// Verify Redpoint was NOT updated (denied access should not record check-in)
	if fakeRedpoint.CheckInCount() != 0 {
		t.Errorf("expected 0 check-ins in Redpoint for denied access, got %d", fakeRedpoint.CheckInCount())
	}
}

// TestCardOverride tests manual NFC card mapping overrides
func TestCardOverride(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeUnifi := testutil.NewFakeUniFi()
	defer fakeUnifi.Close()

	fakeRedpoint := testutil.NewFakeRedpoint()
	defer fakeRedpoint.Close()

	fakeRedpoint.AddCustomer(testutil.FakeCustomer{
		ID:         "rp-cust-3",
		FirstName:  "Charlie",
		LastName:   "Override",
		ExternalID: "04AABBCCDD", // original card
		Active:     true,
		Badge:      "ACTIVE",
	})

	unifiClient := unifi.NewClient(
		"wss://fake-unifi/api/v1/developer/devices/notifications",
		fakeUnifi.BaseURL(),
		"test-token",
		500,
		"",
		logger,
	)

	redpointClient := redpoint.NewClient(
		fakeRedpoint.GraphQLURL(),
		"test-api-key",
		"TEST",
		logger,
	)

	storeDir := t.TempDir()
	db, err := store.Open(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      "04AABBCCDD",
		CustomerID:  "rp-cust-3",
		FirstName:   "Charlie",
		LastName:    "Override",
		BadgeStatus: "ACTIVE",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}

	cardMapper, err := cardmap.New(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Set override: new card (04FFEEDDCC) maps to customer ID (rp-cust-3)
	cardMapper.SetOverride("04FFEEDDCC", "rp-cust-3")

	handler := checkin.NewHandler(
		unifiClient,
		redpointClient,
		cardMapper,
		db,
		"gate-1",
		logger,
	)

	// Tap with the override card
	ctx := context.Background()
	accessEvent := unifi.AccessEvent{
		EventType:    "access.logs.add",
		DoorName:     "Front Door",
		DoorID:       "door-1",
		CredentialID: "04FFEEDDCC", // new card
		AuthType:     "NFC",
		Result:       "ACCESS",
	}

	handler.HandleEvent(ctx, accessEvent)
	time.Sleep(50 * time.Millisecond)

	stats := handler.GetStats()
	if stats.SuccessfulCheckins != 1 {
		t.Errorf("expected 1 successful check-in with override, got %d", stats.SuccessfulCheckins)
	}
}

// TestNonNFCEvent tests that non-NFC events are ignored
func TestNonNFCEvent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeUnifi := testutil.NewFakeUniFi()
	defer fakeUnifi.Close()

	fakeRedpoint := testutil.NewFakeRedpoint()
	defer fakeRedpoint.Close()

	unifiClient := unifi.NewClient(
		"wss://fake-unifi/api/v1/developer/devices/notifications",
		fakeUnifi.BaseURL(),
		"test-token",
		500,
		"",
		logger,
	)

	redpointClient := redpoint.NewClient(
		fakeRedpoint.GraphQLURL(),
		"test-api-key",
		"TEST",
		logger,
	)

	storeDir := t.TempDir()
	db, err := store.Open(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cardMapper, err := cardmap.New(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	handler := checkin.NewHandler(
		unifiClient,
		redpointClient,
		cardMapper,
		db,
		"gate-1",
		logger,
	)

	// Send PIN code event (not NFC)
	ctx := context.Background()
	accessEvent := unifi.AccessEvent{
		EventType:    "access.logs.add",
		DoorName:     "Front Door",
		DoorID:       "door-1",
		AuthType:     "PIN_CODE",
		CredentialID: "1234",
		Result:       "ACCESS",
	}

	handler.HandleEvent(ctx, accessEvent)
	time.Sleep(50 * time.Millisecond)

	stats := handler.GetStats()
	if stats.TotalEvents != 1 {
		t.Errorf("expected 1 event tracked, got %d", stats.TotalEvents)
	}
	if stats.SuccessfulCheckins != 0 {
		t.Errorf("expected 0 successful check-ins (PIN code ignored), got %d", stats.SuccessfulCheckins)
	}
}

// TestShadowMode_NoSideEffects verifies that when shadow mode is on:
//   - The handler evaluates the tap and records a successful check-in event
//     locally (so dashboards still show activity).
//   - ZERO unlock calls are sent to UniFi.
//   - ZERO createCheckIn mutations are sent to Redpoint.
// This is the contract the deployment relies on for safe parallel-run.
func TestShadowMode_NoSideEffects(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeUnifi := testutil.NewFakeUniFi()
	defer fakeUnifi.Close()

	fakeRedpoint := testutil.NewFakeRedpoint()
	defer fakeRedpoint.Close()

	fakeRedpoint.AddCustomer(testutil.FakeCustomer{
		ID:         "rp-cust-shadow",
		FirstName:  "Sasha",
		LastName:   "Shadow",
		ExternalID: "DEADBEEF",
		Active:     true,
		Badge:      "ACTIVE",
		BadgeName:  "Monthly",
	})

	unifiClient := unifi.NewClient(
		"wss://fake-unifi/api/v1/developer/devices/notifications",
		fakeUnifi.BaseURL(),
		"test-token",
		500,
		"",
		logger,
	)
	redpointClient := redpoint.NewClient(
		fakeRedpoint.GraphQLURL(),
		"test-api-key",
		"TEST",
		logger,
	)

	storeDir := t.TempDir()
	db, err := store.Open(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      "DEADBEEF",
		CustomerID:  "rp-cust-shadow",
		FirstName:   "Sasha",
		LastName:    "Shadow",
		BadgeStatus: "ACTIVE",
		BadgeName:   "Monthly",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	cardMapper, err := cardmap.New(storeDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	handler := checkin.NewHandler(
		unifiClient,
		redpointClient,
		cardMapper,
		db,
		"gate-1",
		logger,
	)
	handler.SetShadowMode(true)

	ctx := context.Background()
	event := unifi.AccessEvent{
		EventType:    "access.logs.add",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		DoorName:     "Front Door",
		DoorID:       "door-1",
		ActorName:    "Sasha Shadow",
		ActorID:      "rp-cust-shadow",
		CredentialID: "DEADBEEF",
		AuthType:     "NFC",
		Result:       "ACCESS",
	}
	handler.HandleEvent(ctx, event)

	// Let any async work drain. In shadow mode there should be none, but the
	// original non-shadow path would dispatch a Redpoint goroutine here.
	time.Sleep(100 * time.Millisecond)

	stats := handler.GetStats()
	if stats.TotalEvents != 1 {
		t.Errorf("TotalEvents = %d, want 1", stats.TotalEvents)
	}
	if stats.SuccessfulCheckins != 1 {
		t.Errorf("SuccessfulCheckins = %d, want 1 (logical allow is still recorded)", stats.SuccessfulCheckins)
	}

	// The two contracts shadow mode must guarantee:
	if got := fakeUnifi.UnlockCount(); got != 0 {
		t.Errorf("shadow mode sent %d UniFi unlock(s); want 0", got)
	}
	if got := fakeRedpoint.CheckInCount(); got != 0 {
		t.Errorf("shadow mode sent %d Redpoint check-in(s); want 0", got)
	}
}
