package api

// Tests for the /ui/frag/health-summary and /ui/frag/needs-match-badge
// handlers (the Health page + sidebar badge, PR 3/3 of the UI overhaul).

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFragHealthSummary_ShadowDefaultsRenderAllHeld(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	// setupTestServer leaves the capability fields at their zero values
	// — the fully-held shadow posture.

	req := httptest.NewRequest("GET", "/ui/frag/health-summary", nil)
	w := httptest.NewRecorder()
	srv.handleFragHealthSummary(w, req)

	body := w.Body.String()
	if got := strings.Count(body, ">held</span>"); got != 4 {
		t.Errorf("want 4 held rungs, got %d; body:\n%s", got, body)
	}
	if strings.Contains(body, ">live</span>") {
		t.Errorf("zero-value capabilities must not render live rungs; body:\n%s", body)
	}
	// Curated page essentials render even with empty stores.
	for _, want := range []string{"Go-live ladder", "UniFi WebSocket", "Taps Today",
		"Pending Needs Match", "Uptime", "Push Alerting", `href="/metrics"`} {
		if !strings.Contains(body, want) {
			t.Errorf("health summary missing %q", want)
		}
	}
}

func TestFragHealthSummary_ResolvedCapabilitiesRenderLiveRungs(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	// Same-package field write stands in for the construction-time
	// wiring (internal/app/build.go passes the resolved values through
	// ServerDeps; NewServer copies them verbatim).
	srv.checkinRecordingLive = true
	srv.statusWritesMode = "activate-only"
	srv.recheckUnlockLive = false
	srv.alertingConfigured = true
	srv.instanceName = "prod"

	// Seed a pending row + a completed job so the counts and last-run
	// sections render real data.
	seedPending(t, db, "ua-health", "no_match", "", 7*24*time.Hour)
	if err := db.CreateJob(t.Context(), "cache_sync-test", "cache_sync"); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteJob(t.Context(), "cache_sync-test", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/ui/frag/health-summary", nil)
	w := httptest.NewRecorder()
	srv.handleFragHealthSummary(w, req)

	body := w.Body.String()
	if got := strings.Count(body, ">live</span>"); got != 2 {
		t.Errorf("checkin+activate-only should light 2 rungs, got %d; body:\n%s", got, body)
	}
	if got := strings.Count(body, ">held</span>"); got != 2 {
		t.Errorf("full-writes+recheck should stay held (2), got %d", got)
	}
	if !strings.Contains(body, ">configured<") {
		t.Error("alerting should render configured")
	}
	if !strings.Contains(body, ">1</a>") {
		t.Errorf("pending count of 1 should render; body:\n%s", body)
	}
	if !strings.Contains(body, "Cache sync") || !strings.Contains(body, "✓") {
		t.Errorf("completed cache_sync run should render; body:\n%s", body)
	}
}

func TestFragNeedsMatchBadge_CountsPending(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	// Zero: invisible but alive.
	w := httptest.NewRecorder()
	srv.handleFragNeedsMatchBadge(w, httptest.NewRequest("GET", "/ui/frag/needs-match-badge", nil))
	if body := w.Body.String(); strings.Contains(body, "badge-pending") ||
		!strings.Contains(body, `id="needs-match-badge"`) {
		t.Errorf("zero pending should render an empty self-polling span; body: %s", body)
	}

	for _, id := range []string{"ua-1", "ua-2", "ua-3"} {
		seedPending(t, db, id, "no_match", "", 7*24*time.Hour)
	}
	w = httptest.NewRecorder()
	srv.handleFragNeedsMatchBadge(w, httptest.NewRequest("GET", "/ui/frag/needs-match-badge", nil))
	if body := w.Body.String(); !strings.Contains(body, ">3</span>") {
		t.Errorf("want a count pill of 3; body: %s", body)
	}
}
