//go:build devhooks

package api

import (
	"context"
	"encoding/json"
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

// setupTestServerWithTestHooks is like setupTestServer but creates the server
// with enableTestHooks=true so the /test-checkin route is registered.
func setupTestServerWithTestHooks(t *testing.T) (*Server, *store.Store, *cardmap.Mapper) {
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

	// Key difference: EnableTestHooks=true
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
		EnableTestHooks:      true,
		BreakerResetter:      br,
		MirrorWalker:         mw,
		UAHubMirrorRefresher: uahub,
	})
	return srv, db, cm
}

func TestTestHooksCompiled_True_InDevhooksBuild(t *testing.T) {
	if !testHooksCompiled {
		t.Error("testHooksCompiled should be true in devhooks build")
	}
}

func TestTestCheckin_Registered_InDevhooksBuild(t *testing.T) {
	srv, _, _ := setupTestServerWithTestHooks(t)

	// /test-checkin lives on the control plane (same threat model as
	// POST /unlock and friends) — drive it through ControlHandler, not
	// the public ServeHTTP.
	body := `{"cardUid":"test-card","doorId":"door-1"}`
	req := httptest.NewRequest("POST", "/test-checkin", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["success"] != true {
		t.Errorf("success = %v, want true", resp["success"])
	}
}

func TestTestCheckin_Validation(t *testing.T) {
	srv, _, _ := setupTestServerWithTestHooks(t)

	// Missing cardUid
	body := `{"doorId":"door-1"}`
	req := httptest.NewRequest("POST", "/test-checkin", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
