// Package unifi implements the UniFi Access API client.
//
// WebSocket: wss://{host}:{port}/api/v1/developer/devices/notifications
// REST:      https://{host}:{port}/api/v1/developer/...
// Auth:      Authorization: Bearer {token}
//
// Hardware: G2 Pro reader + UA-Hub door controller
package unifi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ConnectionState represents the health state of the WebSocket connection.
type ConnectionState string

const (
	Connecting    ConnectionState = "connecting"
	Connected     ConnectionState = "connected"
	Degraded      ConnectionState = "degraded"
	Disconnected  ConnectionState = "disconnected"
)

// HealthStatus represents the current health of the UniFi client connection.
type HealthStatus struct {
	State             string    `json:"state"`
	Connected         bool      `json:"connected"`
	LastConnected     time.Time `json:"lastConnected,omitempty"`
	LastDisconnected  time.Time `json:"lastDisconnected,omitempty"`
	ReconnectCount    int64     `json:"reconnectCount"`
	LastEventAt       time.Time `json:"lastEventAt,omitempty"`
	Uptime            string    `json:"uptime,omitempty"`
	DegradedThreshold int       `json:"degradedThreshold"` // seconds without event before degraded
}

// StateChangeCallback is called when the connection state changes.
type StateChangeCallback func(newState ConnectionState, health *HealthStatus)

// ReconnectCallback is called after a successful WebSocket reconnect (not
// the initial connect). Receives the timestamp of the last event seen
// before the outage so the caller can backfill missed events.
type ReconnectCallback func(lastEventBeforeOutage time.Time)

// AccessEvent represents a parsed door access event from the UniFi WebSocket
// or the REST /system/logs poller.
type AccessEvent struct {
	// LogID is the stable UniFi Access log identifier for this event.
	// Populated from the `_id` field on REST results; empty on legacy WS
	// messages that predate the envelope change. Used downstream as the
	// primary dedup key so the poller can fetch an overlapping time
	// window without creating duplicate checkins rows.
	LogID string `json:"logId,omitempty"`

	EventType    string `json:"eventType"`
	Timestamp    string `json:"timestamp"`
	DoorName     string `json:"doorName"`
	DoorID       string `json:"doorId"`
	ActorName    string `json:"actorName"`
	ActorID      string `json:"actorId"`
	CredentialID string `json:"credentialId"`
	AuthType     string `json:"authType"` // NFC, PIN_CODE, MOBILE, etc.
	Result       string `json:"result"`   // ACCESS, BLOCKED
	// IsBackfill is true when the event was fetched from the REST access
	// log during a reconnect-time replay OR by the steady-state poller
	// rather than streamed live. The check-in handler still records it
	// for audit purposes but must NOT unlock the door or create a new
	// Redpoint record — the door already had its chance at tap time and
	// the member is either inside or long gone.
	IsBackfill bool `json:"isBackfill,omitempty"`
}

// Door represents a door configured in UniFi Access.
type Door struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EventHandler is called for each access event received via WebSocket.
type EventHandler func(AccessEvent)

// Client connects to a UniFi Access Hub for real-time events and door control.
type Client struct {
	wsURL    string
	baseURL  string
	apiToken string
	logger   *slog.Logger

	conn      *websocket.Conn
	connMu    sync.Mutex
	connected atomic.Bool
	handler   EventHandler
	done      chan struct{}

	unlockDurationMs int
	tlsConfig        *tls.Config

	httpClient *http.Client

	// Health tracking
	state              atomic.Value // stores ConnectionState
	lastConnected      atomic.Value // stores time.Time
	lastDisconnected   atomic.Value // stores time.Time
	lastEventAt        atomic.Value // stores time.Time
	reconnectCount     atomic.Int64
	onStateChange      StateChangeCallback
	onReconnect        ReconnectCallback
	healthMu           sync.RWMutex
	degradedThreshold  time.Duration // default 5 minutes

	// Throughput counters. Snapshotted into the metrics registry at scrape
	// time by main.go's /metrics handler; kept here so the unifi package
	// doesn't need to depend on internal/metrics.
	messagesReceived atomic.Int64
	eventsProcessed  atomic.Int64
}

// NewClient creates a new UniFi Access client.
// tlsFingerprint is optional — if set, it pins the expected SHA-256 fingerprint
// of the UA-Hub's TLS certificate (hex-encoded, colons optional). If empty,
// all certificates are accepted (necessary for self-signed certs).
func NewClient(wsURL, baseURL, apiToken string, unlockDurationMs int, tlsFingerprint string, logger *slog.Logger) *Client {
	tlsCfg := &tls.Config{InsecureSkipVerify: true}

	if tlsFingerprint != "" {
		// Parse the expected fingerprint (strip colons, lowercase)
		expected := strings.ToLower(strings.ReplaceAll(tlsFingerprint, ":", ""))
		tlsCfg = &tls.Config{
			InsecureSkipVerify: true, // still needed — cert is self-signed and won't chain to a CA
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return fmt.Errorf("no peer certificate presented")
				}
				cert := cs.PeerCertificates[0]
				fingerprint := sha256.Sum256(cert.Raw)
				actual := hex.EncodeToString(fingerprint[:])
				if actual != expected {
					return fmt.Errorf("TLS certificate fingerprint mismatch: got %s, want %s", actual, expected)
				}
				logger.Debug("UniFi TLS certificate fingerprint verified", "fingerprint", actual)
				return nil
			},
		}
		logger.Info("UniFi TLS certificate pinning enabled", "fingerprint", expected)
	}

	c := &Client{
		wsURL:            wsURL,
		baseURL:          baseURL,
		apiToken:         apiToken,
		logger:           logger,
		done:             make(chan struct{}),
		unlockDurationMs: unlockDurationMs,
		tlsConfig:        tlsCfg,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		degradedThreshold: 5 * time.Minute,
	}
	// Initialize state to Disconnected
	c.state.Store(Disconnected)
	return c
}

// OnEvent registers the handler for access events.
func (c *Client) OnEvent(handler EventHandler) {
	c.handler = handler
}

// OnStateChange registers the callback for connection state changes.
func (c *Client) OnStateChange(callback StateChangeCallback) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.onStateChange = callback
}

// OnReconnect registers a callback fired on every successful reconnect
// (but not the initial connect).
//
// CONTRACT: the callback is invoked synchronously on the connect/reconnect
// goroutine, immediately before the read loop starts. It MUST NOT block —
// any meaningful work (REST calls, replays) must be deferred to the
// caller's own background mechanism (e.g. bg.Group.Go) so the read loop
// is not stalled. A2 in docs/architecture-review.md: prior to A2, this
// site fired its own anonymous `go cb(...)`, which leaked the
// reconnect-backfill goroutine outside any supervised context. The
// caller is now in charge of supervision, which lets shutdowns drain
// the backfill cleanly and lets the per-name gauge see it.
func (c *Client) OnReconnect(callback ReconnectCallback) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.onReconnect = callback
}

// Connected returns whether the WebSocket is currently connected.
func (c *Client) Connected() bool {
	return c.connected.Load()
}

// ReconnectCount returns the total number of reconnect attempts since boot.
// Monotonically increasing; suitable for a Prometheus counter.
func (c *Client) ReconnectCount() int64 {
	return c.reconnectCount.Load()
}

// MessagesReceived returns the total number of WebSocket messages read since
// boot (includes keepalives and non-access events).
func (c *Client) MessagesReceived() int64 {
	return c.messagesReceived.Load()
}

// EventsProcessed returns the total number of AccessEvent instances dispatched
// to the registered handler since boot (the subset of messages that were
// recognised access.logs.add or access.data.device.update events).
func (c *Client) EventsProcessed() int64 {
	return c.eventsProcessed.Load()
}

// Health returns the current health status of the connection.
func (c *Client) Health() HealthStatus {
	c.healthMu.RLock()
	state := c.state.Load().(ConnectionState)
	lastConnected := c.getTime(c.lastConnected)
	lastDisconnected := c.getTime(c.lastDisconnected)
	lastEventAt := c.getTime(c.lastEventAt)
	c.healthMu.RUnlock()

	// Compute uptime if connected
	uptime := ""
	if c.connected.Load() && !lastConnected.IsZero() {
		uptime = time.Since(lastConnected).String()
	}

	// Check if degraded: connected but no event for threshold duration
	if c.connected.Load() && !lastEventAt.IsZero() {
		if time.Since(lastEventAt) > c.degradedThreshold {
			state = Degraded
		}
	}

	return HealthStatus{
		State:             string(state),
		Connected:         c.connected.Load(),
		LastConnected:     lastConnected,
		LastDisconnected:  lastDisconnected,
		ReconnectCount:    c.reconnectCount.Load(),
		LastEventAt:       lastEventAt,
		Uptime:            uptime,
		DegradedThreshold: int(c.degradedThreshold.Seconds()),
	}
}

// getTime safely retrieves a time.Time from an atomic.Value.
func (c *Client) getTime(av atomic.Value) time.Time {
	v := av.Load()
	if v == nil {
		return time.Time{}
	}
	t, ok := v.(time.Time)
	if !ok {
		return time.Time{}
	}
	return t
}

// setState transitions to a new connection state and calls the callback if registered.
func (c *Client) setState(newState ConnectionState) {
	c.healthMu.RLock()
	oldState := c.state.Load().(ConnectionState)
	callback := c.onStateChange
	c.healthMu.RUnlock()

	if oldState == newState {
		return // no state change
	}

	c.state.Store(newState)
	c.logger.Debug("connection state changed", "from", oldState, "to", newState)

	if callback != nil {
		health := c.Health()
		callback(newState, &health)
	}
}

// ─── WebSocket: Real-time event stream ───────────────────────

// Connect establishes the WebSocket connection and starts listening.
// It reconnects automatically on disconnect with exponential backoff.
func (c *Client) Connect(ctx context.Context) {
	go c.connectLoop(ctx)
}

func (c *Client) connectLoop(ctx context.Context) {
	backoff := 5 * time.Second
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			c.setState(Disconnected)
			return
		case <-c.done:
			c.setState(Disconnected)
			return
		default:
		}

		c.setState(Connecting)
		c.logger.Info("connecting to UniFi Access WebSocket", "url", c.wsURL)

		dialer := websocket.Dialer{
			TLSClientConfig:  c.tlsConfig,
			HandshakeTimeout: 10 * time.Second,
		}

		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+c.apiToken)

		conn, _, err := dialer.DialContext(ctx, c.wsURL, headers)
		if err != nil {
			c.logger.Error("WebSocket connect failed", "error", err, "retryIn", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				c.setState(Disconnected)
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Snapshot the last-event timestamp from the *previous* connection
		// before we start the new one. If this is a reconnect (not the
		// initial connect — signalled by reconnectCount > 0) and we have
		// a reconnect callback, fire it so the caller can backfill the
		// outage window.
		//
		// The callback is invoked SYNCHRONOUSLY (A2): the contract on
		// OnReconnect documents that the callback must not block, so a
		// well-behaved caller (cmd/bridge) hands the actual REST work to
		// its own bg.Group. Calling cb in a fresh anonymous goroutine
		// here would leak that work outside any supervision and was the
		// last unmanaged goroutine in the bridge.
		lastBeforeOutage := c.getTime(c.lastEventAt)
		isReconnect := c.reconnectCount.Load() > 0

		c.connMu.Lock()
		c.conn = conn
		c.connMu.Unlock()
		c.connected.Store(true)
		c.lastConnected.Store(time.Now())
		c.setState(Connected)
		c.logger.Info("UniFi Access WebSocket connected")
		backoff = 5 * time.Second // reset

		if isReconnect {
			c.healthMu.RLock()
			cb := c.onReconnect
			c.healthMu.RUnlock()
			if cb != nil {
				cb(lastBeforeOutage)
			}
		}

		c.readLoop(ctx, conn)
		c.connected.Store(false)
		c.lastDisconnected.Store(time.Now())
		c.reconnectCount.Add(1)
		c.setState(Disconnected)
		c.logger.Warn("UniFi WebSocket disconnected, reconnecting...")
	}
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) {
	// ── Ping/Pong keepalive ─────────────────────────────────
	// Send a ping every 30s. If no pong comes back within 10s, the read
	// deadline fires and we reconnect. This catches silently-dead connections
	// (e.g., upstream switch reboot, NIC reset).
	const pingInterval = 30 * time.Second
	const pongTimeout = 10 * time.Second

	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
		return nil
	})
	conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))

	// Ping sender goroutine
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.connMu.Lock()
				err := conn.WriteControl(
					websocket.PingMessage, nil,
					time.Now().Add(5*time.Second),
				)
				c.connMu.Unlock()
				if err != nil {
					c.logger.Debug("ping send failed", "error", err)
					return
				}
			}
		}
	}()
	defer func() { <-pingDone }()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				c.logger.Info("WebSocket closed normally")
			} else {
				c.logger.Warn("WebSocket read error", "error", err)
			}
			return
		}

		c.safeHandleMessage(message)
	}
}

// safeHandleMessage wraps handleMessage with panic recovery so one bad
// message can't crash the entire service.
func (c *Client) safeHandleMessage(data []byte) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("PANIC in message handler (recovered)",
				"panic", r,
				"rawPrefix", string(data[:min(len(data), 200)]),
			)
		}
	}()
	c.handleMessage(data)
}

func (c *Client) handleMessage(data []byte) {
	c.messagesReceived.Add(1)

	// Try to parse as JSON event
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		c.logger.Debug("non-JSON WebSocket message (keepalive)", "raw", string(data[:min(len(data), 100)]))
		return
	}

	eventType, _ := raw["event"].(string)
	if eventType == "" {
		eventType, _ = raw["type"].(string)
	}

	c.logger.Debug("UniFi event received", "type", eventType)

	// Parse access log events
	if eventType != "access.logs.add" && eventType != "access.data.device.update" {
		return
	}

	// Update lastEventAt to track activity
	c.lastEventAt.Store(time.Now())

	eventData, _ := raw["data"].(map[string]any)
	if eventData == nil {
		eventData = raw
	}

	event := AccessEvent{
		EventType: eventType,
		Timestamp: stringFromMap(eventData, "timestamp"),
	}

	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	// Extract nested fields
	if obj, ok := eventData["object"].(map[string]any); ok {
		event.DoorName = stringFromMap(obj, "location")
		event.DoorID = stringFromMap(obj, "id")
		event.Result = stringFromMap(obj, "result")
	}
	if event.DoorName == "" {
		event.DoorName = stringFromMap(eventData, "door_name")
	}
	if event.DoorID == "" {
		event.DoorID = stringFromMap(eventData, "door_id")
	}

	if actor, ok := eventData["actor"].(map[string]any); ok {
		event.ActorName = stringFromMap(actor, "name")
		event.ActorID = stringFromMap(actor, "id")
		event.CredentialID = stringFromMap(actor, "credential_id")
	}
	if event.CredentialID == "" {
		event.CredentialID = stringFromMap(eventData, "credential_id")
	}

	event.AuthType = stringFromMap(eventData, "authentication_type")
	if event.AuthType == "" {
		event.AuthType = stringFromMap(eventData, "auth_type")
	}

	c.logger.Info("door access event",
		"door", event.DoorName,
		"authType", event.AuthType,
		"credential", event.CredentialID,
		"result", event.Result,
	)

	if c.handler != nil {
		c.eventsProcessed.Add(1)
		c.handler(event)
	}
}

// ─── REST: Door control ──────────────────────────────────────

// UnlockDoor sends a remote unlock command to the specified door.
// Per the UniFi Access API (7.9), this is PUT /doors/:id/unlock with an optional
// body containing actor_id, actor_name, and extra fields for system log attribution.
func (c *Client) UnlockDoor(ctx context.Context, doorID string) error {
	return c.UnlockDoorForMember(ctx, doorID, "", "")
}

// UnlockDoorForMember sends a remote unlock with actor attribution so the
// check-in shows up in UniFi system logs with the member's name.
func (c *Client) UnlockDoorForMember(ctx context.Context, doorID, memberName, customerID string) error {
	c.logger.Info("sending remote unlock", "doorId", doorID, "member", memberName)

	url := fmt.Sprintf("%s/doors/%s/unlock", c.baseURL, doorID)

	// Build request body with actor info for UniFi system logs
	bodyMap := map[string]any{}
	if memberName != "" && customerID != "" {
		bodyMap["actor_id"] = customerID
		bodyMap["actor_name"] = memberName
		bodyMap["extra"] = map[string]any{
			"source": "mosaic-bridge",
		}
	}

	bodyBytes, _ := json.Marshal(bodyMap)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create unlock request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unlock request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return fmt.Errorf("unlock returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	c.logger.Info("door unlocked", "doorId", doorID, "member", memberName)
	return nil
}

// ListDoors returns all doors configured in UniFi Access.
func (c *Client) ListDoors(ctx context.Context) ([]Door, error) {
	url := c.baseURL + "/doors"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// UniFi may wrap in { "data": [...] } or return array directly
	var wrapper struct {
		Data []Door `json:"data"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err == nil && len(wrapper.Data) > 0 {
		return wrapper.Data, nil
	}

	var doors []Door
	if err := json.Unmarshal(respBody, &doors); err != nil {
		return nil, fmt.Errorf("unmarshal doors: %w", err)
	}
	return doors, nil
}

// Close shuts down the WebSocket connection.
func (c *Client) Close() {
	close(c.done)
	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMu.Unlock()
}

// ─── REST: Users & Credentials ──────────────────────────────

// UniFiUser represents a user in UniFi Access with their NFC credentials.
type UniFiUser struct {
	ID        string   `json:"id"`
	FirstName string   `json:"first_name"`
	LastName  string   `json:"last_name"`
	Name      string   `json:"name"`      // display name (may be "First Last")
	Email     string   `json:"email"`
	Status    string   `json:"status"`    // ACTIVE, etc.
	NfcTokens []string `json:"nfcTokens"` // extracted NFC card UIDs
}

func (u UniFiUser) FullName() string {
	if u.Name != "" {
		return u.Name
	}
	n := u.FirstName
	if u.LastName != "" {
		if n != "" {
			n += " "
		}
		n += u.LastName
	}
	return n
}

// ListUsers fetches all users from UniFi Access along with their NFC credentials.
// The UniFi Access API returns users with embedded credential objects.
func (c *Client) ListUsers(ctx context.Context) ([]UniFiUser, error) {
	c.logger.Info("fetching UniFi Access users")

	const pageSize = 100
	var allUsers []UniFiUser
	totalRaw := 0

	for page := 1; ; page++ {
		url := fmt.Sprintf("%s/users?page_num=%d&page_size=%d", c.baseURL, page, pageSize)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list users page %d: %w", page, err)
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()

		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		}

		// The API response is wrapped in {"data": [...]}
		var rawUsers []json.RawMessage
		var wrapper struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(respBody, &wrapper); err == nil && len(wrapper.Data) > 0 {
			rawUsers = wrapper.Data
		} else {
			if err := json.Unmarshal(respBody, &rawUsers); err != nil {
				return nil, fmt.Errorf("unmarshal users page %d: %w", page, err)
			}
		}

		totalRaw += len(rawUsers)
		for _, raw := range rawUsers {
			user := parseUniFiUser(raw)
			// Include all users with NFC credentials (any status)
			if len(user.NfcTokens) > 0 {
				allUsers = append(allUsers, user)
			}
		}

		c.logger.Info("UniFi users page fetched", "page", page, "count", len(rawUsers), "withNFC", len(allUsers))

		// If we got fewer than pageSize results, we've hit the last page
		if len(rawUsers) < pageSize {
			break
		}
	}

	c.logger.Info("UniFi users fetch complete", "totalUsers", totalRaw, "withNFC", len(allUsers))
	return allUsers, nil
}

// parseUniFiUser extracts user info and NFC credentials from a raw JSON user object.
// UniFi Access API user objects can have credentials in several formats depending
// on firmware version, so we try multiple field names.
func parseUniFiUser(raw json.RawMessage) UniFiUser {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return UniFiUser{}
	}

	// UA-Hub stores the operator-entered address in `user_email`; the top-level
	// `email` field is a separate verification-workflow slot that is blank for
	// the vast majority of users (see v0.5.5 postmortem: empirically 1613/1618
	// users at LEF have `email=""` but `user_email` populated). Prefer
	// `user_email`, fall back to `email` so we don't regress the handful of
	// users where it was populated the other way.
	email := stringFromAny(obj["user_email"])
	if email == "" {
		email = stringFromAny(obj["email"])
	}

	user := UniFiUser{
		ID:        stringFromAny(obj["id"]),
		FirstName: stringFromAny(obj["first_name"]),
		LastName:  stringFromAny(obj["last_name"]),
		Name:      stringFromAny(obj["name"]),
		Email:     email,
		Status:    stringFromAny(obj["status"]),
	}

	// Try to find NFC card tokens in various possible field locations
	user.NfcTokens = extractNfcTokens(obj)

	return user
}

// extractNfcTokens digs through a UniFi user object to find NFC credential tokens.
// Tries multiple field names that different UniFi Access firmware versions use.
func extractNfcTokens(obj map[string]any) []string {
	var tokens []string

	// Try "nfc_cards" field (array of card objects)
	tokens = append(tokens, extractTokensFromField(obj, "nfc_cards")...)

	// Try "credentials" field (general credential list, filter for NFC type)
	if creds, ok := obj["credentials"].([]any); ok {
		for _, c := range creds {
			if cm, ok := c.(map[string]any); ok {
				credType := stringFromAny(cm["type"])
				if credType == "nfc" || credType == "ua_card" || credType == "NFC" || credType == "nfc_card" {
					if token := stringFromAny(cm["token"]); token != "" {
						tokens = append(tokens, token)
					} else if token := stringFromAny(cm["card_id"]); token != "" {
						tokens = append(tokens, token)
					} else if token := stringFromAny(cm["uid"]); token != "" {
						tokens = append(tokens, token)
					}
				}
			}
		}
	}

	// Try "nfc_credential" (single credential)
	if nfc, ok := obj["nfc_credential"].(map[string]any); ok {
		if token := stringFromAny(nfc["token"]); token != "" {
			tokens = append(tokens, token)
		}
	}

	// Try top-level "nfc_token" (simplest format)
	if token := stringFromAny(obj["nfc_token"]); token != "" {
		tokens = append(tokens, token)
	}

	return tokens
}

func extractTokensFromField(obj map[string]any, fieldName string) []string {
	var tokens []string
	if cards, ok := obj[fieldName].([]any); ok {
		for _, card := range cards {
			if cm, ok := card.(map[string]any); ok {
				// Try common token field names
				for _, key := range []string{"token", "card_id", "uid", "nfc_token", "credential_id"} {
					if t := stringFromAny(cm[key]); t != "" {
						tokens = append(tokens, t)
						break
					}
				}
			} else if s, ok := card.(string); ok && s != "" {
				tokens = append(tokens, s)
			}
		}
	}
	return tokens
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ─── REST: User Status Management ──────────────────────────

// UpdateUserStatus sets a UniFi Access user's status to ACTIVE or DEACTIVATED.
// This is the core mechanism for the daily membership sync: deactivated users
// cannot tap in, active users can.
func (c *Client) UpdateUserStatus(ctx context.Context, userID, status string) error {
	url := fmt.Sprintf("%s/users/%s", c.baseURL, userID)

	body, _ := json.Marshal(map[string]string{"status": status})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create update request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("update user status HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	c.logger.Debug("user status updated", "userId", userID, "status", status)
	return nil
}

// FetchUser retrieves a single UniFi Access user by ID.
func (c *Client) FetchUser(ctx context.Context, userID string) (*UniFiUser, error) {
	url := fmt.Sprintf("%s/users/%s", c.baseURL, userID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch user: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch user HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err != nil {
		return nil, fmt.Errorf("unmarshal user response: %w", err)
	}

	user := parseUniFiUser(wrapper.Data)
	return &user, nil
}

// ListAllUsersWithStatus fetches all UniFi Access users (paginated) and returns
// them with their full status and NFC tokens. Unlike ListUsers, this includes
// users without NFC cards too, so we can track all managed users.
func (c *Client) ListAllUsersWithStatus(ctx context.Context) ([]UniFiUser, error) {
	c.logger.Info("fetching all UniFi Access users with status")

	const pageSize = 100
	var allUsers []UniFiUser

	for page := 1; ; page++ {
		url := fmt.Sprintf("%s/users?page_num=%d&page_size=%d", c.baseURL, page, pageSize)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list users page %d: %w", page, err)
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()

		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		}

		var wrapper struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(respBody, &wrapper); err != nil {
			return nil, fmt.Errorf("unmarshal users page %d: %w", page, err)
		}

		for _, raw := range wrapper.Data {
			user := parseUniFiUser(raw)
			allUsers = append(allUsers, user)
		}

		c.logger.Debug("users page fetched", "page", page, "count", len(wrapper.Data))

		if len(wrapper.Data) < pageSize {
			break
		}
	}

	c.logger.Info("all users fetched", "total", len(allUsers))
	return allUsers, nil
}

// ─── REST: Access log ingestion & backfill ──────────────────

// FetchAccessLogsSince retrieves door-opening log entries from the UA-Hub
// REST API that occurred at or after `since`. Used in two paths:
//
//  1. Steady-state tap ingestion (StartEventPoller) — the WebSocket
//     notifications channel on UA-Hub 4.11.19.0 / UniFi Access 4.2.16
//     no longer emits `access.logs.add` or `access.data.device.update`
//     events for door taps. The developer-API system log is now the
//     authoritative source.
//  2. Reconnect-time backfill after an outage — the caller points this
//     at the moment the connection dropped and drains whatever taps
//     happened in between. (The door obviously can't be retroactively
//     unlocked, but the checkins table, shadow-decisions panel, and
//     Redpoint records stay complete.)
//
// API reference (UniFi Access 4.2.16, March 2026) §9.2: the endpoint is
//
//	POST /api/v1/developer/system/logs
//	Content-Type: application/json
//	Body: {"topic":"door_openings","since":<unix_seconds>}
//
// NOT the earlier GET /access_logs?since_ms=… surface (removed). UniFi
// returns a "no-man zone" 404 for method mismatches rather than 405,
// which is how the regression originally escaped notice.
//
// Returns events ordered oldest-first, which is the order the caller needs
// to replay them through the handler so disagreement counters stay in sync
// with the original timeline.
func (c *Client) FetchAccessLogsSince(ctx context.Context, since time.Time) ([]AccessEvent, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"topic": "door_openings",
		"since": since.Unix(),
	})
	url := c.baseURL + "/system/logs"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create system/logs request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch system/logs: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("system/logs HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Envelope per API reference §9.2:
	//   { "code": "SUCCESS",
	//     "data": { "hits": [ { "_id": "...", "_source": {...} }, ... ] },
	//     "msg": "..." }
	//
	// Older firmware builds wrapped hits differently ({"data":[...]});
	// we accept both to keep this method tolerant across a 4.1 → 4.2
	// upgrade on the same install.
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err != nil {
		return nil, fmt.Errorf("unmarshal system/logs: %w", err)
	}

	var rawEvents []map[string]any
	// Try the 4.2.16 shape: data.hits[]
	var hitsShape struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(wrapper.Data, &hitsShape); err == nil && hitsShape.Hits != nil {
		rawEvents = hitsShape.Hits
	} else {
		// Fallback: data is a bare array of hits.
		if err := json.Unmarshal(wrapper.Data, &rawEvents); err != nil {
			return nil, fmt.Errorf("unmarshal system/logs.data: %w", err)
		}
	}

	events := make([]AccessEvent, 0, len(rawEvents))
	for _, raw := range rawEvents {
		event := parseAccessLogEntry(raw)
		// Only care about NFC and PIN taps for check-in auditing. Mobile
		// keys and admin unlocks flow through the same topic but don't
		// represent a member-facing check-in.
		auth := strings.ToUpper(event.AuthType)
		if auth != "NFC" && auth != "PIN_CODE" {
			continue
		}
		event.IsBackfill = true
		events = append(events, event)
	}

	// Sort oldest-first by timestamp (string compare valid for RFC3339).
	// Simple insertion sort; backfill windows are rarely more than a few
	// hundred events, so sort.Slice's allocation overhead isn't worth it.
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j-1].Timestamp > events[j].Timestamp; j-- {
			events[j-1], events[j] = events[j], events[j-1]
		}
	}

	return events, nil
}

// parseAccessLogEntry extracts an AccessEvent from a single system-log hit.
//
// Envelope (UniFi Access 4.2.16, verified against production on Apr 18 2026):
//
//	{ "_id": "73118",
//	  "_source": {
//	    "actor":          { "id":"...", "display_name":"Ash Smith" },
//	    "authentication": { "credential_provider":"NFC" },
//	    "event":          { "published":1744979...123, "result":"ACCESS",
//	                        "type":"access.door.unlock", "log_key":"..." },
//	    "target":         [ {"type":"door","id":"...","display_name":"Front"},
//	                        {"type":"nfc_id","id":"04A1B2..."}, ... ]
//	  }
//	}
//
// The parser is permissive: anything missing is simply left empty so a
// schema tweak in a future Access build can't crash the poller.
func parseAccessLogEntry(hit map[string]any) AccessEvent {
	event := AccessEvent{
		LogID: stringFromMap(hit, "_id"),
	}

	src, _ := hit["_source"].(map[string]any)
	if src == nil {
		// Ancient / unwrapped shape — fall back to treating the hit
		// itself as _source. Harmless on the new shape (no keys match).
		src = hit
	}

	// ── event.* — timestamp, result, type ──
	if ev, ok := src["event"].(map[string]any); ok {
		// `published` is unix milliseconds per the API reference.
		if ms, ok := ev["published"].(float64); ok {
			event.Timestamp = time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
		}
		if event.Timestamp == "" {
			// Some builds emit a string RFC3339 directly.
			event.Timestamp = stringFromMap(ev, "published")
		}
		event.Result = strings.ToUpper(stringFromMap(ev, "result"))
		event.EventType = stringFromMap(ev, "type")
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if event.EventType == "" {
		event.EventType = "access.logs.add" // keep downstream filters happy
	}

	// ── actor.* — member identity ──
	if actor, ok := src["actor"].(map[string]any); ok {
		event.ActorID = stringFromMap(actor, "id")
		event.ActorName = stringFromMap(actor, "display_name")
		if event.ActorName == "" {
			event.ActorName = stringFromMap(actor, "name")
		}
	}

	// ── authentication.* — how the member identified themselves ──
	if auth, ok := src["authentication"].(map[string]any); ok {
		event.AuthType = stringFromMap(auth, "credential_provider")
	}

	// ── target[] — door, nfc card, hub; each entry is typed ──
	if targets, ok := src["target"].([]any); ok {
		for _, raw := range targets {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch stringFromMap(t, "type") {
			case "door":
				event.DoorID = stringFromMap(t, "id")
				event.DoorName = stringFromMap(t, "display_name")
				if event.DoorName == "" {
					event.DoorName = stringFromMap(t, "name")
				}
			case "nfc_id", "nfc_token":
				// The physical card UID. UA stores this as the hex
				// NFC UID; we pass it through as CredentialID so the
				// downstream matcher can look it up in the mirror.
				event.CredentialID = stringFromMap(t, "id")
			}
		}
	}

	return event
}

// ─── REST: Steady-state tap poller ───────────────────────────

// StartEventPoller drives tap ingestion by polling POST /system/logs
// on a fixed cadence. Each poll fetches the window `[cursorTime, now]`
// from UA-Hub, dedups against the highest LogID seen so far, and
// dispatches new events to the registered OnEvent handler with
// IsBackfill=true.
//
// Cadence is 5s in production (the taps arrive at ~9/day, so a tight
// loop is fine and gives the door the perception of near-realtime
// recording in the dashboard). Callers pass the initial `since` —
// typically today-midnight-local on first boot, so same-day taps
// are backfilled, or the tail of the previous checkins row on
// restart.
//
// This method is a supervised blocking loop: it returns when ctx is
// cancelled. Start it under bg.Group.Go so shutdown drains cleanly.
//
// Why IsBackfill for every poller event, not just the initial catch-up:
// the physical door has already opened (or been denied) by the UA-Hub
// at the moment the tap happened — otherwise the event wouldn't be in
// the system log at all. Issuing a downstream UnlockDoor from the
// bridge would at best be a no-op and at worst double-open the door
// at a time the member has already left. Handlers use IsBackfill as
// the "record-only, no side-effects" gate.
func (c *Client) StartEventPoller(ctx context.Context, since time.Time, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if c.handler == nil {
		return fmt.Errorf("StartEventPoller: no event handler registered (call OnEvent first)")
	}

	cursorTime := since
	var maxLogID int64 // monotonic; skip any hit whose _id <= this

	c.logger.Info("tap poller starting",
		"since", cursorTime.Format(time.RFC3339),
		"interval", interval,
	)

	// Run an immediate poll on startup so operators see today's taps
	// backfill in within one tick rather than waiting `interval`.
	c.pollOnce(ctx, &cursorTime, &maxLogID)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("tap poller stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			c.pollOnce(ctx, &cursorTime, &maxLogID)
		}
	}
}

// pollOnce runs one fetch cycle, updating the cursor + dedup mark in
// place. Errors are logged and swallowed so a transient UA-Hub blip
// doesn't terminate the poller goroutine; the next tick retries.
func (c *Client) pollOnce(ctx context.Context, cursorTime *time.Time, maxLogID *int64) {
	// Fetch with a small timeout so one stuck request can't wedge the
	// poller. Use a slight lookback (30s) off the cursor so a tap that
	// arrived at UA-Hub right at the boundary isn't missed due to
	// clock skew between the bridge and the UDM Pro.
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	lookback := 30 * time.Second
	fetchSince := *cursorTime
	if !fetchSince.IsZero() {
		fetchSince = fetchSince.Add(-lookback)
	}

	events, err := c.FetchAccessLogsSince(fetchCtx, fetchSince)
	if err != nil {
		c.logger.Warn("tap poller fetch failed", "error", err, "since", fetchSince.Format(time.RFC3339))
		return
	}

	if len(events) == 0 {
		return
	}

	// Dispatch only events strictly newer than the last LogID we saw.
	// FetchAccessLogsSince returns oldest-first, so we advance the
	// cursor in time-order and update maxLogID as we go.
	dispatched := 0
	for _, ev := range events {
		// LogID may be numeric-string; compare as int64 when possible.
		// Fall back to a string-equality check against the last one
		// seen (via timestamp) on the off chance UA-Hub switches to
		// opaque ids in a future build.
		idN, ok := parseLogIDInt(ev.LogID)
		if ok {
			if idN <= *maxLogID {
				continue
			}
			*maxLogID = idN
		}

		// Track cursor in time as a secondary index in case LogID is
		// absent. Parse the RFC3339 timestamp; tolerate parse failure.
		if t, perr := time.Parse(time.RFC3339, ev.Timestamp); perr == nil && t.After(*cursorTime) {
			*cursorTime = t
		}

		c.lastEventAt.Store(time.Now())
		c.eventsProcessed.Add(1)
		c.handler(ev)
		dispatched++
	}

	if dispatched > 0 {
		c.logger.Info("tap poller dispatched events",
			"count", dispatched,
			"maxLogID", *maxLogID,
			"cursor", cursorTime.Format(time.RFC3339),
		)
	}
}

// parseLogIDInt extracts the numeric form of a system-log `_id` if it's
// a plain monotonic integer (the 4.2.16 shape). Returns (0,false) when
// the id is missing or non-numeric, in which case callers fall back to
// timestamp-based cursoring.
func parseLogIDInt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < '0' || b > '9' {
			return 0, false
		}
		n = n*10 + int64(b-'0')
		if n < 0 { // overflow guard
			return 0, false
		}
	}
	return n, true
}

// ─── Helpers ─────────────────────────────────────────────────

func stringFromMap(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
