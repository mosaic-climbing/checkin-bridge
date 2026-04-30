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

// Start launches the background sync loops. Call once at startup.
// Deprecated: Use Run instead, which combines initial refresh and periodic loop in a single goroutine.
func (s *Syncer) Start(ctx context.Context) {
	// Run an initial status refresh immediately (does NOT re-fetch all customers)
	go func() {
		s.logger.Info("running initial membership status refresh...")
		if err := s.RefreshAllStatuses(ctx); err != nil {
			s.logger.Error("initial status refresh failed (will retry on schedule)", "error", err)
		}
	}()

	// Periodic status refresh (default: every 24 hours)
	go s.syncLoop(ctx)
}

// Run performs an initial membership status refresh and then enters the periodic
// sync loop. It blocks until ctx is cancelled. Returns ctx.Err() when cancelled.
// This is the preferred way to launch the cache syncer in a supervised group.
//
// Each refresh — the initial one and every scheduled tick — is bracketed by
// a jobs.Track call so the row lands in the jobs table. Without this, the
// /ui/sync page's "Last run" pill silently dropped every scheduled run; only
// manual triggers via POST /cache/sync (which bracket via the api package)
// were visible to operators. See internal/jobs for the lifecycle story.
func (s *Syncer) Run(ctx context.Context) error {
	// Run an initial status refresh immediately
	s.logger.Info("running initial membership status refresh...")
	if err := s.trackedRefresh(ctx); err != nil {
		s.logger.Error("initial status refresh failed (will retry on schedule)", "error", err)
	}

	// Periodic status refresh (default: every 24 hours)
	s.syncLoop(ctx)
	return ctx.Err()
}

// trackedRefresh wraps RefreshAllStatuses in a jobs.Track bracket and
// captures cache stats + duration into the result row, matching the
// shape that the manual /cache/sync handler writes. Used by Run and
// the periodic syncLoop tick.
func (s *Syncer) trackedRefresh(ctx context.Context) error {
	return jobs.Track(ctx, s.store, s.logger, jobs.TypeCacheSync,
		func(ctx context.Context) (any, error) {
			started := time.Now()
			if err := s.RefreshAllStatuses(ctx); err != nil {
				return nil, err
			}
			duration := time.Since(started).Round(100 * time.Millisecond)
			var stats *store.MemberStats
			if s.store != nil {
				stats, _ = s.store.MemberStats(ctx)
			}
			return map[string]any{
				"cache":    stats,
				"duration": duration.String(),
			}, nil
		})
}

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

func (s *Syncer) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(s.config.SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.trackedRefresh(ctx); err != nil {
				s.logger.Error("scheduled status refresh failed", "error", err)
			}
		}
	}
}
