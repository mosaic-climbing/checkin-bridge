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
	)

	if cfg.Bridge.ShadowMode {
		logger.Warn("╔══════════════════════════════════════════════╗")
		logger.Warn("║  SHADOW MODE ACTIVE                          ║")
		logger.Warn("║  • No door unlocks will be sent to UniFi     ║")
		logger.Warn("║  • No check-ins will be recorded in Redpoint ║")
		logger.Warn("║  • No UniFi user status changes will be made ║")
		logger.Warn("║  All decisions are logged only.              ║")
		logger.Warn("╚══════════════════════════════════════════════╝")
	}

	logger.Info("config loaded",
		"unifiHost", cfg.UniFi.Host,
		"redpointUrl", cfg.Redpoint.APIURL,
		"facilityCode", cfg.Redpoint.FacilityCode,
		"gateId", cfg.Redpoint.GateID,
		"dataDir", cfg.Bridge.DataDir,
		"syncInterval", cfg.Sync.Interval,
	)
}
