//go:build !devhooks

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTestHooksCompiled_False_InDefaultBuild(t *testing.T) {
	if testHooksCompiled {
		t.Error("testHooksCompiled should be false in default build")
	}
}

func TestTestCheckin_Absent_InDefaultBuild(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	// POST /test-checkin should return 404 Not Found in the default build
	body := `{"cardUid":"test-card","doorId":"door-1"}`
	req := httptest.NewRequest("POST", "/test-checkin", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
