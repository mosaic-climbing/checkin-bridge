package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// degradedServer answers every customer(id:) lookup except the ids in
// dead, which always 503.
func degradedServer(t *testing.T, dead ...string) *httptest.Server {
	t.Helper()
	deadSet := map[string]bool{}
	for _, d := range dead {
		deadSet[d] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		id, _ := req.Variables["id"].(string)
		if deadSet[id] {
			http.Error(w, "nope", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Everyone who answers is now EXPIRED, so we can tell "updated"
		// from "left untouched" by looking at the badge afterwards.
		fmt.Fprintf(w, `{"data":{"customer":{"id":%q,"active":true,"firstName":"Test","lastName":%q,"badge":{"status":"EXPIRED","customerBadge":{"id":"b","name":"Member"}}}}}`, id, id)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshAllStatuses_SmallFailureIsDegradedSuccess(t *testing.T) {
	logger := discardLogger()
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const n = 40
	for i := 1; i <= n; i++ {
		seedRefreshMember(t, db, fmt.Sprintf("rp-%d", i))
	}
	srv := degradedServer(t, "rp-7") // 1 of 40 = 2.5% < 5% tolerance

	rp := redpoint.NewClient(srv.URL, "k", "TST", logger)
	rp.SetRateLimit(0, 1)
	s := NewSyncer(db, rp, SyncConfig{SyncInterval: time.Hour}, logger)

	if err := s.RefreshAllStatuses(context.Background()); err != nil {
		t.Fatalf("RefreshAllStatuses = %v; a 2.5%% miss must be a degraded success, not a failed job", err)
	}

	// The unreachable member keeps its cached status; the rest updated.
	m, _ := db.GetMemberByNFC(context.Background(), "TOK-RP-7")
	if m == nil || m.BadgeStatus != "ACTIVE" {
		t.Fatalf("unreachable member = %+v, want untouched ACTIVE", m)
	}
	m, _ = db.GetMemberByNFC(context.Background(), "TOK-RP-8")
	if m == nil || m.BadgeStatus != "EXPIRED" {
		t.Fatalf("reachable member = %+v, want refreshed EXPIRED", m)
	}
}

func TestRefreshAllStatuses_FailureAboveToleranceFailsJob(t *testing.T) {
	logger := discardLogger()
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 1; i <= 10; i++ {
		seedRefreshMember(t, db, fmt.Sprintf("rp-%d", i))
	}
	srv := degradedServer(t, "rp-1", "rp-2") // 20% > 5%

	rp := redpoint.NewClient(srv.URL, "k", "TST", logger)
	rp.SetRateLimit(0, 1)
	s := NewSyncer(db, rp, SyncConfig{SyncInterval: time.Hour}, logger)

	err = s.RefreshAllStatuses(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want refresh-incomplete failure above tolerance", err)
	}
	// Even a failed job applies the subset it did reach.
	m, _ := db.GetMemberByNFC(context.Background(), "TOK-RP-3")
	if m == nil || m.BadgeStatus != "EXPIRED" {
		t.Fatalf("reachable member = %+v, want refreshed EXPIRED even when the job fails", m)
	}
}

func TestRefreshAllStatuses_ToleranceIsConfigurable(t *testing.T) {
	logger := discardLogger()
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 1; i <= 10; i++ {
		seedRefreshMember(t, db, fmt.Sprintf("rp-%d", i))
	}
	srv := degradedServer(t, "rp-1", "rp-2") // 20%

	rp := redpoint.NewClient(srv.URL, "k", "TST", logger)
	rp.SetRateLimit(0, 1)
	s := NewSyncer(db, rp, SyncConfig{SyncInterval: time.Hour, MaxFailedFraction: 0.25}, logger)
	if err := s.RefreshAllStatuses(context.Background()); err != nil {
		t.Fatalf("RefreshAllStatuses = %v; 20%% is under the configured 25%%", err)
	}
}
