package cache

import (
	"context"
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
}

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
	defaultBackoffMax   = 5 * time.Minute // cap on doubled waits
)

// RefreshAllStatuses fetches fresh membership status for every member in the
// cache, by their Redpoint customer ID. This does NOT add or remove members —
// it only updates their status (badge, active flag, name, etc.).
//
// Members whose badge goes FROZEN/EXPIRED stay in the cache with updated status
// so they get re-activated automatically if their membership is restored.
func (s *Syncer) RefreshAllStatuses(ctx context.Context) error {
	customerIDs, err := s.store.AllMemberCustomerIDs(ctx)
	if err != nil {
		return err
	}

	if len(customerIDs) == 0 {
		s.logger.Warn("cache is empty — nothing to refresh. Run POST /ingest/unifi to populate the cache first.")
		return nil
	}

	s.logger.Info("refreshing membership status for all cached members", "count", len(customerIDs))
	start := time.Now()

	refreshed, err := s.redpoint.RefreshCustomers(ctx, customerIDs)
	if err != nil {
		return err
	}

	// Build a map for quick lookup
	byID := make(map[string]*redpoint.Customer, len(refreshed))
	for _, c := range refreshed {
		byID[c.ID] = c
	}

	now := time.Now().UTC().Format(time.RFC3339)
	updated := 0
	staleCount := 0

	for _, id := range customerIDs {
		existing, err := s.store.GetMemberByCustomerID(ctx, id)
		if err != nil {
			s.logger.Error("failed to get member by customer ID", "customerId", id, "error", err)
			continue
		}
		if existing == nil {
			continue
		}

		cust, found := byID[id]
		if !found {
			// Customer no longer exists in Redpoint — mark inactive but keep in cache
			if existing.Active {
				existing.Active = false
				existing.BadgeStatus = "DELETED"
				existing.CachedAt = now
				if err := s.store.UpsertMember(ctx, existing); err != nil {
					s.logger.Error("failed to upsert member", "customerId", id, "error", err)
				}
				staleCount++
				s.logger.Info("customer no longer in Redpoint, marked inactive",
					"name", existing.FirstName+" "+existing.LastName,
					"customerId", id,
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
			s.logger.Error("failed to upsert member", "customerId", id, "error", err)
			continue
		}
		updated++
	}

	stats, err := s.store.MemberStats(ctx)
	if err != nil {
		s.logger.Error("failed to get member stats", "error", err)
	} else {
		s.logger.Info("status refresh complete",
			"requested", len(customerIDs),
			"updated", updated,
			"stale", staleCount,
			"cacheTotal", stats.Total,
			"cacheActive", stats.Active,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	}

	return nil
}

