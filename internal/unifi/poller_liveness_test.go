package unifi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Phase-1 alerting: the watchdog's poller-staleness check keys on
// LastPollSuccessAt. These tests pin its contract at the pollOnce level:
// a successful fetch stamps it (even with zero events), a failed fetch
// does not.

func pollTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewClient("wss://unused", baseURL, "test-token", 500, "", logger)
	c.OnEvent(func(AccessEvent) {})
	return c
}

func TestLastPollSuccessAt_ZeroBeforeAnyPoll(t *testing.T) {
	c := pollTestClient(t, "https://unused.invalid")
	if got := c.LastPollSuccessAt(); !got.IsZero() {
		t.Errorf("LastPollSuccessAt before any poll = %v, want zero", got)
	}
}

func TestPollOnce_StampsSuccessOnEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/system/logs" {
			http.NotFound(w, r)
			return
		}
		// 4.2.16 envelope with zero hits — a quiet gym, not a failure.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"SUCCESS","data":{"hits":[]},"msg":"ok"}`))
	}))
	defer srv.Close()

	c := pollTestClient(t, srv.URL)
	before := time.Now()
	cursor := time.Now().Add(-time.Hour)
	var maxID int64
	c.pollOnce(context.Background(), &cursor, &maxID)

	got := c.LastPollSuccessAt()
	if got.IsZero() {
		t.Fatal("LastPollSuccessAt still zero after successful empty poll")
	}
	if got.Before(before) {
		t.Errorf("LastPollSuccessAt = %v, want >= %v", got, before)
	}
}

func TestPollOnce_DoesNotStampOnFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := pollTestClient(t, srv.URL)
	cursor := time.Now().Add(-time.Hour)
	var maxID int64
	c.pollOnce(context.Background(), &cursor, &maxID)

	if got := c.LastPollSuccessAt(); !got.IsZero() {
		t.Errorf("LastPollSuccessAt = %v after failed poll, want zero", got)
	}
}
