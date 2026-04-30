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

	"github.com/mosaic-climbing/checkin-bridge/internal/jobs"
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
	// InitialDelay is how long to wait before the first refresh fires.
	// Used by cmd/bridge to stagger boot-time fires across the four
	// schedulers that share Sync.Interval. Zero = run immediately.
	InitialDelay time.Duration
}

// unifiClient is the narrow subset of unifi.Client the mirror calls.
// Defining it as an interface here (rather than reaching into the
// concrete type) keeps the unit test able to inject a fake upstream
// without bringing up a real UA-Hub server.
//
// FetchUser is a per-user hydration call. The paginated
// ListAllUsersWithStatus endpoint at LEF returns a payload that omits
// email for the vast majority of users (1613 of 1618 observed) — a
// shape-only quirk of UA-Hub's list endpoint. The per-user GET
// /users/{id} returns the full record including email, so the mirror
// uses it to backfill blank-email rows after the initial walk.
type unifiClient interface {
	ListAllUsersWithStatus(ctx context.Context) ([]unifi.UniFiUser, error)
	FetchUser(ctx context.Context, userID string) (*unifi.UniFiUser, error)
}

// hydrateInterval is the pause between per-user FetchUser calls during
// the email backfill pass. Small enough to finish in minutes for ~1.6k
// users, large enough not to hammer UA-Hub. Exposed as a package var
// so tests can drop it to zero.
var hydrateInterval = 75 * time.Millisecond

// hydrateProgressEvery throttles how often the hydrate loop writes a
// jobs.progress update. ~25 rows × 75ms ≈ a refresh every ~2s, which
// is a reasonable upper bound on how fast staff want the pill to
// twitch (they're glancing at it once every few seconds). Exposed
// as a package var so unit tests can drop it to 1 and observe every
// emit.
var hydrateProgressEvery = 25

// ProgressFunc is the optional phase reporter the Syncer calls during
// a Refresh. Each call carries a short human-readable phase label
// ("listing users", "hydrating 450/1568", "reconciling pending") that
// the api package writes into jobs.progress so the staff /ui/sync page
// can render mid-flight progress in the per-card "Last run" pill.
//
// The reporter is best-effort and runs synchronously on the refresh
// goroutine; implementations MUST NOT block (the cmd/bridge wiring
// just calls Store.UpdateJobProgress, which is a single indexed
// SQLite UPDATE under the store mutex). A nil reporter is supported
// — the package-internal helper short-circuits to a no-op so callers
// that don't care about progress (the nightly ticker, the test
// fixtures) need no setup.
type ProgressFunc func(phase string)

// progressKey is the unexported context key used by WithProgress and
// the package-internal reportProgress helper. Defined at package
// scope so tests in the same package can assert against it without
// reflection.
type progressKey struct{}

// WithProgress returns a derived context that carries fn as the
// progress reporter for any Syncer.Refresh / RefreshWithStats call
// dispatched with it. Passing a nil fn is a no-op (returns ctx
// unchanged), matching the rest of the Syncer surface where progress
// reporting is strictly optional.
//
// The cmd/bridge wiring calls this in the api.UAHubRefresher closure
// to inject a per-job progress writer that updates the jobs.progress
// row keyed on the in-flight jobID. See cmd/bridge/main.go (search
// for SetUAHubMirrorRefresher) and internal/api/sync_ux.go's
// detached-ctx pattern for the lifetime story.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// reportProgress is the package-internal accessor. Lives here (not at
// the call sites) so a future refactor that wants to add structured
// fields ("phase", "current", "total") to the reporter signature
// only has to touch this one helper, not every emit site.
func reportProgress(ctx context.Context, phase string) {
	if ctx == nil {
		return
	}
	if fn, ok := ctx.Value(progressKey{}).(ProgressFunc); ok && fn != nil {
		fn(phase)
	}
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

// Run performs an initial Refresh after InitialDelay and then ticks
// every Interval (with jitter and exponential backoff on consecutive
// failures), blocking until ctx is cancelled. Returns ctx.Err() on
// cancellation. Designed to be passed directly to bg.Group.Go so the
// lifetime is supervised.
//
// Each refresh — initial or scheduled — is bracketed by jobs.Track so
// the staff /ui/sync page sees the row. Errors are logged and the loop
// continues so a transient UA-Hub blip doesn't wedge the mirror.
func (s *Syncer) Run(ctx context.Context) error {
	return jobs.Loop(ctx, jobs.LoopConfig{
		Interval:     s.config.Interval,
		InitialDelay: s.config.InitialDelay,
		Jitter:       defaultJitter,
		BackoffStart: defaultBackoffStart,
		BackoffMax:   defaultBackoffMax,
	}, s.store, s.logger, jobs.TypeUAHubSync, s.refreshFn)
}

// refreshFn is the inner body each scheduler tick wraps. The captured
// result mirrors the shape internal/api/server.go's handleUAHubSync
// writes so manual and scheduled rows interleave cleanly in the
// Recent Jobs list.
func (s *Syncer) refreshFn(ctx context.Context) (any, error) {
	stats, err := s.RefreshWithStats(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"observed":    stats.Observed,
		"upserted":    stats.Upserted,
		"hydrated":    stats.Hydrated,
		"rechecked":   stats.Rechecked,
		"mirrorTotal": stats.MirrorTotal,
		"duration":    stats.Duration.String(),
	}, nil
}

// Hardening defaults applied to the unifimirror loop. Match the
// cache.Syncer values so all four Sync.Interval-bound schedulers
// behave identically under upstream pressure.
const (
	defaultJitter       = 0.1
	defaultBackoffStart = 5 * time.Second
	defaultBackoffMax   = 5 * time.Minute
)

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
	_, err := s.RefreshWithStats(ctx)
	return err
}

// upsertAndHydrate writes the initial mirror rows, then hydrates emails
// per-user for rows that came back from ListAllUsersWithStatus with a
// blank email. Returns (upserted, hydrated, listHadEmail).
//
// Two-phase structure keeps the common case fast: the first pass stores
// everything we got, the second pass only hits UA-Hub for the subset
// that the list endpoint shortchanged. Errors from FetchUser are logged
// and the original row is left in place — a per-user network blip
// should not abort the whole refresh, and the next sync will try again.
//
// Progress reporting (v0.5.7.1): emits "upserting list rows" before
// the first pass and "hydrating N/M" once per hydrateProgressEvery
// users during the slow per-user pass so the staff /ui/sync pill
// shows mid-flight progress instead of a static "Running ⟳". The
// per-row cadence keeps the SQLite UPDATE for jobs.progress to a
// few dozen writes per refresh — same order as the slog Info we
// already emit per phase.
func (s *Syncer) upsertAndHydrate(ctx context.Context, users []unifi.UniFiUser) (upserted, hydrated, listEmails int) {
	reportProgress(ctx, fmt.Sprintf("upserting %d list rows", len(users)))
	var needHydrate []string
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
		upserted++
		if u.Email != "" {
			listEmails++
			continue
		}
		if u.ID == "" {
			continue
		}
		needHydrate = append(needHydrate, u.ID)
	}

	if len(needHydrate) == 0 {
		return upserted, 0, listEmails
	}
	s.logger.Info("UA-Hub mirror hydrating blank-email rows",
		"toHydrate", len(needHydrate),
		"listEmails", listEmails,
		"observed", len(users))

	for i, id := range needHydrate {
		if err := ctx.Err(); err != nil {
			s.logger.Info("UA-Hub mirror hydrate cancelled",
				"done", i, "remaining", len(needHydrate)-i, "error", err)
			return upserted, hydrated, listEmails
		}
		// Emit progress on the first iteration and then every
		// hydrateProgressEvery rows. The modulo check keeps the
		// jobs.progress write rate sane on large refreshes (at
		// LEF: len(needHydrate)≈1500 / every=25 → ~60 writes
		// spread across ~2 minutes).
		if i == 0 || i%hydrateProgressEvery == 0 {
			reportProgress(ctx, fmt.Sprintf("hydrating %d/%d", i+1, len(needHydrate)))
		}
		u, err := s.unifi.FetchUser(ctx, id)
		if err != nil {
			s.logger.Warn("UA-Hub FetchUser failed (leaving list row in place)",
				"uaUserId", id, "error", err)
		} else if u != nil && u.Email != "" {
			row := &store.UAUser{
				ID:        u.ID,
				FirstName: u.FirstName,
				LastName:  u.LastName,
				Name:      u.Name,
				Email:     u.Email,
				Status:    u.Status,
			}
			if err := s.store.UpsertUAUser(ctx, row, u.NfcTokens); err != nil {
				s.logger.Warn("UA-Hub mirror hydrate upsert failed",
					"uaUserId", id, "error", err)
			} else {
				hydrated++
			}
		}
		// Pause between calls so we don't pin UA-Hub's CPU.
		if hydrateInterval > 0 && i+1 < len(needHydrate) {
			select {
			case <-ctx.Done():
				return upserted, hydrated, listEmails
			case <-time.After(hydrateInterval):
			}
		}
	}
	return upserted, hydrated, listEmails
}

// Stats is the summary a handler can show the operator after a manual
// refresh. Kept flat so the SyncStat list renders cleanly.
//
// Hydrated and Rechecked track work that happens as side-effects of
// the refresh: Hydrated is the number of mirror rows backfilled via
// per-user FetchUser because the paginated list omitted their email,
// Rechecked is the number of pending match rows promoted to a
// confirmed mapping after a hydrated email landed a single Redpoint
// customer.
type Stats struct {
	Observed    int
	Upserted    int
	Hydrated    int
	Rechecked   int
	MirrorTotal int
	Duration    time.Duration
}

// RefreshWithStats runs a Refresh and returns structured results for
// the staff UI. Errors are propagated as-is; the Stats value is
// populated on success.
//
// Phase progress is emitted via reportProgress (see ProgressFunc /
// WithProgress in this package) at four boundaries: just before the
// paginated UA-Hub list call, when the upsert+hydrate pass begins
// and every 25 hydrates thereafter, just before the pending recheck,
// and on completion. Consumers that don't install a progress
// reporter see zero overhead.
func (s *Syncer) RefreshWithStats(ctx context.Context) (Stats, error) {
	started := time.Now()
	reportProgress(ctx, "listing UA-Hub users")
	users, err := s.unifi.ListAllUsersWithStatus(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("ListAllUsersWithStatus: %w", err)
	}

	upserted, hydrated, listEmails := s.upsertAndHydrate(ctx, users)

	// Run recheck unconditionally.
	//
	// v0.5.5 gated this on `hydrated > 0` under the assumption that
	// new emails only ever arrive via the per-user FetchUser hydrate
	// pass. That assumption broke after the v0.5.6 parser fix: once
	// parseUniFiUser started reading `user_email`, the LIST endpoint
	// began returning emails for some users directly (listEmails went
	// from 5 → 52 at LEF on the first refresh after deploy), bypassing
	// the hydrated counter and leaving those rows' pending records
	// stuck even though they had become single-hit resolvable.
	//
	// recheckPending is cheap: one indexed SELECT joining ua_users +
	// cache.customers on email, plus an UpsertMapping + DeletePending
	// for each of at most a few hundred rows. It is idempotent — if
	// there's nothing new to promote the query returns zero rows and
	// the pass is a no-op. Running it every refresh is strictly safer
	// than trying to predict which code path delivered an email.
	reportProgress(ctx, "reconciling pending mappings")
	rechecked, err := s.recheckPending(ctx)
	if err != nil {
		s.logger.Warn("pending-mapping recheck failed (continuing)",
			"error", err)
	}

	total, _ := s.store.UAUserCount(ctx)
	s.logger.Info("UA-Hub directory mirror refresh complete",
		"observed", len(users),
		"upserted", upserted,
		"listEmails", listEmails,
		"hydrated", hydrated,
		"rechecked", rechecked,
		"mirrorTotal", total,
		"duration", time.Since(started).Round(time.Millisecond))

	return Stats{
		Observed:    len(users),
		Upserted:    upserted,
		Hydrated:    hydrated,
		Rechecked:   rechecked,
		MirrorTotal: total,
		Duration:    time.Since(started).Round(100 * time.Millisecond),
	}, nil
}
