package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/jobs"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// SyncConfig controls the daily membership sync.
type SyncConfig struct {
	// SyncInterval is how often to do a full cache refresh from Redpoint.
	SyncInterval time.Duration
	// PageSize for paginating through Redpoint customers (legacy, unused).
	PageSize int
	// InitialDelay is how long to wait before the first refresh fires.
	// Set per-syncer in cmd/bridge to stagger boot-time fires across
	// the four schedulers that share Sync.Interval, so they don't all
	// hit Redpoint and UA-Hub simultaneously. Zero = run immediately.
	InitialDelay time.Duration
	// MaxFailedFraction is the share of cached customers a refresh may
	// fail to reach and still count as a successful (degraded) run.
	// Zero means DefaultMaxFailedFraction. Failures above it — or a
	// walk with no successes at all — fail the job so an outage shows
	// red on /ui/sync and jobs.Loop backs off.
	//
	// Why tolerate any: the walk is ~1,200 single-customer lookups.
	// A handful tripping Redpoint's rate limiter is not an outage, and
	// treating it as one made jobs.Loop re-run the entire walk within
	// minutes, which tripped the limiter again — a self-sustaining
	// storm (232 refreshes in 9 days, Sep 2026) that also starved the
	// analytics service sharing the API key. Unreached customers keep
	// their cached status and are retried on the next scheduled tick.
	MaxFailedFraction float64
}

// DefaultMaxFailedFraction tolerates 5% unreachable customers per
// refresh — well above the 0.2–0.6% seen during rate-limit blips and
// well below anything that looks like an upstream outage.
const DefaultMaxFailedFraction = 0.05

// Syncer periodically refreshes the local membership cache from Redpoint.
type Syncer struct {
	store    *store.Store
	redpoint *redpoint.Client
	config   SyncConfig
	logger   *slog.Logger
}

func NewSyncer(s *store.Store, rp *redpoint.Client, cfg SyncConfig, logger *slog.Logger) *Syncer {
	return &Syncer{
		store:    s,
		redpoint: rp,
		config:   cfg,
		logger:   logger,
	}
}

// Run performs an initial membership status refresh after the
// InitialDelay stagger, then ticks every SyncInterval (with jitter and
// exponential backoff on consecutive failures). Blocks until ctx is
// cancelled. Returns ctx.Err() when cancelled. This is the preferred
// way to launch the cache syncer in a supervised group.
//
// Each refresh — the initial one and every scheduled tick — is bracketed
// by a jobs.Track call so the row lands in the jobs table. Without this,
// the /ui/sync page's "Last run" pill silently dropped every scheduled
// run; only manual triggers via POST /cache/sync (which bracket via the
// api package) were visible to operators. See internal/jobs for the
// lifecycle story and the scheduler-hardening rationale.
func (s *Syncer) Run(ctx context.Context) error {
	return jobs.Loop(ctx, jobs.LoopConfig{
		Interval:     s.config.SyncInterval,
		InitialDelay: s.config.InitialDelay,
		Jitter:       defaultJitter,
		BackoffStart: defaultBackoffStart,
		BackoffMax:   defaultBackoffMax,
	}, s.store, s.logger, jobs.TypeCacheSync, s.refreshFn)
}

// refreshFn is the inner body each scheduler tick wraps. Pulled out as
// a method so jobs.Loop can call it directly without an anonymous
// closure that re-allocates per tick.
func (s *Syncer) refreshFn(ctx context.Context) (any, error) {
	started := time.Now()
	if err := s.RefreshAllStatuses(ctx); err != nil {
		return nil, err
	}
	duration := time.Since(started).Round(100 * time.Millisecond)
	var stats *store.MemberStats
	if s.store != nil {
		var err error
		stats, err = s.store.MemberStats(ctx)
		if err != nil {
			// Don't fail the job — the refresh itself succeeded; we
			// just can't render the post-run stats pill. Log so the
			// gap on /ui/sync is explainable rather than silent.
			s.logger.Warn("MemberStats failed; job result will omit cache counts", "error", err)
		}
	}
	return map[string]any{
		"cache":    stats,
		"duration": duration.String(),
	}, nil
}

// Hardening defaults applied uniformly to all schedulers backed by
// jobs.Loop. The values are not yet exposed as config — operators have
// no observed need to tune them per-deployment. Exposing them later is
// a one-liner if that changes.
const (
	defaultJitter       = 0.1             // ±10% of Interval per tick
	defaultBackoffStart = 5 * time.Second // first wait after a failure
	defaultBackoffMax   = 1 * time.Hour   // cap on doubled waits — a 24 h job re-running every 5 min was a storm amplifier
)

// RefreshAllStatuses fetches fresh membership status for every member in the
// cache, by their Redpoint customer ID. This does NOT add or remove members —
// it only updates their status (badge, active flag, name, etc.).
//
// Members whose badge goes FROZEN/EXPIRED stay in the cache with updated status
// so they get re-activated automatically if their membership is restored.
func (s *Syncer) RefreshAllStatuses(ctx context.Context) error {
	// One read of the members table instead of per-customer GetMemberByCustomerID
	// inside the loop below. RefreshCustomers gets a deduplicated customer-id
	// list so we don't ask Redpoint about the same customer twice when a
	// customer has multiple NFC cards bound (unusual today but legal in the
	// schema).
	members, err := s.store.AllMembers(ctx)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		s.logger.Warn("cache is empty — nothing to refresh. Run POST /ingest/unifi to populate the cache first.")
		return nil
	}

	customerIDs := make([]string, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if _, dup := seen[m.CustomerID]; dup {
			continue
		}
		seen[m.CustomerID] = struct{}{}
		customerIDs = append(customerIDs, m.CustomerID)
	}

	s.logger.Info("refreshing membership status for all cached members",
		"members", len(members),
		"customers", len(customerIDs),
	)
	start := time.Now()

	outcome, err := s.redpoint.RefreshCustomers(ctx, customerIDs)
	if err != nil {
		return err
	}

	byID := make(map[string]*redpoint.Customer, len(outcome.Customers))
	for _, c := range outcome.Customers {
		byID[c.ID] = c
	}
	// Only IDs Redpoint POSITIVELY answered "no such customer" for may
	// be marked DELETED. Failed lookups mean the customer's state is
	// unknown — a Redpoint outage must never read as a mass deletion
	// (it used to: every member got DELETED and the job stayed green).
	deletedIDs := make(map[string]struct{}, len(outcome.DeletedIDs))
	for _, id := range outcome.DeletedIDs {
		deletedIDs[id] = struct{}{}
	}
	failedIDs := make(map[string]struct{}, len(outcome.FailedIDs))
	for _, id := range outcome.FailedIDs {
		failedIDs[id] = struct{}{}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	updated := 0
	staleCount := 0
	skippedUnknown := 0

	for i := range members {
		existing := &members[i]

		cust, found := byID[existing.CustomerID]
		if !found {
			if _, failed := failedIDs[existing.CustomerID]; failed {
				// No information — keep the member exactly as cached.
				skippedUnknown++
				continue
			}
			if _, deleted := deletedIDs[existing.CustomerID]; !deleted {
				// Not fetched, not failed, not confirmed-deleted: an ID
				// we never asked about (shouldn't happen — the request
				// list is built from this member set). Leave untouched.
				continue
			}
			// Customer confirmed gone from Redpoint — mark inactive but keep in cache
			if existing.Active {
				existing.Active = false
				existing.BadgeStatus = "DELETED"
				existing.CachedAt = now
				if err := s.store.UpsertMember(ctx, existing); err != nil {
					s.logger.Error("failed to upsert member", "customerId", existing.CustomerID, "error", err)
					continue
				}
				staleCount++
				s.logger.Info("customer no longer in Redpoint, marked inactive",
					"name", existing.FirstName+" "+existing.LastName,
					"customerId", existing.CustomerID,
				)
			}
			continue
		}

		badgeStatus := ""
		badgeName := ""
		if cust.Badge != nil {
			badgeStatus = cust.Badge.Status
			if cust.Badge.CustomerBadge != nil {
				badgeName = cust.Badge.CustomerBadge.Name
			}
		}

		// Log status changes
		oldAllowed := existing.Active && existing.BadgeStatus == "ACTIVE"
		newAllowed := cust.Active && badgeStatus == "ACTIVE"
		if oldAllowed != newAllowed {
			s.logger.Info("membership status changed",
				"name", existing.FirstName+" "+existing.LastName,
				"oldStatus", existing.BadgeStatus,
				"newStatus", badgeStatus,
				"oldActive", existing.Active,
				"newActive", cust.Active,
			)
		}

		// Update fields — preserve NFC mapping and last check-in
		existing.FirstName = cust.FirstName
		existing.LastName = cust.LastName
		existing.BadgeStatus = badgeStatus
		existing.BadgeName = badgeName
		existing.Active = cust.Active
		existing.Barcode = cust.Barcode
		existing.CachedAt = now

		if err := s.store.UpsertMember(ctx, existing); err != nil {
			s.logger.Error("failed to upsert member", "customerId", existing.CustomerID, "error", err)
			continue
		}
		updated++
	}

	stats, err := s.store.MemberStats(ctx)
	if err != nil {
		s.logger.Warn("failed to get member stats after refresh", "error", err)
	} else {
		s.logger.Info("status refresh complete",
			"requested", len(customerIDs),
			"updated", updated,
			"stale", staleCount,
			"skippedUnknown", skippedUnknown,
			"cacheTotal", stats.Total,
			"cacheActive", stats.Active,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	}

	// A refresh that could not reach more than MaxFailedFraction of the
	// customers — or reached none — is a FAILED job even though the
	// successful subset was applied above: returning an error makes
	// jobs.Loop back off and turns the sync page's last-run pill red,
	// so an outage is visible instead of silently green with a stale
	// cache. Below that line it is a DEGRADED success: logged loudly,
	// unreached statuses left untouched, retried next tick. See
	// SyncConfig.MaxFailedFraction for why the tolerance exists.
	if failed := len(outcome.FailedIDs); failed > 0 {
		reached := len(outcome.Customers) + len(outcome.DeletedIDs)
		frac := float64(failed) / float64(len(customerIDs))
		if reached == 0 || frac > s.maxFailedFraction() {
			return fmt.Errorf("refresh incomplete: %d of %d customers unreachable (statuses left untouched)",
				failed, len(customerIDs))
		}
		s.logger.Warn("refresh degraded: some customers unreachable, statuses left untouched until next tick",
			"failed", failed, "requested", len(customerIDs), "tolerance", s.maxFailedFraction())
	}
	return nil
}

func (s *Syncer) maxFailedFraction() float64 {
	if s.config.MaxFailedFraction > 0 {
		return s.config.MaxFailedFraction
	}
	return DefaultMaxFailedFraction
}
