package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/cache"
	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// noopServerCallbacks returns no-op stubs for the three callbacks
// NewServer requires. Tests that don't exercise /debug/reset-breakers,
// /admin/mirror/resync, or /ua-hub/sync can pass these without
// constructing real Walker / Service / unifimirror objects.
func noopServerCallbacks() (
	br func() bool,
	mw func(context.Context) error,
	uahub func(context.Context, func(string)) (UAHubRefreshStats, error),
) {
	return func() bool { return false },
		func(context.Context) error { return nil },
		func(context.Context, func(string)) (UAHubRefreshStats, error) { return UAHubRefreshStats{}, nil }
}

func setupTestServer(t *testing.T) (*Server, *store.Store, *cardmap.Mapper) {
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

	unifiClient := unifi.NewClient("wss://fake:12445/ws", "https://fake:12445/api", "fake-token", 5000, "", logger)
	rpClient := redpoint.NewClient("https://fake.rphq.com/api/graphql", "fake-key", "TST", logger)

	syncer := cache.NewSyncer(db, rpClient, cache.SyncConfig{
		SyncInterval: 24 * 60 * 60 * 1e9, // 24h in nanoseconds
		PageSize:     100,
	}, logger)

	handler := checkin.NewHandler(checkin.HandlerDeps{
		UniFi: unifiClient, Redpoint: rpClient, CardMapper: cm,
		Store: db, GateID: "gate-1", Logger: logger,
	})
	statusSyncer := statusync.New(unifiClient, rpClient, db, statusync.Config{
		SyncInterval: 24 * 60 * 60 * 1e9,
	}, false /* shadowMode */, nil /* metrics */, logger)
	ingester := ingest.NewIngester(rpClient, db, logger)
	sessionMgr := NewSessionManager("test-password")

	// Create a supervised group for background tasks
	bgGroup := bg.New(context.Background(), logger)
	t.Cleanup(func() {
		bgGroup.Shutdown(context.Background())
	})

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
		BreakerResetter:      br,
		MirrorWalker:         mw,
		UAHubMirrorRefresher: uahub,
	})
	return srv, db, cm
}

func TestHealthEndpoint(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	ctx := context.Background()
	err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC001",
		CustomerID:  "c1",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	if resp["mode"] != "store-first" {
		t.Errorf("mode = %v, want store-first", resp["mode"])
	}
	if resp["cacheMembers"].(float64) != 1 {
		t.Errorf("cacheMembers = %v, want 1", resp["cacheMembers"])
	}
}

func TestStatsEndpoint(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestCacheStatsEndpoint(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	ctx := context.Background()
	err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC001",
		CustomerID:  "c1",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/cache", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp store.MemberStats
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1", resp.Total)
	}
}

func TestCacheMembersEndpoint(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	ctx := context.Background()
	err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC001",
		CustomerID:  "c1",
		FirstName:   "A",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC002",
		CustomerID:  "c2",
		FirstName:   "B",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/cache/members", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp struct {
		Members []store.Member `json:"members"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Members) != 2 {
		t.Errorf("members count = %d, want 2", len(resp.Members))
	}
}

func TestCardsEndpoints(t *testing.T) {
	srv, _, cm := setupTestServer(t)

	// List (empty)
	req := httptest.NewRequest("GET", "/cards", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /cards status = %d", w.Code)
	}

	// Add a card
	body := `{"cardUid":"TAG001","customerId":"CUST001"}`
	req = httptest.NewRequest("POST", "/cards", strings.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("POST /cards status = %d", w.Code)
	}

	if !cm.HasOverride("TAG001") {
		t.Error("override should be set")
	}

	// List (should have one)
	req = httptest.NewRequest("GET", "/cards", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var resp struct {
		Overrides map[string]string `json:"overrides"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Overrides["TAG001"] != "CUST001" {
		t.Errorf("override TAG001 = %q, want CUST001", resp.Overrides["TAG001"])
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/cards/TAG001", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("DELETE /cards/TAG001 status = %d", w.Code)
	}
	if cm.HasOverride("TAG001") {
		t.Error("override should be deleted")
	}
}

func TestAddCard_Validation(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	// Missing fields
	body := `{"cardUid":"TAG001"}`
	req := httptest.NewRequest("POST", "/cards", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}

	// Invalid JSON
	req = httptest.NewRequest("POST", "/cards", strings.NewReader("not json"))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCustomerLookup_NotFound(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/customer/NONEXISTENT", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Will return data (possibly with errors from Redpoint being unreachable)
	// but should not panic
	if w.Code == 0 {
		t.Error("expected a response")
	}
}

func TestCustomerLookup_CachedMember(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	ctx := context.Background()
	err := db.UpsertMember(ctx, &store.Member{
		CustomerID:  "cust-1",
		NfcUID:      "TAG_KNOWN",
		FirstName:   "Known",
		LastName:    "User",
		BadgeStatus: "ACTIVE",
		Active:      true,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/customer/TAG_KNOWN", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["cachedAllowed"] != true {
		t.Errorf("cachedAllowed = %v, want true", resp["cachedAllowed"])
	}
}

func TestDirectoryStatus(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/directory/status", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["customers"].(float64) != 0 {
		t.Errorf("customers = %v, want 0", resp["customers"])
	}
}
