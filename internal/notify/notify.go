// Package notify sends push notifications to the operator's phone via
// ntfy (https://ntfy.sh — or a self-hosted instance).
//
// This is the bridge's only outbound alerting channel. The design goals,
// in order:
//
//  1. Nil-safe and disabled-by-default. Every caller can hold a *Notifier
//     and call Send unconditionally; when NTFY_TOPIC is unset New returns
//     nil and a nil receiver no-ops. No wiring site needs an "is alerting
//     configured?" branch.
//  2. Never block a hot path. Send is a single bounded HTTP POST (10s
//     cap). Callers on latency-sensitive paths (the tap-time breaker
//     hook) must additionally dispatch from a goroutine — Send itself
//     doesn't spawn one so that scheduled callers (sync digests) get
//     synchronous error reporting.
//  3. No member PII in payloads. Alerts carry counts and component names,
//     not names or emails — ntfy topics are bearer-capability channels
//     and treated as semi-public. This is a convention enforced at call
//     sites, noted here because it's the reason bodies look terse.
package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Priority maps to ntfy's five priority levels. High and Urgent make the
// phone buzz through Do-Not-Disturb depending on the operator's app
// settings; Default is a normal notification; Min/Low batch silently.
type Priority string

const (
	PriorityMin     Priority = "min"
	PriorityLow     Priority = "low"
	PriorityDefault Priority = "default"
	PriorityHigh    Priority = "high"
	PriorityUrgent  Priority = "urgent"
)

// Event is one notification. Title becomes the notification headline,
// Body the expanded text. Tags render as emoji shortcodes in the ntfy
// apps when they match (e.g. "warning", "white_check_mark").
type Event struct {
	Title    string
	Body     string
	Priority Priority
	Tags     []string
}

// Notifier posts events to a single ntfy topic. Construct with New; the
// zero value is not usable but a nil *Notifier is (it no-ops), which is
// the intended disabled state.
type Notifier struct {
	url    string // server base, e.g. https://ntfy.sh
	topic  string
	token  string // optional access token (self-hosted or reserved topics)
	client *http.Client
	logger *slog.Logger
}

// New returns a Notifier for the given server + topic, or nil when topic
// is empty (alerting disabled). A nil logger falls back to slog.Default.
func New(serverURL, topic, token string, logger *slog.Logger) *Notifier {
	if topic == "" {
		return nil
	}
	if serverURL == "" {
		serverURL = "https://ntfy.sh"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{
		url:    strings.TrimRight(serverURL, "/"),
		topic:  topic,
		token:  token,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

// Enabled reports whether notifications are configured. Callers rarely
// need this — Send on a nil Notifier is safe — but it lets boot logging
// say whether the operator will actually hear about problems.
func (n *Notifier) Enabled() bool { return n != nil }

// Send posts one event. Returns nil immediately on a nil (disabled)
// receiver. Errors are returned AND logged at Warn — most call sites
// fire-and-forget, and a lost alert should still leave a trace in the
// local log for the postmortem.
func (n *Notifier) Send(ctx context.Context, ev Event) error {
	if n == nil {
		return nil
	}
	err := n.send(ctx, ev)
	if err != nil {
		n.logger.Warn("notification send failed", "title", ev.Title, "error", err)
	}
	return err
}

func (n *Notifier) send(ctx context.Context, ev Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.url+"/"+n.topic, strings.NewReader(ev.Body))
	if err != nil {
		return fmt.Errorf("build ntfy request: %w", err)
	}
	if ev.Title != "" {
		req.Header.Set("X-Title", ev.Title)
	}
	if ev.Priority != "" {
		req.Header.Set("X-Priority", string(ev.Priority))
	}
	if len(ev.Tags) > 0 {
		req.Header.Set("X-Tags", strings.Join(ev.Tags, ","))
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Read a little of the body for the log line; ntfy returns JSON
		// error details ("topic reserved", "unauthorized", …).
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("ntfy responded %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
