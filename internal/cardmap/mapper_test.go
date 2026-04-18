package cardmap

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestMapper(t *testing.T) (*Mapper, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := New(dir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, dir
}

func TestResolveNoOverride(t *testing.T) {
	m, _ := newTestMapper(t)

	// Without override, Resolve returns the input unchanged
	if got := m.Resolve("AABB1122"); got != "AABB1122" {
		t.Errorf("Resolve(AABB1122) = %q, want AABB1122", got)
	}
}

func TestResolveWithOverride(t *testing.T) {
	m, _ := newTestMapper(t)

	m.SetOverride("AABB1122", "customer-123")

	if got := m.Resolve("AABB1122"); got != "customer-123" {
		t.Errorf("Resolve(AABB1122) = %q, want customer-123", got)
	}

	// Other UIDs still pass through
	if got := m.Resolve("CCDD3344"); got != "CCDD3344" {
		t.Errorf("Resolve(CCDD3344) = %q, want CCDD3344", got)
	}
}

func TestHasOverride(t *testing.T) {
	m, _ := newTestMapper(t)

	if m.HasOverride("AABB1122") {
		t.Error("should not have override initially")
	}

	m.SetOverride("AABB1122", "customer-123")

	if !m.HasOverride("AABB1122") {
		t.Error("should have override after SetOverride")
	}
}

func TestDeleteOverride(t *testing.T) {
	m, _ := newTestMapper(t)

	m.SetOverride("AABB1122", "customer-123")
	m.DeleteOverride("AABB1122")

	if m.HasOverride("AABB1122") {
		t.Error("override should be deleted")
	}
	if got := m.Resolve("AABB1122"); got != "AABB1122" {
		t.Errorf("after delete, Resolve = %q, want AABB1122", got)
	}
}

func TestAllOverrides(t *testing.T) {
	m, _ := newTestMapper(t)

	m.SetOverride("A", "1")
	m.SetOverride("B", "2")
	m.SetOverride("C", "3")

	all := m.AllOverrides()
	if len(all) != 3 {
		t.Errorf("AllOverrides() has %d entries, want 3", len(all))
	}
	if all["B"] != "2" {
		t.Errorf("all[B] = %q, want 2", all["B"])
	}

	// Modifying returned map should not affect internal state
	all["D"] = "4"
	if m.HasOverride("D") {
		t.Error("modifying returned map should not affect mapper")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	m1, err := New(dir, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	m1.SetOverride("TAG1", "CUST1")
	m1.SetOverride("TAG2", "CUST2")

	// Verify file was created
	fp := filepath.Join(dir, "card_overrides.json")
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("overrides file not created: %v", err)
	}

	// Load into new mapper
	m2, err := New(dir, discardLogger())
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}

	if got := m2.Resolve("TAG1"); got != "CUST1" {
		t.Errorf("after reload, Resolve(TAG1) = %q, want CUST1", got)
	}
	if !m2.HasOverride("TAG2") {
		t.Error("TAG2 override should persist across reload")
	}
}

func TestDeleteOverrideNonexistent(t *testing.T) {
	m, _ := newTestMapper(t)

	// Should not error when deleting nonexistent override
	if err := m.DeleteOverride("NONEXISTENT"); err != nil {
		t.Errorf("DeleteOverride(NONEXISTENT) returned error: %v", err)
	}
}
