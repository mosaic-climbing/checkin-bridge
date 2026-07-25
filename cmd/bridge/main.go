// Mosaic Climbing – UniFi Access ↔ Redpoint HQ Check-in Bridge
//
// A single-binary service that connects your G2 Pro reader + UA-Hub
// to Redpoint HQ. Members tap their NFC card and walk in.
//
// Build:  go build -o mosaic-bridge ./cmd/bridge
// Run:    ./mosaic-bridge
//
// This file is the entrypoint shell only — every subsystem lives in
// internal/app, which exposes a thin Build / Run / Close surface.
// Pre-PR4 main.go was ~890 lines of inline wiring; that wiring now
// lives in internal/app/build.go where it can be tested and reasoned
// about as a unit.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/mosaic-climbing/checkin-bridge/internal/app"
	"github.com/mosaic-climbing/checkin-bridge/internal/config"
)

// Build-time ldflags inject these. See .github/workflows/{ci,release}.yml.
//
//	-ldflags "-X main.version=$TAG -X main.buildTime=$TS"
var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	// -version exits before loading config so deploy/macbook/update.sh
	// can ask "what's installed?" without needing .env to be valid.
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()
	if showVersion {
		fmt.Printf("mosaic-bridge %s (built %s)\n", version, buildTime)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.Bridge.LogLevel)
	slog.SetDefault(logger)
	logBootBanner(logger, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridge, err := app.Build(ctx, app.BuildOptions{
		Cfg:       cfg,
		Logger:    logger,
		Version:   version,
		BuildTime: buildTime,
	})
	if err != nil {
		logger.Error("app build failed", "error", err)
		os.Exit(1)
	}
	defer bridge.Close()

	if err := bridge.Run(ctx); err != nil {
		logger.Error("app run failed", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

// newLogger constructs the structured slog logger main owns. Kept here
// (not in internal/app) so app.Build can take a *slog.Logger and stay
// free of any "build-the-default-logger" responsibility — tests and
// future callers might want a different handler entirely.
func newLogger(level string) *slog.Logger {
	logLevel := slog.LevelInfo
	if level == "debug" {
		logLevel = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
}

// logBootBanner emits the "I am alive" header. Lives in main rather
// than app.Build because the version + buildTime ldflag-injected
// strings only exist in the main package.
func logBootBanner(logger *slog.Logger, cfg *config.Config) {
	logger.Info("════════════════════════════════════════")
	logger.Info("  Mosaic Climbing – Check-in Bridge v2")
	logger.Info("════════════════════════════════════════")
	logger.Info("build info",
		"version", version,
		"buildTime", buildTime,
		"configHash", cfg.NonSecretHash(),
		"instance", cfg.Bridge.InstanceName,
	)

	// Announce a non-prod instance loudly. The pair of (instance=stage,
	// shadow=true) is enforced in config.validate(), so the binary won't
	// boot in any other combination — but the operator tailing logs still
	// benefits from a banner that makes it obvious which process they've
	// just started. Same shape as the shadow-mode banner below for visual
	// consistency.
	if cfg.Bridge.InstanceName != "" && cfg.Bridge.InstanceName != "prod" {
		logger.Warn("╔══════════════════════════════════════════════╗")
		logger.Warn("║  NON-PROD INSTANCE                           ║",
			"instance", cfg.Bridge.InstanceName)
		logger.Warn("╚══════════════════════════════════════════════╝")
	}

	// Per-capability live/shadow report — the go-live trust ladder means
	// "shadow" is no longer all-or-nothing, so the banner states each
	// capability's resolved mode explicitly. The old single SHADOW box
	// implied all three were coupled; these lines are what an operator
	// greps to confirm which rung the deployment is on.
	recording := cfg.Bridge.CheckinRecordingLive()
	statusWrites := cfg.Bridge.StatusWritesMode()
	recheckUnlock := cfg.Bridge.RecheckUnlockLive()
	if !recording || statusWrites != "full" || !recheckUnlock {
		logger.Warn("╔══════════════════════════════════════════════╗")
		logger.Warn("║  NOT FULLY LIVE — capability report below    ║")
		logger.Warn("╚══════════════════════════════════════════════╝")
	}
	logger.Warn("capability: redpoint check-in recording", "live", recording)
	logger.Warn("capability: UA-Hub status writes", "mode", statusWrites)
	logger.Warn("capability: recheck-reactivation unlock", "live", recheckUnlock)

	logger.Info("config loaded",
		"unifiHost", cfg.UniFi.Host,
		"redpointUrl", cfg.Redpoint.APIURL,
		"facilityCode", cfg.Redpoint.FacilityCode,
		"gateId", cfg.Redpoint.GateID,
		"dataDir", cfg.Bridge.DataDir,
		"syncInterval", cfg.Sync.Interval,
	)
}
