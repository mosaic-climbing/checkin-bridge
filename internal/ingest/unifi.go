// Package ingest handles the import of NFC tag → member mappings
// from UniFi Access into the local membership cache.
//
// The process:
//  1. Fetch all users from UniFi Access that have NFC credentials
//  2. For each user, search the local SQLite customer directory by name
//     (or by email if available)
//  3. Match UniFi users to Redpoint customers
//  4. Fetch fresh membership status from Redpoint for matched customers
//  5. Build cache entries: NFC tag UID → Redpoint customer + status
//
// This is designed to run as a one-shot import with a dry-run/review mode
// so staff can verify the mappings before committing them.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// MatchMethod describes how a UniFi user was matched to a Redpoint customer.
type MatchMethod string

const (
	MatchByMapping MatchMethod = "mapping" // resolved from ua_user_mappings (manual or prior auto-match)
	MatchByEmail   MatchMethod = "email"
	MatchByName    MatchMethod = "name"
	MatchManual    MatchMethod = "manual"
	MatchNone      MatchMethod = "unmatched"
)

// UserMapping represents one UniFi user matched (or not) to a Redpoint customer.
type UserMapping struct {
	// UniFi side
	UniFiUserID string   `json:"unifiUserId"`
	UniFiName   string   `json:"unifiName"`
	UniFiEmail  string   `json:"unifiEmail"`
	NfcTokens   []string `json:"nfcTokens"`

	// Redpoint side (populated if matched)
	RedpointID    string `json:"redpointId,omitempty"`
	RedpointName  string `json:"redpointName,omitempty"`
	RedpointEmail string `json:"redpointEmail,omitempty"`
	BadgeStatus   string `json:"badgeStatus,omitempty"`
	BadgeName     string `json:"badgeName,omitempty"`
	Active        bool   `json:"active,omitempty"`

	// Match info
	Method  MatchMethod `json:"matchMethod"`
	Warning string      `json:"warning,omitempty"`
}

// IngestResult contains the results of a UniFi → Redpoint mapping operation.
type IngestResult struct {
	Timestamp  string         `json:"timestamp"`
	UniFiUsers int            `json:"unifiUsersTotal"`
	WithNFC    int            `json:"unifiUsersWithNfc"`
	Matched    int            `json:"matched"`
	Unmatched  int            `json:"unmatched"`
	Skipped    int            `json:"skipped"`  // matched but badge not ACTIVE
	Applied    int            `json:"applied"`   // written to cache (0 if dry run)
	DryRun     bool           `json:"dryRun"`
	Mappings   []*UserMapping `json:"mappings"`
}

// Ingester handles the UniFi → Redpoint user mapping process.
//
// Note: prior versions held a *unifi.Client for the live ListUsers
// walk that Step 1 used to drive. That field was removed when Step 1
// switched to reading the local ua_users mirror — the ingester is
// now a pure store + redpoint consumer. Keep it that way; if a future
// caller needs UA-Hub live access, it should reach for the live client
// directly rather than re-coupling the ingester to upstream uptime.
type Ingester struct {
	redpoint *redpoint.Client
	store    *store.Store
	logger   *slog.Logger
}

func NewIngester(
	redpointClient *redpoint.Client,
	db *store.Store,
	logger *slog.Logger,
) *Ingester {
	return &Ingester{
		redpoint: redpointClient,
		store:    db,
		logger:   logger,
	}
}

// Run performs the full ingest pipeline:
//  1. Read UA-Hub users with NFC credentials from the local ua_users mirror
//     (refreshed on every ua-hub-mirror tick).
//  2. Match each against the local SQLite customer directory (by email, then name)
//  3. For matched customers, fetch fresh status from Redpoint
//  4. If dryRun=false, write to cache
//
// Step 1 used to call unifi.Client.ListUsers (live, paginated) which
// took ~3 minutes at LEF and was the direct cause of the wedged-ingest
// behaviour operators kept hitting in the field — a flaky UA-Hub
// session left the HTTP client stuck on a page that would never
// return, and the whole ingest goroutine wedged with no progress
// logging. Reading from the mirror is one indexed SQLite query and
// inherits no upstream uptime risk; the mirror is refreshed on every
// ua-hub-mirror tick (cfg.Sync.Interval) so the data is at most one
// sync cycle stale, which is the same staleness window every other
// scheduled job already operates on.
func (ing *Ingester) Run(ctx context.Context, dryRun bool) (*IngestResult, error) {
	start := time.Now()
	result := &IngestResult{
		Timestamp: start.UTC().Format(time.RFC3339),
		DryRun:    dryRun,
	}

	// Check that the customer store is populated
	count, err := ing.store.CustomerCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("check customer store: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("customer store is empty — run POST /directory/sync first to load Redpoint customers")
	}
	ing.logger.Info("customer store ready", "customers", count)

	// ── Step 1: Read UA-Hub users from the local mirror ──────
	ing.logger.Info("step 1: reading UA-Hub users from local mirror")
	uaUsers, err := ing.store.AllUAUsersWithNFC(ctx)
	if err != nil {
		return nil, fmt.Errorf("read UA-Hub mirror: %w", err)
	}
	if len(uaUsers) == 0 {
		// Mirror empty almost always means a fresh deploy where
		// ua-hub-mirror hasn't completed its first refresh yet.
		// Distinct from "UA-Hub returned zero users" because the
		// mirror would've persisted that as zero rows after a real
		// successful refresh — we'd see the customers store empty
		// the same way.
		return nil, fmt.Errorf("UA-Hub mirror is empty — wait for the next ua-hub-mirror tick or POST /ua-hub/sync to populate it")
	}
	result.UniFiUsers = len(uaUsers)
	result.WithNFC = len(uaUsers)

	ing.logger.Info("UA-Hub users with NFC (from mirror)", "count", len(uaUsers))

	// ── Step 2: Match against SQLite directory ───────────────
	ing.logger.Info("step 2: matching UA-Hub users against local customer directory")
	mappings := make([]*UserMapping, 0, len(uaUsers))

	for _, u := range uaUsers {
		tokens := u.NfcTokens()
		m := &UserMapping{
			UniFiUserID: u.ID,
			UniFiName:   u.FullName(),
			UniFiEmail:  u.Email,
			NfcTokens:   tokens,
			Method:      MatchNone,
		}

		// Step 2a: prefer an existing ua_user_mappings row. Staff-confirmed
		// manual matches (and prior auto-matches) live here. Without this
		// the email/name fallbacks below can either (a) re-skip a user
		// who was previously rescued from needs-match because the auto
		// heuristics still don't fire — leaving them visually "Not
		// enrolled" indefinitely — or (b) silently re-bind them to a
		// *different* customer if a typo'd email later collides with
		// someone else's record. Honoring the mapping first fixes both.
		if mp, err := ing.store.GetMapping(ctx, u.ID); err == nil && mp != nil {
			if rec, err := ing.store.GetCustomerByID(ctx, mp.RedpointCustomer); err == nil && rec != nil {
				m.RedpointID = rec.RedpointID
				m.RedpointName = rec.FirstName + " " + rec.LastName
				m.RedpointEmail = rec.Email
				m.Active = rec.Active
				m.Method = MatchByMapping
			}
			// If GetCustomerByID returned nothing the mapping points at a
			// customer the local directory hasn't seen — fall through to
			// the heuristics rather than dropping the user. The next
			// /directory/sync will plug the gap.
		}

		// Try email match first (most reliable)
		if m.Method == MatchNone && u.Email != "" {
			rec, err := ing.store.SearchCustomersByEmail(ctx, u.Email)
			if err == nil && rec != nil {
				m.RedpointID = rec.RedpointID
				m.RedpointName = rec.FirstName + " " + rec.LastName
				m.RedpointEmail = rec.Email
				m.Active = rec.Active
				m.Method = MatchByEmail
			}
		}

		// Fall back to name match
		if m.Method == MatchNone {
			first, last := parseUAUserName(u)
			if first != "" || last != "" {
				records, err := ing.store.SearchCustomersByName(ctx, first, last)
				if err == nil {
					if len(records) == 1 {
						rec := records[0]
						m.RedpointID = rec.RedpointID
						m.RedpointName = rec.FirstName + " " + rec.LastName
						m.RedpointEmail = rec.Email
						m.Active = rec.Active
						m.Method = MatchByName
					} else if len(records) > 1 {
						// Surface the candidate Redpoint IDs (capped) so staff
						// can resolve the ambiguity from logs / the Needs Match
						// panel without re-running the ingest in dry-run.
						const maxCandidates = 5
						ids := make([]string, 0, maxCandidates)
						for i, r := range records {
							if i >= maxCandidates {
								break
							}
							ids = append(ids, r.RedpointID)
						}
						suffix := ""
						if len(records) > maxCandidates {
							suffix = fmt.Sprintf(", +%d more", len(records)-maxCandidates)
						}
						m.Warning = fmt.Sprintf(
							"multiple Redpoint customers match this name (%d) — needs manual mapping; candidates: [%s]%s",
							len(records), strings.Join(ids, ", "), suffix,
						)
					}
				}
			}
		}

		if m.Method == MatchNone && m.Warning == "" {
			m.Warning = "no matching Redpoint customer found"
		}

		mappings = append(mappings, m)
	}

	// ── Step 3: Use SQLite directory data for initial status ─
	// The bulk-loaded directory has active/inactive status already.
	// The daily syncer (RefreshAllStatuses) will fetch live badge status
	// from Redpoint once the cache is populated — no need to hit the
	// rate-limited API during ingest.
	ing.logger.Info("step 3: using directory data for initial status (daily syncer will refresh live status)")

	result.Mappings = mappings

	// Count results
	for _, m := range mappings {
		if m.Method != MatchNone {
			result.Matched++
			if !m.Active {
				result.Skipped++
			}
		} else {
			result.Unmatched++
		}
	}

	// ── Step 4: Apply to store (if not dry run) ──────────────
	if !dryRun {
		ing.logger.Info("step 4: applying all matched mappings to store")
		applied := 0
		for _, m := range mappings {
			if m.Method == MatchNone {
				continue
			}

			// Write ALL matched members to store — including inactive ones
			// so their status gets tracked and they auto-reactivate later.
			for _, token := range m.NfcTokens {
				// Use directory active status; badge details populated by daily syncer
				badgeStatus := "PENDING_SYNC"
				if m.Active {
					badgeStatus = "ACTIVE"
				}
				member := &store.Member{
					NfcUID:      strings.ToUpper(token),
					CustomerID:  m.RedpointID,
					FirstName:   firstName(m.RedpointName),
					LastName:    lastName(m.RedpointName),
					BadgeStatus: badgeStatus,
					BadgeName:   m.BadgeName,
					Active:      m.Active,
					CachedAt:    result.Timestamp,
				}
				if err := ing.store.UpsertMember(ctx, member); err != nil {
					ing.logger.Error("failed to upsert member",
						"customerId", m.RedpointID,
						"nfcUid", member.NfcUID,
						"error", err,
					)
					continue
				}
				applied++
			}
		}
		result.Applied = applied

		ing.logger.Info("ingest applied to store",
			"matched", result.Matched,
			"applied", result.Applied,
			"unmatched", result.Unmatched,
			"skipped", result.Skipped,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	} else {
		ing.logger.Info("dry run complete — no changes applied",
			"matched", result.Matched,
			"unmatched", result.Unmatched,
			"skipped", result.Skipped,
		)
	}

	return result, nil
}

// ─── Helpers ─────────────────────────────────────────────────

func parseUAUserName(u store.UAUser) (first, last string) {
	if u.FirstName != "" || u.LastName != "" {
		return u.FirstName, u.LastName
	}
	if u.Name != "" {
		parts := strings.Fields(u.Name)
		if len(parts) >= 2 {
			return parts[0], parts[len(parts)-1]
		}
		if len(parts) == 1 {
			return parts[0], ""
		}
	}
	return "", ""
}

func firstName(fullName string) string {
	parts := strings.Fields(fullName)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func lastName(fullName string) string {
	parts := strings.Fields(fullName)
	if len(parts) > 1 {
		return strings.Join(parts[1:], " ")
	}
	return ""
}
