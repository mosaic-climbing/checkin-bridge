package redpoint

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPacer_TokenBucketPaces(t *testing.T) {
	p := newPacer(50, 1) // 20 ms per token, no burst
	start := time.Now()
	for i := 0; i < 6; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// First token is free; five more at 20 ms each.
	if got := time.Since(start); got < 90*time.Millisecond {
		t.Fatalf("6 waits at 50 rps took %v, want >= ~100ms", got)
	}
}

func TestPacer_DisabledRateStillHonoursCooldown(t *testing.T) {
	p := newPacer(0, 1)
	for i := 0; i < 100; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, extended := p.pause(60 * time.Millisecond); !extended {
		t.Fatal("first pause should extend")
	}
	if p.cooldownRemaining() <= 0 {
		t.Fatal("cooldownRemaining should be positive right after pause")
	}
	start := time.Now()
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := time.Since(start); got < 55*time.Millisecond {
		t.Fatalf("wait during cooldown returned after %v, want >= 60ms", got)
	}
	if p.cooldownRemaining() != 0 {
		t.Fatal("cooldownRemaining should be zero once the pause has elapsed")
	}
}

func TestPacer_PauseOnlyExtendsForward(t *testing.T) {
	p := newPacer(0, 1)
	until1, ext1 := p.pause(200 * time.Millisecond)
	until2, ext2 := p.pause(10 * time.Millisecond)
	if !ext1 || ext2 {
		t.Fatalf("extended = %v,%v; want true,false", ext1, ext2)
	}
	if !until2.Equal(until1) {
		t.Fatalf("shorter pause moved the deadline: %v -> %v", until1, until2)
	}
}

func TestPacer_PauseFallsBackToDefaultCooldown(t *testing.T) {
	prev := defaultCooldown
	defaultCooldown = 30 * time.Millisecond
	t.Cleanup(func() { defaultCooldown = prev })
	p := newPacer(0, 1)
	p.pause(0)
	if r := p.cooldownRemaining(); r <= 0 || r > 30*time.Millisecond {
		t.Fatalf("cooldownRemaining = %v, want (0, 30ms]", r)
	}
}

func TestPacer_WaitHonoursContext(t *testing.T) {
	p := newPacer(0, 1)
	p.pause(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.wait(ctx)
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("wait did not return promptly on ctx expiry")
	}
}

func TestPacer_NilIsSafe(t *testing.T) {
	var p *pacer
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ext := p.pause(time.Second); ext {
		t.Fatal("nil pacer should not report an extension")
	}
	if p.cooldownRemaining() != 0 {
		t.Fatal("nil pacer should report no cooldown")
	}
}

// A 429 seen by one request must pause every subsequent request on the
// client, not just the retry of the request that saw it.
func TestExec_429PausesClientWide(t *testing.T) {
	withFastBackoff(t)
	prev := defaultCooldown
	defaultCooldown = 40 * time.Millisecond
	t.Cleanup(func() { defaultCooldown = prev })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0") // present but useless → defaultCooldown
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		graphqlOK(w)
	}))
	defer srv.Close()

	c := newClientFor(t, srv.URL)
	c.SetRateLimit(0, 1)

	// Request A trips the limiter; its own retry succeeds after the cooldown.
	start := time.Now()
	if _, err := c.execWithRetry(context.Background(), "query{}", nil); err != nil {
		t.Fatal(err)
	}
	if got := time.Since(start); got < 35*time.Millisecond {
		t.Fatalf("retry after 429 took %v, want >= cooldown (40ms)", got)
	}
	if c.CooldownRemaining() != 0 {
		t.Fatal("cooldown should have elapsed before the retry was sent")
	}

	// A fresh 429 must delay an UNRELATED request B.
	calls.Store(0)
	if _, err := c.execWithRetry(context.Background(), "query{}", nil); err != nil {
		t.Fatal(err)
	}
	// calls: 1 (429) + 1 (retry ok) — and both the retry and any
	// concurrent caller waited on the same client-wide pause.
	if n := calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func TestSummarizeBody(t *testing.T) {
	html := "<!DOCTYPE html><html><head><title>Too Many Requests</title></head><body>x</body></html>"
	cases := []struct{ in, want string }{
		{"rate limited", "rate limited"},
		{html, fmt.Sprintf("[html body, %d bytes: Too Many Requests]", len(html))},
	}
	for _, tc := range cases {
		if got := summarizeBody([]byte(tc.in)); got != tc.want {
			t.Errorf("summarizeBody(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := make([]byte, 2*maxErrorBody)
	for i := range long {
		long[i] = 'a'
	}
	if got := summarizeBody(long); len([]rune(got)) != maxErrorBody+1 {
		t.Errorf("long body not truncated: len=%d", len(got))
	}
}
