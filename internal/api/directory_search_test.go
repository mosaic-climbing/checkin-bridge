package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// P4 — `/directory/search` is now a single FTS5 query against the
// `customers_fts` virtual table. These tests cover the API surface end to
// end (routing, query-string parsing, FTS join, cache annotation).

type searchResp struct {
	Query   string `json:"query"`
	Count   int    `json:"count"`
	Results []struct {
		store.Customer
		InCache     bool   `json:"inCache"`
		CacheNfcUID string `json:"cacheNfcUid"`
	} `json:"results"`
}

func TestDirectorySearch_MissingQReturns400(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	req := httptest.NewRequest("GET", "/directory/search", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDirectorySearch_FindsByNamePrefix(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	_ = db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "c1", FirstName: "Alice", LastName: "Smith", Email: "alice@example.com",
	})
	_ = db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "c2", FirstName: "Bob", LastName: "Brown", Email: "bob@example.com",
	})

	req := httptest.NewRequest("GET", "/directory/search?q=ali", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp searchResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || resp.Results[0].RedpointID != "c1" {
		t.Errorf("expected only c1, got %+v", resp)
	}
}

func TestDirectorySearch_EmailMatchedByLocalPart(t *testing.T) {
	// The old handler had a special-case for queries containing "@" that
	// did an exact-email lookup. The FTS5 path tokenises the email on `@`
	// and `.` so prefix-of-local-part also matches — which is what staff
	// actually want when typing into the search box.
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()
	_ = db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "c1", FirstName: "Alice", LastName: "Smith", Email: "alice@example.com",
	})

	req := httptest.NewRequest("GET", "/directory/search?q=alice@example.com", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp searchResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Count != 1 {
		t.Errorf("expected 1 hit for full email, got %d", resp.Count)
	}
}

func TestDirectorySearch_AnnotatesCacheStatus(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	_ = db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "c1", FirstName: "Alice", LastName: "Smith",
	})
	// c1 has an enrolled NFC card; the response should reflect that.
	_ = db.UpsertMember(ctx, &store.Member{
		NfcUID: "TAG-ALICE", CustomerID: "c1", Active: true, BadgeStatus: "ACTIVE",
	})
	_ = db.UpsertCustomer(ctx, &store.Customer{
		RedpointID: "c2", FirstName: "Alex", LastName: "Brown",
	})

	req := httptest.NewRequest("GET", "/directory/search?q=al", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var resp searchResp
	json.NewDecoder(w.Body).Decode(&resp)

	byID := map[string]struct {
		inCache bool
		nfc     string
	}{}
	for _, r := range resp.Results {
		byID[r.RedpointID] = struct {
			inCache bool
			nfc     string
		}{r.InCache, r.CacheNfcUID}
	}
	if !byID["c1"].inCache || byID["c1"].nfc != "TAG-ALICE" {
		t.Errorf("c1 cache status wrong: %+v", byID["c1"])
	}
	if byID["c2"].inCache {
		t.Errorf("c2 should not be in cache: %+v", byID["c2"])
	}
}

func TestDirectorySearch_MultiTokenAndSemantics(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()
	_ = db.UpsertCustomer(ctx, &store.Customer{RedpointID: "c1", FirstName: "Alice", LastName: "Smith"})
	_ = db.UpsertCustomer(ctx, &store.Customer{RedpointID: "c2", FirstName: "Alice", LastName: "Brown"})
	_ = db.UpsertCustomer(ctx, &store.Customer{RedpointID: "c3", FirstName: "Bob", LastName: "Smith"})

	req := httptest.NewRequest("GET", "/directory/search?q=alice+smith", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var resp searchResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Count != 1 || resp.Results[0].RedpointID != "c1" {
		t.Errorf("expected only c1 (Alice Smith), got %+v", resp)
	}
}

func TestDirectorySearch_QueryWithMetaCharsReturnsEmpty(t *testing.T) {
	// A query of pure FTS5 meta-characters should sanitise to empty and
	// return zero results — no 500, no FTS5 syntax error to the client.
	srv, db, _ := setupTestServer(t)
	_ = db.UpsertCustomer(context.Background(), &store.Customer{
		RedpointID: "c1", FirstName: "Alice", LastName: "Smith",
	})
	req := httptest.NewRequest("GET", "/directory/search?q=%28%29", nil) // "()" url-encoded
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp searchResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Count != 0 {
		t.Errorf("expected 0 results for sanitised-empty query, got %d", resp.Count)
	}
}
