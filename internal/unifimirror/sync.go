// Package unifimirror hydrates a local mirror of the UA-Hub user
// directory into SQLite (audit.db / ua_users). Parallels the Redpoint
// mirror owned by internal/cache + internal/mirror.
//
// Why a mirror: the UA-Hub ListUsers API paginates sequentially with a
// 10s per-page HTTP timeout; at LEF's directory size (~1600 users)
// that's 17 pages = up to ~3 minutes when UA-Hub is slow. Every code
// path that used to walk the upstream directly (handleFragUnmatchedList,
// the ingest matcher, the denied-tap recheck) paid that cost on its own
// schedule. With a nightly mirror plus ingest-path side-effects keeping
// it fresh between runs, those code paths can answer from SQLite.
//
// Scope: the mirror is advisory state. It does NOT drive door
// decisions, Redpoint writebacks, or UA-Hub UpdateUser calls — those
// all still go against the live client. If the mirror is stale
// (e.g. UA-Hub has been down for a day) the only consequence is that
// the Needs Match page shows yesterday's cached identity and a new
// UA-Hub-side-only user won't appear there until the next sync.
//
// Cadence: the daily sync fires once per SyncConfig.Interval, with an
// initial refresh on boot so a fresh install populates immediately.
// Manual refreshes via POST /ua-hub/sync take the same Refresh path.
package unifimirror

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// SyncConfig governs the daily UA-Hub directory refresh cadence.
// Mirrors cache.SyncConfig so the two syncers look alike at the call
// site in cmd/bridge.
type SyncConfig struct {
	// Interval between full refreshes. The bridge normally uses the
	// same daily cadence as the Redpoint directory sync so both
	// mirrors tend to be consistent as of the same wall-clock hour.
	Interval time.Duration
}

// unifiClient is the narrow subset of unifi.Client the mirror calls.
// Defining it as an interface here (rather than reaching into the
// concrete type) keeps the unit test able to inject a fake upstream
// without bringing up a real UA-Hub server.
type unifiClient interface {
	ListAllUsersWithStatus(ctx context.Context) ([]unifi.UniFiUser, error)
}

// Syncer writes unifi.UniFiUser rows to store.ua_users on a daily
// tick. Not safe for concurrent Refresh calls — the store itself
// serializes writes via Store.mu, but the sync loop is expected to
// be the single producer.
type Syncer struct {
	unifi  unifiClient
	store  *store.Store
	config SyncConfig
	logger *slog.Logger
}

// New constructs a Syncer. Callers plug it into bg.Group via Run.
func New(u unifiClient, s *store.Store, cfg SyncConfig, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		// Defensive: a zero-value Config{} would spin the ticker
		// loop hot; pick a safe daily default so a caller that
		// forgets to set Interval doesn't cripple the host.
		cfg.Interval = 24 * time.Hour
	}
	return &Syncer{
		unifi:  u,
		store:  s,
		config: cfg,
		logger: logger,
	}
}

// Run performs an initial Refresh and then enters the periodic sync
// loop, blocking until ctx is cancelled. Returns ctx.Err() on
// cancellation. Designed to be passed directly to bg.Group.Go so the
// lifetime is supervised.
//
// An initial-refresh error is logged and swallowed — the periodic
// loop should still start so a transient UA-Hub blip at boot doesn't
// wedge the mirror until the next restart.
func (s *Syncer) Run(ctx context.Context) error {
	s.logger.Info("running initial UA-Hub directory mirror refresh...")
	if err := s.Refresh(ctx); err != nil {
		s.logger.Error("initial UA-Hub mirror refresh failed (will retry on schedule)",
			"error", err)
	}

	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.Refresh(ctx); err != nil {
				s.logger.Error("scheduled UA-Hub mirror refresh failed",
					"error", err)
			}
		}
	}
}

// Refresh fetches the full UA-Hub user directory and upserts each
// user into ua_users. Returns the set of rows observed; the caller
// (handler or scheduler) can feed that into its result payload.
//
// We use ListAllUsersWithStatus rather than ListUsers so the mirror
// captures users without NFC credentials too. That matters for the
// Needs Match page — a UA-Hub user can be pending while still not
// having a tag enrolled (the operator might pre-create a member
// account before handing them a wristband), and we want the mirror
// to answer for them as well.
//
// This function does NOT delete mirror rows for UA-Hub users that
// have disappeared since the last sync. A missing upstream user is
// usually a short-term blip (UA-Hub paging inconsistency under
// load); paying the deletion bookkeeping just to see rows re-appear
// a few hours later is worse than keeping a slightly stale row.
// We revisit this if staff ever ask for a "prune stale UA-Hub users"
// button.
func (s *Syncer) Refresh(ctx context.Context) error {
	started := time.Now()
	users, err := s.unifi.ListAllUsersWithStatus(ctx)
	if err != nil {
		return fmt.Errorf("ListAllUsersWithStatus: %w", err)
	}

	inserted := 0
	for _, u := range users {
		row := &store.UAUser{
			ID:        u.ID,
			FirstName: u.FirstName,
			LastName:  u.LastName,
			Name:      u.Name,
			Email:     u.Email,
			Status:    u.Status,
		}
		if err := s.store.UpsertUAUser(ctx, row, u.NfcTokens); err != nil {
			// Log and keep going — a single bad row shouldn't
			// abort the whole refresh. The next sync will try
			// this user again.
			s.logger.Warn("UA-Hub mirror upsert failed",
				"uaUserId", u.ID, "error", err)
			continue
		}
		inserted++
	}

	total, _ := s.store.UAUserCount(ctx)
	s.logger.Info("UA-Hub directory mirror refresh complete",
		"observed", len(users),
		"upserted", inserted,
		"mirrorTotal", total,
		"duration", time.Since(started).Round(time.Millisecond))
	return nil
}

// Stats is the summary a handler can show the operator after a manual
// refresh. Kept flat so the SyncStat list renders cleanly.
type Stats struct {
	Observed    int
	Upserted    int
	MirrorTotal int
	Duration    time.Duration
}

// RefreshWithStats runs a Refresh and returns structured results for
// the staff UI. Errors are propagated as-is; the Stats value is
// populated on success.
func (s *Syncer) RefreshWithStats(ctx context.Context) (Stats, error) {
	started := time.Now()
	users, err := s.unifi.ListAllUsersWithStatus(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("ListAllUsersWithStatus: %w", err)
	}
	upserted := 0
	for _, u := range users {
		row := &store.UAUser{
			ID:        u.ID,
			FirstName: u.FirstName,
			LastName:  u.LastName,
			Name:      u.Name,
			Email:     u.Email,
			Status:    u.Status,
		}
		if err := s.store.UpsertUAUser(ctx, row, u.NfcTokens); err != nil {
			s.logger.Warn("UA-Hub mirror upsert failed",
				"uaUserId", u.ID, "error", err)
			continue
		}
		upserted++
	}
	total, _ := s.store.UAUserCount(ctx)
	return Stats{
		Observed:    len(users),
		Upserted:    upserted,
		MirrorTotal: total,
		Duration:    time.Since(started).Round(100 * time.Millisecond),
	}, nil
}
