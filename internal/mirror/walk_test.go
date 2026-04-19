package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Test fakes ───────────────────────────────────────────────────

// fakeClient serves canned GraphQL responses in order. Each call pops
// the next response off the queue; an exhausted queue returns io.EOF
// (which surfaces through Walk as a fetch error, useful for
// "walker must stop after N pages" assertions).
type fakeClient struct {
	mu        sync.Mutex
	responses []fakeResponse
	calls     int
}

type fakeResponse struct {
	body json.RawMessage
	err  error
}

func (f *fakeClient) ExecQuery(ctx context.Context, query string, vars map[string]any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.responses) == 0 {
		return nil, io.EOF
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return r.body, r.err
}

// fakeStore is an in-memory implementation of mirror.Store. Captures
// each batch written and the full sequence of sync_state transitions
// so tests can make strong claims about ordering, not just end state.
type fakeStore struct {
	mu          sync.Mutex
	state       *SyncState
	batches     [][]Customer
	transitions []string // "StartSync", "UpdateSyncState:running@<cursor>", "MarkSyncComplete:<status>"
	errOn       map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{state: &SyncState{Status: "idle"}}
}

func (s *fakeStore) GetSyncState(ctx context.Context) (*SyncState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return nil, nil
	}
	cp := *s.state
	return &cp, nil
}

func (s *fakeStore) StartSync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errOn["StartSync"]; err != nil {
		return err
	}
	s.transitions = append(s.transitions, "StartSync")
	s.state = &SyncState{
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

func (s *fakeStore) UpdateSyncState(ctx context.Context, st *SyncState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errOn["UpdateSyncState"]; err != nil {
		return err
	}
	s.transitions = append(s.transitions,
		fmt.Sprintf("UpdateSyncState:%s@%s", st.Status, st.LastCursor))
	cp := *st
	s.state = &cp
	return nil
}

func (s *fakeStore) UpsertCustomerWithBadgeBatch(ctx context.Context, customers []Customer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errOn["UpsertCustomerWithBadgeBatch"]; err != nil {
		return err
	}
	// Deep-copy so tests inspecting state can't be perturbed by later writes.
	cp := make([]Customer, len(customers))
	copy(cp, customers)
	s.batches = append(s.batches, cp)
	return nil
}

func (s *fakeStore) MarkSyncComplete(ctx context.Context, status, lastError string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errOn["MarkSyncComplete"]; err != nil {
		return err
	}
	s.transitions = append(s.transitions, "MarkSyncComplete:"+status)
	if s.state != nil {
		s.state.Status = status
		s.state.LastError = lastError
		s.state.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return nil
}

// ─── Payload helpers ──────────────────────────────────────────────

// pageJSON builds a GraphQL response body shaped like walkQuery's
// expected pageShape.
func pageJSON(t *testing.T, hasNext bool, endCursor string, edges []map[string]any) json.RawMessage {
	t.Helper()
	body := map[string]any{
		"customers": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
			"edges":    edgeList(edges),
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal page: %v", err)
	}
	return b
}

func edgeList(nodes []map[string]any) []map[string]any {
	out := make([]map[string]any, len(nodes))
	for i, n := range nodes {
		out[i] = map[string]any{"node": n}
	}
	return out
}

// quietLogger is a slog logger that discards output. Keeps test output
// clean when the walker logs info/warn during happy-path runs.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ─── Happy path ───────────────────────────────────────────────────

// TestWalk_HappyPath_TwoPages verifies that a two-page response is
// turned into two UpsertCustomerWithBadgeBatch calls with the right
// rows, and that sync_state ends at "complete" with last_cursor
// cleared (so the next run starts fresh).
func TestWalk_HappyPath_TwoPages(t *testing.T) {
	client := &fakeClient{
		responses: []fakeResponse{
			{body: pageJSON(t, true, "cursor-page-1", []map[string]any{
				{"id": "rp-1", "active": true, "firstName": "Alice", "lastName": "Smith",
					"email": "a@x", "pastDueBalance": 0,
					"badge": map[string]any{"status": "ACTIVE",
						"customerBadge": map[string]any{"id": "b1", "name": "Adult"}},
					"homeFacility": map[string]any{"shortName": "Mosaic"}},
			})},
			{body: pageJSON(t, false, "", []map[string]any{
				{"id": "rp-2", "active": true, "firstName": "Bob", "lastName": "Jones",
					"pastDueBalance": 12.5,
					"badge":          map[string]any{"status": "FROZEN"}},
			})},
		},
	}
	store := newFakeStore()
	w := New(client, store, quietLogger(), Config{
		PageSize:       100,
		InterPageDelay: 1 * time.Millisecond, // keep the test fast
	})

	if err := w.Walk(context.Background()); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// Exactly two batches, totalling the two rows with badge state.
	if len(store.batches) != 2 {
		t.Fatalf("batches = %d, want 2", len(store.batches))
	}
	if store.batches[0][0].RedpointID != "rp-1" || store.batches[0][0].BadgeStatus != "ACTIVE" {
		t.Errorf("batch[0]: %+v", store.batches[0][0])
	}
	if store.batches[0][0].BadgeName != "Adult" {
		t.Errorf("batch[0] name: %q", store.batches[0][0].BadgeName)
	}
	if store.batches[0][0].HomeFacilityShortName != "Mosaic" {
		t.Errorf("batch[0] facility: %q", store.batches[0][0].HomeFacilityShortName)
	}
	if store.batches[1][0].RedpointID != "rp-2" || store.batches[1][0].PastDueBalance != 12.5 {
		t.Errorf("batch[1]: %+v", store.batches[1][0])
	}

	// End state: complete, last_cursor cleared.
	if store.state.Status != "complete" {
		t.Errorf("final status = %q, want complete", store.state.Status)
	}
	if store.state.LastCursor != "" {
		t.Errorf("final cursor = %q, want empty", store.state.LastCursor)
	}
	if store.state.TotalFetched != 2 {
		t.Errorf("total = %d, want 2", store.state.TotalFetched)
	}
}

// TestWalk_FirstPageEmpty_NoBatch verifies that a zero-edge page
// doesn't blow up the batch commit (SQLite prepared-stmt reuse is
// sensitive to empty Exec paths) and still transitions to complete.
func TestWalk_FirstPageEmpty_NoBatch(t *testing.T) {
	client := &fakeClient{
		responses: []fakeResponse{
			{body: pageJSON(t, false, "", nil)},
		},
	}
	store := newFakeStore()
	w := New(client, store, quietLogger(), Config{InterPageDelay: time.Millisecond})

	if err := w.Walk(context.Background()); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(store.batches) != 0 {
		t.Errorf("unexpected batch writes: %d", len(store.batches))
	}
	if store.state.Status != "complete" {
		t.Errorf("status = %q, want complete", store.state.Status)
	}
}

// ─── Resume ───────────────────────────────────────────────────────

// TestWalk_Resume_PicksUpFromCursor verifies that when sync_state
// arrives with a non-empty last_cursor and non-complete status, the
// walker does NOT call StartSync, and the first GraphQL request uses
// the resumed cursor in `after`.
func TestWalk_Resume_PicksUpFromCursor(t *testing.T) {
	store := newFakeStore()
	store.state = &SyncState{
		Status:       "error", // e.g., previous run crashed
		LastCursor:   "resume-me",
		TotalFetched: 100,
		StartedAt:    "2026-04-18T00:00:00Z",
	}

	// Capture the variables passed to the first ExecQuery to prove
	// `after` was set to the resume cursor.
	var firstVars map[string]any
	client := &capturingClient{
		inner: &fakeClient{
			responses: []fakeResponse{
				{body: pageJSON(t, false, "", []map[string]any{
					{"id": "rp-9", "firstName": "Zoe", "lastName": "End"},
				})},
			},
		},
		onCall: func(q string, v map[string]any) {
			if firstVars == nil {
				cp := map[string]any{}
				for k, x := range v {
					cp[k] = x
				}
				firstVars = cp
			}
		},
	}

	w := New(client, store, quietLogger(), Config{InterPageDelay: time.Millisecond})
	if err := w.Walk(context.Background()); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if got := firstVars["after"]; got != "resume-me" {
		t.Errorf("after var = %v, want 'resume-me'", got)
	}
	// TotalFetched should continue from 100, not restart at 0.
	if store.state.TotalFetched != 101 {
		t.Errorf("total = %d, want 101 (100 carried + 1 new)", store.state.TotalFetched)
	}
	// StartSync must NOT have fired.
	for _, tr := range store.transitions {
		if tr == "StartSync" {
			t.Errorf("unexpected StartSync on resume path; transitions = %v", store.transitions)
		}
	}
}

// capturingClient lets a test peek at the vars passed to ExecQuery
// without reimplementing the fake. It delegates to inner and invokes
// onCall once per request.
type capturingClient struct {
	inner  *fakeClient
	onCall func(query string, vars map[string]any)
}

func (c *capturingClient) ExecQuery(ctx context.Context, query string, vars map[string]any) (json.RawMessage, error) {
	if c.onCall != nil {
		c.onCall(query, vars)
	}
	return c.inner.ExecQuery(ctx, query, vars)
}

// TestWalk_Resume_IgnoredWhenStatusComplete verifies that a prior
// "complete" status causes a fresh start even if last_cursor happens
// to be non-empty (which shouldn't be possible post-completion, but
// we defend against a malformed state).
func TestWalk_Resume_IgnoredWhenStatusComplete(t *testing.T) {
	store := newFakeStore()
	store.state = &SyncState{
		Status:     "complete",
		LastCursor: "stale-should-ignore",
	}
	client := &capturingClient{
		inner: &fakeClient{
			responses: []fakeResponse{
				{body: pageJSON(t, false, "", nil)},
			},
		},
		onCall: func(q string, v map[string]any) {
			if _, ok := v["after"]; ok {
				t.Errorf("first request used 'after' on a complete-state resume; vars = %v", v)
			}
		},
	}

	w := New(client, store, quietLogger(), Config{InterPageDelay: time.Millisecond})
	if err := w.Walk(context.Background()); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// StartSync must HAVE fired.
	sawStart := false
	for _, tr := range store.transitions {
		if tr == "StartSync" {
			sawStart = true
		}
	}
	if !sawStart {
		t.Errorf("expected StartSync on fresh path; transitions = %v", store.transitions)
	}
}

// ─── Error paths ──────────────────────────────────────────────────

// TestWalk_FetchErrorPersistsCursor verifies that when page 2 errors,
// sync_state retains page 1's last_cursor and status transitions to
// "error" — so a subsequent run resumes from page 1 rather than
// restarting the whole walk.
func TestWalk_FetchErrorPersistsCursor(t *testing.T) {
	boom := errors.New("rate limited after retries")
	client := &fakeClient{
		responses: []fakeResponse{
			{body: pageJSON(t, true, "cursor-after-page-1", []map[string]any{
				{"id": "rp-1", "firstName": "A", "lastName": "B"},
			})},
			{err: boom},
		},
	}
	store := newFakeStore()
	w := New(client, store, quietLogger(), Config{InterPageDelay: time.Millisecond})

	err := w.Walk(context.Background())
	if err == nil {
		t.Fatal("expected error from page-2 fetch")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err = %v, want wrapped rate-limit", err)
	}
	if store.state.Status != "error" {
		t.Errorf("status = %q, want error", store.state.Status)
	}
	if store.state.LastCursor != "cursor-after-page-1" {
		t.Errorf("last_cursor = %q, want page-1 cursor", store.state.LastCursor)
	}
	// One batch landed — page 1 committed before page 2 failed.
	if len(store.batches) != 1 {
		t.Errorf("batches = %d, want 1", len(store.batches))
	}
}

// TestWalk_UpsertErrorDoesNotAdvanceCursor verifies that when the
// batch upsert fails mid-walk, last_cursor is NOT bumped forward —
// a retry reprocesses the same page rather than silently skipping.
func TestWalk_UpsertErrorDoesNotAdvanceCursor(t *testing.T) {
	client := &fakeClient{
		responses: []fakeResponse{
			{body: pageJSON(t, true, "unreachable-cursor", []map[string]any{
				{"id": "rp-1", "firstName": "A", "lastName": "B"},
			})},
		},
	}
	store := newFakeStore()
	store.errOn = map[string]error{
		"UpsertCustomerWithBadgeBatch": errors.New("disk full"),
	}

	w := New(client, store, quietLogger(), Config{InterPageDelay: time.Millisecond})
	err := w.Walk(context.Background())
	if err == nil {
		t.Fatal("expected upsert error")
	}
	if store.state.LastCursor == "unreachable-cursor" {
		t.Errorf("last_cursor was advanced past a failed upsert: %q", store.state.LastCursor)
	}
	if store.state.Status != "error" {
		t.Errorf("status = %q, want error", store.state.Status)
	}
}

// TestWalk_ContextCancellationReturnsCtxErr verifies that cancelling
// the context between pages stops the walk and propagates ctx.Err().
// sync_state is left in "running" so a restart resumes.
func TestWalk_ContextCancellationReturnsCtxErr(t *testing.T) {
	client := &fakeClient{
		responses: []fakeResponse{
			{body: pageJSON(t, true, "c1", []map[string]any{
				{"id": "rp-1"},
			})},
			// If we ever reach this second response, cancellation didn't work.
			{body: pageJSON(t, false, "", nil)},
		},
	}
	store := newFakeStore()
	w := New(client, store, quietLogger(), Config{
		InterPageDelay: 200 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel well before the 200ms inter-page delay finishes on page 2.
	time.AfterFunc(20*time.Millisecond, cancel)

	err := w.Walk(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The status should still be "running" — cancellation is not a
	// walk-level error and we want the next run to resume.
	if store.state.Status != "running" {
		t.Errorf("status = %q, want running (resume-able)", store.state.Status)
	}
}

// TestWalk_ConfigDefaults verifies zero-value Config resolves to the
// documented defaults. Cheap but load-bearing: a regression here would
// have the walker hammering Redpoint with no delay.
func TestWalk_ConfigDefaults(t *testing.T) {
	w := New(nil, nil, quietLogger(), Config{})
	if w.cfg.PageSize != DefaultPageSize {
		t.Errorf("PageSize = %d, want %d", w.cfg.PageSize, DefaultPageSize)
	}
	if w.cfg.InterPageDelay != DefaultInterPageDelay {
		t.Errorf("InterPageDelay = %v, want %v", w.cfg.InterPageDelay, DefaultInterPageDelay)
	}
}

// TestFlexF64_NumberAndString verifies the number-or-string decoder
// used for Redpoint's pastDueBalance field. Number-form is the common
// case; string-form shows up on certain endpoint versions.
func TestFlexF64_NumberAndString(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{`0`, 0},
		{`12.5`, 12.5},
		{`-1.25`, -1.25},
		{`"0"`, 0},
		{`"12.50"`, 12.5},
		{`""`, 0},
		{`null`, 0},
	}
	for _, tc := range cases {
		var f flexF64
		if err := json.Unmarshal([]byte(tc.in), &f); err != nil {
			t.Errorf("Unmarshal(%q): %v", tc.in, err)
			continue
		}
		if float64(f) != tc.want {
			t.Errorf("Unmarshal(%q) = %v, want %v", tc.in, float64(f), tc.want)
		}
	}
}
