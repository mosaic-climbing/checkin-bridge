package api

// Tests for the Sync page's pending-summary chip — the replacement for
// the "Unmatched UniFi Users" panel, whose Open buttons targeted a div
// that only exists on the Needs Match page (silent htmx targetError).

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFragPendingSummary_ZeroRendersEmptyState(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/ui/frag/pending-summary", nil)
	w := httptest.NewRecorder()
	srv.handleFragPendingSummary(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "No users pending") {
		t.Errorf("zero pending should render the empty state; body: %s", body)
	}
}

func TestFragPendingSummary_CountsAndLinksToNeedsMatch(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	for _, id := range []string{"ua-1", "ua-2"} {
		seedPending(t, db, id, "no_match", "", 7*24*time.Hour)
	}

	req := httptest.NewRequest("GET", "/ui/frag/pending-summary", nil)
	w := httptest.NewRecorder()
	srv.handleFragPendingSummary(w, req)

	body := w.Body.String()
	if !strings.Contains(body, ">2</span> users pending") {
		t.Errorf("want a count of 2; body: %s", body)
	}
	if !strings.Contains(body, `href="/ui/needs-match"`) {
		t.Errorf("chip should deep-link to /ui/needs-match; body: %s", body)
	}
}
