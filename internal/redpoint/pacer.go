package redpoint

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// pacer is the client-wide governor for outbound Redpoint requests.
// Every request the Client sends passes through it, so the three
// background walks (cache refresh, directory sync, ingest) and the
// interactive tap path share ONE budget against Redpoint's undocumented
// rate limit instead of each pacing itself and colliding.
//
// Two mechanisms:
//
//   - A token bucket (rate tokens/s, burst capacity). Background walks
//     drain it to a steady trickle; a tap arriving mid-walk waits at
//     most ~1/rate for the next token. Rate <= 0 disables pacing.
//
//   - A cooldown. When ANY request is answered 429, the server's
//     Retry-After (or defaultCooldown when absent) becomes a
//     client-wide pause: nobody sends until it expires. Before this
//     existed a 429 only delayed the one request that saw it, while
//     the other 1,199 lookups in the same walk kept firing, burned
//     their three attempts on the same closed window, and were
//     reported as "unreachable" — which failed the job and re-ran the
//     whole walk. See cache.Syncer for the other half of that story.
//
// Both waits honour ctx, so an interactive caller with a 10 s deadline
// gives up cleanly instead of sleeping through a 50 s cooldown.
type pacer struct {
	mu          sync.Mutex
	rate        float64 // tokens per second; <= 0 → no token pacing
	burst       float64
	tokens      float64
	last        time.Time
	pausedUntil time.Time
	now         func() time.Time
}

// defaultCooldown is used for a 429 that carries no usable Retry-After.
// Redpoint always sends one (≈50 s, the remainder of its window), so
// this is a guess for other/misbehaving upstreams: long enough that a
// walk doesn't immediately re-trip the limiter, short enough that an
// interactive caller's 10 s budget survives one occurrence. Tests
// override it to keep runtime short.
var defaultCooldown = 5 * time.Second

func newPacer(rate float64, burst int) *pacer {
	if burst < 1 {
		burst = 1
	}
	p := &pacer{rate: rate, burst: float64(burst), now: time.Now}
	p.tokens = p.burst
	p.last = p.now()
	return p
}

// wait blocks until the caller may send: cooldown expired and a token
// is available. Returns ctx.Err() if ctx ends first.
func (p *pacer) wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	for {
		p.mu.Lock()
		now := p.now()
		var d time.Duration
		if now.Before(p.pausedUntil) {
			d = p.pausedUntil.Sub(now)
		} else if p.rate <= 0 {
			p.mu.Unlock()
			return nil
		} else {
			p.refillLocked(now)
			if p.tokens >= 1 {
				p.tokens--
				p.mu.Unlock()
				return nil
			}
			d = time.Duration((1 - p.tokens) / p.rate * float64(time.Second))
		}
		p.mu.Unlock()

		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (p *pacer) refillLocked(now time.Time) {
	elapsed := now.Sub(p.last).Seconds()
	if elapsed > 0 {
		p.tokens += elapsed * p.rate
		if p.tokens > p.burst {
			p.tokens = p.burst
		}
	}
	p.last = now
}

// pause opens (or extends) the client-wide cooldown. retryAfter <= 0
// falls back to defaultCooldown. Returns true when this call extended
// the pause (so the caller logs once per cooldown, not once per 429).
func (p *pacer) pause(retryAfter time.Duration) (until time.Time, extended bool) {
	if p == nil {
		return time.Time{}, false
	}
	if retryAfter <= 0 {
		retryAfter = defaultCooldown
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	until = p.now().Add(retryAfter)
	if until.After(p.pausedUntil) {
		p.pausedUntil = until
		// Drain the bucket too: resuming with a full burst right after
		// a 429 is exactly how you earn the next one. Leave exactly one
		// token so the request that waited out the cooldown goes first
		// and the rest fall in behind it at the steady rate.
		p.tokens = 1
		p.last = until
		return until, true
	}
	return p.pausedUntil, false
}

// cooldownRemaining reports how long the client-wide pause has left
// (zero when not paused). Exposed on Client for /health-style probes.
func (p *pacer) cooldownRemaining() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.pausedUntil.Sub(p.now()); d > 0 {
		return d
	}
	return 0
}

// SetRateLimit installs the client-wide pacer. rps <= 0 disables token
// pacing (the 429 cooldown still applies). Call once, before use —
// it replaces the pacer rather than adjusting it.
func (c *Client) SetRateLimit(rps float64, burst int) {
	c.pacer = newPacer(rps, burst)
}

// CooldownRemaining reports how long the client is refusing to send
// because of a recent 429. Zero when not in cooldown.
func (c *Client) CooldownRemaining() time.Duration {
	return c.pacer.cooldownRemaining()
}

// String makes the pacer readable in "config loaded" log lines.
func (p *pacer) String() string {
	if p == nil {
		return "off"
	}
	return fmt.Sprintf("%.2f rps, burst %.0f", p.rate, p.burst)
}
