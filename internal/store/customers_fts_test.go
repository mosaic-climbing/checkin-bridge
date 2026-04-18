package store

import (
	"context"
	"strings"
	"testing"
)

// P4 — `/directory/search` was three sequential LIKE scans against the
// single-writer SQLite. It now goes through `customers_fts`, an FTS5
// virtual table populated by triggers on the `customers` base table.
//
// These tests pin:
//
//   1. Migration 6 actually creates the FTS5 table (would error on missing
//      FTS5 support in the modernc/sqlite build, which we want to know).
//   2. The triggers keep the index in sync with INSERT, UPDATE, DELETE on
//      customers — no manual rebuild step.
//   3. Prefix search works on each indexed column.
//   4. Multi-token queries are AND-joined (alice smith ≠ alice OR smith).
//   5. The query sanitiser strips FTS5 meta-characters that would
//      otherwise crash or change semantics.
//   6. BM25 ranks "more matches" higher than "one match".
//   7. Diacritics are folded (ramirez matches Ramírez).
//   8. The EXPLAIN QUERY PLAN actually walks the FTS index.

func seedCustomers(t *testing.T, s *Store, recs []Customer) {
	t.Helper()
	ctx := context.Background()
	for i, c := range recs {
		if err := s.UpsertCustomer(ctx, &c); err != nil {
			t.Fatalf("seed[%d] (%s): %v", i, c.RedpointID, err)
		}
	}
}

func TestSearchCustomersFTS_PrefixOnName(t *testing.T) {
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Smith", Email: "alice@example.com"},
		{RedpointID: "r2", FirstName: "Alicia", LastName: "Jones", Email: "alicia@example.com"},
		{RedpointID: "r3", FirstName: "Bob", LastName: "Brown", Email: "bob@example.com"},
	})

	got, err := s.SearchCustomersFTS(context.Background(), "ali", 10)
	if err != nil {
		t.Fatalf("FTS search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (Alice + Alicia): %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.RedpointID] = true
	}
	if !ids["r1"] || !ids["r2"] {
		t.Errorf("expected r1+r2, got %v", ids)
	}
}

func TestSearchCustomersFTS_PrefixOnEmail(t *testing.T) {
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Smith", Email: "alice@example.com"},
		{RedpointID: "r2", FirstName: "Bob", LastName: "Brown", Email: "bob@elsewhere.org"},
	})

	got, err := s.SearchCustomersFTS(context.Background(), "elsewhere", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RedpointID != "r2" {
		t.Errorf("expected r2 only, got %+v", got)
	}
}

func TestSearchCustomersFTS_PrefixOnExternalID(t *testing.T) {
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Smith", ExternalID: "EXT-12345"},
		{RedpointID: "r2", FirstName: "Bob", LastName: "Brown", ExternalID: "EXT-99999"},
	})

	got, err := s.SearchCustomersFTS(context.Background(), "EXT-123", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RedpointID != "r1" {
		t.Errorf("expected r1 only, got %+v", got)
	}
}

func TestSearchCustomersFTS_PrefixOnBarcode(t *testing.T) {
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Smith", Barcode: "BAR-AAAA"},
		{RedpointID: "r2", FirstName: "Bob", LastName: "Brown", Barcode: "BAR-BBBB"},
	})

	got, err := s.SearchCustomersFTS(context.Background(), "BAR-AA", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RedpointID != "r1" {
		t.Errorf("expected r1 only, got %+v", got)
	}
}

func TestSearchCustomersFTS_MultiTokenAndSemantics(t *testing.T) {
	// "alice smith" should match Alice Smith only — not Alicia Jones,
	// who matches "alice" alone but not "smith". Implicit AND across
	// tokens is the contract that makes the search useful.
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Smith"},
		{RedpointID: "r2", FirstName: "Alicia", LastName: "Jones"},
		{RedpointID: "r3", FirstName: "Bob", LastName: "Smith"},
	})

	got, err := s.SearchCustomersFTS(context.Background(), "alice smith", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RedpointID != "r1" {
		t.Errorf("expected only r1, got %+v", got)
	}
}

func TestSearchCustomersFTS_TriggerSyncOnUpsert(t *testing.T) {
	// Insert, then update, then delete — the FTS index must reflect each
	// change without a manual rebuild step.
	s := testStore(t)
	ctx := context.Background()

	// 1. Insert via UpsertCustomer
	if err := s.UpsertCustomer(ctx, &Customer{
		RedpointID: "r1", FirstName: "Alice", LastName: "Smith", Email: "alice@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.SearchCustomersFTS(ctx, "smith", 10)
	if len(got) != 1 {
		t.Fatalf("post-insert: expected 1, got %d", len(got))
	}

	// 2. Update last name; old name must drop out, new name must appear
	if err := s.UpsertCustomer(ctx, &Customer{
		RedpointID: "r1", FirstName: "Alice", LastName: "Johnson", Email: "alice@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SearchCustomersFTS(ctx, "smith", 10)
	if len(got) != 0 {
		t.Errorf("post-update: expected 0 'smith' hits, got %d (%+v)", len(got), got)
	}
	got, _ = s.SearchCustomersFTS(ctx, "johnson", 10)
	if len(got) != 1 {
		t.Errorf("post-update: expected 1 'johnson' hit, got %d", len(got))
	}

	// 3. Delete via raw SQL (no Store method exists), confirm trigger fires
	if _, err := s.db.ExecContext(ctx, `DELETE FROM customers WHERE redpoint_id = ?`, "r1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SearchCustomersFTS(ctx, "johnson", 10)
	if len(got) != 0 {
		t.Errorf("post-delete: expected 0, got %d", len(got))
	}
}

func TestSearchCustomersFTS_BatchUpsertSyncs(t *testing.T) {
	// UpsertCustomerBatch goes through a transaction with a prepared
	// statement; the FTS triggers must still fire row-by-row.
	s := testStore(t)
	ctx := context.Background()
	err := s.UpsertCustomerBatch(ctx, []Customer{
		{RedpointID: "b1", FirstName: "Bulk", LastName: "One"},
		{RedpointID: "b2", FirstName: "Bulk", LastName: "Two"},
		{RedpointID: "b3", FirstName: "Bulk", LastName: "Three"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SearchCustomersFTS(ctx, "bulk", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 bulk hits, got %d (%+v)", len(got), got)
	}
}

func TestSearchCustomersFTS_DiacriticFolding(t *testing.T) {
	// `tokenize='unicode61 remove_diacritics 2'` should fold accented
	// characters so plain-ASCII typing still matches.
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "José", LastName: "Ramírez"},
	})

	got, err := s.SearchCustomersFTS(context.Background(), "ramirez", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("expected diacritic fold to match, got %d (%+v)", len(got), got)
	}

	// The opposite direction should also work — typing accents matches
	// plain stored text. (Both forms decompose to the same fold.)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r2", FirstName: "Jose", LastName: "Garcia"},
	})
	got, err = s.SearchCustomersFTS(context.Background(), "josé", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected accented query to match both rows, got %d (%+v)", len(got), got)
	}
}

func TestBuildFTSQuery_SanitisesMetaCharacters(t *testing.T) {
	// FTS5 reserves "():*-" inside the MATCH expression. Letting them
	// through unquoted would either error or change semantics. The
	// sanitiser should silently drop them before quoting.
	cases := []struct {
		in   string
		want string
	}{
		{"smith", `"smith"*`},
		{`"smith"`, `"smith"*`},
		{"smith*", `"smith"*`},
		{"al(ice)", `"alice"*`},
		{"alice smith", `"alice"* "smith"*`},
		{"  spaced   tokens  ", `"spaced"* "tokens"*`},
		{"alice@example.com", `"alice@example.com"*`},
		{"smith-jones", `"smith-jones"*`},
		{"!!! ::: ()", ""},     // all stripped
		{"", ""},               // empty input
		{"  ", ""},             // whitespace only
		{`a"b"c`, `"abc"*`},    // quotes inside token stripped
		{"OR AND NOT", `"OR"* "AND"* "NOT"*`}, // operator words quoted, neutralised
	}
	for _, c := range cases {
		if got := buildFTSQuery(c.in); got != c.want {
			t.Errorf("buildFTSQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSearchCustomersFTS_EmptyQueryReturnsEmpty(t *testing.T) {
	// Both an empty string and a string of pure meta-characters should
	// return zero rows rather than (a) erroring inside FTS5 or (b)
	// matching everything.
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Smith"},
	})
	for _, q := range []string{"", "   ", `"`, "()", "***"} {
		got, err := s.SearchCustomersFTS(context.Background(), q, 10)
		if err != nil {
			t.Errorf("query %q returned error: %v", q, err)
		}
		if len(got) != 0 {
			t.Errorf("query %q returned %d rows, want 0", q, len(got))
		}
	}
}

func TestSearchCustomersFTS_HardCapAt200(t *testing.T) {
	// Limit > 200 should clamp to 200; we don't seed 200+ rows, so we
	// just exercise the branch and assert no error.
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Smith"},
	})
	got, err := s.SearchCustomersFTS(context.Background(), "alice", 99999)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d, want 1", len(got))
	}
}

func TestSearchCustomersFTS_BM25Ranking(t *testing.T) {
	// Two rows both match "smith"; the row that matches across more
	// fields (or has shorter fields) should rank higher.
	s := testStore(t)
	seedCustomers(t, s, []Customer{
		// Pure name match — short doc, name field weighted highest.
		{RedpointID: "r1", FirstName: "Smith", LastName: ""},
		// Buried inside a longer name field.
		{RedpointID: "r2", FirstName: "John Q. Smithson Jr.", LastName: "Withappendage"},
	})
	got, err := s.SearchCustomersFTS(context.Background(), "smith", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 1 {
		t.Fatal("expected at least one hit")
	}
	if got[0].RedpointID != "r1" {
		t.Errorf("expected r1 to rank first under BM25, got order %+v", got)
	}
}

func TestSearchCustomersFTS_UsesFTSIndex(t *testing.T) {
	// EXPLAIN QUERY PLAN should report a virtual-table scan against
	// customers_fts, not a full table scan of customers.
	s := testStore(t)
	rows, err := s.db.QueryContext(context.Background(), `
        EXPLAIN QUERY PLAN
        SELECT c.*
        FROM customers_fts f
        JOIN customers c ON c.redpoint_id = f.redpoint_id
        WHERE customers_fts MATCH ?
        ORDER BY bm25(customers_fts, 10.0, 5.0, 2.0, 2.0)
        LIMIT ?
    `, `"alice"*`, 50)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	got := plan.String()
	// SQLite's EXPLAIN QUERY PLAN labels the row by table alias when one is
	// present, so we look for the virtual-table marker that FTS5 emits.
	// "VIRTUAL TABLE INDEX 0:M5" (or similar M-codes) means the planner is
	// walking the FTS5 module rather than scanning a base table.
	if !strings.Contains(got, "VIRTUAL TABLE") {
		t.Errorf("plan should walk FTS5 virtual table:\n%s", got)
	}
	// The base `customers` table should be looked up by primary key for
	// each FTS hit, not scanned. The autoindex name SQLite generates for
	// the TEXT PRIMARY KEY is `sqlite_autoindex_customers_1`.
	if !strings.Contains(got, "USING INDEX sqlite_autoindex_customers_1") {
		t.Errorf("plan should join customers via PK index, not scan:\n%s", got)
	}
}
