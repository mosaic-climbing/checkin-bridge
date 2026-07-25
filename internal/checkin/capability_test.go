package checkin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// Tests for the CheckinRecordingLive capability (rung 1 of the go-live
// ladder) and the ViaPoller freshness semantics in the handler.

// countingCheckInServer returns a fake Redpoint GraphQL endpoint that
// counts createCheckIn calls and replies success.
func countingCheckInServer(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createCheckIn":{"__typename":"CreateCheckInResult","recordId":"ci-1"}}}`))
	}))
}

func seedActiveMember(t *testing.T, db *store.Store, nfc, customerID string) {
	t.Helper()
	if err := db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      nfc,
		CustomerID:  customerID,
		FirstName:   "Cap",
		LastName:    "Ability",
		BadgeStatus: "ACTIVE",
		Active:      true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckinRecording_HeldWhenCapabilityOff(t *testing.T) {
	var calls atomic.Int64
	srv := countingCheckInServer(&calls)
	defer srv.Close()

	h, db, _ := setupHandler(t)
	h.gateID = "gate-test"
	h.checkinRecordingLive = false // capability held (rung 1 not flipped)
	h.redpointClient = redpoint.NewClient(srv.URL, "k", "F", discardLogger())
	seedActiveMember(t, db, "TAG1", "cust-1")

	h.HandleEvent(context.Background(), unifi.AccessEvent{
		CredentialID: "TAG1", AuthType: "NFC",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	sCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.Shutdown(sCtx)

	if got := calls.Load(); got != 0 {
		t.Errorf("Redpoint createCheckIn calls = %d, want 0 when recording is held", got)
	}
	// The local audit trail must still be written — held recording only
	// suppresses the upstream write.
	events, err := db.RecentCheckIns(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("local checkins rows = %d, want 1 (local recording is unconditional)", len(events))
	}
}

func TestCheckinRecording_FreshPollerEventRecordsWhenLive(t *testing.T) {
	var calls atomic.Int64
	srv := countingCheckInServer(&calls)
	defer srv.Close()

	h, db, _ := setupHandler(t)
	h.gateID = "gate-test"
	h.checkinRecordingLive = true
	h.redpointClient = redpoint.NewClient(srv.URL, "k", "F", discardLogger())
	seedActiveMember(t, db, "TAG2", "cust-2")

	// A fresh poller event: ViaPoller=true (UA-Hub already decided) but
	// IsBackfill=false (it just happened). Rung 1's whole point: this
	// must reach Redpoint.
	h.HandleEvent(context.Background(), unifi.AccessEvent{
		CredentialID: "TAG2", AuthType: "NFC", ViaPoller: true,
		Result:    "ACCESS",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	sCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.Shutdown(sCtx)

	if got := calls.Load(); got != 1 {
		t.Errorf("Redpoint createCheckIn calls = %d, want 1 for a fresh poller event in live mode", got)
	}
}

func TestCheckinRecording_BackfillNeverRecordsEvenWhenLive(t *testing.T) {
	var calls atomic.Int64
	srv := countingCheckInServer(&calls)
	defer srv.Close()

	h, db, _ := setupHandler(t)
	h.gateID = "gate-test"
	h.checkinRecordingLive = true
	h.redpointClient = redpoint.NewClient(srv.URL, "k", "F", discardLogger())
	seedActiveMember(t, db, "TAG3", "cust-3")

	h.HandleEvent(context.Background(), unifi.AccessEvent{
		CredentialID: "TAG3", AuthType: "NFC", ViaPoller: true, IsBackfill: true,
		Timestamp: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})
	sCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.Shutdown(sCtx)

	if got := calls.Load(); got != 0 {
		t.Errorf("Redpoint createCheckIn calls = %d, want 0 for a backfill replay", got)
	}
}

func TestUnlock_SuppressedForPollerEvents(t *testing.T) {
	// A fresh poller ACCESS event with a DoorID must NOT trigger the
	// main-path unlock even in fully-live mode: UA-Hub already opened
	// the door before the poller saw the event. unifiClient is nil in
	// this harness, so an attempted unlock would panic — passing is the
	// proof of suppression.
	h, db, _ := setupHandler(t)
	h.checkinRecordingLive = false
	h.shadowMode = false // fully live; unlock suppression must come from ViaPoller
	seedActiveMember(t, db, "TAG4", "cust-4")

	h.HandleEvent(context.Background(), unifi.AccessEvent{
		CredentialID: "TAG4", AuthType: "NFC", ViaPoller: true,
		DoorID: "door-1", DoorName: "Front", Result: "ACCESS",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})

	events, err := db.RecentCheckIns(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("local checkins rows = %d, want 1 (event recorded despite unlock suppression)", len(events))
	}
}
