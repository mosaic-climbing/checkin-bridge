// Package config loads bridge configuration from a JSON file with env var overrides.
//
// Priority (highest wins):
//  1. Environment variables
//  2. Config file (bridge.json or path in BRIDGE_CONFIG env)
//  3. Defaults
//
// Secrets (API keys, passwords) should always be set via env vars in production.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the top-level configuration for the bridge.
type Config struct {
	UniFi    UniFiConfig    `json:"unifi"`
	Redpoint RedpointConfig `json:"redpoint"`
	Bridge   BridgeConfig   `json:"bridge"`
	Sync     SyncConfig     `json:"sync"`
}

// UniFiConfig holds UniFi Access connection settings.
type UniFiConfig struct {
	Host           string `json:"host"`            // default: "127.0.0.1"
	Port           int    `json:"port"`            // default: 12445
	APIToken       string `json:"apiToken"`        // REQUIRED (prefer env: UNIFI_API_TOKEN)
	TLSFingerprint string `json:"tlsFingerprint"`  // optional SHA-256 hex
}

func (u UniFiConfig) WSURL() string {
	return fmt.Sprintf("wss://%s:%d/api/v1/developer/devices/notifications", u.Host, u.Port)
}

func (u UniFiConfig) BaseURL() string {
	return fmt.Sprintf("https://%s:%d/api/v1/developer", u.Host, u.Port)
}

// RedpointConfig holds Redpoint HQ API settings.
type RedpointConfig struct {
	APIURL       string `json:"apiUrl"`       // default: "https://lefclimbing.rphq.com"
	APIKey       string `json:"apiKey"`       // REQUIRED (prefer env: REDPOINT_API_KEY)
	FacilityCode string `json:"facilityCode"` // default: "Mosaic"
	GateID       string `json:"gateId"`       // optional, omit to disable check-in recording
}

func (r RedpointConfig) GraphQLURL() string {
	return strings.TrimRight(r.APIURL, "/") + "/api/graphql"
}

// BridgeConfig holds local bridge server settings.
type BridgeConfig struct {
	Port             int    `json:"port"`             // default: 3500
	LogLevel         string `json:"logLevel"`         // "debug" or "info" (default)
	DataDir          string `json:"dataDir"`          // default: "data"
	UnlockDurationMs int    `json:"unlockDurationMs"` // default: 5000
	AdminAPIKey      string `json:"adminApiKey"`      // optional (prefer env: ADMIN_API_KEY)
	StaffPassword    string `json:"staffPassword"`    // REQUIRED (prefer env: STAFF_PASSWORD)
	AllowedNetworks  string `json:"allowedNetworks"`  // comma-separated CIDRs, optional
	// TrustedProxies is a comma-separated CIDR list of upstream
	// forwarders whose X-Forwarded-For / X-Real-IP headers the bridge
	// will honour. Empty (default) means "no proxy in front of us" —
	// forwarding headers are ignored and r.RemoteAddr is the peer
	// identity. Set this when fronting the bridge with nginx, Traefik,
	// Caddy, or a load balancer on the UDM Pro; otherwise leave empty.
	TrustedProxies   string `json:"trustedProxies"`
	// BindAddr is the TCP bind address for the public data-plane listener.
	// Defaults to "127.0.0.1" (loopback-only); operators who need LAN
	// reachability must set this explicitly AND set ALLOWED_NETWORKS to
	// the staff subnet. Setting BindAddr="" means "all interfaces".
	BindAddr         string `json:"bindAddr"`

	// ControlPort is the TCP port for the control-plane listener which hosts
	// the mutating admin endpoints (POST /unlock/{doorId}, /cache/sync,
	// /directory/sync, /ingest/unifi, /status-sync, and the devhooks-gated
	// /test-checkin). Separating these from the public data-plane listener
	// (BindAddr:Port, which serves /ui/* and the read-only /checkins and
	// /directory/search) means the control plane can be bound to loopback
	// only (via ControlBindAddr, default "127.0.0.1") so an attacker who
	// pivots into the LAN still can't pop doors without a foothold on the
	// bridge host itself.
	//
	// When zero (the default), Load() sets ControlPort = Port + 1 so a
	// default bridge listens on :3500 (public) and 127.0.0.1:3501
	// (control). Operators who need a specific port — e.g. because Port+1
	// collides with something else on the host — can set
	// BRIDGE_CONTROL_PORT explicitly. Validation refuses equal ports and
	// out-of-range values at boot.
	ControlPort      int    `json:"controlPort"`
	ControlBindAddr  string `json:"controlBindAddr"`  // default: "127.0.0.1"; set "" to mirror BindAddr
	ShadowMode       bool   `json:"shadowMode"`       // if true: no door unlocks, no Redpoint writes, no UniFi status writes

	// RecheckMaxStaleness is the freshness budget for the cached membership
	// state used by the denied-tap recheck path (see internal/recheck). When
	// a tap is denied based on local cache, the recheck normally pays a
	// live Redpoint query to find out whether the member just renewed. If
	// RecheckMaxStaleness is non-zero and the cached member's CachedAt is
	// younger than this duration, the recheck is SKIPPED — the cache is
	// trusted and the denial stands without contacting Redpoint.
	//
	// Zero (the default) reproduces the pre-A3 behaviour: every denied
	// tap pays a recheck, modulo the breaker. A gym whose Redpoint sync
	// runs hourly might set "2h" to absorb tap storms by frustrated
	// (correctly-denied) members; a gym on the daily default should keep
	// this at 0 because a 24-hour-stale cache is too coarse to trust.
	//
	// Parsed by Go's time.ParseDuration when set via JSON or env
	// (BRIDGE_RECHECK_MAX_STALENESS=2h). Validation refuses negatives.
	RecheckMaxStaleness time.Duration `json:"recheckMaxStaleness"`

	// BackfillOnReconnect, when true, causes the bridge to fetch UniFi
	// access-log entries missed during a WebSocket outage and replay them
	// through the check-in handler. The door obviously can't be retro-
	// actively unlocked, but the audit trail (checkins table, shadow-
	// decisions stats, Redpoint records) stays complete.
	//
	// Off by default because replaying taps into Redpoint during an outage
	// recovery could double-record events if a partial recovery already
	// went through; enable once you're confident about deduplication.
	BackfillOnReconnect bool `json:"backfillOnReconnect"`

	// AllowNewMembers enables the /ui/members/new provisioning flow. When
	// false (the default), the endpoint returns 404 even for authenticated
	// staff — intended for gyms that prefer to create UA-Hub users via
	// UA-Hub's native admin and only use the bridge for sync. When true,
	// DefaultAccessPolicyIDs must be non-empty (boot validation enforces).
	AllowNewMembers bool `json:"allowNewMembers"`

	// DefaultAccessPolicyIDs is the list of UA-Hub access-policy IDs
	// attached to a user created via /ui/members/new. Configured once at
	// install time to point at the "members" access group. Without a
	// policy attached, a freshly-created UA-Hub user exists but every tap
	// denies — the most confusing possible failure mode, so boot refuses
	// to start with AllowNewMembers=true and an empty list.
	DefaultAccessPolicyIDs []string `json:"defaultAccessPolicyIds"`

	// UnmatchedGraceDays sets the window (in days) a UA-Hub user stays in
	// ua_user_mappings_pending before the bridge default-deactivates them
	// in UA-Hub. Default 7. Zero means "deactivate immediately" which is
	// almost never what an operator wants; the only reason to set it is
	// to exercise the expiry path in tests.
	UnmatchedGraceDays int `json:"unmatchedGraceDays"`

	// RequireMinimumUAHubVersion, when true, refuses to enable
	// AllowNewMembers if the UA-Hub firmware does not support the
	// user_email field at create time (requires 1.22.16+). When false,
	// the bridge degrades gracefully to POST /users followed by a
	// PUT /users/:id email write.
	RequireMinimumUAHubVersion bool `json:"requireMinimumUAHubVersion"`

	// EnableTestHooks is the runtime kill-switch for the /test-checkin
	// simulation endpoint (S5 in the architecture review). The route is
	// also compile-time-gated behind the `devhooks` build tag — the
	// default production binary ignores this field entirely. When the
	// binary IS compiled with `-tags devhooks`, EnableTestHooks must
	// additionally be set to true (env: BRIDGE_ENABLE_TEST_HOOKS=true)
	// to actually register the route. Keep false unless you know what
	// you're doing; a stolen admin API key with this on becomes
	// unlimited free check-ins + unlock pulses.
	EnableTestHooks bool `json:"enableTestHooks"`

	// HTTPS, when true, enables HTTPS-aware cookie and header security.
	// Sets Secure=true on session and CSRF cookies, and emits the
	// Strict-Transport-Security header on every response. Only enable
	// when the bridge is deployed behind a TLS-terminating reverse proxy
	// (nginx, Traefik, Caddy, or load balancer). Preserves HTTP-only
	// mode (Secure=false, no HSTS) when false (the default), for
	// LAN/HTTP deployments.
	HTTPS bool `json:"https"`
}

// SyncConfig holds synchronization schedule settings.
type SyncConfig struct {
	Interval      time.Duration `json:"-"`             // parsed from IntervalHours
	IntervalHours int           `json:"intervalHours"` // default: 24
	PageSize      int           `json:"pageSize"`      // default: 100

	// TimeLocal, if non-empty, pins the daily sync to this wall-clock time
	// in the host's local timezone (HH:MM, e.g. "03:00"). When set, the
	// statusync loop sleeps until the next occurrence of TimeLocal instead
	// of using a drifting interval ticker. This matters because Redpoint
	// updates membership state on a daily cadence; the bridge should sync
	// shortly after that window to minimise effective propagation lag.
	// Leave empty to fall back to interval-ticker behaviour.
	TimeLocal string `json:"timeLocal"`
}

// Load reads configuration from file + env vars.
// Order: defaults → config file → env vars.
func Load() (*Config, error) {
	cfg := defaults()

	// Try loading config file
	configPath := os.Getenv("BRIDGE_CONFIG")
	if configPath == "" {
		// Check common locations
		for _, p := range []string{"bridge.json", "/etc/mosaic/bridge.json"} {
			if _, err := os.Stat(p); err == nil {
				configPath = p
				break
			}
		}
	}

	if configPath != "" {
		if err := loadFromFile(configPath, cfg); err != nil {
			return nil, fmt.Errorf("config file %s: %w", configPath, err)
		}
	}

	// Also try legacy .env file
	loadDotEnv(".env")

	// Env vars override everything
	applyEnvOverrides(cfg)

	// Parse derived fields
	if cfg.Sync.IntervalHours > 0 {
		cfg.Sync.Interval = time.Duration(cfg.Sync.IntervalHours) * time.Hour
	}

	// Default the control-plane port to public-port+1 when unset. We only
	// auto-derive; if the operator set both Port and ControlPort explicitly
	// we keep their choice (even if equal — validate() below is the gate
	// that refuses to boot in that case with a clear message).
	if cfg.Bridge.ControlPort == 0 && cfg.Bridge.Port > 0 {
		cfg.Bridge.ControlPort = cfg.Bridge.Port + 1
	}

	// Validate
	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func defaults() *Config {
	return &Config{
		UniFi: UniFiConfig{
			Host: "127.0.0.1",
			Port: 12445,
		},
		Redpoint: RedpointConfig{
			APIURL:       "https://lefclimbing.rphq.com",
			FacilityCode: "Mosaic",
		},
		Bridge: BridgeConfig{
			Port:               3500,
			LogLevel:           "info",
			DataDir:            "data",
			UnlockDurationMs:   5000,
			UnmatchedGraceDays: 7,
			BindAddr:           "127.0.0.1",
			ControlBindAddr:    "127.0.0.1",
		},
		Sync: SyncConfig{
			IntervalHours: 24,
			PageSize:      100,
		},
	}
}

func loadFromFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, cfg)
}

func applyEnvOverrides(cfg *Config) {
	// UniFi
	envStr(&cfg.UniFi.Host, "UNIFI_HOST")
	envInt(&cfg.UniFi.Port, "UNIFI_PORT")
	envStr(&cfg.UniFi.APIToken, "UNIFI_API_TOKEN")
	envStr(&cfg.UniFi.TLSFingerprint, "UNIFI_TLS_FINGERPRINT")

	// Redpoint
	envStr(&cfg.Redpoint.APIURL, "REDPOINT_API_URL")
	envStr(&cfg.Redpoint.APIKey, "REDPOINT_API_KEY")
	envStr(&cfg.Redpoint.FacilityCode, "REDPOINT_FACILITY_CODE")
	envStr(&cfg.Redpoint.GateID, "REDPOINT_GATE_ID")

	// Bridge
	envInt(&cfg.Bridge.Port, "BRIDGE_PORT")
	envStr(&cfg.Bridge.LogLevel, "LOG_LEVEL")
	envStr(&cfg.Bridge.DataDir, "DATA_DIR")
	envInt(&cfg.Bridge.UnlockDurationMs, "UNLOCK_DURATION_MS")
	envStr(&cfg.Bridge.AdminAPIKey, "ADMIN_API_KEY")
	envStr(&cfg.Bridge.StaffPassword, "STAFF_PASSWORD")
	envStr(&cfg.Bridge.AllowedNetworks, "ALLOWED_NETWORKS")
	envStr(&cfg.Bridge.TrustedProxies, "TRUSTED_PROXIES")
	envStr(&cfg.Bridge.BindAddr, "BIND_ADDR")
	envInt(&cfg.Bridge.ControlPort, "BRIDGE_CONTROL_PORT")
	envStr(&cfg.Bridge.ControlBindAddr, "BRIDGE_CONTROL_BIND_ADDR")
	envBool(&cfg.Bridge.ShadowMode, "BRIDGE_SHADOW_MODE")
	envDuration(&cfg.Bridge.RecheckMaxStaleness, "BRIDGE_RECHECK_MAX_STALENESS")
	envBool(&cfg.Bridge.BackfillOnReconnect, "BRIDGE_BACKFILL_ON_RECONNECT")
	envBool(&cfg.Bridge.AllowNewMembers, "BRIDGE_ALLOW_NEW_MEMBERS")
	envStringSlice(&cfg.Bridge.DefaultAccessPolicyIDs, "BRIDGE_DEFAULT_ACCESS_POLICY_IDS")
	envInt(&cfg.Bridge.UnmatchedGraceDays, "BRIDGE_UNMATCHED_GRACE_DAYS")
	envBool(&cfg.Bridge.RequireMinimumUAHubVersion, "BRIDGE_REQUIRE_MINIMUM_UAHUB_VERSION")
	envBool(&cfg.Bridge.EnableTestHooks, "BRIDGE_ENABLE_TEST_HOOKS")
	envBool(&cfg.Bridge.HTTPS, "BRIDGE_HTTPS")

	// Sync
	envInt(&cfg.Sync.IntervalHours, "SYNC_INTERVAL_HOURS")
	envInt(&cfg.Sync.PageSize, "SYNC_PAGE_SIZE")
	envStr(&cfg.Sync.TimeLocal, "SYNC_TIME_LOCAL")
}

func validate(cfg *Config) error {
	var missing []string
	if cfg.UniFi.APIToken == "" {
		missing = append(missing, "UNIFI_API_TOKEN")
	}
	if cfg.Redpoint.APIKey == "" {
		missing = append(missing, "REDPOINT_API_KEY")
	}
	if cfg.Redpoint.FacilityCode == "" {
		missing = append(missing, "REDPOINT_FACILITY_CODE")
	}
	if cfg.Bridge.StaffPassword == "" {
		missing = append(missing, "STAFF_PASSWORD")
	}
	// ADMIN_API_KEY is required: when empty, the admin-auth gate in
	// api/middleware.go falls open ("if cfg.AdminAPIKey != "" && ..."), leaving
	// mutating endpoints like /members, /cache/sync, /ingest/unifi
	// unauthenticated. Refuse to boot without one.
	if cfg.Bridge.AdminAPIKey == "" {
		missing = append(missing, "ADMIN_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration:\n  %s\n\nSet via environment variables or bridge.json config file", strings.Join(missing, "\n  "))
	}
	// SyncTimeLocal is optional, but if it's set we want a parse failure at
	// boot — not a silent fall-through to interval ticking with the operator
	// believing the wall-clock schedule is in effect.
	if cfg.Sync.TimeLocal != "" {
		if _, _, err := ParseHHMM(cfg.Sync.TimeLocal); err != nil {
			return fmt.Errorf("invalid Sync.TimeLocal %q: %w (expected HH:MM, e.g. \"03:00\")", cfg.Sync.TimeLocal, err)
		}
	}
	// If new-member provisioning is on, DefaultAccessPolicyIDs must be
	// non-empty. Creating a UA-Hub user with no access policies attached
	// produces a user that looks normal in the admin UI but denies every
	// tap, which is the most confusing possible failure — refuse to boot.
	if cfg.Bridge.AllowNewMembers {
		if len(cfg.Bridge.DefaultAccessPolicyIDs) == 0 {
			return fmt.Errorf("Bridge.AllowNewMembers=true requires Bridge.DefaultAccessPolicyIDs to be non-empty (set BRIDGE_DEFAULT_ACCESS_POLICY_IDS or defaultAccessPolicyIds in bridge.json)")
		}
	}
	// UnmatchedGraceDays cannot be negative; zero is allowed (but only
	// sensible in tests / operator-forced immediate-deactivate mode).
	if cfg.Bridge.UnmatchedGraceDays < 0 {
		return fmt.Errorf("Bridge.UnmatchedGraceDays must be >= 0 (got %d)", cfg.Bridge.UnmatchedGraceDays)
	}
	// RecheckMaxStaleness cannot be negative. Zero is the documented
	// "always recheck" default; any positive duration turns on the
	// freshness gate in internal/recheck.
	if cfg.Bridge.RecheckMaxStaleness < 0 {
		return fmt.Errorf("Bridge.RecheckMaxStaleness must be >= 0 (got %s)", cfg.Bridge.RecheckMaxStaleness)
	}
	// Control-plane port must be in the legal TCP range and distinct from
	// the public port — collapsing them defeats the whole point of the
	// split (the public listener would expose the mutating endpoints
	// again). We don't try to detect "Port+1 collides with another local
	// service"; that's an OS-level Listen() failure and surfaces clearly
	// at boot.
	if cfg.Bridge.ControlPort < 1 || cfg.Bridge.ControlPort > 65535 {
		return fmt.Errorf("Bridge.ControlPort must be in [1, 65535] (got %d)", cfg.Bridge.ControlPort)
	}
	if cfg.Bridge.ControlPort == cfg.Bridge.Port {
		return fmt.Errorf("Bridge.ControlPort (%d) must differ from Bridge.Port (%d) — the split exists so the control plane can be bound to loopback only", cfg.Bridge.ControlPort, cfg.Bridge.Port)
	}
	return nil
}

// ParseHHMM parses a "HH:MM" string into hour (0-23) and minute (0-59).
// Returns an error if the string isn't well-formed or values are out of range.
func ParseHHMM(s string) (hour, minute int, err error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	h, herr := strconv.Atoi(parts[0])
	if herr != nil {
		return 0, 0, fmt.Errorf("hour: %w", herr)
	}
	m, merr := strconv.Atoi(parts[1])
	if merr != nil {
		return 0, 0, fmt.Errorf("minute: %w", merr)
	}
	if h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("hour %d out of range [0,23]", h)
	}
	if m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("minute %d out of range [0,59]", m)
	}
	return h, m, nil
}

// NonSecretHash returns a short hex hash of the configuration's non-secret
// fields. Emitted at startup so an operator can tell at a glance whether a
// config change has taken effect, and postmortem readers can correlate a
// restart with a config change. Secrets (API keys, passwords) are explicitly
// zeroed before hashing so they don't contribute to the hash and can be
// rotated without appearing to "drift".
func (c *Config) NonSecretHash() string {
	// Copy so we don't mutate the caller's Config.
	redacted := *c
	redacted.UniFi.APIToken = ""
	redacted.UniFi.TLSFingerprint = ""
	redacted.Redpoint.APIKey = ""
	redacted.Bridge.AdminAPIKey = ""
	redacted.Bridge.StaffPassword = ""

	// time.Duration doesn't JSON-marshal as a number consistently across
	// versions, and is derived anyway — zero it so we hash only inputs.
	redacted.Sync.Interval = 0

	buf, err := json.Marshal(redacted)
	if err != nil {
		return "err"
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])[:12]
}

// loadDotEnv reads a .env file (KEY=VALUE per line) into os environment.
// Strips surrounding quotes from values. Ignores missing files.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Strip surrounding quotes
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}

		// Only set if not already in environment (env takes precedence)
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

// Helper functions for env var overrides

func envStr(target *string, key string) {
	if v := os.Getenv(key); v != "" {
		*target = v
	}
}

func envInt(target *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*target = n
		}
	}
}

// envBool accepts 1/true/yes/on (case-insensitive) as true; 0/false/no/off as false.
// Unrecognized values leave the target unchanged.
func envBool(target *bool, key string) {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return
	}
	switch v {
	case "1", "true", "yes", "on":
		*target = true
	case "0", "false", "no", "off":
		*target = false
	}
}

// envDuration parses a Go duration string (e.g. "2h", "500ms", "90s") into
// the target. Empty or unparseable values leave the target unchanged — we
// log nothing here to keep Load() side-effect-free, so typos silently
// retain the JSON/default value. Validation in validate() catches negative
// durations where the field disallows them.
func envDuration(target *time.Duration, key string) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	if d, err := time.ParseDuration(v); err == nil {
		*target = d
	}
}

// envStringSlice parses a comma-separated env value into a string slice.
// Empty entries are dropped and each entry is trimmed. Missing env var
// leaves the target unchanged (so a JSON-file-provided value survives).
func envStringSlice(target *[]string, key string) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	*target = out
}
