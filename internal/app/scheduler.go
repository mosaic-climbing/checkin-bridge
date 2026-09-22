package app

import "time"

// Hardening constants for the scheduler closures (directory-syncer,
// unifi-ingest). The cache.Syncer and unifimirror.Syncer carry their
// own mirror of these constants so an in-package test can pin them
// without importing app; values must match.
//
// BackoffMax is 1 h, not minutes: these jobs run once a day and each
// run is a 20–25 minute walk against Redpoint's rate limit. Retrying a
// failed walk every 5 min meant a walk that failed BECAUSE of the rate
// limit immediately re-tripped it (Sep 2026 storm). Blips still recover
// fast — 5 s, 10 s, 20 s … — only a sustained failure reaches the cap.
const (
	schedulerJitter       = 0.1
	schedulerBackoffStart = 5 * time.Second
	schedulerBackoffMax   = 1 * time.Hour
)
