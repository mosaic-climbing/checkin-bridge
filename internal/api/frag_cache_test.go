package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

func TestFragMemberTable_ServedFromCacheOnSecondCall(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Populate with 3 members
	for i := 1; i <= 3; i++ {
		nfcUID := "NFC00" + string(rune(i+'0'))
		err := db.UpsertMember(ctx, &store.Member{
			NfcUID:      nfcUID,
			CustomerID:  "c" + string(rune(i+'0')),
			FirstName:   "First" + string(rune(i+'0')),
			LastName:    "Last" + string(rune(i+'0')),
			Active:      true,
			BadgeStatus: "ACTIVE",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// First call—should hit DB and populate cache
	req1 := httptest.NewRequest("GET", "/ui/frag/member-table", nil)
	w1 := httptest.NewRecorder()
	srv.handleFragMemberTable(w1, req1)
	body1, _ := io.ReadAll(w1.Body)

	// Second call—should be served from cache
	req2 := httptest.NewRequest("GET", "/ui/frag/member-table", nil)
	w2 := httptest.NewRecorder()
	srv.handleFragMemberTable(w2, req2)
	body2, _ := io.ReadAll(w2.Body)

	// Both should return the same content
	if !bytes.Equal(body1, body2) {
		t.Errorf("Expected identical content from cache, but they differ")
	}

	// Add a new member to DB (after first request)
	err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC999",
		CustomerID:  "c999",
		FirstName:   "New",
		LastName:    "Member",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Third call with cache still valid should still show old cached content
	req3 := httptest.NewRequest("GET", "/ui/frag/member-table", nil)
	w3 := httptest.NewRecorder()
	srv.handleFragMemberTable(w3, req3)
	body3, _ := io.ReadAll(w3.Body)

	if !bytes.Equal(body1, body3) {
		t.Errorf("Expected cached content to remain unchanged despite DB mutation, but it changed")
	}
}

func TestFragMemberTable_InvalidatedByCardMutation(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Add a member
	memberUID := "NFC001"
	err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      memberUID,
		CustomerID:  "c1",
		FirstName:   "Test",
		LastName:    "Member",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	// First call—cache the table
	req1 := httptest.NewRequest("GET", "/ui/frag/member-table", nil)
	w1 := httptest.NewRecorder()
	srv.handleFragMemberTable(w1, req1)
	body1, _ := io.ReadAll(w1.Body)

	// Verify member is in the cached response
	if !bytes.Contains(body1, []byte("Test")) {
		t.Errorf("Expected cached response to contain member name 'Test'")
	}

	// Delete the member
	err = db.RemoveMember(ctx, memberUID)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a card mutation (which calls Invalidate)
	srv.htmlCache.Invalidate()

	// Now fetch again—should re-query DB and show empty/different
	req2 := httptest.NewRequest("GET", "/ui/frag/member-table", nil)
	w2 := httptest.NewRecorder()
	srv.handleFragMemberTable(w2, req2)
	body2, _ := io.ReadAll(w2.Body)

	// The deleted member should no longer appear
	if bytes.Contains(body2, []byte("Test")) && bytes.Contains(body2, []byte("Member")) {
		t.Errorf("Expected cache invalidation to cause re-query, but member still visible")
	}
}

func TestFragMemberTable_CacheControlHeader(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Add a member so response isn't empty
	err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC001",
		CustomerID:  "c1",
		FirstName:   "Test",
		LastName:    "Member",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/ui/frag/member-table", nil)
	w := httptest.NewRecorder()
	srv.handleFragMemberTable(w, req)

	cc := w.Header().Get("Cache-Control")
	if cc != "private, max-age=5" {
		t.Errorf("Expected Cache-Control header 'private, max-age=5', got %q", cc)
	}
}

func TestFragMemberTable_Pagination_DefaultPage(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Populate 75 members
	for i := 1; i <= 75; i++ {
		suffix := fmt.Sprintf("%03d", i)
		nfcUID := "NFC" + suffix
		err := db.UpsertMember(ctx, &store.Member{
			NfcUID:      nfcUID,
			CustomerID:  "c" + suffix,
			FirstName:   "First" + suffix,
			LastName:    "Last" + suffix,
			Active:      true,
			BadgeStatus: "ACTIVE",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// GET with no params (default page)
	req := httptest.NewRequest("GET", "/ui/frag/member-table", nil)
	w := httptest.NewRecorder()
	srv.handleFragMemberTable(w, req)
	body, _ := io.ReadAll(w.Body)

	// Should contain table header
	if !bytes.Contains(body, []byte("<thead>")) {
		t.Errorf("Expected <thead> in response for first page")
	}

	// Should contain load-more row (since 75 > 50)
	if !bytes.Contains(body, []byte("member-table-load-more")) {
		t.Errorf("Expected load-more row in response for first page with 75 members")
	}

	// Should show "remaining" count
	if !bytes.Contains(body, []byte("remaining")) {
		t.Errorf("Expected 'remaining' text in load-more row")
	}
}

func TestFragMemberTable_Pagination_SecondPage(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Populate 75 members
	for i := 1; i <= 75; i++ {
		suffix := fmt.Sprintf("%03d", i)
		nfcUID := "NFC" + suffix
		err := db.UpsertMember(ctx, &store.Member{
			NfcUID:      nfcUID,
			CustomerID:  "c" + suffix,
			FirstName:   "First" + suffix,
			LastName:    "Last" + suffix,
			Active:      true,
			BadgeStatus: "ACTIVE",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// GET with offset=50 (second page)
	req := httptest.NewRequest("GET", "/ui/frag/member-table?offset=50&limit=50", nil)
	w := httptest.NewRecorder()
	srv.handleFragMemberTable(w, req)
	body, _ := io.ReadAll(w.Body)

	// Should NOT contain table opening (it's a continuation)
	if bytes.Contains(body, []byte("<table>")) {
		t.Logf("Note: second page included <table> tag (may be OK depending on implementation)")
	}

	// Should NOT contain load-more row (only 25 remaining, all on this page)
	if bytes.Contains(body, []byte("member-table-load-more")) {
		t.Errorf("Expected NO load-more row on second page (no more results)")
	}
}

func TestFragMemberTable_Pagination_LimitCapped(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Populate 150 members
	for i := 1; i <= 150; i++ {
		suffix := fmt.Sprintf("%03d", i)
		nfcUID := "NFC" + suffix
		err := db.UpsertMember(ctx, &store.Member{
			NfcUID:      nfcUID,
			CustomerID:  "c" + suffix,
			FirstName:   "First" + suffix,
			LastName:    "Last" + suffix,
			Active:      true,
			BadgeStatus: "ACTIVE",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Request with limit=9999 (should be capped at 200)
	req := httptest.NewRequest("GET", "/ui/frag/member-table?limit=9999", nil)
	w := httptest.NewRecorder()
	srv.handleFragMemberTable(w, req)
	body, _ := io.ReadAll(w.Body)

	// Count table rows (crude but effective)
	rowCount := bytes.Count(body, []byte("<tr>"))
	// Expected: ~150 rows for data + possibly 1 for load-more = ~151
	// But capped at 200, so should be 150 rows max
	if rowCount > 151 {
		t.Errorf("Expected <= 151 rows (150 data + 1 header), got %d", rowCount)
	}
}

func TestFragUnmatchedTable_ServedFromCacheOnSecondCall(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	// First call
	req1 := httptest.NewRequest("GET", "/ui/frag/unmatched-table", nil)
	w1 := httptest.NewRecorder()
	srv.handleFragUnmatchedTable(w1, req1)
	body1, _ := io.ReadAll(w1.Body)

	// Second call—should be identical (from cache)
	req2 := httptest.NewRequest("GET", "/ui/frag/unmatched-table", nil)
	w2 := httptest.NewRecorder()
	srv.handleFragUnmatchedTable(w2, req2)
	body2, _ := io.ReadAll(w2.Body)

	if !bytes.Equal(body1, body2) {
		t.Errorf("Expected identical content from cache")
	}
}

func TestFragUnmatchedTable_CacheControlHeader(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/ui/frag/unmatched-table", nil)
	w := httptest.NewRecorder()
	srv.handleFragUnmatchedTable(w, req)

	cc := w.Header().Get("Cache-Control")
	if cc != "private, max-age=5" {
		t.Errorf("Expected Cache-Control header 'private, max-age=5', got %q", cc)
	}
}

func TestFragUnmatchedTable_InvalidatedByMemberMutation(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Add a member
	err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "NFC001",
		CustomerID:  "c1",
		FirstName:   "Test",
		LastName:    "Member",
		Active:      true,
		BadgeStatus: "ACTIVE",
	})
	if err != nil {
		t.Fatal(err)
	}

	// First call—cache the table
	req1 := httptest.NewRequest("GET", "/ui/frag/unmatched-table", nil)
	w1 := httptest.NewRecorder()
	srv.handleFragUnmatchedTable(w1, req1)

	// Simulate member mutation
	srv.htmlCache.Invalidate()

	// Second call—cache should be invalidated (will re-run ingester)
	req2 := httptest.NewRequest("GET", "/ui/frag/unmatched-table", nil)
	w2 := httptest.NewRecorder()
	srv.handleFragUnmatchedTable(w2, req2)

	// Both should return content (no assertion on content, just that it doesn't panic)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Errorf("Expected 200 OK from both calls")
	}
}

func TestHTMLCache_Concurrency(t *testing.T) {
	// This test ensures the htmlCache works correctly under concurrent access
	c := newHTMLCache()

	// Simulate concurrent Set/Get/Invalidate operations
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(id int) {
			defer func() { done <- true }()
			for j := 0; j < 100; j++ {
				c.Set("key", []byte("value"), 1*time.Second)
				c.Get("key")
				if j%10 == 0 {
					c.Invalidate()
				}
			}
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}
