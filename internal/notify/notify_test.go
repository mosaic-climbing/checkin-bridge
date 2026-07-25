package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSend_PostsToTopicWithHeaders(t *testing.T) {
	var got struct {
		method, path, title, priority, tags, auth, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.title = r.Header.Get("X-Title")
		got.priority = r.Header.Get("X-Priority")
		got.tags = r.Header.Get("X-Tags")
		got.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, "mosaic-alerts", "tk_secret", discardLogger())
	err := n.Send(context.Background(), Event{
		Title:    "Sync failed",
		Body:     "statusync error count: 3",
		Priority: PriorityHigh,
		Tags:     []string{"warning", "door"},
	})
	if err != nil {
		t.Fatalf("Send returned %v, want nil", err)
	}
	if got.method != "POST" {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/mosaic-alerts" {
		t.Errorf("path = %s, want /mosaic-alerts", got.path)
	}
	if got.title != "Sync failed" {
		t.Errorf("X-Title = %q", got.title)
	}
	if got.priority != "high" {
		t.Errorf("X-Priority = %q, want high", got.priority)
	}
	if got.tags != "warning,door" {
		t.Errorf("X-Tags = %q", got.tags)
	}
	if got.auth != "Bearer tk_secret" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.body != "statusync error count: 3" {
		t.Errorf("body = %q", got.body)
	}
}

func TestSend_OmitsOptionalHeaders(t *testing.T) {
	var hasTitle, hasPriority, hasTags, hasAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasTitle = r.Header["X-Title"]
		_, hasPriority = r.Header["X-Priority"]
		_, hasTags = r.Header["X-Tags"]
		_, hasAuth = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, "t", "", discardLogger())
	if err := n.Send(context.Background(), Event{Body: "hello"}); err != nil {
		t.Fatalf("Send returned %v", err)
	}
	if hasTitle || hasPriority || hasTags || hasAuth {
		t.Errorf("optional headers sent when empty: title=%v priority=%v tags=%v auth=%v",
			hasTitle, hasPriority, hasTags, hasAuth)
	}
}

func TestSend_NonOKStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	n := New(srv.URL, "t", "", discardLogger())
	if err := n.Send(context.Background(), Event{Body: "x"}); err == nil {
		t.Fatal("Send returned nil, want error on 403")
	}
}

func TestSend_UnreachableServerIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // immediately closed — connection refused

	n := New(srv.URL, "t", "", discardLogger())
	if err := n.Send(context.Background(), Event{Body: "x"}); err == nil {
		t.Fatal("Send returned nil, want transport error")
	}
}

func TestNew_EmptyTopicDisables(t *testing.T) {
	n := New("https://ntfy.sh", "", "", discardLogger())
	if n != nil {
		t.Fatal("New with empty topic should return nil (disabled)")
	}
	if n.Enabled() {
		t.Error("nil notifier reports Enabled")
	}
	// The core contract: a nil receiver's Send is a safe no-op.
	if err := n.Send(context.Background(), Event{Title: "x", Body: "y"}); err != nil {
		t.Errorf("nil Send returned %v, want nil", err)
	}
}

func TestNew_DefaultServerURL(t *testing.T) {
	n := New("", "topic", "", discardLogger())
	if n.url != "https://ntfy.sh" {
		t.Errorf("url = %q, want https://ntfy.sh", n.url)
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	n := New("https://ntfy.example.com/", "topic", "", discardLogger())
	if n.url != "https://ntfy.example.com" {
		t.Errorf("url = %q, want trailing slash trimmed", n.url)
	}
}
