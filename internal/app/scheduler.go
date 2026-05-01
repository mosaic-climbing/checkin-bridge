package app

import "time"

// Hardening constants for the scheduler closures (directory-syncer,
// unifi-ingest). The cache.Syncer and unifimirror.Syncer carry their
// own mirror of these constants so an in-package test can pin them
// without importing app; values must match.
const (
	schedulerJitter       = 0.1
	schedulerBackoffStart = 5 * time.Second
	schedulerBackoffMax   = 5 * time.Minute
)
