// Package auditlog provides an append-only log of security-relevant operations.
//
// Events are written to a JSONL file (one JSON object per line) for easy
// ingestion into log aggregators. The file is append-only and never truncated
// by the bridge. File permissions are set to 0600 (owner read/write only).
//
// Logged operations include: login/logout, member add/remove, ingest runs,
// status sync runs, cache sync, and manual unlocks.
package auditlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event represents a single audit log entry.
type Event struct {
	Timestamp string         `json:"ts"`
	Action    string         `json:"action"`
	Actor     string         `json:"actor,omitempty"` // IP address or "system"
	Detail    map[string]any `json:"detail,omitempty"`
}

// Logger writes audit events to an append-only JSONL file.
type Logger struct {
	mu   sync.Mutex
	file *os.File
}

// Open creates or opens the audit log file.
func Open(dataDir string) (*Logger, error) {
	path := filepath.Join(dataDir, "audit.jsonl")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create audit dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}

	return &Logger{file: f}, nil
}

// Log writes an audit event. Safe for concurrent use.
func (l *Logger) Log(action, actor string, detail map[string]any) {
	if l == nil || l.file == nil {
		return
	}

	evt := Event{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Action:    action,
		Actor:     actor,
		Detail:    detail,
	}

	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	data = append(data, '\n')

	l.mu.Lock()
	l.file.Write(data)
	l.mu.Unlock()
}

// Close flushes and closes the audit log file.
func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
