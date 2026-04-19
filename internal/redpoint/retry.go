package redpoint

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseRetryAfter parses an HTTP Retry-After header value (RFC 7231 §7.1.3)
// into a duration. The header takes one of two forms:
//
//   - delta-seconds — a non-negative integer of seconds to wait.
//     Example: "120".
//   - HTTP-date — an absolute time. Example: "Fri, 17 Apr 2026 12:34:56 GMT".
//
// Returns 0 for:
//   - an empty header
//   - unparseable garbage
//   - a negative delta-seconds (servers shouldn't send these, but we refuse
//     to interpret them as "retry immediately")
//   - an HTTP-date in the past (already elapsed — no wait needed)
//
// A zero return never means "retry immediately"; the caller combines it
// with an unconditional jittered backoff floor so a misbehaving server
// can't trigger a tight reconnect loop.
//
// We intentionally don't surface a parse error: the retry loop can always
// fall back to its own backoff schedule. A malformed header is a server
// bug, not something worth aborting over.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}

	// delta-seconds form: a bare non-negative integer.
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}

	// HTTP-date form. RFC 7231 permits three formats; http.ParseTime
	// handles all three (IMF-fixdate, RFC 850, ANSI C asctime).
	t, err := http.ParseTime(h)
	if err != nil {
		return 0
	}
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	return d
}
