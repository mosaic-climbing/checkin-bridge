// Package recheck implements the bridge's "denied-tap live recheck" policy:
// when a member tap is denied based on the local cache, the bridge can pay
// a one-shot live query to Redpoint to find out whether the member has
// renewed since the last sync. If they have, the cache is updated and (in
// live mode) UA-Hub is reactivated so the member walks through the next
// time their card scans — which usually happens within seconds because they
// just tap again.
//
// This package was extracted from internal/statusync as part of A3 in
// docs/architecture-review.md to:
//
//   - Make the recheck a first-class business rule with its own interface
//     (`Rechecker`) that the check-in handler depends on, instead of a
//     method on the daily-sync orchestrator.
//   - Expose a single config knob (`MaxStaleness`) for "how recent must
//     the last sync be before we trust a denial?" — pinning the staleness
//     contract that previously lived implicitly in the always-recheck
//     behaviour.
//   - Surface the four-quadrant decision matrix (UA=allow|deny ×
//     Redpoint=allow|deny) as a testable unit. Before A3 the only way to
//     exercise the recheck path was to spin up a Syncer with a real
//     Redpoint client.
//
// Side note: the breaker that guards Redpoint outages moved with us
// (breaker.go). It was always coupled to the recheck path — statusync's
// daily loop has its own retry/backoff and never used the breaker.
package recheck

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// Rechecker is the interface the check-in handler depends on. Keeping it
// narrow (one method) means tests of the handler can stub it trivially
// without dragging in a Redpoint client or a SQLite store.
//
// CONTRACT:
//   - A nil error with a non-nil Result means the recheck completed
//     (either Reactivated=true or Reactivated=false with a Reason). The
//     handler decides what to do based on Reactivated.
//   - A non-nil error means the recheck failed for an upstream-health
//     reason (network, 5xx, auth). The handler should treat this as
//     "denial stands" and log it — the breaker will already have been
//     incremented internally if applicable.
//   - Implementations MUST be safe for concurrent use. The check-in
//     handler dispatches a separate goroutine per tap.
type Rechecker interface {
	RecheckDeniedTap(ctx context.Context, nfcToken string) (*Result, error)
}

// Result holds the outcome of a denied-tap live recheck. The shape is
// identical to the prior `statusync.RecheckResult` so the JSON wire
// format the staff UI consumes is unchanged.
type Result struct {
	CustomerID  string `json:"customerId"`
	Name        string `json:"name"`
	Reactivated bool   `json:"reactivated"`
	Reason      string `json:"reason"`
}

// Store is the slice of *store.Store the recheck path depends on. Defining
// it locally keeps the recheck package free of any test-time dependency on
// SQLite — tests can pass a fake Store without spinning up a database.
type Store interface {
	GetMemberByNFC(ctx context.Context, nfcUID string) (*store.Member, error)
	UpsertMember(ctx context.Context, m *store.Member) error
}

// RedpointClient is the slice of *redpoint.Client the recheck path
// depends on. Same rationale as Store: tests can stub this without a
// live GraphQL endpoint or recorded fixtures.
type RedpointClient interface {
	RefreshCustomers(ctx context.Context, customerIDs []string) ([]*redpoint.Customer, error)
}

// UnifiClient is the slice of *unifi.Client the recheck path depends on.
// Same rationale as Store.
type UnifiClient interface {
	ListUsers(ctx context.Context) ([]unifi.UniFiUser, error)
	UpdateUserStatus(ctx context.Context, userID, status string) error
}

// Config controls the recheck policy and the breaker that guards
// Redpoint. Zero values are safe — they reproduce the pre-A3 behaviour
// (always recheck, breaker at 5/60s).
type Config struct {
	// BreakerThreshold is the number of consecutive Redpoint upstream
	// failures that trip the breaker. Defaults to 5.
	BreakerThreshold int

	// BreakerCooldown is the time the breaker stays open before
	// admitting a probe. Defaults to 60s.
	BreakerCooldown time.Duration

	// MaxStaleness is the freshness budget for the cached membership
	// state. If non-zero, denials based on a Member whose CachedAt is
	// younger than MaxStaleness are TRUSTED — the recheck is skipped
	// and the result reports `Reason="recheck skipped: cache fresh
	// enough"` with Reactivated=false. The handler treats this the
	// same as a "still denied" result.
	//
	// Zero (the default) reproduces the pre-A3 behaviour: every denied
	// tap pays a recheck, modulo the breaker.
	//
	// The motivating use case: a gym whose Redpoint sync runs every
	// hour can set MaxStaleness=2h and avoid hammering Redpoint with
	// recheck requests for tap storms by frustrated members whose
	// status really is correctly denied. A gym whose sync runs daily
	// (the current default) should leave this at 0 — a 24-hour stale
	// cache is too coarse to trust for door decisions.
	MaxStaleness time.Duration

	// ShadowMode, when true, skips the live UA-Hub mutation after a
	// successful Redpoint reactivation. The store is still updated and
	// Reactivated=true is still returned, so the handler can log "would
	// have reactivated" without changing UA state.
	ShadowMode bool

	// Now is injectable for testing the staleness check deterministically.
	// Defaults to time.Now.
	Now func() time.Time
}

// Service is the production Rechecker. It speaks to the local store, the
// Redpoint GraphQL API, and (on success) the UA-Hub REST API.
type Service struct {
	store      Store
	redpoint   RedpointClient
	unifi      UnifiClient
	breaker    *breaker
	logger     *slog.Logger
	shadowMode bool
	maxStale   time.Duration
	now        func() time.Time
}

// New constructs a Service. The dependencies are passed as interfaces so
// that callers from cmd/bridge can use the concrete client types (which
// satisfy the interfaces structurally) and tests can substitute fakes.
func New(s Store, rp RedpointClient, ua UnifiClient, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		// A nil logger would crash on the very first denied tap; refuse to
		// boot with an obvious zero-value sentinel instead. Production
		// callers always pass a real logger.
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	br := newBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown)
	// A5: route breaker transition logs through the Service's structured
	// logger so they share component scoping with the rest of recheck (the
	// breaker is internal — we don't want it logging at slog.Default while
	// every other recheck event lives under our scoped handler).
	br.logger = logger.With("component", "recheck.breaker")
	return &Service{
		store:      s,
		redpoint:   rp,
		unifi:      ua,
		breaker:    br,
		logger:     logger,
		shadowMode: cfg.ShadowMode,
		maxStale:   cfg.MaxStaleness,
		now:        now,
	}
}

// SetShadowMode toggles shadow mode at runtime. Mirrors the Setter on
// statusync.Syncer so cmd/bridge can flip both with the same flag.
func (s *Service) SetShadowMode(on bool) { s.shadowMode = on }

// RecheckDeniedTap implements Rechecker.
//
// Flow:
//  1. Look up the NFC token in store to find the cached member.
//  2. If MaxStaleness is set and the cache is fresh, skip the recheck.
//  3. Check the breaker; if open, return immediately with a "breaker
//     open" reason (denial stands).
//  4. Query Redpoint live; on upstream failure, increment the breaker.
//  5. If Redpoint says the member is now active, update the store and
//     (in live mode) reactivate the UA-Hub user.
func (s *Service) RecheckDeniedTap(ctx context.Context, nfcToken string) (*Result, error) {
	// Step 1: Find in store
	cached, err := s.store.GetMemberByNFC(ctx, nfcToken)
	if err != nil {
		return nil, fmt.Errorf("failed to look up member by NFC: %w", err)
	}
	if cached == nil {
		return &Result{
			Reason: "unknown card — not in membership store",
		}, nil
	}

	result := &Result{
		CustomerID: cached.CustomerID,
		Name:       cached.FullName(),
	}

	// Step 2: MaxStaleness gate. Skip the recheck if the cache is
	// fresh enough that we trust the denial. Any parse error on
	// CachedAt falls through to recheck (fail-open: better to do an
	// extra Redpoint call than to wrongly trust a corrupt timestamp).
	if s.maxStale > 0 && cached.CachedAt != "" {
		if cachedAt, perr := time.Parse(time.RFC3339, cached.CachedAt); perr == nil {
			if s.now().Sub(cachedAt) < s.maxStale {
				s.logger.Info("denied-tap recheck: cache fresh enough, skipping",
					"name", cached.FullName(),
					"customerId", cached.CustomerID,
					"cacheAgeSeconds", s.now().Sub(cachedAt).Seconds(),
					"maxStalenessSeconds", s.maxStale.Seconds(),
				)
				result.Reason = "recheck skipped: cache fresh enough"
				return result, nil
			}
		}
	}

	// Step 3: Breaker gate. If Redpoint has been failing, skip the live
	// query and let the denial stand. The member still sees a denial
	// (what they'd see anyway) but the door responds instantly instead
	// of waiting 10s for a request we already know will fail.
	if !s.breaker.allow() {
		s.logger.Warn("denied-tap recheck: circuit breaker OPEN, skipping Redpoint live query",
			"name", cached.FullName(),
			"customerId", cached.CustomerID,
		)
		result.Reason = "Redpoint recheck unavailable (circuit breaker open)"
		return result, nil
	}

	// Step 4: Live check against Redpoint
	s.logger.Info("denied-tap recheck: querying Redpoint live",
		"name", cached.FullName(),
		"customerId", cached.CustomerID,
	)

	customers, err := s.redpoint.RefreshCustomers(ctx, []string{cached.CustomerID})
	if err != nil {
		// Count this as a breaker failure — network/5xx/auth errors are
		// the signals the breaker exists to protect against.
		s.breaker.failure()
		return nil, fmt.Errorf("redpoint live query failed: %w", err)
	}
	// Redpoint responded successfully; reset the breaker regardless of
	// whether the customer was found. A missing customer is an
	// application-level answer, not an upstream health signal.
	s.breaker.success()

	if len(customers) == 0 {
		result.Reason = "customer not found in Redpoint"
		return result, nil
	}

	cust := customers[0]

	// Check fresh status
	badgeStatus := ""
	badgeName := ""
	if cust.Badge != nil {
		badgeStatus = cust.Badge.Status
		if cust.Badge.CustomerBadge != nil {
			badgeName = cust.Badge.CustomerBadge.Name
		}
	}

	nowAllowed := cust.Active && badgeStatus == "ACTIVE"
	if !nowAllowed {
		result.Reason = fmt.Sprintf("still not allowed: active=%v, badge=%s", cust.Active, badgeStatus)
		s.logger.Info("denied-tap recheck: still denied",
			"name", cached.FullName(),
			"active", cust.Active,
			"badgeStatus", badgeStatus,
		)
		return result, nil
	}

	// Step 5: Member has renewed! Update store
	s.logger.Info("denied-tap recheck: MEMBER RENEWED — reactivating",
		"name", cached.FullName(),
		"oldBadge", cached.BadgeStatus,
		"newBadge", badgeStatus,
	)

	cached.Active = cust.Active
	cached.BadgeStatus = badgeStatus
	cached.BadgeName = badgeName
	cached.FirstName = cust.FirstName
	cached.LastName = cust.LastName
	cached.CachedAt = s.now().UTC().Format(time.RFC3339)
	if err := s.store.UpsertMember(ctx, cached); err != nil {
		s.logger.Error("failed to upsert member in store", "error", err)
		// Continue anyway; cache is updated in memory and will be synced next run
	}

	// Step 6: We need the UniFi user ID to reactivate. Search by NFC
	// token. In shadow mode we skip the live UniFi mutation but still
	// mark reactivated so the caller can log what the live system would
	// have done.
	if s.shadowMode {
		s.logger.Info("SHADOW: would reactivate in UniFi after live recheck",
			"name", cached.FullName(),
			"customerId", cached.CustomerID,
		)
		result.Reactivated = true
		result.Reason = "shadow: renewed in Redpoint (UniFi update skipped)"
		return result, nil
	}

	unifiUsers, err := s.unifi.ListUsers(ctx)
	if err != nil {
		s.logger.Error("failed to fetch UniFi users for reactivation", "error", err)
		// Still return reactivated=true since the cache is updated and
		// the checkin handler can use it next time
		result.Reactivated = true
		result.Reason = "renewed in Redpoint, cache updated (UniFi reactivation pending next sync)"
		return result, nil
	}

	// Find the UniFi user with this NFC token
	for _, u := range unifiUsers {
		for _, token := range u.NfcTokens {
			if token == nfcToken {
				if err := s.unifi.UpdateUserStatus(ctx, u.ID, "ACTIVE"); err != nil {
					s.logger.Error("failed to reactivate in UniFi", "unifiId", u.ID, "error", err)
					result.Reactivated = true
					result.Reason = "renewed in Redpoint, cache updated (UniFi update failed, will retry next sync)"
					return result, nil
				}
				s.logger.Info("user reactivated in UniFi",
					"name", cached.FullName(),
					"unifiId", u.ID,
				)
				result.Reactivated = true
				result.Reason = "membership renewed — reactivated in UniFi"
				return result, nil
			}
		}
	}

	// Didn't find matching UniFi user (shouldn't happen but handle gracefully)
	result.Reactivated = true
	result.Reason = "renewed in Redpoint, cache updated (UniFi user not found by NFC token)"
	return result, nil
}
