// Package api provides the local admin HTTP API for health, stats, and management.
package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
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
	mux          *http.ServeMux
	// controlMux serves the control plane: the two routes that cause
	// physical-world side effects (POST /unlock/{doorId}, devhooks-gated
	// POST /test-checkin) and aren't called by the staff UI directly.
	// Wired by cmd/bridge to a second http.Server bound to ControlBindAddr
	// (default 127.0.0.1) on ControlPort. The bulk-sync mutations stay on
	// mux because the staff UI posts to them from the browser. See A1 in
	// docs/architecture-review.md.
	controlMux   *http.ServeMux
	store        *store.Store
	ui           *ui.Handler
	metrics      *metrics.Registry
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
	// allowNewMembers gates the /ui/members/new provisioning routes (C2
	// Layer 4d). Mirrors cfg.Bridge.AllowNewMembers. When false the routes
	// are still registered but every handler short-circuits with a 403 +
	// "feature disabled" alert fragment — same defence-in-depth shape as
	// enableTestHooks. The boot-time config validator in internal/config
	// already refuses to start with AllowNewMembers=true and an empty
	// DefaultAccessPolicyIDs list, so by the time this field is true we
	// know defaultAccessPolicyIDs is non-empty.
	allowNewMembers bool
	// defaultAccessPolicyIDs is the list of UA-Hub access-policy IDs the
	// /ui/members/new flow attaches to every freshly-created user (§3.6
	// of the UA-Hub API). UA-Hub creates users with no policies attached
	// by default, so this is mandatory — without it the user exists but
	// every tap denies. Boot validation enforces non-empty when
	// AllowNewMembers=true, see config.validate().
	defaultAccessPolicyIDs []string
	// htmlCache caches rendered HTML fragments with TTL invalidation.
	// See P1 in docs/architecture-review.md.
	htmlCache *htmlCache
	// breakerResetter, when non-nil, is called by POST /debug/reset-breakers
	// to force-close the recheck circuit breaker. Wired via
	// SetBreakerResetter from cmd/bridge after the recheck.Service is
	// constructed. We use a function-typed setter rather than a direct
	// *recheck.Service reference to avoid an import cycle back into recheck
	// (which is deliberately upstream of api) and to keep the Server's
	// dependency surface narrow — the debug endpoint genuinely only needs
	// "press the reset button" and nothing else from the Service. The
	// return value propagates the "was it open?" flag up into the HTTP
	// response so operators can tell a meaningful reset from a no-op.
	breakerResetter func() (wasOpen bool)

	// mirrorWalk, when non-nil, runs one pass of the local customer
	// mirror when POST /admin/mirror/resync is invoked. Wired via
	// SetMirrorWalker from cmd/bridge so the Server's constructor
	// signature stays stable (same rationale as breakerResetter:
	// NewServer already has 18 positional args; a 19th for a
	// cross-package ref would be noisy).
	//
	// We store a bare func rather than a *mirror.Walker to keep this
	// package free of a dependency on internal/mirror. The handler
	// doesn't need anything from the walker beyond "run once with
	// this ctx" — that's exactly what a func captures.
	mirrorWalk func(ctx context.Context) error

	// uaHubMirrorRefresh, when non-nil, runs one pass of the UA-Hub
	// directory mirror when POST /ua-hub/sync is invoked. Wired via
	// SetUAHubMirrorRefresher from cmd/bridge after the unifimirror
	// Syncer is constructed. The function returns a UAHubRefreshStats
	// value so the handler can show the operator what the pass
	// observed without the api package having to import the
	// unifimirror package (and pull in the Syncer type into every
	// test file transitively).
	//
	// Same setter-based rationale as breakerResetter and mirrorWalk:
	// keep NewServer's constructor signature stable, and keep the
	// api → unifimirror dependency implicit (via callback) rather
	// than explicit (via struct field).
	uaHubMirrorRefresh func(ctx context.Context) (UAHubRefreshStats, error)
}

// UAHubRefreshStats is the result payload the UA-Hub mirror refresh
// callback returns. Mirrors unifimirror.Stats shape so the wiring in
// cmd/bridge is a one-liner, but lives here so the api package
// doesn't import unifimirror (keeping the dependency direction clean
// and avoiding an import cycle when tests pass a fake refresher).
type UAHubRefreshStats struct {
	Observed    int
	Upserted    int
	MirrorTotal int
	Duration    time.Duration
}

func NewServer(
	handler *checkin.Handler,
	unifiClient *unifi.Client,
	redpointClient *redpoint.Client,
	cardMapper *cardmap.Mapper,
	syncer *cache.Syncer,
	statusSyncer *statusync.Syncer,
	ingester *ingest.Ingester,
	sessionMgr *SessionManager,
	auditLogger *auditlog.Logger,
	gateID string,
	logger *slog.Logger,
	db *store.Store,
	uiHandler *ui.Handler,
	met *metrics.Registry,
	trustedProxies []*net.IPNet,
	bgGroup *bg.Group,
	enableTestHooks bool,
	allowNewMembers bool,
	defaultAccessPolicyIDs []string,
) *Server {
	s := &Server{
		handler:                handler,
		unifi:                  unifiClient,
		redpoint:               redpointClient,
		cardMapper:             cardMapper,
		syncer:                 syncer,
		statusSyncer:           statusSyncer,
		ingester:               ingester,
		sessions:               sessionMgr,
		audit:                  auditLogger,
		gateID:                 gateID,
		logger:                 logger,
		mux:                    http.NewServeMux(),
		controlMux:             http.NewServeMux(),
		store:                  db,
		ui:                     uiHandler,
		metrics:                met,
		trustedProxies:         trustedProxies,
		bg:                     bgGroup,
		enableTestHooks:        enableTestHooks,
		allowNewMembers:        allowNewMembers,
		defaultAccessPolicyIDs: defaultAccessPolicyIDs,
		htmlCache:              newHTMLCache(),
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

// SetBreakerResetter registers the callback used by POST /debug/reset-breakers
// to force-close the recheck circuit breaker. Pass nil to disable the
// endpoint (it will 503); cmd/bridge always wires the recheck.Service's
// ResetBreaker method here.
//
// Setter-based rather than constructor-arg because NewServer's signature
// is already wide (19 positional args as of C2 Layer 4d) and the
// rechecker is constructed after the Server in cmd/bridge today. Adding
// this as a 20th positional arg would be a noisy refactor for a single
// debug-only wire-up.
func (s *Server) SetBreakerResetter(fn func() bool) {
	s.breakerResetter = fn
}

// SetMirrorWalker registers the callback used by POST /admin/mirror/resync
// to kick off a mirror walk. Pass nil to leave the endpoint disabled (it
// will 503). cmd/bridge wires mirror.Walker.Walk here after the Walker is
// constructed.
//
// Setter-based for the same reason as SetBreakerResetter: keep NewServer's
// signature stable, and avoid pulling internal/mirror into internal/api's
// import graph (this package is already wide; the admin endpoint only
// needs the "run once" verb).
func (s *Server) SetMirrorWalker(fn func(ctx context.Context) error) {
	s.mirrorWalk = fn
}

// SetUAHubMirrorRefresher registers the callback used by POST /ua-hub/sync
// to refresh the local UA-Hub user directory mirror. Pass nil to leave
// the endpoint disabled (it will 503). cmd/bridge wires the unifimirror
// Syncer's RefreshWithStats method here after constructing the Syncer.
//
// Setter-based to keep the NewServer constructor signature stable and
// to avoid an api → unifimirror import (which would otherwise need to
// be reversed because the api test suite builds fake Server values
// without the mirror package loaded).
func (s *Server) SetUAHubMirrorRefresher(fn func(ctx context.Context) (UAHubRefreshStats, error)) {
	s.uaHubMirrorRefresh = fn
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

	// Member management
	s.mux.HandleFunc("POST /members", withTimeout(shortTimeout, s.handleAddMember))
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

	// "New Member" provisioning UI (C2 Layer 4d). All six routes are
	// gated by the requireProvisioning() guard which 403s with a friendly
	// fragment when AllowNewMembers=false. The page route serves the
	// static form skeleton via the existing UI page renderer; the other
	// five are HTMX fragment endpoints driving the orchestration:
	//
	//   GET    /ui/members/new                         — form page
	//   GET    /ui/members/new/lookup?email=…          — live email validation
	//   POST   /ui/members/new                         — §3.2 + §3.6 + map + audit
	//   POST   /ui/members/new/{id}/enroll             — §6.2 start enrollment
	//   GET    /ui/members/new/{id}/enroll/{sid}/poll  — §6.3 + §6.7 + §3.7
	//   DELETE /ui/members/new/{id}/enroll/{sid}       — §6.4 cleanup
	//
	// See docs/architecture-review.md C2 §"New-user provisioning flow"
	// for the call orchestration and guardrail rationale.
	s.mux.HandleFunc("GET /ui/members/new", s.handleMembersNewPage)
	s.mux.HandleFunc("GET /ui/members/new/lookup", withTimeout(shortTimeout, s.handleMembersNewLookup))
	s.mux.HandleFunc("POST /ui/members/new", withTimeout(shortTimeout, s.handleMembersNewCreate))
	s.mux.HandleFunc("POST /ui/members/new/{uaUserId}/enroll", withTimeout(shortTimeout, s.handleMembersNewEnrollStart))
	s.mux.HandleFunc("GET /ui/members/new/{uaUserId}/enroll/{sessionId}/poll", withTimeout(shortTimeout, s.handleMembersNewEnrollPoll))
	s.mux.HandleFunc("DELETE /ui/members/new/{uaUserId}/enroll/{sessionId}", withTimeout(shortTimeout, s.handleMembersNewEnrollCancel))

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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var totalMembers, activeMembers int
	if s.store != nil {
		if stats, err := s.store.MemberStats(r.Context()); err == nil && stats != nil {
			totalMembers = stats.Total
			activeMembers = stats.Active
		}
	}
	writeJSON(w, map[string]any{
		"status":             "ok",
		"service":            "mosaic-checkin-bridge",
		"mode":               "store-first",
		"unifiConnected":     s.unifi.Connected(),
		"cacheMembers":       totalMembers,
		"cacheActiveMembers": activeMembers,
		"cardOverrides":      len(s.cardMapper.AllOverrides()),
		"redpointGateId":     s.gateID,
		"uptime":             time.Since(startTime).String(),
		"timestamp":          time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.handler.GetStats())
}

// ─── UniFi ───────────────────────────────────────────────────

func (s *Server) handleDoors(w http.ResponseWriter, r *http.Request) {
	doors, err := s.unifi.ListDoors(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"doors": doors})
}

// ─── Redpoint ────────────────────────────────────────────────

func (s *Server) handleGates(w http.ResponseWriter, r *http.Request) {
	gates, err := s.redpoint.ListGates(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"gates": gates,
		"hint":  "Set REDPOINT_GATE_ID in .env to the id of your entrance gate",
	})
}

// handleCheckins returns the last N check-ins. Source is controlled by the
// `source` query parameter:
//
//   - `source=local` (default): reads from the bridge's own sqlite `checkins`
//     table. Free (no outbound calls), returns denied/shadow events too, and
//     is what the UI fragments use. This is the right choice for polling.
//   - `source=redpoint`: proxies live to Redpoint's GraphQL API. Returns only
//     what Redpoint has recorded; each call costs Redpoint quota. Use only
//     when callers specifically want the authoritative Redpoint view.
//
// Response envelope is identical for both sources:
//
//	{"checkIns": [...source-native items...], "total": N, "source": "local|redpoint"}
//
// Item shape DIFFERS between sources — local events are flat (timestamp,
// customerId, customerName, doorId, doorName, result, unifiResult,
// redpointRecorded); Redpoint items have nested customer/gate/facility
// objects. Clients should branch on the `source` field if they need to
// interpret individual items.
//
// See P2 in docs/architecture-review.md — before this fix, the default was
// `redpoint`, which meant a single UI tab polling every few seconds cost
// ~28k Redpoint calls/day.
func (s *Server) handleCheckins(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500 // hard cap to prevent accidental fanout
	}

	source := r.URL.Query().Get("source")
	if source == "" {
		source = "local"
	}

	switch source {
	case "local":
		if s.store == nil {
			writeError(w, http.StatusServiceUnavailable, "local store not configured")
			return
		}
		events, err := s.store.RecentCheckIns(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"checkIns": events,
			"total":    len(events),
			"source":   "local",
		})
	case "redpoint":
		list, err := s.redpoint.ListRecentCheckIns(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		checkIns := list.CheckIns
		if checkIns == nil {
			checkIns = []redpoint.CheckIn{}
		}
		writeJSON(w, map[string]any{
			"checkIns": checkIns,
			"total":    list.Total,
			"source":   "redpoint",
		})
	default:
		writeError(w, http.StatusBadRequest, "source must be 'local' or 'redpoint'")
	}
}

// handleExportCheckins streams the local store's check-in events for a given
// date range as either CSV (default) or JSON. Sources from the bridge's own
// sqlite database — not Redpoint — so it includes denied events, shadow-mode
// decisions, and the UniFi result column that live in our store only.
//
// Query params:
//   from=YYYY-MM-DD or RFC3339    (optional — unbounded if empty)
//   to=YYYY-MM-DD or RFC3339      (optional — unbounded if empty, bare dates
//                                  are expanded to end-of-day inside the store)
//   format=csv|json               (default: csv)
//
// Admin-auth only: this route is not in the public middleware allowlist, so
// the security middleware requires admin API key or a staff session.
func (s *Server) handleExportCheckins(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}

	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "csv"
	}

	events, err := s.store.CheckInsBetween(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}

	// Filename label: use date range if provided, else "all".
	label := "all"
	if from != "" || to != "" {
		label = strings.TrimSpace(from + "_to_" + to)
	}

	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="checkins_%s.json"`, label))
		writeJSON(w, map[string]any{
			"from":   from,
			"to":     to,
			"count":  len(events),
			"events": events,
		})
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="checkins_%s.csv"`, label))
		cw := csv.NewWriter(w)
		// Header row — keep column order stable so downstream parsers don't
		// need to rediscover schema between exports.
		_ = cw.Write([]string{
			"id", "timestamp", "nfc_uid", "customer_id", "customer_name",
			"door_id", "door_name", "result", "deny_reason",
			"redpoint_recorded", "redpoint_checkin_id", "unifi_result",
		})
		for _, e := range events {
			_ = cw.Write([]string{
				strconv.Itoa(e.ID),
				e.Timestamp,
				e.NfcUID,
				e.CustomerID,
				e.CustomerName,
				e.DoorID,
				e.DoorName,
				e.Result,
				e.DenyReason,
				strconv.FormatBool(e.RedpointRecorded),
				e.RedpointCheckInID,
				e.UnifiResult,
			})
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			s.logger.Error("csv export flush failed", "error", err)
		}
	default:
		writeError(w, http.StatusBadRequest, "format must be csv or json")
	}
}

func (s *Server) handleCustomerLookup(w http.ResponseWriter, r *http.Request) {
	extID := r.PathValue("externalId")

	// Show both live Redpoint data and cached data for comparison
	resp := map[string]any{}

	// Check local store first (always available)
	if s.store != nil {
		if member, err := s.store.GetMemberByNFC(r.Context(), extID); err == nil && member != nil {
			resp["cached"] = member
			resp["cachedAllowed"] = member.IsAllowed()
		}
	}

	// Also try live Redpoint lookup
	cust, err := s.redpoint.LookupByExternalID(r.Context(), extID)
	if err != nil {
		resp["redpointError"] = err.Error()
	} else if cust == nil {
		resp["redpointCustomer"] = nil
	} else {
		validation := s.redpoint.ValidateCheckIn(cust)
		resp["redpointCustomer"] = cust
		resp["redpointValidation"] = validation
	}

	if len(resp) == 0 {
		writeError(w, http.StatusNotFound, "not found in cache or Redpoint")
		return
	}
	writeJSON(w, resp)
}

// ─── Card Override Mappings ──────────────────────────────────

func (s *Server) handleListCards(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"overrides": s.cardMapper.AllOverrides(),
	})
}

func (s *Server) handleAddCard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CardUID    string `json:"cardUid"`
		CustomerID string `json:"customerId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.CardUID == "" || body.CustomerID == "" {
		writeError(w, http.StatusBadRequest, "cardUid and customerId are required")
		return
	}
	if err := s.cardMapper.SetOverride(body.CardUID, body.CustomerID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Log("card_override_add", r.RemoteAddr, map[string]any{
		"cardUid": body.CardUID, "customerId": body.CustomerID,
	})
	s.htmlCache.Invalidate()
	writeJSON(w, map[string]any{"success": true, "cardUid": body.CardUID, "customerId": body.CustomerID})
}

func (s *Server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	cardUID := r.PathValue("cardUid")
	if err := s.cardMapper.DeleteOverride(cardUID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Log("card_override_delete", r.RemoteAddr, map[string]any{"cardUid": cardUID})
	s.htmlCache.Invalidate()
	writeJSON(w, map[string]any{"success": true})
}

// ─── Cache Management ────────────────────────────────────────

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, map[string]any{})
		return
	}
	stats, err := s.store.MemberStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, stats)
}

func (s *Server) handleCacheMembers(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, map[string]any{"members": []any{}})
		return
	}
	members, err := s.store.AllMembers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"members": members})
}

// handleCacheSync refreshes every cached member's status against Redpoint
// in a single synchronous pass. The call is wrapped in a jobs-table
// lifecycle (running → completed / failed) so the /ui/sync page's "Last
// run" pill and Recent Jobs list can show staff that a sync fired. The
// HTMX response is a rich confirmation fragment; API callers still get
// the original JSON body.
func (s *Server) handleCacheSync(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("manual membership status refresh triggered via API")
	s.audit.Log("cache_sync", r.RemoteAddr, nil)

	jobID := s.startSyncJob(r.Context(), jobTypeCacheSync)

	started := time.Now()
	refreshErr := s.syncer.RefreshAllStatuses(r.Context())

	var stats *store.MemberStats
	if s.store != nil {
		stats, _ = s.store.MemberStats(r.Context())
	}
	duration := time.Since(started).Round(100 * time.Millisecond)

	if refreshErr != nil {
		s.finishSyncJob(r.Context(), jobID, nil, refreshErr)
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeCacheSync, http.StatusOK, false,
				"Cache sync failed",
				"Refresh against Redpoint returned an error. Leaving member cache as it was.",
				[]ui.SyncStat{
					{Label: "Error", Value: refreshErr.Error()},
					{Label: "Duration", Value: duration.String()},
				}, nil)
			return
		}
		writeError(w, http.StatusBadGateway, "status refresh failed: "+refreshErr.Error())
		return
	}

	s.finishSyncJob(r.Context(), jobID, map[string]any{
		"cache":    stats,
		"duration": duration.String(),
	}, nil)

	s.writeSyncResult(w, r, jobTypeCacheSync, http.StatusOK, true,
		"Cache sync complete",
		"Refreshed every cached member's status from Redpoint.",
		syncStatsFromMemberStats(stats, duration),
		map[string]any{
			"success": true,
			"cache":   stats,
		})
}

// handleUAHubSync refreshes the local UA-Hub directory mirror (ua_users)
// synchronously. Added in v0.5.2 alongside the nightly unifimirror
// Syncer — the Syncer owns the daily cadence; this handler lets staff
// force an immediate refresh after a UA-Hub-side edit without waiting
// for the next tick.
//
// Shape mirrors handleCacheSync on purpose: jobs-table lifecycle for
// the /ui/sync page's "last run" pill, HTMX-aware response via
// writeSyncResult, plain JSON fallback for API callers. The refresher
// callback is wired via SetUAHubMirrorRefresher from cmd/bridge;
// when unset, we 503 with a clear message rather than silently
// succeed, so operators notice the wiring gap in dev builds.
func (s *Server) handleUAHubSync(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("manual UA-Hub directory mirror refresh triggered via API")
	s.audit.Log("ua_hub_sync", r.RemoteAddr, nil)

	if s.uaHubMirrorRefresh == nil {
		writeError(w, http.StatusServiceUnavailable, "UA-Hub mirror refresher not wired")
		return
	}

	jobID := s.startSyncJob(r.Context(), jobTypeUAHubSync)
	started := time.Now()

	stats, refreshErr := s.uaHubMirrorRefresh(r.Context())
	duration := time.Since(started).Round(100 * time.Millisecond)

	if refreshErr != nil {
		s.finishSyncJob(r.Context(), jobID, nil, refreshErr)
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeUAHubSync, http.StatusOK, false,
				"UA-Hub sync failed",
				"Couldn't complete the UA-Hub directory refresh. The local mirror is unchanged.",
				[]ui.SyncStat{
					{Label: "Error", Value: refreshErr.Error()},
					{Label: "Duration", Value: duration.String()},
				}, nil)
			return
		}
		writeError(w, http.StatusBadGateway, "UA-Hub sync failed: "+refreshErr.Error())
		return
	}

	s.finishSyncJob(r.Context(), jobID, map[string]any{
		"observed":    stats.Observed,
		"upserted":    stats.Upserted,
		"mirrorTotal": stats.MirrorTotal,
		"duration":    duration.String(),
	}, nil)

	s.writeSyncResult(w, r, jobTypeUAHubSync, http.StatusOK, true,
		"UA-Hub sync complete",
		"Refreshed the local UA-Hub user directory mirror. The Needs Match page and ingest matcher now read from this cache instead of hitting UA-Hub live.",
		[]ui.SyncStat{
			{Label: "Observed", Value: fmt.Sprintf("%d", stats.Observed)},
			{Label: "Upserted", Value: fmt.Sprintf("%d", stats.Upserted)},
			{Label: "Mirror total", Value: fmt.Sprintf("%d", stats.MirrorTotal)},
			{Label: "Duration", Value: duration.String()},
		},
		map[string]any{
			"success":     true,
			"observed":    stats.Observed,
			"upserted":    stats.Upserted,
			"mirrorTotal": stats.MirrorTotal,
		})
}

// syncStatsFromMemberStats unpacks *store.MemberStats into the uniform
// []ui.SyncStat rows the fragment renders. Nil stats (store absent or
// read failure) degrades to just the duration row rather than blowing
// up the response.
func syncStatsFromMemberStats(stats *store.MemberStats, duration time.Duration) []ui.SyncStat {
	rows := []ui.SyncStat{{Label: "Duration", Value: duration.String()}}
	if stats == nil {
		return rows
	}
	rows = append(rows,
		ui.SyncStat{Label: "Members total", Value: fmt.Sprintf("%d", stats.Total)},
		ui.SyncStat{Label: "Active", Value: fmt.Sprintf("%d", stats.Active)},
	)
	if stats.Frozen > 0 {
		rows = append(rows, ui.SyncStat{Label: "Frozen", Value: fmt.Sprintf("%d", stats.Frozen)})
	}
	if stats.Expired > 0 {
		rows = append(rows, ui.SyncStat{Label: "Expired", Value: fmt.Sprintf("%d", stats.Expired)})
	}
	return rows
}

// ─── Customer Directory (SQLite) ─────────────────────────────

// GET /directory/status — check the Redpoint → SQLite sync status.
func (s *Server) handleDirectoryStatus(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, map[string]any{"customers": 0})
		return
	}
	count, _ := s.store.CustomerCount(r.Context())
	state, _ := s.store.GetSyncState(r.Context())
	writeJSON(w, map[string]any{
		"customers": count,
		"sync":      state,
	})
}

// POST /directory/sync — start the Redpoint → SQLite bulk load.
// Runs in the background; poll GET /directory/status to monitor progress.
func (s *Server) handleDirectorySync(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	state, _ := s.store.GetSyncState(r.Context())
	if state != nil && state.Status == "running" {
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeDirectorySync, http.StatusOK, true,
				"Directory sync already running",
				"A bulk customer load kicked off earlier is still in progress. Wait for the Last-run pill to flip to ✓, or check /directory/status for a row count.",
				nil, nil)
			return
		}
		writeJSON(w, map[string]any{
			"message": "sync already in progress",
			"sync":    state,
		})
		return
	}

	s.logger.Info("Redpoint → SQLite directory sync triggered via API")

	jobID := s.startSyncJob(r.Context(), jobTypeDirectorySync)

	// Run in background via the supervised group. The jobID is captured
	// by value so the goroutine can stamp the terminal status even after
	// the request context has long returned.
	s.bg.Go("directory-sync", func(ctx context.Context) error {
		started := time.Now()
		s.bulkLoadCustomers(ctx)
		// bulkLoadCustomers writes its own sync_state row on success/
		// failure; mirror that into the jobs-table job so the pill
		// and Recent Jobs list reflect the same outcome.
		finalState, _ := s.store.GetSyncState(ctx)
		duration := time.Since(started).Round(time.Second)
		if finalState != nil && finalState.Status == "error" {
			s.finishSyncJob(ctx, jobID, nil, fmt.Errorf("%s", finalState.LastError))
			return nil
		}
		result := map[string]any{"duration": duration.String()}
		if finalState != nil {
			result["totalFetched"] = finalState.TotalFetched
			result["completedAt"] = finalState.CompletedAt
		}
		s.finishSyncJob(ctx, jobID, result, nil)
		return nil
	})

	if wantsHTMX(r) {
		s.writeSyncResult(w, r, jobTypeDirectorySync, http.StatusAccepted, true,
			"Directory sync started",
			"Bulk-loading every active Redpoint customer into the local mirror. This can take several minutes for large directories; the Last-run pill will flip to ✓ when done.",
			nil, nil)
		return
	}

	writeJSON(w, map[string]any{
		"message": "sync started — poll GET /directory/status to monitor",
	})
}

// bulkLoadCustomers pages through all Redpoint customers and upserts them into the store.
func (s *Server) bulkLoadCustomers(ctx context.Context) {
	s.store.UpdateSyncState(ctx, &store.SyncState{
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})

	var cursor *string
	totalFetched := 0
	pageSize := 100

	for {
		vars := map[string]any{
			"filter": map[string]any{"active": "ACTIVE"},
			"first":  pageSize,
		}
		if cursor != nil {
			vars["after"] = *cursor
		}

		data, err := s.redpoint.ExecQuery(ctx, `
			query Customers($filter: CustomerFilter!, $first: Int, $after: String) {
				customers(filter: $filter, first: $first, after: $after) {
					pageInfo { hasNextPage endCursor }
					edges {
						node {
							id active firstName lastName email barcode externalId
						}
					}
				}
			}
		`, vars)
		if err != nil {
			s.logger.Error("directory sync page fetch failed", "error", err)
			s.store.UpdateSyncState(ctx, &store.SyncState{
				Status:    "error",
				LastError: err.Error(),
			})
			return
		}

		var result struct {
			Customers struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Edges []struct {
					Node struct {
						ID         string `json:"id"`
						Active     bool   `json:"active"`
						FirstName  string `json:"firstName"`
						LastName   string `json:"lastName"`
						Email      string `json:"email"`
						Barcode    string `json:"barcode"`
						ExternalID string `json:"externalId"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"customers"`
		}

		if err := json.Unmarshal(data, &result); err != nil {
			s.logger.Error("directory sync parse failed", "error", err)
			s.store.UpdateSyncState(ctx, &store.SyncState{
				Status:    "error",
				LastError: err.Error(),
			})
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		batch := make([]store.Customer, len(result.Customers.Edges))
		for i, e := range result.Customers.Edges {
			batch[i] = store.Customer{
				RedpointID: e.Node.ID,
				FirstName:  e.Node.FirstName,
				LastName:   e.Node.LastName,
				Email:      e.Node.Email,
				Barcode:    e.Node.Barcode,
				ExternalID: e.Node.ExternalID,
				Active:     e.Node.Active,
				UpdatedAt:  now,
			}
		}

		if err := s.store.UpsertCustomerBatch(ctx, batch); err != nil {
			s.logger.Error("directory sync batch upsert failed", "error", err)
			s.store.UpdateSyncState(ctx, &store.SyncState{
				Status:    "error",
				LastError: err.Error(),
			})
			return
		}

		totalFetched += len(batch)
		s.logger.Info("directory sync progress", "fetched", totalFetched)

		if !result.Customers.PageInfo.HasNextPage {
			break
		}
		endCursor := result.Customers.PageInfo.EndCursor
		cursor = &endCursor

		s.store.UpdateSyncState(ctx, &store.SyncState{
			Status:       "running",
			TotalFetched: totalFetched,
			LastCursor:   endCursor,
		})
	}

	s.store.UpdateSyncState(ctx, &store.SyncState{
		Status:       "complete",
		TotalFetched: totalFetched,
		CompletedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	s.logger.Info("directory sync complete", "total", totalFetched)
}

// ─── UniFi Ingest ────────────────────────────────────────────

// GET /unifi/users — list all UniFi Access users with NFC credentials.
// Useful for seeing who has NFC tags before running the ingest.
func (s *Server) handleUniFiUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.unifi.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch UniFi users: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"users": users,
		"count": len(users),
	})
}

// POST /ingest/unifi — run the UniFi → Redpoint user mapping pipeline.
//
// Query params:
//
//	?dry_run=true  (default) — preview mappings without writing to cache
//	?dry_run=false           — match users and write to cache
//
// Flow:
//  1. Fetches all UniFi users with NFC credentials
//  2. Fetches all active Redpoint customers
//  3. Matches by email (primary), then by name (fallback)
//  4. Returns full mapping table for review
//  5. If dry_run=false, writes matched+active members to cache
func (s *Server) handleIngestUniFi(w http.ResponseWriter, r *http.Request) {
	dryRun := true
	if r.URL.Query().Get("dry_run") == "false" {
		dryRun = false
	}

	s.logger.Info("UniFi ingest triggered", "dryRun", dryRun)
	s.audit.Log("ingest_start", r.RemoteAddr, map[string]any{"dryRun": dryRun})

	jobID := s.startSyncJob(r.Context(), jobTypeUniFiIngest)
	started := time.Now()

	result, err := s.ingester.Run(r.Context(), dryRun)
	if err != nil {
		s.finishSyncJob(r.Context(), jobID, nil, err)
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeUniFiIngest, http.StatusOK, false,
				"UniFi ingest failed",
				"Couldn't complete the UniFi → Redpoint match pass. Members table unchanged.",
				[]ui.SyncStat{
					{Label: "Error", Value: err.Error()},
					{Label: "Duration", Value: time.Since(started).Round(100 * time.Millisecond).String()},
				}, nil)
			return
		}
		writeError(w, http.StatusBadGateway, "ingest failed: "+err.Error())
		return
	}
	s.audit.Log("ingest_complete", r.RemoteAddr, map[string]any{
		"dryRun": dryRun, "matched": result.Matched,
		"unmatched": result.Unmatched, "applied": result.Applied,
	})
	duration := time.Since(started).Round(100 * time.Millisecond)
	s.finishSyncJob(r.Context(), jobID, map[string]any{
		"dryRun":     dryRun,
		"unifiUsers": result.UniFiUsers,
		"withNfc":    result.WithNFC,
		"matched":    result.Matched,
		"unmatched":  result.Unmatched,
		"applied":    result.Applied,
		"duration":   duration.String(),
	}, nil)

	if wantsHTMX(r) {
		title := "UniFi ingest complete"
		body := fmt.Sprintf("Scanned %d UniFi users (%d with NFC tags) and resolved them against Redpoint. Wrote %d members to the cache.",
			result.UniFiUsers, result.WithNFC, result.Applied)
		if dryRun {
			title = "UniFi ingest — dry run"
			body = fmt.Sprintf("Previewed %d UniFi users (%d with NFC tags) against Redpoint. No writes to the members table. Click \"Run (writes)\" to apply.",
				result.UniFiUsers, result.WithNFC)
		}
		s.writeSyncResult(w, r, jobTypeUniFiIngest, http.StatusOK, true,
			title, body,
			[]ui.SyncStat{
				{Label: "UniFi users", Value: fmt.Sprintf("%d", result.UniFiUsers)},
				{Label: "With NFC", Value: fmt.Sprintf("%d", result.WithNFC)},
				{Label: "Matched", Value: fmt.Sprintf("%d", result.Matched)},
				{Label: "Unmatched", Value: fmt.Sprintf("%d", result.Unmatched)},
				{Label: "Applied", Value: fmt.Sprintf("%d", result.Applied)},
				{Label: "Duration", Value: duration.String()},
			}, nil)
		return
	}

	// ?summary=true returns counts + unmatched/warning list only (no full mappings)
	if r.URL.Query().Get("summary") == "true" {
		type problemEntry struct {
			UniFiName string `json:"unifiName"`
			Warning   string `json:"warning"`
		}
		var unmatched []problemEntry
		var warnings []problemEntry
		for _, m := range result.Mappings {
			if m.Method == ingest.MatchNone {
				unmatched = append(unmatched, problemEntry{m.UniFiName, m.Warning})
			} else if m.Warning != "" {
				warnings = append(warnings, problemEntry{m.UniFiName, m.Warning})
			}
		}
		writeJSON(w, map[string]any{
			"timestamp":  result.Timestamp,
			"dryRun":     result.DryRun,
			"unifiUsers": result.UniFiUsers,
			"withNfc":    result.WithNFC,
			"matched":    result.Matched,
			"unmatched":  result.Unmatched,
			"skipped":    result.Skipped,
			"applied":    result.Applied,
			"unmatchedUsers": unmatched,
			"warningUsers":   warnings,
		})
		return
	}

	writeJSON(w, result)
}

// ─── Testing & Manual Control ────────────────────────────────
//
// handleTestCheckin lives in testhooks_on.go (build tag: devhooks). The
// default production build compiles testhooks_off.go instead, which
// registers no routes. See registerTestHooks above and S5 in the review.

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	doorID := sanitizeID(r.PathValue("doorId"))
	if doorID == "" {
		writeError(w, http.StatusBadRequest, "invalid door ID")
		return
	}
	if err := s.unifi.UnlockDoor(r.Context(), doorID); err != nil {
		writeError(w, http.StatusBadGateway, "unlock failed")
		return
	}
	s.audit.Log("manual_unlock", r.RemoteAddr, map[string]any{"doorId": doorID})
	writeJSON(w, map[string]any{"success": true, "doorId": doorID})
}

// sanitizeID strips path traversal attempts and non-alphanumeric characters
// from resource identifiers. IDs from UniFi/Redpoint are hex/alphanumeric.
func sanitizeID(id string) string {
	// Strip any path components
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		id = id[idx+1:]
	}
	// Allow only alphanumeric, dash, underscore
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return ""
		}
	}
	return id
}

// ─── Staff Auth ─────────────────────────────────────────────

// POST /ui/login — staff login, returns a session cookie.
//
// IMPORTANT: per-IP lockout MUST key off s.clientIP(r), not r.RemoteAddr
// directly. Otherwise, with a trusted proxy in front of the bridge, every
// request would appear to come from the proxy's IP and a single failed
// login from any real user would lock out the entire customer base. And
// without a trusted proxy, using s.clientIP(r) still matches the allowlist
// identity so lockout and allowlist can't disagree about "who is this".
func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	peer := s.clientIP(r)
	// Check if IP is locked out before even reading the body
	if s.sessions.IsLockedOut(peer) {
		s.logger.Warn("login attempt from locked-out IP", "ip", peer)
		if r.Header.Get("HX-Request") == "true" {
			w.WriteHeader(http.StatusTooManyRequests)
			ui.RenderFragment(w, `<span class="error show">Too many failed attempts — try again in a few minutes</span>`)
			return
		}
		writeError(w, http.StatusTooManyRequests, "too many failed attempts — try again in a few minutes")
		return
	}

	// Parse password - support both JSON and form-encoded
	var password string
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		r.ParseForm()
		password = r.FormValue("password")
	} else {
		var body struct {
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		password = body.Password
	}

	if !s.sessions.Authenticate(password, peer) {
		s.logger.Warn("failed staff login attempt", "ip", peer)
		s.audit.Log("login_failed", peer, nil)
		// HTMX response: return error text into the error div
		if r.Header.Get("HX-Request") == "true" {
			w.WriteHeader(http.StatusUnauthorized)
			ui.RenderFragment(w, `<span class="error show">Invalid password</span>`)
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	token, csrf, err := s.sessions.CreateSession()
	if err != nil {
		if r.Header.Get("HX-Request") == "true" {
			w.WriteHeader(http.StatusInternalServerError)
			ui.RenderFragment(w, `<span class="error show">Session error</span>`)
			return
		}
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}

	s.sessions.SetCookie(w, token)
	s.sessions.SetCSRFCookie(w, csrf)
	s.logger.Info("staff logged in via UI", "ip", peer)
	s.audit.Log("login_success", peer, nil)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/ui/")
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, map[string]any{"success": true})
}

// POST /ui/logout — destroy the session.
func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.sessions.cookieName); err == nil {
		s.sessions.DestroySession(c.Value)
	}
	s.sessions.ClearCookie(w)
	s.sessions.ClearCSRFCookie(w)
	s.audit.Log("logout", s.clientIP(r), nil)
	writeJSON(w, map[string]any{"success": true})
}

// GET /ui and /ui/ — serve the staff UI dashboard or login.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if s.ui == nil {
		writeError(w, http.StatusServiceUnavailable, "UI not available")
		return
	}
	// Check if user has a valid session
	if s.sessions != nil && s.sessions.GetSessionFromRequest(r) {
		s.ui.ServePage(w, r, "dashboard", s.sessions.CSRFTokenFromRequest(r))
		return
	}
	// No session — show login
	s.ui.ServeLogin(w)
}

// GET /ui/page/{page} — serve a specific UI page.
func (s *Server) handleUIPage(w http.ResponseWriter, r *http.Request) {
	page := r.PathValue("page")
	if s.ui != nil {
		csrf := ""
		if s.sessions != nil {
			csrf = s.sessions.CSRFTokenFromRequest(r)
		}
		s.ui.ServePage(w, r, page, csrf)
	} else {
		writeError(w, http.StatusServiceUnavailable, "UI not available")
	}
}

// ─── Member Management (Staff UI) ────────────────────────────

// GET /directory/search?q=smith — search the local customer directory.
//
// Backed by the `customers_fts` FTS5 virtual table (migration 6). One
// indexed query replaces the previous fan-out of three sequential LIKE
// scans (email, name, last-name) — the planner walks the FTS index in
// O(log N) and BM25-ranks results across name/email/external_id/barcode in
// a single pass. Triggers on `customers` keep the FTS table in sync, so
// search reflects writes immediately.
func (s *Server) handleDirectorySearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "q parameter required")
		return
	}
	if s.store == nil {
		writeJSON(w, map[string]any{"query": q, "results": []any{}, "count": 0})
		return
	}

	ctx := r.Context()
	results, err := s.store.SearchCustomersFTS(ctx, q, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Annotate each result with cache status so the UI can show whether the
	// customer already has an enrolled NFC card.
	type searchResult struct {
		store.Customer
		InCache     bool   `json:"inCache"`
		CacheNfcUID string `json:"cacheNfcUid,omitempty"`
	}
	annotated := make([]searchResult, len(results))
	for i, rec := range results {
		annotated[i] = searchResult{Customer: rec}
		if member, err := s.store.GetMemberByCustomerID(ctx, rec.RedpointID); err == nil && member != nil {
			annotated[i].InCache = true
			annotated[i].CacheNfcUID = member.NfcUID
		}
	}

	writeJSON(w, map[string]any{
		"query":   q,
		"results": annotated,
		"count":   len(annotated),
	})
}

// POST /members — manually add a member to the cache.
// Body: {"redpointId": "...", "nfcUid": "...", "firstName": "...", "lastName": "..."}
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedpointID string `json:"redpointId"`
		NfcUID     string `json:"nfcUid"`
		FirstName  string `json:"firstName"`
		LastName   string `json:"lastName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.RedpointID == "" || body.NfcUID == "" {
		writeError(w, http.StatusBadRequest, "redpointId and nfcUid are required")
		return
	}

	nfcUID := strings.ToUpper(strings.TrimSpace(body.NfcUID))

	// If name not provided, look it up from the directory
	firstName := strings.TrimSpace(body.FirstName)
	lastName := strings.TrimSpace(body.LastName)
	if firstName == "" && lastName == "" && s.store != nil {
		rec, err := s.store.GetCustomerByID(r.Context(), body.RedpointID)
		if err == nil && rec != nil {
			firstName = rec.FirstName
			lastName = rec.LastName
		}
	}

	member := &store.Member{
		CustomerID:  body.RedpointID,
		NfcUID:      nfcUID,
		FirstName:   firstName,
		LastName:    lastName,
		BadgeStatus: "ACTIVE",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	if s.store != nil {
		if err := s.store.UpsertMember(r.Context(), member); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to add member: "+err.Error())
			return
		}
	}

	s.logger.Info("member manually added via UI",
		"redpointId", body.RedpointID,
		"nfcUid", nfcUID,
		"name", firstName+" "+lastName,
	)
	s.audit.Log("member_add", r.RemoteAddr, map[string]any{
		"redpointId": body.RedpointID, "nfcUid": nfcUID,
		"name": firstName + " " + lastName,
	})

	s.htmlCache.Invalidate()
	writeJSON(w, map[string]any{
		"success": true,
		"member":  member,
	})
}

// DELETE /members/{externalId} — remove a member from the store.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	extID := strings.ToUpper(r.PathValue("externalId"))
	if extID == "" {
		writeError(w, http.StatusBadRequest, "externalId required")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}

	existing, err := s.store.GetMemberByNFC(r.Context(), extID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "member not found")
		return
	}

	if err := s.store.RemoveMember(r.Context(), extID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("member removed via UI", "externalId", extID, "name", existing.FullName())
	s.audit.Log("member_remove", r.RemoteAddr, map[string]any{
		"externalId": extID, "name": existing.FullName(),
	})
	s.htmlCache.Invalidate()
	writeJSON(w, map[string]any{"success": true})
}

// GET /ingest/unmatched — get the list of unmatched/conflicted UniFi users from the last ingest.
func (s *Server) handleUnmatched(w http.ResponseWriter, r *http.Request) {
	result, err := s.ingester.Run(r.Context(), true) // always dry run
	if err != nil {
		writeError(w, http.StatusBadGateway, "ingest scan failed: "+err.Error())
		return
	}

	type unmatchedEntry struct {
		UniFiUserID string   `json:"unifiUserId"`
		UniFiName   string   `json:"unifiName"`
		UniFiEmail  string   `json:"unifiEmail"`
		NfcTokens   []string `json:"nfcTokens"`
		Warning     string   `json:"warning"`
		Category    string   `json:"category"` // "no_match" or "multiple_match"
	}

	var entries []unmatchedEntry
	for _, m := range result.Mappings {
		if m.Method != ingest.MatchNone {
			continue
		}
		cat := "no_match"
		if strings.Contains(m.Warning, "multiple") {
			cat = "multiple_match"
		}
		entries = append(entries, unmatchedEntry{
			UniFiUserID: m.UniFiUserID,
			UniFiName:   m.UniFiName,
			UniFiEmail:  m.UniFiEmail,
			NfcTokens:   m.NfcTokens,
			Warning:     m.Warning,
			Category:    cat,
		})
	}

	writeJSON(w, map[string]any{
		"totalUnifi":  result.UniFiUsers,
		"matched":     result.Matched,
		"unmatched":   result.Unmatched,
		"entries":     entries,
	})
}

// ─── UniFi Status Sync ──────────────────────────────────────

// POST /status-sync — trigger a manual sync of Redpoint membership status → UniFi user status.
//
// Runs asynchronously in the supervised background group so that a long sync
// (a matching pass against a large UA-Hub population can take many minutes)
// isn't killed when the triggering HTTP client disconnects. The request
// returns 202 with a pointer to GET /status-sync for progress polling. If a
// sync is already in flight, returns 200 with a "sync already in progress"
// message rather than queueing another one — RunSync enforces the
// single-runner invariant internally too, but returning early here avoids
// logging a bogus "start" event.
func (s *Server) handleStatusSync(w http.ResponseWriter, r *http.Request) {
	if s.statusSyncer == nil {
		writeError(w, http.StatusServiceUnavailable, "status syncer not configured")
		return
	}
	if s.statusSyncer.IsRunning() {
		if wantsHTMX(r) {
			s.writeSyncResult(w, r, jobTypeStatusSync, http.StatusOK, true,
				"Status sync already running",
				"A status sync kicked off earlier hasn't finished yet — watch the Last-run pill or the Recent Jobs list; it'll flip to ✓ when done.",
				nil, nil)
			return
		}
		writeJSON(w, map[string]any{
			"message": "sync already in progress — poll GET /status-sync to monitor",
			"running": true,
		})
		return
	}

	s.logger.Info("manual UniFi status sync triggered via API")
	s.audit.Log("status_sync_start", r.RemoteAddr, nil)

	// Snapshot the caller's IP so the completion audit event has the same
	// attribution as the start event. The background goroutine runs with
	// its own context, so r.RemoteAddr is not safe to read there.
	peer := r.RemoteAddr

	// Create the jobs-table row before dispatching so the initial pill
	// flips to "running" immediately on the next /ui/frag/sync-last-run
	// poll. The bg goroutine owns the completion write since the sync
	// outlives the HTTP request.
	jobID := s.startSyncJob(r.Context(), jobTypeStatusSync)

	// Run in background via the supervised group. ctx is the bridge
	// shutdown context, not the request context — so a client disconnect
	// mid-sync no longer cancels in-flight Redpoint or DB work.
	s.bg.Go("status-sync", func(ctx context.Context) error {
		result, err := s.statusSyncer.RunSync(ctx)
		if err != nil {
			s.logger.Error("background status sync failed", "error", err)
			s.audit.Log("status_sync_error", peer, map[string]any{"error": err.Error()})
			s.finishSyncJob(ctx, jobID, nil, err)
			return nil // swallow — bg.Go's error is logged but we've already handled it
		}
		s.audit.Log("status_sync_complete", peer, map[string]any{
			"activated":    result.Activated,
			"deactivated":  result.Deactivated,
			"unchanged":    result.Unchanged,
			"errors":       result.Errors,
			"newlyMatched": result.NewlyMatched,
			"newlyPending": result.NewlyPending,
			"duration":     result.Duration,
		})
		s.finishSyncJob(ctx, jobID, map[string]any{
			"activated":    result.Activated,
			"deactivated":  result.Deactivated,
			"unchanged":    result.Unchanged,
			"errors":       result.Errors,
			"newlyMatched": result.NewlyMatched,
			"newlyPending": result.NewlyPending,
			"duration":     result.Duration,
		}, nil)
		return nil
	})

	if wantsHTMX(r) {
		s.writeSyncResult(w, r, jobTypeStatusSync, http.StatusAccepted, true,
			"Status sync started",
			"Running in the background. The Last-run pill will flip to ✓ when done; you can leave this page and come back.",
			nil, nil)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message": "sync started — poll GET /status-sync to monitor",
	})
}

// GET /status-sync — check last sync result and whether a sync is running.
func (s *Server) handleStatusSyncStatus(w http.ResponseWriter, r *http.Request) {
	if s.statusSyncer == nil {
		writeError(w, http.StatusServiceUnavailable, "status syncer not configured")
		return
	}

	writeJSON(w, map[string]any{
		"running":    s.statusSyncer.IsRunning(),
		"lastResult": s.statusSyncer.LastResult(),
	})
}

// POST /debug/reset-breakers — force-close the recheck circuit breaker.
//
// Operator-invoked recovery endpoint for the case where the breaker has
// tripped (e.g. during a brief Redpoint outage) and the operator wants
// to short-circuit the 60s cooldown because they've already confirmed
// upstream is healthy. Without this, the only remediation was a bridge
// restart — which drops the UA-Hub WebSocket, pauses the check-in queue,
// and takes ~5-10s to come back.
//
// Auth: SecurityMiddleware gates this under the admin-key-OR-session
// branch (it's not /health, not /ui/*, not /ui/login). Listed in the
// audit log for both success and no-op cases so the forensic trail shows
// when an on-call operator manually overrode the breaker.
//
// Response shape is deliberately small so a curl one-liner from a
// runbook prints cleanly:
//
//	{ "ok": true, "wasOpen": true, "breaker": "recheck" }
//
// `wasOpen` distinguishes a meaningful recovery from a no-op press.
func (s *Server) handleDebugResetBreakers(w http.ResponseWriter, r *http.Request) {
	if s.breakerResetter == nil {
		writeError(w, http.StatusServiceUnavailable, "breaker resetter not configured")
		return
	}
	wasOpen := s.breakerResetter()
	peer := s.clientIP(r)
	s.logger.Info("manual breaker reset via /debug/reset-breakers",
		"breaker", "recheck",
		"wasOpen", wasOpen,
		"peer", peer,
	)
	if s.audit != nil {
		s.audit.Log("breaker_reset", peer, map[string]any{
			"breaker": "recheck",
			"wasOpen": wasOpen,
		})
	}
	writeJSON(w, map[string]any{
		"ok":      true,
		"breaker": "recheck",
		"wasOpen": wasOpen,
	})
}

// ─── HTMX Fragment Handlers ──────────────────────────────────

func (s *Server) handleFragStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var active, total, allowedToday, deniedToday int

	if s.store != nil {
		memberStats, _ := s.store.MemberStats(ctx)
		checkinStats, _ := s.store.CheckInStats(ctx)

		if memberStats != nil {
			active = memberStats.Active
			total = memberStats.Total
		}
		if checkinStats != nil {
			allowedToday = checkinStats.AllowedToday
			deniedToday = checkinStats.DeniedToday
		}
	}

	html := ui.StatsFragment(active, total, allowedToday, deniedToday, s.unifi.Connected())
	ui.RenderFragment(w, html)
}

func (s *Server) handleFragRecentCheckins(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	ctx := r.Context()
	events, _ := s.store.RecentCheckIns(ctx, 20)

	rows := make([]ui.CheckInRow, len(events))
	for i, e := range events {
		name := e.CustomerName
		if name == "" {
			name = "Unknown"
		}
		// Format time to HH:MM:SS
		t := e.Timestamp
		if len(t) > 19 {
			t = t[11:19]
		}
		rows[i] = ui.CheckInRow{
			Time: t, Name: name, NfcUID: e.NfcUID,
			Door: e.DoorName, Result: e.Result, DenyReason: e.DenyReason,
		}
	}
	ui.RenderFragment(w, ui.CheckInTableFragment(rows))
}

// intParam is a small helper to extract integer query parameters.
func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (s *Server) handleFragMemberTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	limit := intParam(r, "limit", 50)
	offset := intParam(r, "offset", 0)
	if limit > 200 {
		limit = 200 // hard cap to prevent abuse
	}

	key := fmt.Sprintf("frag-member-table:offset=%d:limit=%d", offset, limit)
	if body, hit := s.htmlCache.Get(key); hit {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "private, max-age=5")
		w.Write(body)
		return
	}

	ctx := r.Context()
	members, total, err := s.store.AllMembersPaged(ctx, limit, offset)
	if err != nil {
		s.logger.Warn("AllMembersPaged failed", "error", err)
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to load members"))
		return
	}

	rows := make([]ui.MemberRow, len(members))
	for i, m := range members {
		rows[i] = ui.MemberRow{
			NfcUID: m.NfcUID, Name: m.FullName(),
			BadgeStatus: m.BadgeStatus, BadgeName: m.BadgeName,
			LastCheckIn: m.LastCheckIn, CustomerID: m.CustomerID,
		}
	}
	html := ui.MemberTableFragmentPaged(rows, offset, total)
	s.htmlCache.Set(key, []byte(html), 30*time.Second)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=5")
	w.Write([]byte(html))
}

func (s *Server) handleFragSearchResults(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) < 2 {
		ui.RenderFragment(w, "")
		return
	}

	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}

	ctx := r.Context()
	var results []ui.SearchResult

	if strings.Contains(q, "@") {
		cust, err := s.store.SearchCustomersByEmail(ctx, q)
		if err == nil && cust != nil {
			member, _ := s.store.GetMemberByCustomerID(ctx, cust.RedpointID)
			sr := ui.SearchResult{
				RedpointID: cust.RedpointID,
				Name:       cust.FirstName + " " + cust.LastName,
				Email:      cust.Email,
			}
			if member != nil {
				sr.InCache = true
				sr.NfcUID = member.NfcUID
			}
			results = append(results, sr)
		}
	} else {
		parts := strings.Fields(q)
		var first, last string
		if len(parts) >= 2 {
			first, last = parts[0], parts[len(parts)-1]
		} else if len(parts) == 1 {
			first = parts[0]
		}

		customers, err := s.store.SearchCustomersByName(ctx, first, last)
		if err == nil {
			for _, c := range customers {
				member, _ := s.store.GetMemberByCustomerID(ctx, c.RedpointID)
				sr := ui.SearchResult{
					RedpointID: c.RedpointID,
					Name:       c.FirstName + " " + c.LastName,
					Email:      c.Email,
				}
				if member != nil {
					sr.InCache = true
					sr.NfcUID = member.NfcUID
				}
				results = append(results, sr)
			}
		}

		// Also try last name search for single-word queries
		if len(parts) == 1 {
			more, err := s.store.SearchCustomersByLastName(ctx, first)
			if err == nil {
				existing := make(map[string]bool)
				for _, r := range results {
					existing[r.RedpointID] = true
				}
				for _, c := range more {
					if existing[c.RedpointID] {
						continue
					}
					member, _ := s.store.GetMemberByCustomerID(ctx, c.RedpointID)
					sr := ui.SearchResult{
						RedpointID: c.RedpointID,
						Name:       c.FirstName + " " + c.LastName,
						Email:      c.Email,
					}
					if member != nil {
						sr.InCache = true
						sr.NfcUID = member.NfcUID
					}
					results = append(results, sr)
				}
			}
		}
	}

	ui.RenderFragment(w, ui.SearchResultsFragment(results))
}

func (s *Server) handleFragCheckinStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	stats, _ := s.store.CheckInStats(r.Context())
	if stats == nil {
		ui.RenderFragment(w, "")
		return
	}
	html := fmt.Sprintf(`<div class="stats-grid">
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Total Today</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Allowed</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Denied</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Unique Members</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">All Time</div></div>
    </div>`, stats.TotalToday, stats.AllowedToday, stats.DeniedToday, stats.UniqueToday, stats.TotalAllTime)
	ui.RenderFragment(w, html)
}

func (s *Server) handleFragCheckinTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	events, _ := s.store.RecentCheckIns(r.Context(), 50)
	rows := make([]ui.CheckInRow, len(events))
	for i, e := range events {
		name := e.CustomerName
		if name == "" {
			name = "Unknown"
		}
		t := e.Timestamp
		if len(t) > 19 {
			t = t[11:19]
		}
		rows[i] = ui.CheckInRow{
			Time: t, Name: name, NfcUID: e.NfcUID,
			Door: e.DoorName, Result: e.Result, DenyReason: e.DenyReason,
		}
	}
	ui.RenderFragment(w, ui.CheckInTableFragment(rows))
}

func (s *Server) handleFragJobTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	jobs, _ := s.store.RecentJobs(r.Context(), 20)
	rows := make([]ui.JobRow, len(jobs))
	for i, j := range jobs {
		rows[i] = ui.JobRow{
			ID: j.ID, Type: j.Type, Status: j.Status,
			CreatedAt: j.CreatedAt, Error: j.Error,
		}
	}
	ui.RenderFragment(w, ui.JobTableFragment(rows))
}

func (s *Server) handleFragPolicyTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	policies, _ := s.store.AllDoorPolicies(r.Context())
	rows := make([]ui.PolicyRow, len(policies))
	for i, p := range policies {
		rows[i] = ui.PolicyRow{
			DoorID: p.DoorID, DoorName: p.DoorName,
			Policy: p.Policy, AllowedBadges: p.AllowedBadges,
		}
	}
	ui.RenderFragment(w, ui.PolicyTableFragment(rows))
}

func (s *Server) handleFragMetricsSummary(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		ui.RenderFragment(w, "")
		return
	}
	summary := s.metrics.JSONSummary()
	uptime := "unknown"
	if u, ok := summary["uptime"].(string); ok {
		uptime = u
	}
	counters := make(map[string]int64)
	if c, ok := summary["counters"].(map[string]int64); ok {
		counters = c
	}
	gauges := make(map[string]float64)
	if g, ok := summary["gauges"].(map[string]float64); ok {
		gauges = g
	}
	ui.RenderFragment(w, ui.MetricsSummaryFragment(uptime, counters, gauges))
}

// handleFragShadowDecisions renders the panel comparing UniFi's native
// verdict (event.Result: ACCESS|BLOCKED) against the bridge's decision.
// It is the primary signal during shadow-mode burn-in — every row is a tap
// that would behave differently between UniFi and the bridge, which must be
// investigated before flipping to live.
func (s *Server) handleFragShadowDecisions(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	ctx := r.Context()

	stats, err := s.store.ShadowDecisionStatsToday(ctx)
	if err != nil {
		s.logger.Error("shadow decision stats failed", "error", err)
		stats = &store.ShadowDecisionStats{}
	}

	events, err := s.store.DisagreementEvents(ctx, 50)
	if err != nil {
		s.logger.Error("disagreement events fetch failed", "error", err)
	}

	rows := make([]ui.ShadowDecisionRow, len(events))
	for i, e := range events {
		name := e.CustomerName
		if name == "" {
			name = "Unknown"
		}
		t := e.Timestamp
		if len(t) > 19 {
			t = t[11:19]
		}
		rows[i] = ui.ShadowDecisionRow{
			Time:        t,
			Name:        name,
			NfcUID:      e.NfcUID,
			Door:        e.DoorName,
			UnifiResult: e.UnifiResult,
			OurResult:   e.Result,
			DenyReason:  e.DenyReason,
		}
	}

	ui.RenderFragment(w, ui.ShadowDecisionsFragment(
		stats.Total, stats.Agree, stats.Disagree, stats.Unknown,
		stats.WouldMiss, stats.WouldAdmit,
		rows,
	))
}

// handleFragUnmatchedTable renders the list of UniFi users with NFC tags
// that couldn't be paired with a Redpoint customer. Backed by a dry-run
// ingest against UniFi — slow-ish (hits the UA-Hub for every user) so this
// fragment is load-triggered once per page visit, not on a poll. Each row
// ends in a "Search Redpoint →" button that deep-links into the Members
// page with the UniFi name/email prefilled.
func (s *Server) handleFragUnmatchedTable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=5")

	if s.ingester == nil {
		ui.RenderFragment(w, `<p style="color: var(--text-muted)">Ingester not configured.</p>`)
		return
	}

	const key = "frag-unmatched-table"
	if body, hit := s.htmlCache.Get(key); hit {
		w.Write(body)
		return
	}

	result, err := s.ingester.Run(r.Context(), true) // always dry run
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Ingest scan failed: "+err.Error()))
		return
	}

	rows := make([]ui.UnmatchedRow, 0)
	for _, m := range result.Mappings {
		if m.Method != ingest.MatchNone {
			continue
		}
		cat := "no_match"
		if strings.Contains(m.Warning, "multiple") {
			cat = "multiple_match"
		}
		rows = append(rows, ui.UnmatchedRow{
			UniFiUserID: m.UniFiUserID,
			UniFiName:   m.UniFiName,
			UniFiEmail:  m.UniFiEmail,
			NfcTokens:   m.NfcTokens,
			Category:    cat,
			Warning:     m.Warning,
		})
	}

	html := ui.UnmatchedTableFragment(
		result.UniFiUsers, result.Matched, result.Unmatched, rows,
	)
	s.htmlCache.Set(key, []byte(html), 30*time.Second)

	w.Write([]byte(html))
}

// ─── "Needs Match" staff UI (C2) ─────────────────────────────
//
// Backed by ua_user_mappings_pending (written by the statusync matching
// phase). Mutating actions go:
//
//   match:  store.UpsertMapping + DeletePending +
//           AppendMatchAudit(field=mapping, source=staff)
//   skip:   UA-Hub UpdateUserStatus(DEACTIVATED) + DeletePending +
//           AppendMatchAudit(field=user_status, source=staff:skip)
//   defer:  UpsertPending(grace_until=now+7d) + AppendMatchAudit(source=staff:defer)
//
// All three actions hx-swap the detail panel back into place with the
// post-mutation state.
//
// Note (v0.5.1): the match action does NOT call UA-Hub UpdateUser(email).
// The original design (docs/architecture-review.md C2 §Matching, pre-v0.5.1)
// mirrored Redpoint email into UA-Hub on every match. We dropped that —
// Redpoint is the source of truth, and the bridge reads UA-Hub email only
// to drive matching. UA-Hub email stays whatever staff typed when creating
// the user. TouchMappingEmailSynced + last_email_synced_at are dead
// columns today; slated for migration-5 cleanup.

// Note (v0.5.2): the lookupUAUser / lookupUAUsersByID helpers that used
// to enrich the Needs Match views with a live UA-Hub ListUsers walk were
// removed. Both call sites now read ua_name + ua_email off the pending
// row (migration 5), so the Needs Match page no longer has any runtime
// UA-Hub dependency. If a future view needs a UA-Hub user detail that
// isn't cached on the pending row, add a dedicated single-user fetch
// path rather than resurrecting the whole-directory walk.

// handleFragUnmatchedList — table view of ua_user_mappings_pending.
//
// Pre-v0.5.2 this handler made a live UA-Hub ListUsers call on every
// render to enrich each row with a display name + email. That walk
// (17 pages × 100/page at LEF, 10s per-page HTTP timeout) hung the
// entire /ui/needs-match page whenever UA-Hub was slow, because the
// fragment is loaded via hx-trigger="load" — a stuck XHR leaves the
// "Loading unmatched users…" placeholder on screen indefinitely.
//
// The fix is to persist ua_name + ua_email onto the pending row at
// UpsertPending time (see auditMigration5_pending_ua_identity and
// statusync.Syncer.persistDecision). This handler now reads every
// column straight off the local row with zero UA-Hub dependency —
// rendering is dominated by the SQLite query, which is ~milliseconds
// on the ~dozen-row pending table. UA-Hub can be completely offline
// and the page still paints.
func (s *Server) handleFragUnmatchedList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	pending, err := s.store.AllPending(r.Context())
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to read pending list: "+err.Error()))
		return
	}

	rows := make([]ui.NeedsMatchRow, 0, len(pending))
	for _, p := range pending {
		row := ui.NeedsMatchRow{
			UAUserID:   p.UAUserID,
			UAName:     p.UAName,
			UAEmail:    p.UAEmail,
			Reason:     p.Reason,
			FirstSeen:  p.FirstSeen,
			GraceUntil: p.GraceUntil,
		}
		if p.Candidates != "" {
			row.CandidateCount = len(strings.Split(p.Candidates, "|"))
		}
		rows = append(rows, row)
	}

	ui.RenderFragment(w, ui.NeedsMatchListFragment(rows))
}

// handleFragUnmatchedDetail — per-user detail panel with candidate list.
// Candidates come from two sources:
//
//  1. store.Pending.Candidates (the list the matcher captured when it
//     couldn't auto-pick). These are labelled "email-match" or
//     "name-match" depending on the Pending.Reason.
//  2. Optional free-text search (handleFragUnmatchedSearch replaces
//     this fragment with one where Candidates is the search hit set).
//
// Detail and search both share NeedsMatchDetailFragment as the renderer.
func (s *Server) handleFragUnmatchedDetail(w http.ResponseWriter, r *http.Request) {
	s.renderNeedsMatchDetail(w, r, "" /* searchQuery */)
}

// handleFragUnmatchedSearch — POST: free-text name search against the
// Redpoint client. Replaces the detail panel with the search results.
func (s *Server) handleFragUnmatchedSearch(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	q := strings.TrimSpace(r.FormValue("q"))
	s.renderNeedsMatchDetail(w, r, q)
}

func (s *Server) renderNeedsMatchDetail(w http.ResponseWriter, r *http.Request, searchQuery string) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if uaUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID"))
		return
	}

	pending, err := s.store.GetPending(r.Context(), uaUserID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to load pending row: "+err.Error()))
		return
	}
	if pending == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"This user is no longer in the pending list — another staff member may have just resolved it. Refresh the list."))
		return
	}

	// Read the UA-Hub display identity straight off the pending row
	// instead of talking to UA-Hub live. See handleFragUnmatchedList
	// (v0.5.2 comment) and auditMigration5_pending_ua_identity for
	// the rationale. statusync refreshes these fields on every
	// observation, so they lag UA-Hub by at most one sync interval.
	uaName := pending.UAName
	uaEmail := pending.UAEmail

	// Build candidate list. Order matters — matcher-suggested candidates
	// first (they're the most likely hit; the pending row captured them
	// during the original decision), then search-query hits.
	candidates := []ui.NeedsMatchCandidate{}
	seen := map[string]bool{}

	if s.redpoint != nil {
		for _, rpID := range splitCandidates(pending.Candidates) {
			if rpID == "" || seen[rpID] {
				continue
			}
			cust, err := s.redpoint.GetCustomer(r.Context(), rpID)
			if err != nil || cust == nil {
				continue
			}
			candidates = append(candidates, ui.NeedsMatchCandidate{
				RedpointCustomerID: cust.ID,
				Name:               strings.TrimSpace(cust.FirstName + " " + cust.LastName),
				Email:              cust.Email,
				Active:             cust.Active,
				BadgeName:          badgeNameFor(cust),
				BadgeStatus:        badgeStatusFor(cust),
				Reason:             matcherReasonLabel(pending.Reason),
			})
			seen[rpID] = true
		}

		if searchQuery != "" {
			// Split "Jane Smith" → firstName="Jane", lastName="Smith".
			firstName, lastName := splitName(searchQuery)
			hits, err := s.redpoint.SearchCustomersByName(r.Context(), firstName, lastName)
			if err != nil {
				s.logger.Warn("needs-match search failed", "q", searchQuery, "error", err)
			}
			for _, cust := range hits {
				if cust == nil || seen[cust.ID] {
					continue
				}
				candidates = append(candidates, ui.NeedsMatchCandidate{
					RedpointCustomerID: cust.ID,
					Name:               strings.TrimSpace(cust.FirstName + " " + cust.LastName),
					Email:              cust.Email,
					Active:             cust.Active,
					BadgeName:          badgeNameFor(cust),
					BadgeStatus:        badgeStatusFor(cust),
					Reason:             "search",
				})
				seen[cust.ID] = true
			}
		}
	}

	ui.RenderFragment(w, ui.NeedsMatchDetailFragment(
		uaUserID, uaName, uaEmail,
		pending.FirstSeen, pending.GraceUntil, pending.Reason,
		candidates, searchQuery,
	))
}

// handleFragUnmatchedMatch — POST: bind UA user to a staff-selected
// Redpoint customer. Writes audit, upserts mapping, deletes pending,
// and (in live mode) mirrors the Redpoint email into UA-Hub.
func (s *Server) handleFragUnmatchedMatch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.redpoint == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store or Redpoint client not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if err := r.ParseForm(); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Bad form: "+err.Error()))
		return
	}
	customerID := strings.TrimSpace(r.FormValue("redpointCustomerId"))
	if uaUserID == "" || customerID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID or Redpoint customer ID"))
		return
	}

	// Collision check — can't bind one Redpoint customer to two UA users.
	if existing, err := s.store.GetMappingByCustomerID(r.Context(), customerID); err == nil && existing != nil && existing.UAUserID != uaUserID {
		ui.RenderFragment(w, ui.AlertFragment(false,
			fmt.Sprintf("Redpoint customer %s is already bound to a different UA-Hub user (%s). Un-match that user first or pick a different customer.",
				customerID, existing.UAUserID)))
		return
	}

	cust, err := s.redpoint.GetCustomer(r.Context(), customerID)
	if err != nil || cust == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"Couldn't read the selected Redpoint customer: "+errOrMissing(err)))
		return
	}

	// Persist mapping + audit + drop pending. Order: audit first so the
	// forensic trail records even if the subsequent write fails.
	if err := s.store.AppendMatchAudit(r.Context(), &store.MatchAudit{
		UAUserID:  uaUserID,
		Field:     "mapping",
		BeforeVal: "",
		AfterVal:  customerID,
		Source:    statusync.MatchSourceStaff,
	}); err != nil {
		s.logger.Error("match audit write failed", "uaUserId", uaUserID, "error", err)
	}
	if err := s.store.UpsertMapping(r.Context(), &store.Mapping{
		UAUserID:         uaUserID,
		RedpointCustomer: customerID,
		MatchedBy:        statusync.MatchSourceStaff,
	}); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to write mapping: "+err.Error()))
		return
	}
	if err := s.store.DeletePending(r.Context(), uaUserID); err != nil {
		s.logger.Warn("pending row delete after match failed", "uaUserId", uaUserID, "error", err)
	}

	s.audit.Log("staff_match", r.RemoteAddr, map[string]any{
		"uaUserId":           uaUserID,
		"redpointCustomerId": customerID,
		"customerName":       strings.TrimSpace(cust.FirstName + " " + cust.LastName),
	})

	// Rebuild the detail fragment with a confirmation alert. Since the
	// pending row is gone, GetPending returns nil — render a success
	// alert in its place.
	//
	// Note: v0.5.1 dropped the "next sync will mirror the email" line.
	// The bridge does NOT push Redpoint email into UA-Hub — it only
	// reads UA-Hub emails to drive its own matching. See
	// docs/architecture-review.md C2 §Matching for the decision.
	ui.RenderFragment(w, ui.AlertFragment(true,
		fmt.Sprintf("Matched UA-Hub user %s → Redpoint %s (%s). Saved.",
			uaUserID, customerID, strings.TrimSpace(cust.FirstName+" "+cust.LastName))))
}

// handleFragUnmatchedSkip — POST: deactivate UA-Hub user immediately and
// drop the pending row. Used for ex-member cards predating the bridge.
func (s *Server) handleFragUnmatchedSkip(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.unifi == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store or UniFi client not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if uaUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID"))
		return
	}
	if err := s.unifi.UpdateUserStatus(r.Context(), uaUserID, "DEACTIVATED"); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "UA-Hub deactivate failed: "+err.Error()))
		return
	}
	if err := s.store.AppendMatchAudit(r.Context(), &store.MatchAudit{
		UAUserID:  uaUserID,
		Field:     "user_status",
		BeforeVal: "ACTIVE",
		AfterVal:  "DEACTIVATED",
		Source:    statusync.MatchSourceStaffSkip,
	}); err != nil {
		s.logger.Error("skip audit write failed", "uaUserId", uaUserID, "error", err)
	}
	if err := s.store.DeletePending(r.Context(), uaUserID); err != nil {
		s.logger.Warn("pending delete after skip failed", "uaUserId", uaUserID, "error", err)
	}
	s.audit.Log("staff_skip", r.RemoteAddr, map[string]any{"uaUserId": uaUserID})

	ui.RenderFragment(w, ui.AlertFragment(true,
		fmt.Sprintf("Skipped UA-Hub user %s — deactivated now. The user will not tap in until staff re-activates.", uaUserID)))
}

// handleFragUnmatchedDefer — POST: extend pending grace window by 7 days.
func (s *Server) handleFragUnmatchedDefer(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if uaUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID"))
		return
	}
	existing, err := s.store.GetPending(r.Context(), uaUserID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Read pending failed: "+err.Error()))
		return
	}
	if existing == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"This user is not in the pending list anymore — nothing to defer."))
		return
	}
	newGrace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	existing.GraceUntil = newGrace
	existing.LastSeen = time.Now().UTC().Format(time.RFC3339)
	if err := s.store.UpsertPending(r.Context(), existing); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Defer failed: "+err.Error()))
		return
	}
	if err := s.store.AppendMatchAudit(r.Context(), &store.MatchAudit{
		UAUserID:  uaUserID,
		Field:     "grace_until",
		BeforeVal: "",
		AfterVal:  newGrace,
		Source:    statusync.MatchSourceStaffDefer,
	}); err != nil {
		s.logger.Warn("defer audit write failed", "uaUserId", uaUserID, "error", err)
	}
	s.audit.Log("staff_defer", r.RemoteAddr, map[string]any{"uaUserId": uaUserID, "graceUntil": newGrace})

	ui.RenderFragment(w, ui.AlertFragment(true,
		fmt.Sprintf("Deferred — grace window extended to %s.", newGrace)))
}

// splitCandidates parses the pipe-separated Candidates field from a
// Pending row into a slice of Redpoint customer IDs. Returns nil for
// empty input.
func splitCandidates(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "|")
}

// splitName turns a free-text query like "Jane Smith" into (first, last).
// If the query is one token, it's used as the first name and last is
// empty — the Redpoint name-search client will match on either side.
func splitName(q string) (first, last string) {
	parts := strings.Fields(q)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

// badgeNameFor / badgeStatusFor pull badge info off the Redpoint customer
// shape without knowing whether the badge struct is populated.
func badgeNameFor(c *redpoint.Customer) string {
	if c == nil || c.Badge == nil || c.Badge.CustomerBadge == nil {
		return ""
	}
	return c.Badge.CustomerBadge.Name
}

func badgeStatusFor(c *redpoint.Customer) string {
	if c == nil || c.Badge == nil {
		return ""
	}
	return c.Badge.Status
}

// matcherReasonLabel maps the symbolic pending reason to a short chip
// string used in the candidate "Why" column.
func matcherReasonLabel(reason string) string {
	switch reason {
	case store.PendingReasonAmbiguousEmail:
		return "email-match"
	case store.PendingReasonAmbiguousName, store.PendingReasonNoEmail, store.PendingReasonNoMatch:
		return "name-match"
	default:
		return reason
	}
}

func errOrMissing(err error) string {
	if err == nil {
		return "customer not found"
	}
	return err.Error()
}

func (s *Server) handleAddDoorPolicy(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	r.ParseForm()
	policy := &store.DoorPolicy{
		DoorID:        r.FormValue("doorId"),
		DoorName:      r.FormValue("doorName"),
		Policy:        r.FormValue("policy"),
		AllowedBadges: r.FormValue("allowedBadges"),
	}
	if policy.DoorID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Door ID is required"))
		return
	}
	if err := s.store.UpsertDoorPolicy(r.Context(), policy); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed: "+err.Error()))
		return
	}
	s.audit.Log("door_policy_update", r.RemoteAddr, map[string]any{"doorId": policy.DoorID, "policy": policy.Policy})
	ui.RenderFragment(w, ui.AlertFragment(true, "Policy saved for "+policy.DoorName))
}

func (s *Server) handleDeleteDoorPolicy(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	doorID := r.PathValue("doorId")
	if err := s.store.DeleteDoorPolicy(r.Context(), doorID); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed: "+err.Error()))
		return
	}
	s.audit.Log("door_policy_delete", r.RemoteAddr, map[string]any{"doorId": doorID})
	w.WriteHeader(http.StatusOK)
}

// ─── Helpers ─────────────────────────────────────────────────

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
