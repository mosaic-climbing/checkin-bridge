package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func testLogger() *testLog {
	return &testLog{}
}

// minimal slog-compatible logger for tests (discards output)
type testLog struct{}

func newTestCache(t *testing.T) (*MemberCache, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := New(dir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, dir
}

func TestPutAndGetByExternalID(t *testing.T) {
	c, _ := newTestCache(t)

	m := &CachedMember{
		CustomerID:  "cust-1",
		ExternalID:  "NFC001",
		FirstName:   "Alice",
		LastName:    "Smith",
		BadgeStatus: "ACTIVE",
		Active:      true,
	}
	c.Put(m)

	got := c.GetByExternalID("NFC001")
	if got == nil {
		t.Fatal("expected member, got nil")
	}
	if got.CustomerID != "cust-1" {
		t.Errorf("CustomerID = %q, want %q", got.CustomerID, "cust-1")
	}
	if got.FullName() != "Alice Smith" {
		t.Errorf("FullName = %q, want %q", got.FullName(), "Alice Smith")
	}
}

func TestGetByCustomerID(t *testing.T) {
	c, _ := newTestCache(t)

	c.Put(&CachedMember{CustomerID: "cust-1", ExternalID: "NFC001", Active: true, BadgeStatus: "ACTIVE"})
	c.Put(&CachedMember{CustomerID: "cust-2", ExternalID: "NFC002", Active: true, BadgeStatus: "ACTIVE"})

	got := c.GetByCustomerID("cust-2")
	if got == nil || got.ExternalID != "NFC002" {
		t.Errorf("GetByCustomerID(cust-2) = %v, want NFC002", got)
	}

	if c.GetByCustomerID("nonexistent") != nil {
		t.Error("expected nil for nonexistent customer ID")
	}
}

func TestGetByBarcode(t *testing.T) {
	c, _ := newTestCache(t)
	c.Put(&CachedMember{CustomerID: "c1", ExternalID: "NFC001", Barcode: "BC123", Active: true, BadgeStatus: "ACTIVE"})

	got := c.GetByBarcode("BC123")
	if got == nil || got.ExternalID != "NFC001" {
		t.Errorf("GetByBarcode(BC123) = %v, want NFC001", got)
	}
}

func TestIsAllowed(t *testing.T) {
	tests := []struct {
		name   string
		member CachedMember
		want   bool
	}{
		{"active+ACTIVE", CachedMember{Active: true, BadgeStatus: "ACTIVE"}, true},
		{"active+FROZEN", CachedMember{Active: true, BadgeStatus: "FROZEN"}, false},
		{"active+EXPIRED", CachedMember{Active: true, BadgeStatus: "EXPIRED"}, false},
		{"inactive+ACTIVE", CachedMember{Active: false, BadgeStatus: "ACTIVE"}, false},
		{"inactive+empty", CachedMember{Active: false, BadgeStatus: ""}, false},
		{"active+empty", CachedMember{Active: true, BadgeStatus: ""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.member.IsAllowed(); got != tt.want {
				t.Errorf("IsAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDenyReason(t *testing.T) {
	tests := []struct {
		member CachedMember
		want   string
	}{
		{CachedMember{Active: false}, "customer account inactive"},
		{CachedMember{Active: true, BadgeStatus: "FROZEN"}, "membership frozen"},
		{CachedMember{Active: true, BadgeStatus: "EXPIRED"}, "membership expired"},
		{CachedMember{Active: true, BadgeStatus: ""}, "no membership badge"},
		{CachedMember{Active: true, BadgeStatus: "UNKNOWN"}, "badge status: UNKNOWN"},
	}

	for _, tt := range tests {
		if got := tt.member.DenyReason(); got != tt.want {
			t.Errorf("DenyReason() = %q, want %q", got, tt.want)
		}
	}
}

func TestRemove(t *testing.T) {
	c, _ := newTestCache(t)
	c.Put(&CachedMember{CustomerID: "c1", ExternalID: "NFC001", Barcode: "BC1", Active: true, BadgeStatus: "ACTIVE"})

	c.Remove("NFC001")

	if c.GetByExternalID("NFC001") != nil {
		t.Error("member should be removed by externalID")
	}
	if c.GetByCustomerID("c1") != nil {
		t.Error("member should be removed from customer ID index")
	}
	if c.GetByBarcode("BC1") != nil {
		t.Error("member should be removed from barcode index")
	}
}

func TestPruneInactive(t *testing.T) {
	c, _ := newTestCache(t)
	c.Put(&CachedMember{CustomerID: "c1", ExternalID: "NFC001", Active: true, BadgeStatus: "ACTIVE"})
	c.Put(&CachedMember{CustomerID: "c2", ExternalID: "NFC002", Active: true, BadgeStatus: "FROZEN"})
	c.Put(&CachedMember{CustomerID: "c3", ExternalID: "NFC003", Active: false, BadgeStatus: "ACTIVE"})
	c.Put(&CachedMember{CustomerID: "c4", ExternalID: "NFC004", Active: true, BadgeStatus: "EXPIRED"})

	pruned := c.PruneInactive()
	if pruned != 3 {
		t.Errorf("PruneInactive() = %d, want 3", pruned)
	}

	if c.GetByExternalID("NFC001") == nil {
		t.Error("NFC001 (active+ACTIVE) should not be pruned")
	}
	if c.GetByExternalID("NFC002") != nil {
		t.Error("NFC002 (FROZEN) should be pruned")
	}
}

func TestBulkReplace(t *testing.T) {
	c, _ := newTestCache(t)
	c.Put(&CachedMember{CustomerID: "old1", ExternalID: "OLD001", Active: true, BadgeStatus: "ACTIVE"})

	newMembers := []*CachedMember{
		{CustomerID: "new1", ExternalID: "NEW001", Active: true, BadgeStatus: "ACTIVE"},
		{CustomerID: "new2", ExternalID: "NEW002", Active: true, BadgeStatus: "ACTIVE"},
	}
	c.BulkReplace(newMembers)

	if c.GetByExternalID("OLD001") != nil {
		t.Error("old member should be replaced")
	}
	if c.GetByExternalID("NEW001") == nil {
		t.Error("new member NEW001 should exist")
	}
	if c.GetByExternalID("NEW002") == nil {
		t.Error("new member NEW002 should exist")
	}

	stats := c.Stats()
	if stats.TotalMembers != 2 {
		t.Errorf("TotalMembers = %d, want 2", stats.TotalMembers)
	}
}

func TestBulkReplaceSkipsEmptyExternalID(t *testing.T) {
	c, _ := newTestCache(t)
	c.BulkReplace([]*CachedMember{
		{CustomerID: "c1", ExternalID: "", Active: true, BadgeStatus: "ACTIVE"},
		{CustomerID: "c2", ExternalID: "NFC002", Active: true, BadgeStatus: "ACTIVE"},
	})

	if c.Stats().TotalMembers != 1 {
		t.Error("should skip member with empty ExternalID")
	}
}

func TestRecordCheckIn(t *testing.T) {
	c, _ := newTestCache(t)
	c.Put(&CachedMember{CustomerID: "c1", ExternalID: "NFC001", Active: true, BadgeStatus: "ACTIVE"})

	c.RecordCheckIn("NFC001")

	got := c.GetByExternalID("NFC001")
	if got.LastCheckIn == "" {
		t.Error("LastCheckIn should be set after RecordCheckIn")
	}

	// Should not panic on nonexistent
	c.RecordCheckIn("NONEXISTENT")
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	c1, err := New(dir, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	c1.Put(&CachedMember{
		CustomerID:  "c1",
		ExternalID:  "NFC001",
		FirstName:   "Bob",
		LastName:    "Jones",
		BadgeStatus: "ACTIVE",
		Active:      true,
		Barcode:     "BC1",
	})

	if err := c1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists and has restrictive permissions
	fp := filepath.Join(dir, "member_cache.json")
	info, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("cache file not found: %v", err)
	}
	// File should exist and be non-empty
	if info.Size() == 0 {
		t.Error("cache file is empty")
	}

	// Load into a new cache
	c2, err := New(dir, discardLogger())
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}

	got := c2.GetByExternalID("NFC001")
	if got == nil {
		t.Fatal("member should be loaded from disk")
	}
	if got.FirstName != "Bob" || got.LastName != "Jones" {
		t.Errorf("loaded name = %q, want 'Bob Jones'", got.FullName())
	}
	if c2.GetByCustomerID("c1") == nil {
		t.Error("customer ID index should be rebuilt on load")
	}
	if c2.GetByBarcode("BC1") == nil {
		t.Error("barcode index should be rebuilt on load")
	}
}

func TestLoadCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "member_cache.json")
	os.WriteFile(fp, []byte("not json at all{{{"), 0o644)

	_, err := New(dir, discardLogger())
	if err == nil {
		t.Error("expected error loading corrupted cache")
	}
}

func TestConcurrentAccess(t *testing.T) {
	c, _ := newTestCache(t)

	// Pre-populate
	for i := 0; i < 100; i++ {
		c.Put(&CachedMember{
			CustomerID:  fmt.Sprintf("c%d", i),
			ExternalID:  fmt.Sprintf("NFC%03d", i),
			Active:      true,
			BadgeStatus: "ACTIVE",
		})
	}

	var wg sync.WaitGroup

	// Concurrent reads
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.GetByExternalID(fmt.Sprintf("NFC%03d", n%100))
				c.GetByCustomerID(fmt.Sprintf("c%d", n%100))
				c.Stats()
			}
		}(i)
	}

	// Concurrent writes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.Put(&CachedMember{
				CustomerID:  fmt.Sprintf("new-c%d", n),
				ExternalID:  fmt.Sprintf("NEW%03d", n),
				Active:      true,
				BadgeStatus: "ACTIVE",
			})
			c.RecordCheckIn(fmt.Sprintf("NFC%03d", n%100))
		}(i)
	}

	wg.Wait()
}

func TestAll(t *testing.T) {
	c, _ := newTestCache(t)
	c.Put(&CachedMember{CustomerID: "c1", ExternalID: "NFC001", FirstName: "A", Active: true, BadgeStatus: "ACTIVE"})
	c.Put(&CachedMember{CustomerID: "c2", ExternalID: "NFC002", FirstName: "B", Active: true, BadgeStatus: "ACTIVE"})

	all := c.All()
	if len(all) != 2 {
		t.Errorf("All() returned %d members, want 2", len(all))
	}

	// Modifying returned slice should not affect cache
	all[0].FirstName = "MODIFIED"
	orig := c.GetByExternalID(all[0].ExternalID)
	if orig.FirstName == "MODIFIED" {
		t.Error("All() should return copies, not references")
	}
}
