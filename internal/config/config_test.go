package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.UniFi.Host != "127.0.0.1" {
		t.Errorf("UniFi.Host = %q, want 127.0.0.1", cfg.UniFi.Host)
	}
	if cfg.UniFi.Port != 12445 {
		t.Errorf("UniFi.Port = %d, want 12445", cfg.UniFi.Port)
	}
	if cfg.Bridge.Port != 3500 {
		t.Errorf("Bridge.Port = %d, want 3500", cfg.Bridge.Port)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "bridge.json")
	os.WriteFile(configFile, []byte(`{
        "unifi": {"host": "10.0.1.1", "port": 12446, "apiToken": "tok"},
        "redpoint": {"apiKey": "rk", "facilityCode": "TestGym"},
        "bridge": {"port": 4000, "staffPassword": "pass123"}
    }`), 0o644)

	cfg := defaults()
	if err := loadFromFile(configFile, cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.UniFi.Host != "10.0.1.1" {
		t.Errorf("Host = %q", cfg.UniFi.Host)
	}
	if cfg.UniFi.Port != 12446 {
		t.Errorf("Port = %d", cfg.UniFi.Port)
	}
	if cfg.Bridge.Port != 4000 {
		t.Errorf("Bridge.Port = %d", cfg.Bridge.Port)
	}
	// DataDir should retain default since not in file
	if cfg.Bridge.DataDir != "data" {
		t.Errorf("DataDir = %q, want data", cfg.Bridge.DataDir)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	// Set env var
	t.Setenv("UNIFI_HOST", "192.168.1.99")
	t.Setenv("BRIDGE_PORT", "9999")

	cfg := defaults()
	applyEnvOverrides(cfg)

	if cfg.UniFi.Host != "192.168.1.99" {
		t.Errorf("Host = %q, want 192.168.1.99", cfg.UniFi.Host)
	}
	if cfg.Bridge.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Bridge.Port)
	}
}

func TestValidation(t *testing.T) {
	cfg := defaults()
	// Missing all required
	err := validate(cfg)
	if err == nil {
		t.Fatal("expected validation error")
	}

	// Set required fields
	cfg.UniFi.APIToken = "tok"
	cfg.Redpoint.APIKey = "key"
	cfg.Redpoint.FacilityCode = "Mosaic"
	cfg.Bridge.StaffPassword = "pass"
	cfg.Bridge.AdminAPIKey = "adminkey"
	// ControlPort is normally derived by Load(); when driving validate
	// directly we set the Port+1 value Load would compute.
	cfg.Bridge.ControlPort = cfg.Bridge.Port + 1

	err = validate(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Removing any single required field should re-trigger the error, so we
	// catch regressions in the validation list (e.g. a required field being
	// silently dropped). Clearing AdminAPIKey specifically is the case that
	// drifted before — keep an explicit check for it so the list can't
	// silently un-require it in the future.
	cfg.Bridge.AdminAPIKey = ""
	if err := validate(cfg); err == nil {
		t.Fatal("expected validation error when AdminAPIKey is unset")
	}
}

func TestWSURL(t *testing.T) {
	u := UniFiConfig{Host: "10.0.1.1", Port: 12445}
	want := "wss://10.0.1.1:12445/api/v1/developer/devices/notifications"
	if got := u.WSURL(); got != want {
		t.Errorf("WSURL = %q, want %q", got, want)
	}
}

func TestGraphQLURL(t *testing.T) {
	r := RedpointConfig{APIURL: "https://lefclimbing.rphq.com"}
	want := "https://lefclimbing.rphq.com/api/graphql"
	if got := r.GraphQLURL(); got != want {
		t.Errorf("GraphQLURL = %q, want %q", got, want)
	}
}

func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in       string
		wantH    int
		wantM    int
		wantErr  bool
	}{
		{in: "03:00", wantH: 3, wantM: 0},
		{in: "00:00", wantH: 0, wantM: 0},
		{in: "23:59", wantH: 23, wantM: 59},
		{in: " 09:30 ", wantH: 9, wantM: 30}, // trim
		{in: "24:00", wantErr: true},          // hour out of range
		{in: "12:60", wantErr: true},          // minute out of range
		{in: "-1:00", wantErr: true},          // negative
		{in: "abc", wantErr: true},            // not numeric
		{in: "12", wantErr: true},             // missing colon
		{in: "12:34:56", wantErr: true},       // extra colon
		{in: "", wantErr: true},               // empty
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			h, m, err := ParseHHMM(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseHHMM(%q) err = nil, want non-nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseHHMM(%q) err = %v", tc.in, err)
			}
			if h != tc.wantH || m != tc.wantM {
				t.Errorf("ParseHHMM(%q) = (%d, %d), want (%d, %d)", tc.in, h, m, tc.wantH, tc.wantM)
			}
		})
	}
}

func TestValidation_SyncTimeLocal(t *testing.T) {
	// A valid TimeLocal should pass validation.
	cfg := defaults()
	cfg.UniFi.APIToken = "tok"
	cfg.Redpoint.APIKey = "key"
	cfg.Redpoint.FacilityCode = "Mosaic"
	cfg.Bridge.StaffPassword = "pass"
	cfg.Bridge.AdminAPIKey = "adminkey"
	cfg.Bridge.ControlPort = cfg.Bridge.Port + 1
	cfg.Sync.TimeLocal = "03:00"

	if err := validate(cfg); err != nil {
		t.Errorf("validate(valid TimeLocal) returned %v", err)
	}

	// A malformed TimeLocal must surface as a validation error so the
	// operator sees it at boot rather than after a confusing day of
	// not-syncing-on-schedule.
	cfg.Sync.TimeLocal = "25:00"
	if err := validate(cfg); err == nil {
		t.Error("validate(malformed TimeLocal) returned nil, want error")
	}

	// Empty TimeLocal is allowed (interval-ticker fallback).
	cfg.Sync.TimeLocal = ""
	if err := validate(cfg); err != nil {
		t.Errorf("validate(empty TimeLocal) returned %v", err)
	}
}

func TestValidation_AllowNewMembersRequiresPolicies(t *testing.T) {
	// Every `cfg.Bridge.AllowNewMembers=true` bridge that boots must have
	// at least one default access policy ID set. Without it, a freshly-
	// created user has no access group attached and every tap denies —
	// the single most confusing failure mode for staff ("I created the
	// user, why can't they get in?").
	cfg := defaults()
	cfg.UniFi.APIToken = "tok"
	cfg.Redpoint.APIKey = "key"
	cfg.Redpoint.FacilityCode = "Mosaic"
	cfg.Bridge.StaffPassword = "pass"
	cfg.Bridge.AdminAPIKey = "adminkey"
	cfg.Bridge.ControlPort = cfg.Bridge.Port + 1

	cfg.Bridge.AllowNewMembers = true
	// DefaultAccessPolicyIDs still empty — must refuse.
	if err := validate(cfg); err == nil {
		t.Fatal("expected validation error when AllowNewMembers=true and policy list empty")
	}

	cfg.Bridge.DefaultAccessPolicyIDs = []string{"pol-members"}
	if err := validate(cfg); err != nil {
		t.Errorf("validate(AllowNewMembers=true + policy) returned %v", err)
	}

	// With AllowNewMembers=false the policy list is unused and its emptiness
	// must not block boot.
	cfg.Bridge.AllowNewMembers = false
	cfg.Bridge.DefaultAccessPolicyIDs = nil
	if err := validate(cfg); err != nil {
		t.Errorf("validate(AllowNewMembers=false) returned %v (should not care about policy list)", err)
	}
}

func TestValidation_UnmatchedGraceDaysNegative(t *testing.T) {
	cfg := defaults()
	cfg.UniFi.APIToken = "tok"
	cfg.Redpoint.APIKey = "key"
	cfg.Redpoint.FacilityCode = "Mosaic"
	cfg.Bridge.StaffPassword = "pass"
	cfg.Bridge.AdminAPIKey = "adminkey"
	cfg.Bridge.ControlPort = cfg.Bridge.Port + 1

	cfg.Bridge.UnmatchedGraceDays = -1
	if err := validate(cfg); err == nil {
		t.Error("expected validation error when UnmatchedGraceDays is negative")
	}

	// Zero is intentionally allowed (exercises the expiry path with no grace).
	cfg.Bridge.UnmatchedGraceDays = 0
	if err := validate(cfg); err != nil {
		t.Errorf("validate(UnmatchedGraceDays=0) returned %v, want nil", err)
	}
}

func TestEnvStringSlice(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{name: "single", env: "pol-1", want: []string{"pol-1"}},
		{name: "multi", env: "pol-1,pol-2,pol-3", want: []string{"pol-1", "pol-2", "pol-3"}},
		{name: "trim", env: "pol-1 , pol-2 ", want: []string{"pol-1", "pol-2"}},
		{name: "drop empty", env: "pol-1,,pol-2", want: []string{"pol-1", "pol-2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_POL_IDS", tc.env)
			var got []string
			envStringSlice(&got, "TEST_POL_IDS")
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
	t.Run("missing env preserves existing value", func(t *testing.T) {
		os.Unsetenv("TEST_POL_IDS_MISSING")
		existing := []string{"keep-me"}
		envStringSlice(&existing, "TEST_POL_IDS_MISSING")
		if len(existing) != 1 || existing[0] != "keep-me" {
			t.Errorf("envStringSlice with missing env should not touch the target; got %v", existing)
		}
	})
}

func TestValidation_ControlPort(t *testing.T) {
	base := func() *Config {
		cfg := defaults()
		cfg.UniFi.APIToken = "tok"
		cfg.Redpoint.APIKey = "key"
		cfg.Redpoint.FacilityCode = "Mosaic"
		cfg.Bridge.StaffPassword = "pass"
		cfg.Bridge.AdminAPIKey = "adminkey"
		return cfg
	}

	// A sensible configured pair passes. Port+1 is the common case
	// Load() will auto-pick, so we mirror that here to prove validate()
	// is happy with the derived default.
	cfg := base()
	cfg.Bridge.Port = 3500
	cfg.Bridge.ControlPort = 3501
	if err := validate(cfg); err != nil {
		t.Errorf("validate(port=3500, control=3501) returned %v", err)
	}

	// Equal ports must refuse — collapsing them defeats the split entirely
	// (the public listener would expose the mutating endpoints again).
	cfg.Bridge.ControlPort = cfg.Bridge.Port
	if err := validate(cfg); err == nil {
		t.Error("validate(Port==ControlPort) returned nil, want error")
	}

	// Out-of-range control port is rejected.
	cfg.Bridge.ControlPort = 0
	if err := validate(cfg); err == nil {
		t.Error("validate(ControlPort=0) returned nil, want error")
	}
	cfg.Bridge.ControlPort = 70000
	if err := validate(cfg); err == nil {
		t.Error("validate(ControlPort=70000) returned nil, want error")
	}
	cfg.Bridge.ControlPort = -1
	if err := validate(cfg); err == nil {
		t.Error("validate(ControlPort=-1) returned nil, want error")
	}
}

func TestDefaults_ControlBindAddr(t *testing.T) {
	cfg := defaults()
	// The whole point of the split is loopback-by-default — if someone
	// changes this to "" or "0.0.0.0" in defaults() they have broken the
	// security posture the refactor exists to create. Freeze it.
	if cfg.Bridge.ControlBindAddr != "127.0.0.1" {
		t.Errorf("defaults Bridge.ControlBindAddr = %q, want 127.0.0.1", cfg.Bridge.ControlBindAddr)
	}
}

func TestLoad_ControlPortDerivation(t *testing.T) {
	// Seed all required secrets + a file that only sets Port so we
	// exercise the Port+1 auto-derivation inside Load().
	dir := t.TempDir()
	configFile := filepath.Join(dir, "bridge.json")
	os.WriteFile(configFile, []byte(`{
        "unifi": {"apiToken": "tok"},
        "redpoint": {"apiKey": "rk", "facilityCode": "TestGym"},
        "bridge": {"port": 4000, "staffPassword": "pass123", "adminApiKey": "adminkey"}
    }`), 0o644)

	t.Setenv("BRIDGE_CONFIG", configFile)
	// Belt-and-braces: the process may inherit these from the test harness.
	t.Setenv("BRIDGE_CONTROL_PORT", "")
	os.Unsetenv("BRIDGE_CONTROL_PORT")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Bridge.Port != 4000 {
		t.Fatalf("Bridge.Port = %d, want 4000", cfg.Bridge.Port)
	}
	if cfg.Bridge.ControlPort != 4001 {
		t.Errorf("Bridge.ControlPort = %d, want 4001 (Port+1 auto-derivation)", cfg.Bridge.ControlPort)
	}

	// Explicit override wins over the auto-derivation.
	t.Setenv("BRIDGE_CONTROL_PORT", "9090")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with BRIDGE_CONTROL_PORT=9090: %v", err)
	}
	if cfg.Bridge.ControlPort != 9090 {
		t.Errorf("Bridge.ControlPort = %d, want 9090 (explicit override)", cfg.Bridge.ControlPort)
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte(`
# Comment
UNIFI_API_TOKEN='quoted-value'
REDPOINT_API_KEY="double-quoted"
PLAIN_KEY=no-quotes
`), 0o644)

	// Clear env first
	t.Setenv("UNIFI_API_TOKEN", "")
	t.Setenv("REDPOINT_API_KEY", "")
	t.Setenv("PLAIN_KEY", "")
	os.Unsetenv("UNIFI_API_TOKEN")
	os.Unsetenv("REDPOINT_API_KEY")
	os.Unsetenv("PLAIN_KEY")

	loadDotEnv(envFile)

	if v := os.Getenv("UNIFI_API_TOKEN"); v != "quoted-value" {
		t.Errorf("UNIFI_API_TOKEN = %q", v)
	}
	if v := os.Getenv("REDPOINT_API_KEY"); v != "double-quoted" {
		t.Errorf("REDPOINT_API_KEY = %q", v)
	}
	if v := os.Getenv("PLAIN_KEY"); v != "no-quotes" {
		t.Errorf("PLAIN_KEY = %q", v)
	}
}
