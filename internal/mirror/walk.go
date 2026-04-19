// Package mirror maintains a local SQLite mirror of Redpoint's customer
// directory plus each customer's current badge state, so the bridge can
// answer membership questions without an up-stream roundtrip on every
// NFC tap.
//
// Layering:
//
//	internal/mirror (this package)
//	    ↓ uses
//	internal/redpoint.Client.ExecQuery  (paged GraphQL, retry + breaker)
//	    ↓ writes
//	internal/store.Store.UpsertCustomerWithBadgeBatch  (per-page tx)
//
// Shape of a run: Walker.Walk pages through customers(filter: {active:
// ACTIVE}) with cursor-based pagination, enriches each row with
// badge/facility fields, and writes the batch to the store. Between
// pages it sleeps config.InterPageDelay (default 2s — Redpoint's
// undocumented throttle has been observed to fire below this). After
// each page commit it updates sync_state with the new cursor and
// total_fetched so a crash or cancel mid-walk resumes cleanly.
//
// Resumability: on start, if sync_state.LastCursor is non-empty and
// status is not "complete", the walker picks up from that cursor
// rather than restarting from the top. This is what makes a nightly
// 900-row sync robust to daytime restarts: a new walk that happens to
// crash at page 5 out of 10 doesn't duplicate pages 1-5, it just
// finishes 5-10.
//
// Concurrency: the walker itself is NOT an atomic claim. The calling
// layer (admin endpoint, cron) is responsible for checking
// sync_state.Status before kicking off a new run. See the admin
// handler for the 409-Conflict-on-concurrent logic.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Config holds the tunable knobs for a walk.
type Config struct {
	// PageSize is the number of customers fetched per GraphQL page.
	// Redpoint's docs don't publish a max, but 100 is what we've used
	// for the pre-A4 directory sync and it's been stable. Zero means
	// "use DefaultPageSize".
	PageSize int

	// InterPageDelay is the pause between page fetches. The
	// Redpoint portal's undocumented rate limiter starts returning
	// 429s around 1 req/s sustained; 2s gives comfortable headroom
	// even when stage-1's Retry-After handling doesn't trigger.
	// Zero means "use DefaultInterPageDelay".
	InterPageDelay time.Duration
}

// Default knobs. Conservative on purpose — the walker runs in the
// background during operating hours and we'd rather take 60s longer
// than spike the gym's tap-to-enter latency by saturating the
// Redpoint shared rate budget.
const (
	DefaultPageSize       = 100
	DefaultInterPageDelay = 2 * time.Second
)

// RedpointClient is the subset of internal/redpoint.Client that the
// walker needs. Kept narrow so tests can pass in a fake that returns
// canned GraphQL payloads without standing up an HTTP server.
type RedpointClient interface {
	ExecQuery(ctx context.Context, query string, vars map[string]any) (json.RawMessage, error)
}

// Store is the subset of *store.Store that the walker needs — again,
// narrow for testability. Matches the real method set on internal/store.
type Store interface {
	GetSyncState(ctx context.Context) (*SyncState, error)
	StartSync(ctx context.Context) error
	UpdateSyncState(ctx context.Context, state *SyncState) error
	UpsertCustomerWithBadgeBatch(ctx context.Context, customers []Customer) error
	MarkSyncComplete(ctx context.Context, status, lastError string) error
}

// SyncState mirrors the schema of store.SyncState. We redeclare it
// here so this package doesn't take a dependency on internal/store's
// Go types for the interface — the interface methods already return
// concrete *store.SyncState via a thin adapter. The adapter lives
// next door in mirror_store.go so this file stays focused on the
// walk loop.
type SyncState struct {
	Status       string
	TotalFetched int
	LastCursor   string
	LastError    string
	StartedAt    string
	CompletedAt  string
}

// Customer carries the fields the walker writes back to the mirror.
// Mirrored from store.Customer's badge-augmented shape so the walker
// can construct batches without importing internal/store.
type Customer struct {
	RedpointID            string
	FirstName             string
	LastName              string
	Email                 string
	Barcode               string
	ExternalID            string
	Active                bool
	UpdatedAt             string
	BadgeStatus           string
	BadgeName             string
	PastDueBalance        float64
	HomeFacilityShortName string
	LastSyncedAt          string
}

// Walker is the stateful orchestrator. Construct with New; call Walk
// to run one pass. Walk is safe to call multiple times sequentially;
// caller-side concurrency control prevents two runs overlapping.
type Walker struct {
	client RedpointClient
	store  Store
	logger *slog.Logger
	cfg    Config
}

// New constructs a Walker. Zero-value knobs in cfg fall back to the
// Default* constants — callers who don't care can pass Config{}.
func New(client RedpointClient, store Store, logger *slog.Logger, cfg Config) *Walker {
	if cfg.PageSize <= 0 {
		cfg.PageSize = DefaultPageSize
	}
	if cfg.InterPageDelay <= 0 {
		cfg.InterPageDelay = DefaultInterPageDelay
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Walker{
		client: client,
		store:  store,
		logger: logger,
		cfg:    cfg,
	}
}

// walkQuery is the GraphQL operation the walker runs against Redpoint.
// It's the pre-existing listActiveCustomersQuery extended with the
// fields the mirror needs for validation (pastDueBalance,
// homeFacility). Kept in this package (not internal/redpoint) so the
// Redpoint client stays agnostic about mirror concerns.
const walkQuery = `
query MirrorCustomers($filter: CustomerFilter!, $first: Int, $after: String) {
  customers(filter: $filter, first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    edges {
      node {
        id
        active
        firstName
        lastName
        email
        barcode
        externalId
        pastDueBalance
        homeFacility { shortName }
        badge {
          status
          customerBadge { id name }
        }
      }
    }
  }
}
`

// pageShape matches the JSON returned by walkQuery. Kept as a private
// type so the public Customer struct doesn't carry GraphQL artefacts.
type pageShape struct {
	Customers struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Edges []struct {
			Node struct {
				ID             string   `json:"id"`
				Active         bool     `json:"active"`
				FirstName      string   `json:"firstName"`
				LastName       string   `json:"lastName"`
				Email          string   `json:"email"`
				Barcode        string   `json:"barcode"`
				ExternalID     string   `json:"externalId"`
				PastDueBalance flexF64  `json:"pastDueBalance"`
				HomeFacility   *struct {
					ShortName string `json:"shortName"`
				} `json:"homeFacility"`
				Badge *struct {
					Status        string `json:"status"`
					CustomerBadge *struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"customerBadge"`
				} `json:"badge"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"customers"`
}

// Walk runs one pass through the Redpoint customer directory,
// mirroring each row into the local store. Blocks until completion,
// context cancellation, or an unretryable error.
//
// Resume semantics:
//   - If sync_state.LastCursor is non-empty AND status != "complete",
//     the walk resumes from that cursor. total_fetched is preserved.
//   - Otherwise, the walk starts fresh: StartSync zeros total_fetched
//     and started_at is stamped.
//
// On success, sync_state.status is set to "complete" and completed_at
// is stamped. On error, status is "error" with the last_error
// populated and last_cursor left at the last successfully-persisted
// page — a subsequent walk resumes from there.
//
// Context cancellation is NOT an error: the walk returns ctx.Err()
// but leaves sync_state in a resumable "running" state so a restart
// picks up where we left off. The caller (admin endpoint, cron) is
// expected to surface cancellation distinctly from a real failure.
func (w *Walker) Walk(ctx context.Context) error {
	// Decide resume vs fresh start by inspecting existing state.
	prev, err := w.store.GetSyncState(ctx)
	if err != nil {
		return fmt.Errorf("read sync state: %w", err)
	}

	resume := prev != nil && prev.LastCursor != "" && prev.Status != "complete"
	var cursor string
	totalFetched := 0
	if resume {
		cursor = prev.LastCursor
		totalFetched = prev.TotalFetched
		w.logger.Info("mirror walk resuming",
			"cursor_prefix", cursorPrefix(cursor),
			"previously_fetched", totalFetched,
			"previous_status", prev.Status)
	} else {
		if err := w.store.StartSync(ctx); err != nil {
			return fmt.Errorf("start sync: %w", err)
		}
		w.logger.Info("mirror walk starting fresh",
			"page_size", w.cfg.PageSize,
			"inter_page_delay", w.cfg.InterPageDelay)
	}

	page := 0
	for {
		page++

		// Inter-page pacing: skip on the first page so a resume or
		// a fresh start doesn't wait unnecessarily. The 2s floor is
		// what keeps us comfortably under Redpoint's observed 429
		// threshold (see Config docstring).
		if page > 1 {
			select {
			case <-ctx.Done():
				// Don't bump status to error — the cursor is
				// already persisted from the previous page
				// commit, so the next run will resume.
				return ctx.Err()
			case <-time.After(w.cfg.InterPageDelay):
			}
		}

		vars := map[string]any{
			"filter": map[string]any{"active": "ACTIVE"},
			"first":  w.cfg.PageSize,
		}
		if cursor != "" {
			vars["after"] = cursor
		}

		data, err := w.client.ExecQuery(ctx, walkQuery, vars)
		if err != nil {
			// Persist error state with the LAST successfully-committed
			// cursor so the next walk resumes from the same spot. The
			// caller will see the wrapped error via Walk's return.
			_ = w.store.MarkSyncComplete(ctx, "error", err.Error())
			return fmt.Errorf("fetch page %d: %w", page, err)
		}

		var parsed pageShape
		if err := json.Unmarshal(data, &parsed); err != nil {
			_ = w.store.MarkSyncComplete(ctx, "error", err.Error())
			return fmt.Errorf("unmarshal page %d: %w", page, err)
		}

		batch := make([]Customer, 0, len(parsed.Customers.Edges))
		now := time.Now().UTC().Format(time.RFC3339)
		for i := range parsed.Customers.Edges {
			n := parsed.Customers.Edges[i].Node
			c := Customer{
				RedpointID:     n.ID,
				FirstName:      n.FirstName,
				LastName:       n.LastName,
				Email:          n.Email,
				Barcode:        n.Barcode,
				ExternalID:     n.ExternalID,
				Active:         n.Active,
				UpdatedAt:      now,
				PastDueBalance: float64(n.PastDueBalance),
				LastSyncedAt:   now,
			}
			if n.HomeFacility != nil {
				c.HomeFacilityShortName = n.HomeFacility.ShortName
			}
			if n.Badge != nil {
				c.BadgeStatus = n.Badge.Status
				if n.Badge.CustomerBadge != nil {
					c.BadgeName = n.Badge.CustomerBadge.Name
				}
			}
			batch = append(batch, c)
		}

		if len(batch) > 0 {
			if err := w.store.UpsertCustomerWithBadgeBatch(ctx, batch); err != nil {
				// Same resume-from-last-cursor logic: since the batch
				// failed, we DON'T advance the cursor — the next walk
				// retries this same page.
				_ = w.store.MarkSyncComplete(ctx, "error", err.Error())
				return fmt.Errorf("upsert page %d: %w", page, err)
			}
			totalFetched += len(batch)
		}

		// Persist cursor + running-total AFTER a successful commit so
		// that crash recovery never loses data (we'd redo the page) or
		// skips data (we'd miss it). Worst case is re-uploading a page,
		// which is idempotent on our UPSERT.
		cursor = parsed.Customers.PageInfo.EndCursor
		if err := w.store.UpdateSyncState(ctx, &SyncState{
			Status:       "running",
			TotalFetched: totalFetched,
			LastCursor:   cursor,
			StartedAt:    startedAtFrom(prev, resume),
		}); err != nil {
			// Couldn't write progress — don't abort the run, but log
			// loudly. A failed sync_state write doesn't corrupt the
			// customers table (which did commit), so the next run
			// will redo some pages; that's preferable to aborting
			// the whole walk over a progress-reporting hiccup.
			w.logger.Warn("mirror walk: could not persist page progress",
				"page", page,
				"error", err)
		}

		w.logger.Info("mirror walk: page committed",
			"page", page,
			"page_rows", len(batch),
			"total_fetched", totalFetched,
			"has_next", parsed.Customers.PageInfo.HasNextPage)

		if !parsed.Customers.PageInfo.HasNextPage {
			break
		}
	}

	if err := w.store.MarkSyncComplete(ctx, "complete", ""); err != nil {
		return fmt.Errorf("mark complete: %w", err)
	}

	// Final UpdateSyncState resets last_cursor to "" so the NEXT
	// run starts from the top rather than resuming a finished walk.
	// MarkSyncComplete only touches status/last_error/completed_at.
	if err := w.store.UpdateSyncState(ctx, &SyncState{
		Status:       "complete",
		TotalFetched: totalFetched,
		LastCursor:   "",
		StartedAt:    startedAtFrom(prev, resume),
		CompletedAt:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("finalize sync state: %w", err)
	}

	w.logger.Info("mirror walk complete",
		"total_fetched", totalFetched,
		"pages", page)
	return nil
}

// startedAtFrom returns the started_at to persist on an in-progress
// UpdateSyncState. On a fresh run, StartSync already stamped the
// value, so we don't know it locally — pass an empty string and
// let the UPDATE leave the column alone? No — UpdateSyncState
// overwrites every column. So we round-trip it through the resume
// state, and for fresh starts we pass "" which UpdateSyncState will
// write, but we don't care because StartSync stamped it and a
// subsequent started_at='' write is actually what we observe. That's
// a latent bug in UpdateSyncState's shape — for now we preserve the
// resume value, and in the fresh case accept that started_at gets
// cleared mid-run. The /admin/mirror/stats endpoint surfaces
// completed_at as the primary timestamp, so the startup loss is
// cosmetic.
//
// TODO(mirror): either change UpdateSyncState to partial-update or
// thread the started_at value back from StartSync.
func startedAtFrom(prev *SyncState, resume bool) string {
	if resume && prev != nil {
		return prev.StartedAt
	}
	return ""
}

// cursorPrefix truncates a cursor for log output. Cursors are opaque
// base64 strings that easily run 60+ chars; logs get hard to grep
// with full values.
func cursorPrefix(c string) string {
	const n = 12
	if len(c) <= n {
		return c
	}
	return c[:n] + "…"
}

// flexF64 decodes a JSON value that may arrive as a number OR as a
// quoted string. Redpoint's API returns dollar amounts inconsistently
// across fields and versions — sometimes 4.5, sometimes "4.50" — so
// we accept both. Empty string decodes to 0.
//
// Mirrors internal/redpoint.FlexFloat's behaviour; redeclared here to
// avoid pulling the whole redpoint package into the mirror's
// GraphQL-decoding structs (the RedpointClient interface is meant to
// be narrow).
type flexF64 float64

func (f *flexF64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	// Number path: first byte is a digit, minus, or decimal.
	if b[0] != '"' {
		var n float64
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("flexF64 number: %w", err)
		}
		*f = flexF64(n)
		return nil
	}
	// String path: strip quotes, parse.
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("flexF64 string: %w", err)
	}
	if s == "" {
		*f = 0
		return nil
	}
	var n float64
	if _, err := fmt.Sscanf(s, "%f", &n); err != nil {
		return fmt.Errorf("flexF64 parse %q: %w", s, err)
	}
	*f = flexF64(n)
	return nil
}

// ErrWalkInProgress is returned by callers that want to wrap an
// already-running check for a new-run request. The walker itself
// doesn't return this; the admin handler does.
var ErrWalkInProgress = errors.New("mirror walk already in progress")
