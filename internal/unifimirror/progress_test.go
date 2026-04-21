package unifimirror

// v0.5.7.1 phase/progress coverage. These tests pin the contract the
// staff /ui/sync page relies on: every refresh emits a known sequence
// of phase strings into the ProgressFunc registered via WithProgress,
// so handleFragSyncLastRun can render mid-flight progress in the
// running pill.
//
// The Syncer itself stays oblivious to where progress goes — the
// reporter is an opaque func — which lets the tests assert on the
// phase log directly instead of threading through a store mock. The
// cmd/bridge wiring that actually writes to jobs.progress is
// exercised in internal/api/sync_ux_progress_test.go.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// captureProgress returns a thread-safe collector and the ProgressFunc
// that appends into it. The mutex matters because upsertAndHydrate may
// emit from a different goroutine than the test fixture reads from —
// in practice both run on the test goroutine, but the reporter has no
// such guarantee, so we lock anyway.
func captureProgress() (*[]string, *sync.Mutex, ProgressFunc) {
	var phases []string
	var mu sync.Mutex
	fn := ProgressFunc(func(phase string) {
		mu.Lock()
		defer mu.Unlock()
		phases = append(phases, phase)
	})
	return &phases, &mu, fn
}

// TestRefresh_EmitsPhaseProgress pins the three "always-fires"
// phases: listing → upserting → reconciling. The hydrate phase only
// fires when there are blank-email rows, which we model here.
func TestRefresh_EmitsPhaseProgress(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "ua-1", Name: "User One", Email: "one@example.com", Status: "active"},
		{ID: "ua-2", Name: "User Two", Email: "two@example.com", Status: "active"},
	}}
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	phases, mu, fn := captureProgress()
	ctx := WithProgress(context.Background(), fn)
	if _, err := syn.RefreshWithStats(ctx); err != nil {
		t.Fatalf("RefreshWithStats: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), *phases...)
	mu.Unlock()

	// Expected sequence, in order:
	//   1. "listing UA-Hub users"           — before the paginated list call
	//   2. "upserting 2 list rows"          — beginning of upsertAndHydrate
	//   3. "reconciling pending mappings"   — before recheckPending
	//
	// No hydrate phase here because both users came back with emails.
	wantPrefixes := []string{
		"listing UA-Hub users",
		"upserting 2 list rows",
		"reconciling pending mappings",
	}
	if len(got) < len(wantPrefixes) {
		t.Fatalf("phases=%v, want at least %d entries", got, len(wantPrefixes))
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(got[i], prefix) {
			t.Errorf("phases[%d] = %q, want prefix %q", i, got[i], prefix)
		}
	}
}

// TestRefresh_EmitsHydrateProgress covers the slow-path phase: when
// the list endpoint omits emails and the Syncer backfills via
// per-user FetchUser, a "hydrating N/M" phase string should appear
// between the upsert and the reconcile phases. hydrateProgressEvery
// is dropped to 1 so every hydrate emits, making the assertion
// tractable even on small fixtures.
func TestRefresh_EmitsHydrateProgress(t *testing.T) {
	prevEvery := hydrateProgressEvery
	prevInterval := hydrateInterval
	hydrateProgressEvery = 1
	hydrateInterval = 0
	t.Cleanup(func() {
		hydrateProgressEvery = prevEvery
		hydrateInterval = prevInterval
	})

	s := openStore(t)
	up := &fakeUnifi{
		users: []unifi.UniFiUser{
			{ID: "ua-1", Name: "Blank One", Status: "active"},
			{ID: "ua-2", Name: "Blank Two", Status: "active"},
		},
		fetchOverrides: map[string]unifi.UniFiUser{
			"ua-1": {ID: "ua-1", Name: "Blank One", Email: "one@example.com", Status: "active"},
			"ua-2": {ID: "ua-2", Name: "Blank Two", Email: "two@example.com", Status: "active"},
		},
	}
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	phases, mu, fn := captureProgress()
	ctx := WithProgress(context.Background(), fn)
	if _, err := syn.RefreshWithStats(ctx); err != nil {
		t.Fatalf("RefreshWithStats: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), *phases...)
	mu.Unlock()

	// At least one "hydrating N/2" phase must appear between the
	// upsert and reconcile emits. Exact count depends on the modulo
	// cadence, but with hydrateProgressEvery=1 we expect both.
	hydrateHits := 0
	for _, p := range got {
		if strings.HasPrefix(p, "hydrating ") {
			hydrateHits++
		}
	}
	if hydrateHits < 1 {
		t.Errorf("expected at least one 'hydrating N/M' phase, got %v", got)
	}
}

// TestRefresh_NoProgressFuncIsNoop is the safety net for all callers
// that don't install a progress reporter (the nightly ticker, the
// legacy ingest path, existing tests). Refresh must stay functional
// and must not panic when context.Value returns nil.
func TestRefresh_NoProgressFuncIsNoop(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "ua-1", Name: "Solo", Email: "solo@example.com", Status: "active"},
	}}
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	// No WithProgress call — bare context. reportProgress must
	// short-circuit without dereferencing a nil func.
	if _, err := syn.RefreshWithStats(context.Background()); err != nil {
		t.Fatalf("RefreshWithStats without progress: %v", err)
	}
}

// TestWithProgress_NilFuncReturnsCtx pins the documented behavior of
// WithProgress: passing nil returns the input context unchanged, so
// callers can unconditionally wrap without branching on the
// optional-progress-func case.
func TestWithProgress_NilFuncReturnsCtx(t *testing.T) {
	parent := context.Background()
	got := WithProgress(parent, nil)
	if got != parent {
		t.Errorf("WithProgress(ctx, nil) = %v, want identical ctx back", got)
	}
}
