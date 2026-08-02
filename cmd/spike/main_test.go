package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := `# comment
REDPOINT_API_URL=https://lefclimbing.rphq.com
REDPOINT_API_KEY="quoted-key"
REDPOINT_FACILITY_CODE='Mosaic'

BROKEN LINE WITHOUT EQUALS
REDPOINT_GATE_ID= spaced-value
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	env, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		key, want string
	}{
		{"REDPOINT_API_URL", "https://lefclimbing.rphq.com"},
		{"REDPOINT_API_KEY", "quoted-key"},
		{"REDPOINT_FACILITY_CODE", "Mosaic"},
		{"REDPOINT_GATE_ID", "spaced-value"},
	}
	for _, tt := range tests {
		if got := env[tt.key]; got != tt.want {
			t.Errorf("env[%q] = %q, want %q", tt.key, got, tt.want)
		}
	}
	if _, ok := env["BROKEN LINE WITHOUT EQUALS"]; ok {
		t.Error("line without '=' should be skipped")
	}
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	after, before := window(now, 24*time.Hour, 0)
	if after != "2026-08-01T12:00:00Z" {
		t.Errorf("after = %q", after)
	}
	if before != "2026-08-02T12:00:00Z" {
		t.Errorf("before = %q", before)
	}
}

func TestDigAndEdges(t *testing.T) {
	data := map[string]any{
		"customers": map[string]any{
			"total": float64(2),
			"edges": []any{
				map[string]any{"node": map[string]any{"id": "c1"}},
				map[string]any{"node": map[string]any{"id": "c2"}},
			},
		},
	}
	if got := dig(data, "customers", "total"); got != float64(2) {
		t.Errorf("dig total = %v", got)
	}
	if got := dig(data, "missing", "path"); got != nil {
		t.Errorf("dig on missing path = %v, want nil", got)
	}
	nodes := edges(data, "customers")
	if len(nodes) != 2 || str(dig(nodes[0], "id")) != "c1" {
		t.Errorf("edges = %v", nodes)
	}
}
