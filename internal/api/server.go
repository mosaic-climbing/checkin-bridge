// Package api provides the local admin HTTP API for health, stats, and management.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/auditlog"
	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/cache"
	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

type Server struct {
	handler      *checkin.Handler
	unifi        *unifi.Client
	redpoint     *redpoint.Client
	cardMapper   *cardmap.Mapper
	syncer       *cache.Syncer
	statusSyncer *statusync.Syncer
	ingester     *ingest.Ingester
	sessions     *SessionManager
	audit        *auditlog.Logger
	gateID       string
	logger       *slog.Logger
	// mux serves the public data plane: /health, /stats, /ui/*, the
	// read-only /checkins, /directory/search, etc. Bound to BindAddr:Port.
	mux *http.ServeMux
	// controlMux serves the control plane: the two routes that cause
	// physical-world side effects (POST /unlock/{doorId}, devhooks-gated
	// POST /test-checkin) and aren't called by the staff UI directly.
	// Wired by cmd/bridge to a second http.Server bound to ControlBindAddr
	// (default 127.0.0.1) on ControlPort. The bulk-sync mutations stay on
	// mux because the staff UI posts to them from the browser. See A1 in
	// docs/architecture-review.md.
	controlMux *http.ServeMux
	store      *store.Store
	ui         *ui.Handler
	metrics    *metrics.Registry
	// trustedProxies is the parsed CIDR list supplied by SecurityConfig.
	// Used by handleUILogin and any other handler that needs a peer
	// identity consistent with the IP allowlist / CSRF logging paths.
	trustedProxies []*net.IPNet
	// bg is the supervised goroutine group for long-running background tasks
	// (directory sync, cache sync, status sync, reconnect backfill). Provides
	// a unified context cancellation and graceful shutdown. See S6 in
	// docs/architecture-review.md.
	bg *bg.Group
	// enableTestHooks gates the /test-checkin simulation route. Has no
	// effect unless the binary was built with -tags devhooks (the default
	// production build compiles a stub that ignores this field). This is
	// belt-and-suspenders defence: a devhooks binary accidentally deployed
	// to prod still won't expose the route unless EnableTestHooks=true.
	// See S5 in docs/architecture-review.md.
	enableTestHooks bool
	// htmlCache caches rendered HTML fragments with TTL invalidation.
	// See P1 in docs/architecture-review.md.
	htmlCache *htmlCache
	// breakerResetter is called by POST /debug/reset-breakers to
	// force-close the recheck circuit breaker. Required at construction
	// (NewServer panics if nil); the debug endpoint exists unconditionally
	// in the binary so requiring the wire-up at boot is the right
	// fail-fast — a missing wire silently 503'd before this PR.
	//
	// We hold a bare func rather than a *recheck.Service to avoid an
	// import cycle and to keep the Server's dependency surface narrow.
	// The endpoint only needs "press the reset button"; the return value
	// is the "was it open?" flag for the HTTP response.
	breakerResetter func() (wasOpen bool)

	// mirrorWalk runs one pass of the local Redpoint customer mirror
	// when POST /admin/mirror/resync is invoked. Required at construction;
	// see breakerResetter for the fail-fast rationale.
	//
	// Bare func rather than *mirror.Walker keeps this package free of a
	// dependency on internal/mirror.
	mirrorWalk func(ctx context.Context) error

	// uaHubMirrorRefresh runs one pass of the UA-Hub directory mirror
	// when POST /ua-hub/sync is invoked. Required at construction.
	//
	// Returns UAHubRefreshStats (mirrors unifimirror.Stats) so the
	// handler can display what the pass observed without the api
	// package having to import unifimirror.
	//
	// progress is optional and may be nil. When non-nil the handler
	// passes a closure that writes to jobs.progress for the in-flight
	// jobID so the staff /ui/sync pill can show mid-flight phase
	// updates.
	uaHubMirrorRefresh func(ctx context.Context, progress func(phase string)) (UAHubRefreshStats, error)

	// instanceName labels this process as "prod" (the default) or "stage"
	// in the /health response. Useful when prod and stage co-exist on the
	// same host with adjacent ports (3500 vs 3600). The runtime invariant
	// "stage implies shadow" is enforced by config.validate(), not here.
	instanceName string
}

// UAHubRefreshStats is the result payload the UA-Hub mirror refresh
// callback returns. Mirrors unifimirror.Stats shape so the wiring in
// cmd/bridge is a one-liner, but lives here so the api package
// doesn't import unifimirror (keeping the dependency direction clean
// and avoiding an import cycle when tests pass a fake refresher).
//
// Hydrated and Rechecked track side-effects of the refresh introduced
// in v0.5.5: Hydrated is the count of mirror rows whose email came back
// blank from the paginated list and was backfilled via a per-user
// FetchUser; Rechecked is the count of pending-match rows that were
// promoted to a confirmed mapping after a hydrated email landed a
// single Redpoint customer. Surfaced here (and in the handler JSON)
// so staff can tell at a glance whether hydration and recheck ran.
type UAHubRefreshStats struct {
	Observed    int
	Upserted    int
	Hydrated    int
	Rechecked   int
	MirrorTotal int
	Duration    time.Duration
}

// ServerDeps groups every dependency NewServer needs. Replaces the
// 17-positional-arg constructor and the four post-construction setters
// (SetInstanceName, SetBreakerResetter, SetMirrorWalker,
// SetUAHubMirrorRefresher) that used to paper over a wiring cycle in
// cmd/bridge.
//
// The pre-PR pattern was to construct the Server with a wide signature,
// then call four setters with callbacks captured from objects that
// hadn't existed at construction time. A forgotten setter silently
// 503'd the corresponding admin endpoint — there was no fail-fast at
// boot. NewServer now panics if any of the four required callbacks are
// missing, so a missed wire shows up at startup, not the first time an
// operator presses Reset Breakers.
//
// InstanceName is optional and defaults to "" (empty string renders as
// the unlabelled /health response). BreakerResetter, MirrorWalker, and
// UAHubMirrorRefresher are required. The seventeen subsystem
// dependencies are required too — none of them have meaningful zero
// values for production.
type ServerDeps struct {
	Handler             *checkin.Handler
	Unifi               *unifi.Client
	Redpoint            *redpoint.Client
	CardMapper          *cardmap.Mapper
	Syncer              *cache.Syncer
	StatusSyncer        *statusync.Syncer
	Ingester            *ingest.Ingester
	Sessions            *SessionManager
	Audit               *auditlog.Logger
	GateID              string
	Logger              *slog.Logger
	Store               *store.Store
	UI                  *ui.Handler
	Metrics             *metrics.Registry
	TrustedProxies      []*net.IPNet
	BG                  *bg.Group
	EnableTestHooks     bool
	InstanceName        string
	BreakerResetter     func() (wasOpen bool)
	MirrorWalker        func(ctx context.Context) error
	UAHubMirrorRefresher func(ctx context.Context, progress func(phase string)) (UAHubRefreshStats, error)
}

// NewServer constructs the api.Server. Panics on a missing required
// dependency — that's deliberate; the alternative (silent 503s on
// admin endpoints) is what this struct exists to prevent.
func NewServer(deps ServerDeps) *Server {
	if deps.BreakerResetter == nil {
		panic("api.NewServer: BreakerResetter is required (POST /debug/reset-breakers depends on it)")
	}
	if deps.MirrorWalker == nil {
		panic("api.NewServer: MirrorWalker is required (POST /admin/mirror/resync depends on it)")
	}
	if deps.UAHubMirrorRefresher == nil {
		panic("api.NewServer: UAHubMirrorRefresher is required (POST /ua-hub/sync depends on it)")
	}
	s := &Server{
		handler:            deps.Handler,
		unifi:              deps.Unifi,
		redpoint:           deps.Redpoint,
		cardMapper:         deps.CardMapper,
		syncer:             deps.Syncer,
		statusSyncer:       deps.StatusSyncer,
		ingester:           deps.Ingester,
		sessions:           deps.Sessions,
		audit:              deps.Audit,
		gateID:             deps.GateID,
		logger:             deps.Logger,
		mux:                http.NewServeMux(),
		controlMux:         http.NewServeMux(),
		store:              deps.Store,
		ui:                 deps.UI,
		metrics:            deps.Metrics,
		trustedProxies:     deps.TrustedProxies,
		bg:                 deps.BG,
		enableTestHooks:    deps.EnableTestHooks,
		htmlCache:          newHTMLCache(),
		breakerResetter:    deps.BreakerResetter,
		mirrorWalk:         deps.MirrorWalker,
		uaHubMirrorRefresh: deps.UAHubMirrorRefresher,
		instanceName:       deps.InstanceName,
	}
	s.routes()
	return s
}

// clientIP returns the effective peer identity for policy decisions,
// honouring the trusted-proxies list. Handlers should prefer this over
// r.RemoteAddr so per-IP lockouts, audit entries, and log lines see the
// same identity the security middleware did.
func (s *Server) clientIP(r *http.Request) string {
	return extractClientIP(r, s.trustedProxies)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ControlHandler returns the control-plane http.Handler: the mux that owns
// the privileged operator-initiated routes (unlock, cache/directory/status
// sync, ingest, devhooks /test-checkin). cmd/bridge wires this to a second
// http.Server bound to 127.0.0.1:ControlPort so these endpoints are only
// reachable from the host itself — an attacker who pivots into the gym LAN
// still can't pop doors without a foothold on the bridge host. See A1 in
// docs/architecture-review.md.
func (s *Server) ControlHandler() http.Handler {
	return s.controlMux
}

// Route timeout constants. Short for quick lookups, long for batch operations.
const (
	shortTimeout = 30 * time.Second
	longTimeout  = 15 * time.Minute
	syncTimeout  = 45 * time.Minute
)

// withTimeout wraps a handler with a per-route context deadline.
func withTimeout(d time.Duration, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		fn(w, r.WithContext(ctx))
	}
}

func (s *Server) routes() {
	// ── Fast endpoints (30s) ────────────────────────────────
	s.mux.HandleFunc("GET /health", withTimeout(shortTimeout, s.handleHealth))
	s.mux.HandleFunc("GET /stats", withTimeout(shortTimeout, s.handleStats))
	s.mux.HandleFunc("GET /doors", withTimeout(shortTimeout, s.handleDoors))
	s.mux.HandleFunc("GET /gates", withTimeout(shortTimeout, s.handleGates))
	s.mux.HandleFunc("GET /checkins", withTimeout(shortTimeout, s.handleCheckins))
	s.mux.HandleFunc("GET /export/checkins", withTimeout(longTimeout, s.handleExportCheckins))
	s.mux.HandleFunc("GET /customer/{externalId}", withTimeout(shortTimeout, s.handleCustomerLookup))

	// Card override mappings
	s.mux.HandleFunc("GET /cards", withTimeout(shortTimeout, s.handleListCards))
	s.mux.HandleFunc("POST /cards", withTimeout(shortTimeout, s.handleAddCard))
	s.mux.HandleFunc("DELETE /cards/{cardUid}", withTimeout(shortTimeout, s.handleDeleteCard))

	// Cache reads
	s.mux.HandleFunc("GET /cache", withTimeout(shortTimeout, s.handleCacheStats))
	s.mux.HandleFunc("GET /cache/members", withTimeout(shortTimeout, s.handleCacheMembers))

	// Directory reads
	s.mux.HandleFunc("GET /directory/status", withTimeout(shortTimeout, s.handleDirectoryStatus))
	s.mux.HandleFunc("GET /directory/search", withTimeout(shortTimeout, s.handleDirectorySearch))

	// Member management. The bridge-side "create a UA-Hub user" flow
	// (v0.4.x–v0.5.8 /ui/members/new + POST /members) was removed in
	// v0.5.9: UA-Hub is the source of truth for user identity, so staff
	// create users there and the bridge auto-binds on sync (or the row
	// lands in Needs Match for manual pairing). DELETE /members stays
	// as the "drop from bridge cache" action and is shared by the
	// member-table row button and the member detail panel.
	s.mux.HandleFunc("DELETE /members/{externalId}", withTimeout(shortTimeout, s.handleRemoveMember))

	// Staff UI (auth handled by session cookies, not admin API key)
	s.mux.HandleFunc("GET /ui", s.handleUI)
	s.mux.HandleFunc("GET /ui/", s.handleUI)
	s.mux.HandleFunc("POST /ui/login", withTimeout(shortTimeout, s.handleUILogin))
	s.mux.HandleFunc("POST /ui/logout", withTimeout(shortTimeout, s.handleUILogout))

	// HTMX UI pages (session required, handled by middleware)
	s.mux.HandleFunc("GET /ui/page/{page}", s.handleUIPage)

	// HTMX fragments (for dynamic partial updates)
	s.mux.HandleFunc("GET /ui/frag/stats", withTimeout(shortTimeout, s.handleFragStats))
	s.mux.HandleFunc("GET /ui/frag/recent-checkins", withTimeout(shortTimeout, s.handleFragRecentCheckins))
	s.mux.HandleFunc("GET /ui/frag/member-table", withTimeout(shortTimeout, s.handleFragMemberTable))
	s.mux.HandleFunc("GET /ui/frag/search-results", withTimeout(shortTimeout, s.handleFragSearchResults))
	s.mux.HandleFunc("GET /ui/frag/checkin-stats", withTimeout(shortTimeout, s.handleFragCheckinStats))
	s.mux.HandleFunc("GET /ui/frag/checkin-table", withTimeout(shortTimeout, s.handleFragCheckinTable))
	s.mux.HandleFunc("GET /ui/frag/job-table", withTimeout(shortTimeout, s.handleFragJobTable))
	s.mux.HandleFunc("GET /ui/frag/policy-table", withTimeout(shortTimeout, s.handleFragPolicyTable))
	s.mux.HandleFunc("GET /ui/frag/metrics-summary", withTimeout(shortTimeout, s.handleFragMetricsSummary))
	s.mux.HandleFunc("GET /ui/frag/shadow-decisions", withTimeout(shortTimeout, s.handleFragShadowDecisions))
	s.mux.HandleFunc("GET /ui/frag/unmatched-table", withTimeout(longTimeout, s.handleFragUnmatchedTable))

	// v0.5.1 sync-page "Last run" pills. Backs the hx-get on each
	// sync card so the pill auto-refreshes after a click (via
	// hx-swap-oob in SyncResultFragment) and on page load. See
	// internal/api/sync_ux.go:handleFragSyncLastRun for the type
	// allowlist and "never run"/"running"/"failed" variants.
	s.mux.HandleFunc("GET /ui/frag/sync-last-run/{type}", withTimeout(shortTimeout, s.handleFragSyncLastRun))

	// v0.5.7.1 stale-running unstick. Backs the "Clear stuck" link
	// rendered inside the running-pill when a job has been in
	// 'running' for >= unstickAgeThreshold (see SyncLastRunPill in
	// internal/ui/fragments.go). Marks the active row failed with a
	// human-readable reason and re-renders the pill so the swap is
	// visible to staff in one click — no ssh required. The original
	// reason this is needed is the v0.5.7.0 ctx-cancel bug
	// (finishSyncJob on a cancelled request silently no-op'd the
	// terminal-state UPDATE), patched in v0.5.7.1's
	// finishSyncJob; the unstick endpoint is the operator escape
	// hatch for any residual cases (mid-refresh daemon crash,
	// SQLite lock contention) where a job legitimately wedges.
	s.mux.HandleFunc("POST /ui/sync/unstick/{type}", withTimeout(shortTimeout, s.handleSyncUnstick))

	// Door policy management (from HTMX UI)
	s.mux.HandleFunc("POST /ui/frag/door-policy", withTimeout(shortTimeout, s.handleAddDoorPolicy))
	s.mux.HandleFunc("DELETE /ui/frag/door-policy/{doorId}", withTimeout(shortTimeout, s.handleDeleteDoorPolicy))

	// "Needs Match" staff UI (C2). List + detail fragments read the
	// ua_user_mappings_pending table directly; mutations (match / skip /
	// defer) hit UA-Hub + audit trail + pending table in a single
	// transaction-ish sequence. All five endpoints go through the
	// /ui/frag/* session-auth branch; mutating methods additionally pass
	// the CSRF gate (middleware.go: SecurityMiddleware /ui/* branch).
	s.mux.HandleFunc("GET /ui/frag/unmatched-list", withTimeout(longTimeout, s.handleFragUnmatchedList))
	s.mux.HandleFunc("GET /ui/frag/unmatched/{uaUserId}/detail", withTimeout(longTimeout, s.handleFragUnmatchedDetail))
	s.mux.HandleFunc("POST /ui/frag/unmatched/{uaUserId}/search", withTimeout(longTimeout, s.handleFragUnmatchedSearch))
	s.mux.HandleFunc("POST /ui/frag/unmatched/{uaUserId}/match", withTimeout(shortTimeout, s.handleFragUnmatchedMatch))
	s.mux.HandleFunc("POST /ui/frag/unmatched/{uaUserId}/skip", withTimeout(shortTimeout, s.handleFragUnmatchedSkip))
	s.mux.HandleFunc("POST /ui/frag/unmatched/{uaUserId}/defer", withTimeout(shortTimeout, s.handleFragUnmatchedDefer))

	// Member detail + recovery actions (v0.5.9). Mirrors the Needs-Match
	// detail panel pattern: the member table renders a "Details" button
	// per row that hx-get's the detail fragment into a sticky panel
	// above the table; the detail fragment exposes Unbind, Reactivate,
	// Remove, and Reassign NFC card. Every mutation writes to
	// match_audit, invalidates the member-table cache, and returns an
	// HTML alert fragment (plus HX-Trigger: member-updated so the
	// table auto-refreshes).
	s.mux.HandleFunc("GET /ui/frag/member/{nfcUid}/detail", withTimeout(shortTimeout, s.handleFragMemberDetail))
	s.mux.HandleFunc("POST /ui/frag/member/{nfcUid}/unbind", withTimeout(shortTimeout, s.handleFragMemberUnbind))
	s.mux.HandleFunc("POST /ui/frag/member/{nfcUid}/reactivate", withTimeout(shortTimeout, s.handleFragMemberReactivate))
	s.mux.HandleFunc("GET /ui/frag/member/{nfcUid}/reassign", withTimeout(shortTimeout, s.handleFragMemberReassign))
	s.mux.HandleFunc("POST /ui/frag/member/{nfcUid}/reassign/search", withTimeout(shortTimeout, s.handleFragMemberReassignSearch))
	s.mux.HandleFunc("POST /ui/frag/member/{nfcUid}/reassign/confirm", withTimeout(shortTimeout, s.handleFragMemberReassignConfirm))

	// ── Long-running endpoints (15–45 min) ──────────────────
	//
	// The bulk-sync mutations stay on the public mux because the staff UI
	// posts to them directly from the browser via HTMX (sync.html hx-post
	// to /cache/sync, /status-sync, /directory/sync, /ingest/unifi). They
	// are still gated by SecurityMiddleware's admin-key-OR-session check;
	// moving them to the loopback-bound control plane would break the UI
	// without a companion /ui/sync/* proxy refactor (tracked separately
	// under the S8 cookie-path-scoping follow-up in internal/api/session.go).
	s.mux.HandleFunc("POST /cache/sync", withTimeout(longTimeout, s.handleCacheSync))
	s.mux.HandleFunc("POST /directory/sync", withTimeout(longTimeout, s.handleDirectorySync))
	s.mux.HandleFunc("GET /unifi/users", withTimeout(longTimeout, s.handleUniFiUsers))
	s.mux.HandleFunc("POST /ingest/unifi", withTimeout(syncTimeout, s.handleIngestUniFi))
	s.mux.HandleFunc("GET /ingest/unmatched", withTimeout(longTimeout, s.handleUnmatched))
	s.mux.HandleFunc("POST /status-sync", withTimeout(syncTimeout, s.handleStatusSync))
	s.mux.HandleFunc("GET /status-sync", withTimeout(shortTimeout, s.handleStatusSyncStatus))
	// POST /ua-hub/sync — refresh the UA-Hub user directory mirror (v0.5.2).
	// longTimeout because ListAllUsersWithStatus walks the full UA-Hub
	// directory (~17 pages × 10s at LEF) and we'd rather the HTTP
	// request wait than fire-and-forget; matches /cache/sync's shape.
	s.mux.HandleFunc("POST /ua-hub/sync", withTimeout(longTimeout, s.handleUAHubSync))

	// Debug / incident-recovery routes. Gated by SecurityMiddleware's
	// admin-key-OR-session path since /debug/* is neither public nor /ui/*.
	// Intentionally kept on the public data-plane mux (not the control
	// plane) because operators trigger it from the same staff browser
	// session they use for /ui/*, and the action is auditable & reversible
	// rather than door-touching. See P3 in docs/architecture-review.md.
	s.mux.HandleFunc("POST /debug/reset-breakers", withTimeout(shortTimeout, s.handleDebugResetBreakers))

	// ── Control-plane routes ────────────────────────────────
	//
	// POST /unlock/{doorId} and the devhooks-gated POST /test-checkin move
	// to controlMux, which cmd/bridge wires to a second http.Server bound
	// to 127.0.0.1 on ControlPort. Both routes can trigger a physical door
	// unlock pulse; isolating them on the loopback listener means an
	// attacker who pivots into the gym LAN still can't pop doors without
	// a foothold on the bridge host itself (plus the admin API key). See
	// A1 in docs/architecture-review.md.
	//
	// The scope was deliberately kept narrow: these are the two routes
	// that (a) cause physical-world side effects and (b) are not called
	// by the staff UI from the browser. The bulk-sync mutations remain on
	// the public mux above because the UI posts to them from the browser.
	//
	// /test-checkin is a development-only simulation hook. It is NOT
	// registered in the default production build — the route only exists
	// when the binary is compiled with `-tags devhooks`. Even in that
	// build it stays unregistered unless EnableTestHooks=true is set in
	// the config. A stolen admin API key on a prod build therefore can't
	// mint fake check-ins or trigger physical unlock pulses via this path.
	// See S5 in docs/architecture-review.md and testhooks_{on,off}.go.
	s.registerTestHooks(shortTimeout)
	s.controlMux.HandleFunc("POST /unlock/{doorId}", withTimeout(shortTimeout, s.handleUnlock))

	// Mirror control endpoints. Control-plane (not public mux) because:
	//   - resync triggers a long-running network operation against
	//     Redpoint; it's an operator/cron action, not a UI-from-browser
	//     one. The Quick-sync UI button goes through /directory/sync on
	//     the public mux for that exact reason.
	//   - stats reports the shape of the mirror (badge_status counts);
	//     operators use it via CLI alongside the rest of the control
	//     surface. Keeping it on the control mux means one bearer token
	//     handles both read and write for mirror operations, matching
	//     the /unlock pattern.
	s.controlMux.HandleFunc("POST /admin/mirror/resync", withTimeout(shortTimeout, s.handleMirrorResync))
	s.controlMux.HandleFunc("GET /admin/mirror/stats", withTimeout(shortTimeout, s.handleMirrorStats))

	// UA-Hub single-user probe. Operator-only; returns what UA-Hub
	// actually sends for a given user ID so staff can diagnose shape
	// issues (e.g. the "list omits email" discovery that motivated the
	// v0.5.5 hydration pass) without shelling into the box.
	s.controlMux.HandleFunc("GET /admin/ua-hub/fetch/{id}", withTimeout(shortTimeout, s.handleUAHubFetchUser))
}

// ─── Health & Stats ──────────────────────────────────────────



var startTime = time.Now()

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
