package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/api"
	"github.com/mosaic-climbing/checkin-bridge/internal/auditlog"
	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/cache"
	"github.com/mosaic-climbing/checkin-bridge/internal/cardmap"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/config"
	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/jobs"
	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/mirror"
	"github.com/mosaic-climbing/checkin-bridge/internal/recheck"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifimirror"
)

// BuildOptions are the inputs Build needs from cmd/bridge. Logger and
// Cfg are required; Version / BuildTime label the /health response and
// the boot banner — pass "dev" / "unknown" if you don't care.
type BuildOptions struct {
	Cfg       *config.Config
	Logger    *slog.Logger
	Version   string
	BuildTime string
}

// Build assembles the full bridge runtime. It performs all the
// dependency wiring main.go used to inline, validates that required
// disk locations are writable, and returns an *App ready for Run.
//
// On any error the partially-constructed resources (an opened DB, an
// opened audit log) are closed before the error is returned, so the
// caller doesn't have to know which subsystems were created.
//
// ctx is the lifecycle context. It is held by the supervised bg.Group
// so cancellation (signal or otherwise) drains background work
// cleanly. Build itself does not block on ctx — long-running
// subsystems start in App.Run, not here.
func Build(ctx context.Context, opts BuildOptions) (*App, error) {
	if opts.Cfg == nil {
		return nil, fmt.Errorf("BuildOptions.Cfg is required")
	}
	if opts.Logger == nil {
		return nil, fmt.Errorf("BuildOptions.Logger is required")
	}
	cfg := opts.Cfg
	logger := opts.Logger

	// Data directory. mode 0o700 because session.key + audit log live
	// here and contain operator state we don't want world-readable.
	if err := os.MkdirAll(cfg.Bridge.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	_ = os.Chmod(cfg.Bridge.DataDir, 0o700)

	if cfg.Redpoint.GateID == "" {
		logger.Warn("REDPOINT_GATE_ID not set – check-ins won't record in Redpoint")
		logger.Warn("run: curl http://localhost:" + fmt.Sprintf("%d", cfg.Bridge.Port) + "/gates")
	}

	// SQLite store. Owned by App; closed by App.Close.
	db, err := store.Open(cfg.Bridge.DataDir, logger.With("component", "store"))
	if err != nil {
		return nil, fmt.Errorf("store init: %w", err)
	}

	// Audit log. Same ownership story.
	auditLogger, err := auditlog.Open(cfg.Bridge.DataDir)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("audit log init: %w", err)
	}

	met := metrics.New()

	// ── Clients ──────────────────────────────────────────────
	unifiClient := unifi.NewClient(
		cfg.UniFi.WSURL(),
		cfg.UniFi.BaseURL(),
		cfg.UniFi.APIToken,
		cfg.Bridge.UnlockDurationMs,
		cfg.UniFi.TLSFingerprint,
		logger.With("component", "unifi"),
	)
	redpointClient := redpoint.NewClient(
		cfg.Redpoint.GraphQLURL(),
		cfg.Redpoint.APIKey,
		cfg.Redpoint.FacilityCode,
		logger.With("component", "redpoint"),
	)
	// A5: latency histogram + outcome counters live on the client. Wired
	// here (not in NewClient) so the redpoint package keeps its
	// construction signature stable for unit tests that don't care.
	redpointClient.SetMetrics(met)

	cardMapper, err := cardmap.New(cfg.Bridge.DataDir, logger.With("component", "cardmap"))
	if err != nil {
		_ = auditLogger.Close()
		_ = db.Close()
		return nil, fmt.Errorf("card mapper init: %w", err)
	}

	// ── Syncers / ingester / rechecker ───────────────────────
	//
	// Stagger plan: the four schedulers driven off cfg.Sync.Interval
	// (directory-syncer, ua-hub-mirror, unifi-ingest, cache-syncer)
	// used to all fire at boot within milliseconds of each other,
	// thunderherding Redpoint + UA-Hub. The InitialDelay values below
	// spread their first runs across ~2 minutes, ordered by upstream
	// dependency:
	//
	//   t=0s    directory-syncer  → fills customers (ingest needs it)
	//   t=30s   ua-hub-mirror     → fills ua_users  (ingest needs it)
	//   t=90s   unifi-ingest      → matches → members
	//   t=120s  cache-syncer      → refreshes badge status on members
	//
	// Subsequent ticks use jitter (±10%) so the four schedules diverge
	// over time even when InitialDelay only applies to the first run.
	syncer := cache.NewSyncer(db, redpointClient, cache.SyncConfig{
		SyncInterval: cfg.Sync.Interval,
		PageSize:     cfg.Sync.PageSize,
		InitialDelay: 120 * time.Second,
	}, logger.With("component", "syncer"))

	ingester := ingest.NewIngester(redpointClient, db, logger.With("component", "ingest"))

	statusSyncer := statusync.New(
		unifiClient,
		redpointClient,
		db,
		statusync.Config{
			SyncInterval:        cfg.Sync.Interval,
			RateLimitDelay:      200 * time.Millisecond,
			SyncTimeLocal:       cfg.Sync.TimeLocal,
			UnmatchedGraceDays:  cfg.Bridge.UnmatchedGraceDays,
			LegacyNFCStatusLoop: cfg.Bridge.LegacyNFCStatusLoop,
		},
		cfg.Bridge.ShadowMode,
		met,
		logger.With("component", "statusync"),
	)

	rechecker := recheck.New(db, redpointClient, unifiClient, recheck.Config{
		MaxStaleness: cfg.Bridge.RecheckMaxStaleness,
		ShadowMode:   cfg.Bridge.ShadowMode,
	}, logger.With("component", "recheck"))

	handler := checkin.NewHandler(checkin.HandlerDeps{
		UniFi:      unifiClient,
		Redpoint:   redpointClient,
		CardMapper: cardMapper,
		Store:      db,
		Rechecker:  rechecker,
		Metrics:    met,
		GateID:     cfg.Redpoint.GateID,
		ShadowMode: cfg.Bridge.ShadowMode,
		Logger:     logger.With("component", "checkin"),
	})

	// ── Session manager ──────────────────────────────────────
	// HMAC signing key persisted to <DataDir>/session.key (mode 0600,
	// atomic write on first boot, reused afterward). Without
	// persistence, every restart invalidated all staff sessions.
	sessionKeyPath := filepath.Join(cfg.Bridge.DataDir, "session.key")
	sessionMgr, err := api.NewSessionManagerWithKeyFile(cfg.Bridge.StaffPassword, sessionKeyPath)
	if err != nil {
		_ = auditLogger.Close()
		_ = db.Close()
		return nil, fmt.Errorf("session manager init: %w", err)
	}
	sessionMgr.StartJanitor(ctx)
	sessionMgr.SetSecureCookies(cfg.Bridge.HTTPS)

	uiHandler, err := ui.New()
	if err != nil {
		_ = auditLogger.Close()
		_ = db.Close()
		return nil, fmt.Errorf("UI init: %w", err)
	}

	// ── IP allowlist + trusted proxies ──────────────────────
	allowedNets, err := api.ParseAllowedNetworks(cfg.Bridge.AllowedNetworks)
	if err != nil {
		_ = auditLogger.Close()
		_ = db.Close()
		return nil, fmt.Errorf("ALLOWED_NETWORKS: %w", err)
	}
	if len(allowedNets) > 0 {
		var cidrStrs []string
		for _, n := range allowedNets {
			cidrStrs = append(cidrStrs, n.String())
		}
		logger.Info("IP allowlist enabled", "networks", cidrStrs)
	}
	// Loud warning: public listener with no allowlist. The staff UI is
	// password-protected and admin endpoints require ADMIN_API_KEY,
	// but any host on the routable network can probe /ui/login and
	// /health. On the UDM Pro's nspawn container in particular, ""
	// or "0.0.0.0" means the whole LAN.
	if cfg.Bridge.BindAddr != "127.0.0.1" && cfg.Bridge.BindAddr != "localhost" && len(allowedNets) == 0 {
		logger.Warn("public listener reachable off-host with no IP allowlist",
			"bind_addr", cfg.Bridge.BindAddr,
			"hint", "set ALLOWED_NETWORKS to the staff subnet, or BIND_ADDR=127.0.0.1")
	}

	trustedProxies, err := api.ParseAllowedNetworks(cfg.Bridge.TrustedProxies)
	if err != nil {
		_ = auditLogger.Close()
		_ = db.Close()
		return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
	}
	if len(trustedProxies) > 0 {
		var cidrStrs []string
		for _, n := range trustedProxies {
			cidrStrs = append(cidrStrs, n.String())
		}
		logger.Info("trusted reverse-proxy CIDRs configured", "networks", cidrStrs)
	} else {
		logger.Info("no trusted proxies configured; X-Forwarded-For / X-Real-IP will be ignored")
	}

	if cfg.Bridge.HTTPS {
		logger.Info("HTTPS-aware mode enabled", "cookies_secure", true, "hsts", true)
	}

	// ── Background group ────────────────────────────────────
	// A2: bgMetricsAdapter exposes per-name gauges. The adapter lives
	// in this package (not in internal/bg) so the bg package keeps no
	// hard dependency on metrics.
	bgGroup := bg.NewWithMetrics(ctx, logger.With("component", "bg"), bgMetricsAdapter{reg: met})

	// ── Reconnect backfill ──────────────────────────────────
	var onReconnect func(time.Time)
	if cfg.Bridge.BackfillOnReconnect {
		logger.Info("WebSocket reconnect backfill enabled")
		onReconnect = makeReconnectBackfill(unifiClient, handler, bgGroup, logger)
		unifiClient.OnReconnect(onReconnect)
	}

	// ── Mirror walkers ──────────────────────────────────────
	mirrorWalker := mirror.New(redpointClient, mirror.NewStoreAdapter(db),
		logger.With("component", "mirror"), mirror.Config{})

	uaHubMirror := unifimirror.New(unifiClient, db, unifimirror.SyncConfig{
		Interval:     cfg.Sync.Interval,
		InitialDelay: 30 * time.Second,
	}, logger.With("component", "unifimirror"))

	uaHubRefresher := func(ctx context.Context, progress func(phase string)) (api.UAHubRefreshStats, error) {
		ctx = unifimirror.WithProgress(ctx, unifimirror.ProgressFunc(progress))
		stats, err := uaHubMirror.RefreshWithStats(ctx)
		if err != nil {
			return api.UAHubRefreshStats{}, err
		}
		return api.UAHubRefreshStats{
			Observed:    stats.Observed,
			Upserted:    stats.Upserted,
			Hydrated:    stats.Hydrated,
			Rechecked:   stats.Rechecked,
			MirrorTotal: stats.MirrorTotal,
			Duration:    stats.Duration,
		}, nil
	}

	// ── API server ──────────────────────────────────────────
	apiServer := api.NewServer(api.ServerDeps{
		Handler:              handler,
		Unifi:                unifiClient,
		Redpoint:             redpointClient,
		CardMapper:           cardMapper,
		Syncer:               syncer,
		StatusSyncer:         statusSyncer,
		Ingester:             ingester,
		Sessions:             sessionMgr,
		Audit:                auditLogger,
		GateID:               cfg.Redpoint.GateID,
		Logger:               logger.With("component", "api"),
		Store:                db,
		UI:                   uiHandler,
		Metrics:              met,
		TrustedProxies:       trustedProxies,
		BG:                   bgGroup,
		EnableTestHooks:      cfg.Bridge.EnableTestHooks,
		BreakerResetter:      rechecker.ResetBreaker,
		MirrorWalker:         mirrorWalker.Walk,
		UAHubMirrorRefresher: uaHubRefresher,
	})

	// ── HTTP handler chains ────────────────────────────────
	// Public plane: full middleware stack, including session/CSRF.
	publicHandler := api.RequestLogger(logger.With("component", "api"), trustedProxies,
		api.SecurityMiddleware(api.SecurityConfig{
			AdminAPIKey:     cfg.Bridge.AdminAPIKey,
			Sessions:        sessionMgr,
			AllowedNetworks: allowedNets,
			TrustedProxies:  trustedProxies,
			Logger:          logger.With("component", "security"),
			HTTPS:           cfg.Bridge.HTTPS,
		}, api.RecoveryMiddleware(logger.With("component", "api"), apiServer)))

	// Control plane: admin Bearer key only, no session / CSRF / /ui
	// fall-through. Bound to ControlBindAddr (default 127.0.0.1) so
	// the routes that cause physical-world side effects (POST /unlock,
	// devhooks /test-checkin) are reachable only from the host itself.
	controlHandler := api.RequestLogger(logger.With("component", "api-control"), trustedProxies,
		api.ControlSecurityMiddleware(api.ControlSecurityConfig{
			AdminAPIKey:     cfg.Bridge.AdminAPIKey,
			AllowedNetworks: allowedNets,
			TrustedProxies:  trustedProxies,
			Logger:          logger.With("component", "security-control"),
			HTTPS:           cfg.Bridge.HTTPS,
		}, api.RecoveryMiddleware(logger.With("component", "api-control"), apiServer.ControlHandler())))

	// Public mux owns /metrics directly so the security middleware
	// chain doesn't gate Prometheus scrapes; everything else falls
	// through to publicHandler.
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler(unifiClient, met))
	mux.Handle("/", publicHandler)

	// ADMIN_API_KEY is enforced by config.validate(); this log line
	// preserves the historical boot output that grep-based tooling
	// (and operators) look for.
	logger.Info("admin API authentication enabled")

	// ── HTTP server config ─────────────────────────────────
	addr := fmt.Sprintf("%s:%d", cfg.Bridge.BindAddr, cfg.Bridge.Port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Minute, // see the long comment in pre-PR4 main.go re sync routes
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	controlAddr := fmt.Sprintf("%s:%d", cfg.Bridge.ControlBindAddr, cfg.Bridge.ControlPort)
	controlSrv := &http.Server{
		Addr:              controlAddr,
		Handler:           controlHandler,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	// ── Schedulers ─────────────────────────────────────────
	schedulers := buildSchedulers(cfg, db, logger, syncer, statusSyncer, uaHubMirror,
		ingester, apiServer, unifiClient)

	logger.Info("starting mosaic checkin bridge v2",
		"port", cfg.Bridge.Port,
		"dataDir", cfg.Bridge.DataDir,
		"gateID", cfg.Redpoint.GateID,
	)

	return &App{
		logger:     logger,
		store:      db,
		audit:      auditLogger,
		unifi:      unifiClient,
		handler:    handler,
		apiServer:  apiServer,
		httpSrv:    httpSrv,
		controlSrv: controlSrv,
		sessions:   sessionMgr,
		bgGroup:    bgGroup,
		schedulers: schedulers,
		onConnect:  onReconnect,
	}, nil
}

// buildSchedulers returns the named bg.Group.Go invocations Run will
// dispatch. Pulled out of Build so Build's body reads as "construct
// subsystems" and this function reads as "describe what the schedulers
// do" — same separation pre-PR4 main.go achieved with section
// comments, now expressed in the type system.
func buildSchedulers(
	cfg *config.Config,
	db *store.Store,
	logger *slog.Logger,
	syncer *cache.Syncer,
	statusSyncer *statusync.Syncer,
	uaHubMirror *unifimirror.Syncer,
	ingester *ingest.Ingester,
	apiServer *api.Server,
	unifiClient *unifi.Client,
) []scheduledTask {
	dirSyncerLogger := logger.With("component", "directory-syncer")
	directorySync := func(bgCtx context.Context) error {
		return jobs.Loop(bgCtx, jobs.LoopConfig{
			Interval:     cfg.Sync.Interval,
			InitialDelay: 0,
			Jitter:       schedulerJitter,
			BackoffStart: schedulerBackoffStart,
			BackoffMax:   schedulerBackoffMax,
		}, db, dirSyncerLogger, jobs.TypeDirectorySync,
			func(ctx context.Context) (any, error) {
				res, err := apiServer.RunDirectorySync(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"totalFetched": res.TotalFetched,
					"completedAt":  res.CompletedAt,
					"duration":     res.Duration.String(),
				}, nil
			})
	}

	// unifi-ingest: walk UniFi users, match against the local customer
	// mirror, and upsert into members. Cache-empty / mirror-empty
	// failures on a fresh deploy are expected while directory-syncer +
	// ua-hub-mirror finish their first runs. Boot-time stagger (90s)
	// gives both upstreams a head start; the exponential backoff inside
	// jobs.Loop handles the case where they're still empty on landing.
	ingestLogger := logger.With("component", "unifi-ingest")
	unifiIngest := func(bgCtx context.Context) error {
		return jobs.Loop(bgCtx, jobs.LoopConfig{
			Interval:     cfg.Sync.Interval,
			InitialDelay: 90 * time.Second,
			Jitter:       schedulerJitter,
			BackoffStart: schedulerBackoffStart,
			BackoffMax:   schedulerBackoffMax,
		}, db, ingestLogger, jobs.TypeUniFiIngest,
			func(ctx context.Context) (any, error) {
				res, err := ingester.Run(ctx, false /* dryRun: scheduled writes */)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"dryRun":     false,
					"unifiUsers": res.UniFiUsers,
					"withNfc":    res.WithNFC,
					"matched":    res.Matched,
					"unmatched":  res.Unmatched,
					"applied":    res.Applied,
				}, nil
			})
	}

	// v0.5.0: REST tap poller — on UA-Hub 4.11.19.0 / UniFi Access
	// 4.2.16 the WebSocket notifications feed no longer emits
	// access.logs.add events for door taps. Initial cursor is
	// today-midnight-local so on first boot we backfill all of today's
	// taps. Dedup on checkins.unifi_log_id makes the rolling window
	// safe against restart-duplicates.
	pollerStart := startOfTodayLocal()
	unifiPoller := func(bgCtx context.Context) error {
		return unifiClient.StartEventPoller(bgCtx, pollerStart, 5*time.Second)
	}

	return []scheduledTask{
		{name: "cache-syncer", fn: syncer.Run},
		{name: "statusync", fn: statusSyncer.Run},
		{name: "ua-hub-mirror", fn: uaHubMirror.Run},
		{name: "directory-syncer", fn: directorySync},
		{name: "unifi-ingest", fn: unifiIngest},
		{name: "unifi-poller", fn: unifiPoller},
	}
}

// metricsHandler snapshots WebSocket client state into gauges on each
// scrape so Prometheus sees up-to-date values, then renders the full
// registry as Prometheus text. Source-of-truth for WS counters stays
// inside the unifi.Client — we don't try to maintain a parallel
// counter that could drift.
func metricsHandler(unifiClient *unifi.Client, met *metrics.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unifiClient != nil {
			connected := 0.0
			if unifiClient.Connected() {
				connected = 1.0
			}
			met.Gauge("ws_connected").Set(connected)
			met.Gauge("ws_reconnect_count").SetInt(unifiClient.ReconnectCount())
			met.Gauge("ws_messages_received").SetInt(unifiClient.MessagesReceived())
			met.Gauge("ws_events_processed").SetInt(unifiClient.EventsProcessed())
		}
		w.Header().Set("Content-Type", "text/plain; version=0.04")
		_, _ = w.Write([]byte(met.PrometheusText()))
	})
}

// startOfTodayLocal returns 00:00:00 in the host's local timezone for
// the current date. Used as the initial poller cursor so the v0.5.0
// tap ingestion backfills same-day taps on first boot. Local, not UTC,
// so "today" matches what the operator sees in the UI and what the
// dashboard's daily-aggregate queries use.
func startOfTodayLocal() time.Time {
	now := time.Now()
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
}
