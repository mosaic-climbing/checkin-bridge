// Package statusync synchronises membership status between Redpoint HQ and
// UniFi Access.
//
// Architecture:
//
//	Daily sync (e.g. 03:00):
//	  1. Fetch all UniFi Access users (with NFC cards and current status).
//	  2. For each user, look up their membership status in the local cache
//	     (which is refreshed from Redpoint by cache.Syncer).
//	  3. If a member is active in Redpoint but DEACTIVATED in UniFi → reactivate.
//	  4. If a member is expired/frozen in Redpoint but ACTIVE in UniFi → deactivate.
//	  5. UniFi natively enforces access: ACTIVE users can tap in, DEACTIVATED cannot.
//
//	Denied-tap recheck:
//	  When a DEACTIVATED user taps and is denied, the bridge queries Redpoint live.
//	  If they've renewed since the last sync → reactivate in UniFi and unlock the door.
//
// This means if the bridge goes down, UniFi continues enforcing with its last-synced state.
package statusync

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/config"
	"github.com/mosaic-climbing/checkin-bridge/internal/jobs"
	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// SyncResult holds the outcome of a single sync run.
//
// Counters split into three groups:
//
//   - Scalars of the UA-side inventory (UniFiUsers).
//   - Outcome of status-sync decisions against already-bound users
//     (Matched, Activated, Deactivated, Unchanged, Unmatched). These are
//     driven by the legacy NFC-token → store.Member lookup and the new
//     email-based mapping path converges into them as Layer-3c rolls out.
//   - New (C2) bucket-counters for the mapping-population phase
//     (Matching, NewlyMatched, NewlyPending, Expired). These track what
//     the matcher did *this run* and let operators tell "did the sync
//     do anything about unmatched users today?" at a glance.
type SyncResult struct {
	StartedAt   time.Time `json:"startedAt"`
	Duration    string    `json:"duration"`
	UniFiUsers  int       `json:"unifiUsers"`
	Matched     int       `json:"matched"`
	Activated   int       `json:"activated"`
	Deactivated int       `json:"deactivated"`
	Unchanged   int       `json:"unchanged"`
	Unmatched   int       `json:"unmatched"`
	Errors      int       `json:"errors"`
	// Matching is the count of UA users the matcher inspected this run
	// (i.e. unmapped users for which matchOne was invoked). Already-bound
	// users are counted in Matched, not here.
	Matching int `json:"matching,omitempty"`
	// NewlyMatched: matcher produced a binding on its first try.
	NewlyMatched int `json:"newlyMatched,omitempty"`
	// NewlyPending: matcher couldn't bind and wrote/refreshed a pending row.
	NewlyPending int `json:"newlyPending,omitempty"`
	// Expired: pending rows whose grace window has elapsed. As of the
	// alert-only expiry policy these users are NOT deactivated — the
	// count surfaces in the digest so staff resolves them in the Needs
	// Match panel; the rows stay pending.
	Expired int `json:"expired,omitempty"`
	// MappedNoCache: UA users with a ua_user_mappings row but no members
	// cache row yet (ingest hasn't materialised them). Skipped by the
	// mapping-driven status pass; a persistently non-zero value means the
	// scheduled unifi-ingest isn't running.
	MappedNoCache int `json:"mappedNoCache,omitempty"`
	// HeldDeactivations: deactivation decisions suppressed by the
	// status-writes mode ("off"/"activate-only") or by the
	// mass-deactivation guard. Rolled into Deactivated as well so
	// dashboard continuity holds ("Deactivated" = decisions, live or not).
	HeldDeactivations int `json:"heldDeactivations,omitempty"`
	// GuardTripped: the mapping-driven pass wanted to deactivate more
	// than MaxDeactivationsPerRun users and held ALL of them. The digest
	// escalates this — it usually means a bad or partial directory sync,
	// not a real mass lapse.
	GuardTripped bool `json:"guardTripped,omitempty"`
}

// (RecheckResult moved to internal/recheck.Result as part of A3 — the
// denied-tap recheck is no longer a method on this Syncer; see the
// recheck package.)

// Config for the status syncer.
type Config struct {
	// SyncInterval between full UniFi status sync runs. Used as the schedule
	// when SyncTimeLocal is empty.
	SyncInterval time.Duration
	// RateLimitDelay between individual UniFi API calls during sync.
	RateLimitDelay time.Duration
	// SyncTimeLocal, if non-empty (HH:MM, e.g. "03:00"), pins each sync run
	// to this wall-clock time in the host's local timezone instead of using
	// SyncInterval as a tick. Set this to align with Redpoint's daily
	// membership-update window so the bridge picks up state changes within
	// hours rather than potentially up-to-24-hours later.
	SyncTimeLocal string
	// InitialDelay is the minimum lead time before the first sync runs. The
	// cache.Syncer needs a moment to populate from Redpoint before statusync
	// can do useful work. Defaults to 2 minutes if zero. The wall-clock
	// scheduler honours this floor: if the next SyncTimeLocal occurrence is
	// closer than InitialDelay, the first run is bumped to the day after.
	InitialDelay time.Duration
	// UnmatchedGraceDays is the number of days a UA-Hub user is left
	// untouched in the pending bucket before the sync run default-
	// deactivates them. 0 falls back to 7 inside the orchestrator — that
	// default matches config.Bridge.UnmatchedGraceDays so zero-value
	// Config{} doesn't accidentally produce an immediate-expiry loop.
	UnmatchedGraceDays int
	// LegacyNFCStatusLoop selects which status-enforcement engine Step 2
	// runs. When true (backward-compat default), the legacy GetMemberByNFC-
	// driven pass runs — it keys on raw NFC tokens, which hashed-token
	// firmware can never match, and it needs the members cache populated
	// via /directory/sync + /ingest/unifi. When false, the mapping-driven
	// pass runs instead: ua_user_mappings → members → IsAllowed(), the
	// C2-completing engine that doesn't depend on raw card UIDs. Exactly
	// one engine runs per sync so a user is never evaluated (or written)
	// twice in one run; flipping the flag swaps engines atomically.
	// Mirrors config.Bridge.LegacyNFCStatusLoop.
	LegacyNFCStatusLoop bool

	// StatusWrites gates UA-Hub status mutations from BOTH engines:
	// "off" (log-only — classic shadow), "activate-only" (ACTIVE writes
	// apply, DEACTIVATED writes are held — the can't-lock-anyone-out
	// rung), "full" (all writes apply). Empty resolves against the
	// positional shadowMode argument to New: "off" when shadow, "full"
	// when live — which is exactly the pre-split behavior.
	StatusWrites string

	// MaxDeactivationsPerRun is the mass-deactivation circuit guard for
	// the mapping-driven pass: if a single run wants to deactivate more
	// users than this, ALL of that run's deactivations are held and the
	// result is flagged (GuardTripped) so the digest pages the operator.
	// Protects against the "bad directory sync locks out the whole gym"
	// failure. 0 means the default of 10.
	MaxDeactivationsPerRun int

	// OnRunComplete, when non-nil, is invoked after every scheduled sync
	// run with the result and error (exactly one of which is meaningful:
	// result is nil on error). Wired by app.Build to push the operator's
	// per-run digest notification. Called synchronously on the sync
	// goroutine — the sync loop is not latency-sensitive, and a bounded
	// notification POST (10s) is an acceptable tail on a minutes-long run.
	//
	// Only scheduled runs (the Run loop) fire this hook; operator-
	// triggered runs via POST /status-sync report their result in the
	// HTTP response and don't need a push.
	OnRunComplete func(result *SyncResult, err error)
}

// Syncer manages the daily Redpoint → UniFi status synchronisation.
//
// Note: prior to A3 this struct also owned the denied-tap recheck (and a
// circuit breaker that guarded it). Both moved to internal/recheck so
// the per-tap policy is a separate concern from the daily sync loop.
type Syncer struct {
	unifi    *unifi.Client
	redpoint *redpoint.Client
	store    *store.Store
	config   Config
	logger   *slog.Logger

	// shadowMode and metrics are set once at construction and read on
	// the sync hot path. Pre-PR3 they were settable post-construction
	// (SetShadowMode, SetMetrics), which #1 hardened with atomic.Bool /
	// atomic.Pointer to close the race. PR3's elimination of setters
	// makes the atomics unnecessary — no concurrent writer can exist
	// after construction — so we revert to plain types.
	shadowMode bool
	// statusWrites is Config.StatusWrites resolved against shadowMode at
	// construction: always one of "off", "activate-only", "full".
	statusWrites string
	metrics      *metrics.Registry

	mu         sync.Mutex
	lastResult *SyncResult
	running    bool
}

// New creates a new status syncer. ShadowMode and Metrics are
// construction-only — see the field comments for the rationale that
// drove the SetShadowMode / SetMetrics setter removal in PR3.
func New(
	unifiClient *unifi.Client,
	rpClient *redpoint.Client,
	db *store.Store,
	cfg Config,
	shadowMode bool,
	met *metrics.Registry,
	logger *slog.Logger,
) *Syncer {
	if cfg.RateLimitDelay == 0 {
		cfg.RateLimitDelay = 200 * time.Millisecond // ~5 updates/sec to avoid hammering UniFi
	}
	if cfg.MaxDeactivationsPerRun <= 0 {
		cfg.MaxDeactivationsPerRun = 10
	}
	statusWrites := cfg.StatusWrites
	if statusWrites == "" {
		if shadowMode {
			statusWrites = "off"
		} else {
			statusWrites = "full"
		}
	}
	return &Syncer{
		unifi:        unifiClient,
		redpoint:     rpClient,
		store:        db,
		config:       cfg,
		logger:       logger,
		shadowMode:   shadowMode,
		statusWrites: statusWrites,
		metrics:      met,
	}
}

// Start launches the background sync loop.
//
// Schedule selection:
//   - If Config.SyncTimeLocal is set (e.g. "03:00"), every run is pinned to
//     that wall-clock time in the host's local timezone. This is the
//     recommended mode because it stays aligned with Redpoint's daily
//     update window across restarts.
//   - Otherwise the loop falls back to a "now + SyncInterval" cadence using
//     a ticker, which is simpler but drifts away from any target wall-clock
//     time across restarts.
//
// In either mode the first run is delayed by at least Config.InitialDelay
// (default 2 minutes) so the upstream cache.Syncer has time to populate.
func (s *Syncer) Start(ctx context.Context) {
	go s.supervisedLoop(ctx)
}

// Run performs supervised scheduling and execution of sync runs until ctx is cancelled.
// It wraps supervisedLoop with panic recovery and automatic restarts. Returns ctx.Err()
// when cancelled. This is the preferred way to launch the status syncer in a supervised group.
func (s *Syncer) Run(ctx context.Context) error {
	s.supervisedLoop(ctx)
	return ctx.Err()
}

// supervisedLoop runs fn in a panic-recover wrapper that re-launches the
// loop if it crashes. This turns a panic inside RunSync (or any of the
// phase helpers) from "silent watchdog-is-dead" into "visible restart with
// a stack trace in logs and a counter in Prometheus".
//
// The fn indirection exists for testability: supervisedLoopWithFn lets
// tests inject a deliberately-panicking body without rewiring the whole
// Syncer. Start calls this with s.runLoop.
//
// Invariants:
//   - If ctx is Done, we exit cleanly; a panic-after-Done does not
//     re-launch (avoids an infinite restart storm during shutdown).
//   - sync_loop_restarted_total is incremented on every re-launch, so an
//     alert rule like `rate(sync_loop_restarted_total[1h]) > 0` surfaces
//     intermittent panics operators would otherwise never notice.
//   - The panic stack is logged at Error level with the full stack trace
//     so postmortems have something to work with.
func (s *Syncer) supervisedLoop(ctx context.Context) {
	s.supervisedLoopWithFn(ctx, s.runLoop)
}

func (s *Syncer) supervisedLoopWithFn(ctx context.Context, fn func(context.Context)) {
	for {
		crashed := s.runWithRecover(ctx, fn)
		if ctx.Err() != nil {
			return
		}
		if !crashed {
			// fn returned cleanly without ctx cancellation — this
			// shouldn't normally happen (runLoop is an infinite ticker
			// loop), but if it does, re-launching beats silently exiting.
			s.logger.Warn("status sync loop returned without ctx cancellation; re-launching")
		}
		if s.metrics != nil {
			s.metrics.Counter("sync_loop_restarted_total").Inc()
		}
	}
}

// runWithRecover wraps fn in a deferred recover. Returns true if fn
// exited via panic (so the supervisor knows to log + restart), false if
// it returned normally.
func (s *Syncer) runWithRecover(ctx context.Context, fn func(context.Context)) (crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			crashed = true
			s.logger.Error("status sync loop panic; supervisor will restart",
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	fn(ctx)
	return false
}

// runLoop is the actual scheduling/execution body for the sync loop. See
// Start for the schedule contract.
func (s *Syncer) runLoop(ctx context.Context) {
	initialDelay := s.config.InitialDelay
	if initialDelay <= 0 {
		initialDelay = 2 * time.Minute
	}

	// First-run scheduling.
	firstRun := s.scheduleNext(time.Now(), initialDelay)
	s.logger.Info("status syncer scheduled",
		"firstRun", firstRun.Format(time.RFC3339),
		"interval", s.config.SyncInterval,
		"syncTimeLocal", s.config.SyncTimeLocal,
	)
	if !sleepUntil(ctx, firstRun) {
		return
	}

	s.logger.Info("running initial UniFi status sync...")
	if result, err := s.trackedRunSync(ctx); err != nil {
		s.logger.Error("initial status sync failed", "error", err)
	} else {
		s.logger.Info("initial status sync complete",
			"activated", result.Activated,
			"deactivated", result.Deactivated,
		)
	}

	// Steady-state loop: recompute the next target after each run so a
	// long-running sync (e.g. 03:00 sync that takes 90 minutes) doesn't
	// cause us to skip the next day or pile up missed runs.
	for {
		next := s.scheduleNext(time.Now(), 0)
		s.logger.Debug("statusync next run scheduled",
			"at", next.Format(time.RFC3339),
			"in", time.Until(next).Round(time.Second),
		)
		if !sleepUntil(ctx, next) {
			return
		}
		s.logger.Info("running scheduled UniFi status sync...")
		if result, err := s.trackedRunSync(ctx); err != nil {
			s.logger.Error("scheduled status sync failed", "error", err)
		} else {
			s.logger.Info("scheduled status sync complete",
				"activated", result.Activated,
				"deactivated", result.Deactivated,
			)
		}
	}
}

// trackedRunSync wraps RunSync in a jobs.Track bracket so scheduled
// runs land in the jobs table with the same shape the manual
// /status-sync handler writes. The captured result fields match
// internal/api/server.go's handleStatusSync so staff see consistent
// rows whether the sync was scheduled or operator-triggered.
func (s *Syncer) trackedRunSync(ctx context.Context) (*SyncResult, error) {
	var captured *SyncResult
	err := jobs.Track(ctx, s.store, s.logger, jobs.TypeStatusSync,
		func(ctx context.Context) (any, error) {
			result, err := s.RunSync(ctx)
			captured = result
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"activated":    result.Activated,
				"deactivated":  result.Deactivated,
				"unchanged":    result.Unchanged,
				"errors":       result.Errors,
				"newlyMatched": result.NewlyMatched,
				"newlyPending": result.NewlyPending,
				"duration":     result.Duration,
			}, nil
		})
	if s.config.OnRunComplete != nil {
		s.config.OnRunComplete(captured, err)
	}
	return captured, err
}

// scheduleNext computes the time at which the next sync run should fire.
//
//   - If SyncTimeLocal is set and parses, the result is the next occurrence
//     of that local-time HH:MM. If that occurrence is closer than minLead
//     (used to enforce the cache-warmup floor on the first run), it's
//     bumped 24h forward.
//   - Otherwise the result is now + SyncInterval (legacy behaviour). The
//     minLead floor is honoured by clamping if it would otherwise undershoot.
//
// Pure: takes time as a parameter so tests don't have to manipulate the
// real clock.
func (s *Syncer) scheduleNext(now time.Time, minLead time.Duration) time.Time {
	if s.config.SyncTimeLocal != "" {
		h, m, err := config.ParseHHMM(s.config.SyncTimeLocal)
		if err == nil {
			next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			if minLead > 0 && next.Sub(now) < minLead {
				next = next.Add(24 * time.Hour)
			}
			return next
		}
		// Malformed value escaped validation (shouldn't happen in normal
		// operation since config.validate parses on boot). Log loudly and
		// fall through to the interval ticker so the bridge keeps running.
		s.logger.Warn("SyncTimeLocal failed to parse; falling back to interval schedule",
			"value", s.config.SyncTimeLocal, "error", err,
		)
	}
	target := now.Add(s.config.SyncInterval)
	if minLead > 0 {
		floor := now.Add(minLead)
		if target.Before(floor) {
			target = floor
		}
	}
	return target
}

// sleepUntil blocks until t arrives or ctx is cancelled. Returns true if
// the wait completed normally, false if ctx was cancelled.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		// Target already in the past — fire immediately, but still respect
		// cancellation by checking ctx.
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RunSync performs a full sync: compare cache status vs UniFi status and update UniFi.
func (s *Syncer) RunSync(ctx context.Context) (*SyncResult, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, fmt.Errorf("sync already in progress")
	}
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	result := &SyncResult{StartedAt: time.Now()}

	// Step 1: Fetch all UniFi users
	unifiUsers, err := s.unifi.ListAllUsersWithStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch UniFi users: %w", err)
	}
	result.UniFiUsers = len(unifiUsers)

	// Step 1.5: Email-based matching phase (C2).
	//
	// For each UA user not yet present in ua_user_mappings, run the matcher
	// and persist a mapping or pending row. Already-mapped users are
	// fast-path through to the legacy NFC loop below (which still drives
	// status decisions until a follow-up change retires it). The matcher
	// phase is additive: its only side-effects are DB writes + Redpoint
	// reads, never UA-Hub writes, so shadow mode is a no-op here.
	s.runMatchingPhase(ctx, unifiUsers, result)

	// Step 2: Legacy NFC-token status loop. Gated by LegacyNFCStatusLoop.
	//
	// This pass drives the old status-enforcement flow: for every UA user
	// with NFC tokens, look them up in the local members cache (populated
	// by /directory/sync + /ingest/unifi), and issue an ACTIVE/DEACTIVATED
	// update against UA-Hub if the Redpoint-derived IsAllowed() disagrees
	// with the current UA status.
	//
	// At LEF we've deprecated the members-cache path in favour of the C2
	// ua_user_mappings matching phase (Step 1.5 above), and the cache is
	// typically half-populated or empty. Running Step 2 there produces a
	// stream of "cached == nil → result.Unmatched++" against every real
	// user and (in shadow mode) spurious "would DEACTIVATE name=Unknown"
	// log noise for the handful of partially-cached rows. When
	// LegacyNFCStatusLoop is false, we skip this pass entirely; Step 4's
	// pending-expiry still runs so grace-window deadlines are enforced,
	// and a follow-up change will replace Step 2 with a mapping-driven
	// status pass that reads from ua_user_mappings.
	if s.config.LegacyNFCStatusLoop {
		for _, user := range unifiUsers {
			if len(user.NfcTokens) == 0 {
				result.Unmatched++
				continue
			}

			// Try to find this user in our store by any of their NFC tokens
			var cached *store.Member
			var lookupErr error
			for _, token := range user.NfcTokens {
				cached, lookupErr = s.store.GetMemberByNFC(ctx, token)
				if lookupErr != nil {
					s.logger.Error("failed to look up member by NFC token",
						"token", token,
						"error", lookupErr,
					)
					result.Errors++
					break
				}
				if cached != nil {
					break
				}
			}

			if cached == nil {
				// Not in our store — could be a staff member, admin, etc. Leave alone.
				result.Unmatched++
				continue
			}

			result.Matched++

			// Step 3: Compare status and update if needed
			memberAllowed := cached.IsAllowed()
			unifiActive := user.Status == "ACTIVE"

			if memberAllowed && !unifiActive {
				// Member is valid in Redpoint but locked out in UniFi → reactivate
				shadow := s.statusWrites == "off"
				action := "REACTIVATING user in UniFi"
				if shadow {
					action = "SHADOW: would REACTIVATE user in UniFi"
				}
				s.logger.Info(action,
					"name", cached.FullName(),
					"unifiId", user.ID,
					"unifiStatus", user.Status,
					"badgeStatus", cached.BadgeStatus,
				)
				if shadow {
					result.Activated++
					continue
				}
				if err := s.unifi.UpdateUserStatus(ctx, user.ID, "ACTIVE"); err != nil {
					s.logger.Error("failed to reactivate user",
						"name", cached.FullName(),
						"unifiId", user.ID,
						"error", err,
					)
					result.Errors++
				} else {
					result.Activated++
				}
				time.Sleep(s.config.RateLimitDelay)

			} else if !memberAllowed && unifiActive {
				// Member is expired/frozen in Redpoint but still active in
				// UniFi → deactivate. Held in both "off" and "activate-only"
				// modes — deactivation is the member-facing risk, so it's
				// the last write to go live.
				shadow := s.statusWrites != "full"
				action := "DEACTIVATING user in UniFi"
				if shadow {
					action = "SHADOW: would DEACTIVATE user in UniFi"
				}
				s.logger.Info(action,
					"name", cached.FullName(),
					"unifiId", user.ID,
					"badgeStatus", cached.BadgeStatus,
					"active", cached.Active,
					"reason", cached.DenyReason(),
				)
				if shadow {
					result.Deactivated++
					result.HeldDeactivations++
					continue
				}
				if err := s.unifi.UpdateUserStatus(ctx, user.ID, "DEACTIVATED"); err != nil {
					s.logger.Error("failed to deactivate user",
						"name", cached.FullName(),
						"unifiId", user.ID,
						"error", err,
					)
					result.Errors++
				} else {
					result.Deactivated++
				}
				time.Sleep(s.config.RateLimitDelay)

			} else {
				result.Unchanged++
			}
		}
	} else {
		// Mapping-driven status pass — the C2-completing replacement for
		// the legacy loop above. Exactly one engine runs per sync.
		s.runMappingStatusPass(ctx, unifiUsers, result)
	}

	// Step 4: Pending expiry pass. After matching is populated and status
	// decisions have been made on the bound set, sweep the pending bucket
	// for rows whose grace window has elapsed. Those UA users are
	// default-deactivated so an unresolved "we can't figure out who this
	// is" ticket doesn't leave the door open indefinitely.
	s.runExpiryPhase(ctx, result)

	result.Duration = time.Since(result.StartedAt).Round(time.Millisecond).String()

	s.mu.Lock()
	s.lastResult = result
	s.mu.Unlock()

	// Liveness signal: stamp the gauge with now() so a Prometheus alert of
	// the form `time() - last_sync_completed_at > 2 * SyncInterval` fires
	// when the loop silently dies. We only stamp on the success path — a
	// run that aborted early (returned nil,err above) leaves the gauge at
	// its previous value and the alert eventually fires, which is the
	// intended behaviour: "no successful sync in too long" is exactly what
	// we want to page on.
	if s.metrics != nil {
		s.metrics.Gauge("last_sync_completed_at").SetInt(time.Now().Unix())
		s.metrics.Counter("sync_runs_total").Inc()
	}

	return result, nil
}

// runMatchingPhase walks the UA-user set and invokes matchOne on any
// user that isn't already bound in ua_user_mappings. Errors from
// individual users are logged and counted but don't abort the phase —
// the matcher is best-effort and the next sync will retry.
//
// Rate-limited against Redpoint: each matchOne call potentially hits the
// upstream once (email) or twice (email + name fallback), and the Redpoint
// API is shared with the cache.Syncer bulk refresh. The delay here uses
// the same RateLimitDelay as the UA side, which is conservative but
// avoids surprise in observability dashboards.
func (s *Syncer) runMatchingPhase(
	ctx context.Context,
	unifiUsers []unifi.UniFiUser,
	result *SyncResult,
) {
	for _, u := range unifiUsers {
		if err := ctx.Err(); err != nil {
			return
		}
		// Skip DEACTIVATED UA-Hub users. ListAllUsersWithStatus returns
		// everyone (no status filter), so without this guard a user
		// staff has just skipped (handleFragUnmatchedSkip → status
		// DEACTIVATED, pending row deleted) re-appears in the pending
		// queue on the next sync. The matcher has no concept of
		// "staff already decided to ignore this person"; the UA-Hub
		// status field is the durable signal.
		//
		// External re-activations (a UA-Hub admin flipping the user
		// back to ACTIVE outside the bridge) flow through this guard
		// correctly: next sync sees ACTIVE + no mapping → falls through
		// to matchOne → re-queued in Needs Match, which is the desired
		// behaviour.
		if u.Status == "DEACTIVATED" {
			continue
		}
		existing, err := s.store.GetMapping(ctx, u.ID)
		if err != nil {
			s.logger.Error("GetMapping failed; skipping match for user",
				"uaUserId", u.ID, "error", err,
			)
			result.Errors++
			continue
		}
		if existing != nil {
			// Already bound. Legacy status-sync path below handles them.
			continue
		}
		result.Matching++
		d, err := s.matchOne(ctx, u)
		if err != nil {
			s.logger.Error("matchOne failed",
				"uaUserId", u.ID, "error", err,
			)
			result.Errors++
			continue
		}
		if d.Matched != nil {
			result.NewlyMatched++
			s.logger.Info("newly matched UA user",
				"uaUserId", u.ID,
				"redpointCustomerId", d.Matched.ID,
				"source", d.Source,
			)
		} else {
			result.NewlyPending++
			s.logger.Info("UA user pending match",
				"uaUserId", u.ID,
				"reason", d.PendingReason,
				"candidates", d.Candidates,
			)
		}
		time.Sleep(s.config.RateLimitDelay)
	}
}

// mappingDecision is one intended status change computed by the read
// phase of runMappingStatusPass. Collected before any write so the
// mass-deactivation guard can see the whole run's intent.
type mappingDecision struct {
	uaUserID   string
	name       string
	activate   bool // true → ACTIVE, false → DEACTIVATED
	badge      string
	denyReason string
}

// runMappingStatusPass is the mapping-driven status engine: for every
// UA-Hub user bound in ua_user_mappings, read the member row the
// mapping points at (whose active/badge state the cache-syncer
// refreshes from Redpoint live — including lapses and deletions,
// which the ACTIVE-filtered directory sync alone cannot see) and
// converge the UA-Hub status to Redpoint truth.
//
// This is the pass promised when Step 2 was first gated off ("a
// follow-up change will replace Step 2 with a mapping-driven status
// pass"): it keys on the authoritative UA-user↔customer binding
// instead of raw NFC tokens, so it works on hashed-token firmware and
// ignores PIN-only staff users (no mapping → no opinion) by design.
//
// Two-phase shape: decisions are computed read-only first, then writes
// apply — so the mass-deactivation guard can veto the whole
// deactivation set atomically when a run wants to lock out more than
// MaxDeactivationsPerRun users (the "bad directory sync locks out the
// gym" failure). Activations are never guarded; wrongly activating is
// self-correcting on the next sync, wrongly deactivating is a member
// at the door.
func (s *Syncer) runMappingStatusPass(ctx context.Context, unifiUsers []unifi.UniFiUser, result *SyncResult) {
	// ── Read phase ───────────────────────────────────────────
	var decisions []mappingDecision
	for _, user := range unifiUsers {
		if ctx.Err() != nil {
			return
		}
		mapping, err := s.store.GetMapping(ctx, user.ID)
		if err != nil {
			s.logger.Error("mapping status pass: GetMapping failed",
				"uaUserId", user.ID, "error", err)
			result.Errors++
			continue
		}
		if mapping == nil {
			// No binding: PIN-only staff, or still pending. The matching
			// phase owns pending users; nothing to enforce here.
			result.Unmatched++
			continue
		}
		member, err := s.store.GetMemberByUAUserID(ctx, user.ID)
		if err != nil {
			s.logger.Error("mapping status pass: member lookup failed",
				"uaUserId", user.ID, "error", err)
			result.Errors++
			continue
		}
		if member == nil {
			// Mapping exists but ingest hasn't materialised the members
			// row yet. Skip rather than guess — the scheduled ingest will
			// close the gap; a persistently non-zero count is the signal
			// that it isn't running.
			result.MappedNoCache++
			s.logger.Warn("mapping status pass: mapped user has no members-cache row yet",
				"uaUserId", user.ID, "redpointCustomerId", mapping.RedpointCustomer)
			continue
		}
		result.Matched++

		memberAllowed := member.IsAllowed()
		unifiActive := user.Status == "ACTIVE"
		switch {
		case memberAllowed && !unifiActive:
			decisions = append(decisions, mappingDecision{
				uaUserID: user.ID, name: member.FullName(),
				activate: true, badge: member.BadgeStatus,
			})
		case !memberAllowed && unifiActive:
			decisions = append(decisions, mappingDecision{
				uaUserID: user.ID, name: member.FullName(),
				activate: false, badge: member.BadgeStatus,
				denyReason: member.DenyReason(),
			})
		default:
			result.Unchanged++
		}
	}

	// ── Guard phase ──────────────────────────────────────────
	wouldDeactivate := 0
	for _, d := range decisions {
		if !d.activate {
			wouldDeactivate++
		}
	}
	guardTripped := wouldDeactivate > s.config.MaxDeactivationsPerRun
	if guardTripped {
		result.GuardTripped = true
		s.logger.Error("mapping status pass: mass-deactivation guard TRIPPED — holding all deactivations",
			"wouldDeactivate", wouldDeactivate,
			"maxPerRun", s.config.MaxDeactivationsPerRun,
			"hint", "usually a bad or partial directory sync, not a real mass lapse; investigate before raising the cap",
		)
	}

	// ── Write phase ──────────────────────────────────────────
	for _, d := range decisions {
		if ctx.Err() != nil {
			return
		}
		if d.activate {
			held := s.statusWrites == "off"
			action := "REACTIVATING user in UniFi (mapping pass)"
			if held {
				action = "SHADOW: would REACTIVATE user in UniFi (mapping pass)"
			}
			s.logger.Info(action, "name", d.name, "unifiId", d.uaUserID, "badgeStatus", d.badge)
			if held {
				result.Activated++
				continue
			}
			if err := s.unifi.UpdateUserStatus(ctx, d.uaUserID, "ACTIVE"); err != nil {
				s.logger.Error("mapping status pass: reactivate failed",
					"name", d.name, "unifiId", d.uaUserID, "error", err)
				result.Errors++
			} else {
				result.Activated++
			}
			time.Sleep(s.config.RateLimitDelay)
			continue
		}

		held := s.statusWrites != "full" || guardTripped
		action := "DEACTIVATING user in UniFi (mapping pass)"
		if held {
			action = "SHADOW: would DEACTIVATE user in UniFi (mapping pass)"
		}
		s.logger.Info(action,
			"name", d.name, "unifiId", d.uaUserID,
			"badgeStatus", d.badge, "reason", d.denyReason,
			"heldByGuard", guardTripped,
		)
		if held {
			result.Deactivated++
			result.HeldDeactivations++
			continue
		}
		if err := s.unifi.UpdateUserStatus(ctx, d.uaUserID, "DEACTIVATED"); err != nil {
			s.logger.Error("mapping status pass: deactivate failed",
				"name", d.name, "unifiId", d.uaUserID, "error", err)
			result.Errors++
		} else {
			result.Deactivated++
		}
		time.Sleep(s.config.RateLimitDelay)
	}
}

// runExpiryPhase surfaces pending UA users whose grace window has
// elapsed. ALERT-ONLY by operator decision: an unmatched user is never
// auto-deactivated, because a wrong auto-deactivation is member-facing
// and not self-healing (the denied-tap recheck cannot resolve an
// unmapped user). Instead:
//
//  1. Each overdue row logs at Warn (names are fine in the local log;
//     push payloads carry only counts).
//  2. result.Expired carries the overdue count into the digest push, so
//     a non-zero queue reaches the operator's phone every sync.
//  3. The pending rows stay put — staff resolves them in the Needs
//     Match panel (Match / Skip / Defer), which is the only path that
//     removes them.
//
// The pre-alert-only implementation deactivated these users in UA-Hub
// (with a match_audit trail); MatchSourceBridgeExpiry remains defined
// for reading that historical audit data.
func (s *Syncer) runExpiryPhase(ctx context.Context, result *SyncResult) {
	if ctx.Err() != nil {
		// Caller-driven shutdown — not a phase error.
		return
	}
	now := time.Now().UTC()
	expired, err := s.store.ExpiredPending(ctx, now)
	if err != nil {
		if ctx.Err() != nil {
			// The ExpiredPending failure was caused by context cancellation;
			// treat it as an orderly shutdown rather than a real error.
			return
		}
		s.logger.Error("ExpiredPending failed; skipping expiry pass", "error", err)
		result.Errors++
		return
	}
	for _, p := range expired {
		if err := ctx.Err(); err != nil {
			return
		}
		s.logger.Warn("unmatched UA user past grace window (alert-only; resolve in Needs Match)",
			"uaUserId", p.UAUserID,
			"reason", p.Reason,
			"firstSeen", p.FirstSeen,
			"graceUntil", p.GraceUntil,
		)
		result.Expired++
	}
}

// LastResult returns the result of the most recent sync run.
func (s *Syncer) LastResult() *SyncResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastResult
}

// IsRunning returns whether a sync is currently in progress.
func (s *Syncer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// (Denied-tap recheck moved to internal/recheck.Service.RecheckDeniedTap
// as part of A3 — see the recheck package. The flow, breaker policy, and
// JSON shape of the result are preserved verbatim; only the home of the
// method changed. Callers — the check-in handler — now depend on the
// recheck.Rechecker interface instead of *statusync.Syncer.)
