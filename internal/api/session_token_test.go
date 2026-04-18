package api

import (
	"strings"
	"testing"
)

// S3 guards — session token format and signature strength.

func TestSessionToken_CarriesVersionPrefix(t *testing.T) {
	sm := NewSessionManager("test-pass")
	token, _, err := sm.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, sessionTokenPrefix) {
		t.Errorf("token %q should start with %q", token, sessionTokenPrefix)
	}
}

func TestSessionToken_FullLengthHMAC(t *testing.T) {
	// Session token format is "<prefix><raw>.<sig>". The signature is the
	// full hex SHA-256 HMAC (64 chars), not the old 16-char truncation.
	sm := NewSessionManager("test-pass")
	token, _, err := sm.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(token, sessionTokenPrefix)
	parts := strings.SplitN(body, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("token body %q should have exactly one dot", body)
	}
	if got := len(parts[1]); got != 64 {
		t.Errorf("signature length = %d, want 64 (SHA-256 hex)", got)
	}
}

func TestValidateSession_RejectsLegacyV1Format(t *testing.T) {
	// Simulate a v1 token (no prefix, 16-char truncated signature). Even if
	// the signature were valid under the old scheme, v2 must reject it on
	// format alone — this is what ensures a clean cut-over on upgrade.
	sm := NewSessionManager("test-pass")

	// Construct something that *looks* like a v1 token would have: 48-hex
	// raw, dot, 16-hex sig. Without the v2| prefix this should fail.
	legacy := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbb"
	if sm.ValidateSession(legacy) {
		t.Error("legacy v1-shaped token must not validate under v2")
	}
}

func TestValidateSession_RejectsWrongVersionPrefix(t *testing.T) {
	sm := NewSessionManager("test-pass")

	// Mint a real token, then rewrite the prefix to something else.
	token, _, err := sm.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(token, sessionTokenPrefix)
	forged := "v3|" + body
	if sm.ValidateSession(forged) {
		t.Error("token with unknown version prefix must not validate")
	}
}

func TestValidateSession_RejectsForgedFullLengthSignature(t *testing.T) {
	sm := NewSessionManager("test-pass")

	// Build a token with the right prefix and a plausible raw body but a
	// signature that's full-length hex yet not the correct HMAC. The check
	// must still reject it.
	forged := sessionTokenPrefix +
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" + "." +
		strings.Repeat("f", 64)
	if sm.ValidateSession(forged) {
		t.Error("forged signature of correct length must not validate")
	}
}

func TestValidateSession_RoundTripV2(t *testing.T) {
	sm := NewSessionManager("test-pass")
	token, _, err := sm.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if !sm.ValidateSession(token) {
		t.Errorf("freshly-minted v2 token %q must validate", token)
	}
	sm.DestroySession(token)
	if sm.ValidateSession(token) {
		t.Error("destroyed v2 token must not validate")
	}
}

func TestValidateSession_RejectsEmptyAndMalformed(t *testing.T) {
	sm := NewSessionManager("test-pass")

	cases := []string{
		"",                     // empty
		"v2|",                  // prefix only
		"v2|raw-only-no-dot",   // missing signature half
		"v2|.onlysigno-raw",    // missing raw half
		sessionTokenPrefix + "extra|dots|in|raw.sig", // sig section looks ok but raw is garbage
	}
	for _, c := range cases {
		if sm.ValidateSession(c) {
			t.Errorf("malformed token %q must not validate", c)
		}
	}
}
