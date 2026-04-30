package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/bg"
	"github.com/mosaic-climbing/checkin-bridge/internal/checkin"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// makeReconnectBackfill returns the OnReconnect callback the unifi
// client invokes whenever the WebSocket reconnects. The callback fetches
// missed access logs since the last seen event and replays them through
// the check-in handler.
//
// Pre-A2 the backfill ran inside an anonymous `go cb(...)` fired from
// inside internal/unifi/client.go — outside any supervised context,
// invisible to the gauge, and not drained on shutdown. The callback is
// now invoked synchronously by the unifi client (per the OnReconnect
// contract); the actual REST work is dispatched to bg.Group so it
// shows up as bg_goroutines_running{name="reconnect-backfill"} and
// Shutdown waits for it before exit.
func makeReconnectBackfill(
	unifiClient *unifi.Client,
	handler *checkin.Handler,
	bgGroup *bg.Group,
	logger *slog.Logger,
) func(time.Time) {
	return func(lastEventAt time.Time) {
		// If we never saw an event before the outage, conservatively
		// don't backfill — we'd flood the handler with everything
		// since boot. An operator can still hand-replay from the REST
		// API if needed.
		if lastEventAt.IsZero() {
			logger.Warn("reconnect backfill skipped: no prior event timestamp")
			return
		}
		// Small overlap so we don't miss an event that landed right
		// at reconnection; the consumer dedups boundary events.
		since := lastEventAt.Add(-5 * time.Second)

		bgGroup.Go("reconnect-backfill", func(bgCtx context.Context) error {
			backfillCtx, cancelBackfill := context.WithTimeout(bgCtx, 60*time.Second)
			defer cancelBackfill()
			events, err := unifiClient.FetchAccessLogsSince(backfillCtx, since)
			if err != nil {
				logger.Error("reconnect backfill fetch failed",
					"since", since.UTC().Format(time.RFC3339),
					"error", err,
				)
				return nil
			}
			logger.Info("reconnect backfill: replaying missed events",
				"since", since.UTC().Format(time.RFC3339),
				"count", len(events),
			)
			for _, evt := range events {
				if bgCtx.Err() != nil {
					logger.Warn("reconnect backfill interrupted by shutdown",
						"replayed", len(events)-1,
					)
					return nil
				}
				// Each replayed event gets its own 15s context, mirroring
				// the live OnEvent path in HandleEvent's Start() wrapper.
				// Parented on bgCtx so a shutdown also drops in-flight
				// HandleEvent calls.
				eventCtx, cancel := context.WithTimeout(bgCtx, 15*time.Second)
				handler.HandleEvent(eventCtx, evt)
				cancel()
			}
			logger.Info("reconnect backfill complete", "replayed", len(events))
			return nil
		})
	}
}
