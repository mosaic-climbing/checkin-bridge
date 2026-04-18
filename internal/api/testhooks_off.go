//go:build !devhooks

package api

import "time"

// testHooksCompiled is false in the production build. Tests can use it to
// skip assertions that only apply under `-tags devhooks`.
const testHooksCompiled = false

// registerTestHooks is a no-op in the default build — the /test-checkin
// route is not registered at all. A stolen admin API key therefore cannot
// mint fake check-ins or trigger unlock pulses through this endpoint in
// production. The devhooks build tag compiles testhooks_on.go instead,
// which installs the handler (still gated on Server.enableTestHooks).
//
// See S5 in docs/architecture-review.md.
func (s *Server) registerTestHooks(shortTimeout time.Duration) {}
