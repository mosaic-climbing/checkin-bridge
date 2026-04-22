package unifi

// v0.5.8 coverage for the reconnect-loop wedge (#76).
//
// The bug: readLoop's defer waited unconditionally on pingDone. The ping
// sender goroutine took connMu and called conn.WriteControl; on a dead
// socket that WriteControl blocked indefinitely holding connMu, so
// readLoop's defer `<-pingDone` never returned, connectLoop never
// advanced to its reconnect step, and the bridge's WebSocket went dark
// until the process was restarted. Seen on LEF's gym Mac the night of
// the 0.5.x WS flap — symptom was zero check-ins for ~9h with a
// "connected" health status.
//
// The fix, three layers deep so one regression doesn't re-wedge the
// loop:
//   1. Scoped pingCtx (tied to readLoop's lifetime) — a well-behaved
//      ping goroutine observes pingCtx.Done() when readLoop returns and
//      exits promptly, instead of waiting for the next 30s tick.
//   2. Bounded wait in readLoop's defer — if the ping goroutine is
//      stuck inside WriteControl and CAN'T observe pingCtx.Done(),
//      readLoop abandons the wait after pingDoneGraceTimeout and
//      returns, so connectLoop's reconnect step can fire.
//   3. Panic recovery in connectLoop — any panic (in dial, readLoop,
//      or a user callback) respawns connectLoop instead of silently
//      killing the reconnect loop.
//
// The bounded-wait test is the functionally important regression — it
// proves that the wedge-under-connMu scenario cannot prevent reconnect.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestReadLoop_BoundedWaitOnWedgedPingGoroutine is the primary #76
// regression. It holds connMu from the test to force the ping goroutine
// to block on Lock() (standing in for a production block inside
// WriteControl-under-connMu), triggers a read error, and asserts that
// readLoop returns within a bounded window rather than blocking
// forever on <-pingDone.
func TestReadLoop_BoundedWaitOnWedgedPingGoroutine(t *testing.T) {
	// Shrink both the ping cadence (so the goroutine hits Lock() quickly)
	// and the grace timeout (so the test doesn't have to wait the prod
	// 2s). Restore originals on exit.
	origInterval := wsPingInterval
	origGrace := pingDoneGraceTimeout
	wsPingInterval = 20 * time.Millisecond
	pingDoneGraceTimeout = 150 * time.Millisecond
	defer func() {
		wsPingInterval = origInterval
		pingDoneGraceTimeout = origGrace
	}()

	// ── Stand up a WebSocket server we can kill mid-read ──
	var (
		srvMu   sync.Mutex
		srvConn *websocket.Conn
	)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		srvMu.Lock()
		srvConn = c
		srvMu.Unlock()
		// Park here until the test closes the conn (or the server shuts
		// down). The handler holding open is what keeps the client-side
		// ReadMessage blocked until we force an error below.
		<-r.Context().Done()
		_ = c.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Wait briefly for the server to have captured srvConn.
	waitForSrvConn := func() *websocket.Conn {
		deadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) {
			srvMu.Lock()
			c := srvConn
			srvMu.Unlock()
			if c != nil {
				return c
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("server never captured srvConn")
		return nil
	}
	sc := waitForSrvConn()

	c := NewClient(wsURL, srv.URL, "test-token", 0, "",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Hold connMu so the ping goroutine, on its first tick, blocks on
	// Lock() instead of progressing to WriteControl. This simulates the
	// production wedge: the goroutine cannot observe pingCtx.Done() or
	// close pingDone, because it's stuck on the mutex acquire.
	c.connMu.Lock()
	// Release it at test exit so the ping goroutine can eventually
	// unblock and not leak for the whole test-binary run.
	defer func() {
		// Best-effort: the goroutine is already past its select by now,
		// but unlocking doesn't hurt.
		c.connMu.Unlock()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readLoopReturned := make(chan struct{})
	go func() {
		c.readLoop(ctx, conn)
		close(readLoopReturned)
	}()

	// Give the ping goroutine enough time to fire at least one tick
	// (20ms interval → 40ms is 2 ticks of headroom) so it's parked on
	// c.connMu.Lock() before we kill the read side.
	time.Sleep(40 * time.Millisecond)

	// Break ReadMessage by closing the server-side conn. This causes
	// readLoop to return — but only AFTER its defer resolves. Pre-fix
	// the defer's `<-pingDone` would block forever because the ping
	// goroutine is wedged on the lock; post-fix it bounds on
	// pingDoneGraceTimeout.
	_ = sc.Close()

	// Allow the defer pingDoneGraceTimeout (150ms) + comfortable slack
	// for scheduling/GC on CI. The pre-fix wedge would never return at
	// all; anything under ~500ms here proves the bound is in effect.
	const bound = 750 * time.Millisecond
	select {
	case <-readLoopReturned:
		// pass
	case <-time.After(bound):
		t.Fatalf("readLoop did not return within %v; pre-fix #76 wedge regressed "+
			"(ping goroutine stuck under connMu → <-pingDone never closes)", bound)
	}
}

// TestReadLoop_PingCtxCancelledOnReturn is the happy-path half of the
// fix: a well-behaved ping goroutine must observe pingCtx.Done() when
// readLoop returns, without having to wait a full pingInterval. We
// don't assert timing as tightly here — the contract is just "exits
// promptly, never has to tick again".
func TestReadLoop_PingCtxCancelledOnReturn(t *testing.T) {
	origInterval := wsPingInterval
	// A long-ish interval so we can prove the goroutine exited via the
	// pingCtx.Done() branch, not by accident via the ticker firing.
	wsPingInterval = 10 * time.Second
	defer func() { wsPingInterval = origInterval }()

	upgrader := websocket.Upgrader{}
	var (
		srvMu   sync.Mutex
		srvConn *websocket.Conn
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		srvMu.Lock()
		srvConn = c
		srvMu.Unlock()
		<-r.Context().Done()
		_ = c.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(1 * time.Second)
	var sc *websocket.Conn
	for time.Now().Before(deadline) {
		srvMu.Lock()
		sc = srvConn
		srvMu.Unlock()
		if sc != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sc == nil {
		t.Fatal("server never captured srvConn")
	}

	c := NewClient(wsURL, srv.URL, "test-token", 0, "",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readLoopReturned := make(chan struct{})
	go func() {
		c.readLoop(ctx, conn)
		close(readLoopReturned)
	}()

	// Break ReadMessage immediately.
	_ = sc.Close()

	// With wsPingInterval=10s, the ticker will NOT fire during this
	// window. The only way readLoop can return quickly is via
	// pingCtx.Done() making the ping goroutine exit promptly and close
	// pingDone. If readLoop returns within 200ms we know the scoped
	// pingCtx path worked; if it takes 10s+ we regressed to
	// parent-ctx-only behavior.
	select {
	case <-readLoopReturned:
		// pass — pingCtx.Done() shortcut observed
	case <-time.After(500 * time.Millisecond):
		t.Fatal("readLoop did not return promptly after ReadMessage error; " +
			"ping goroutine likely still waiting on parent ctx / next tick " +
			"(scoped pingCtx regressed)")
	}
}

// TestConnectLoop_PanicRecoveryRespawns verifies the panic-recovery
// defer in connectLoop: if anything inside panics (here, a user
// onStateChange callback), the goroutine re-spawns rather than silently
// dying and leaving the WebSocket dark until process restart.
//
// Observability trick: setState() only fires the callback on
// transitions. The panic happens during the first setState(Connecting)
// callback. After respawn, the new goroutine's setState(Connecting)
// is a no-op (state is already Connecting). But on ctx.Done() the
// respawned goroutine calls setState(Disconnected) — that transition
// fires the callback, and observing it proves a new goroutine was
// alive to receive the cancellation. Pre-fix (no recover) the
// original goroutine would die on the panic, no respawn would happen,
// and Disconnected would never be seen.
func TestConnectLoop_PanicRecoveryRespawns(t *testing.T) {
	// Point the client at a URL that will fail dial immediately, so
	// connectLoop spins in its error-retry path without needing a real
	// server.
	c := NewClient(
		"ws://127.0.0.1:1", // port 1 — nothing listens here
		"http://127.0.0.1:1",
		"test-token", 0, "",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	var (
		sawConnecting   = make(chan struct{}, 1)
		sawDisconnected = make(chan struct{}, 1)
		panickedOnce    sync.Once
	)
	c.OnStateChange(func(newState ConnectionState, _ *HealthStatus) {
		switch newState {
		case Connecting:
			select {
			case sawConnecting <- struct{}{}:
			default:
			}
			panickedOnce.Do(func() {
				panic("synthetic panic to exercise connectLoop's recover/respawn")
			})
		case Disconnected:
			select {
			case sawDisconnected <- struct{}{}:
			default:
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.connectLoop(ctx)

	// Wait for the Connecting callback to have fired (and panicked).
	select {
	case <-sawConnecting:
	case <-time.After(2 * time.Second):
		t.Fatal("connectLoop never reached Connecting state — test setup broken")
	}

	// Small grace for the recover+respawn to actually take hold before
	// we signal shutdown.
	time.Sleep(50 * time.Millisecond)

	// Signal shutdown. The respawned goroutine should observe ctx.Done
	// and fire setState(Disconnected). Pre-fix, no goroutine is alive
	// to see the cancellation and the Disconnected callback never
	// fires.
	//
	// Gotcha: the original goroutine might still be mid-backoff-sleep
	// when the panic propagates up (the panic fires BEFORE the dial
	// call, so we're in the top-of-loop). The respawned goroutine
	// re-enters and goes through its own dial-fail → 5s backoff. Wait
	// long enough for a backoff iteration to notice ctx.Done.
	cancel()

	select {
	case <-sawDisconnected:
		// pass — respawned goroutine saw ctx.Done and transitioned
	case <-time.After(2 * time.Second):
		t.Fatal("respawned connectLoop never fired Disconnected on ctx cancel; " +
			"pre-fix regression — original goroutine died on panic and " +
			"no respawn happened (#76 layer 3)")
	}
}
