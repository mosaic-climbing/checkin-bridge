package unifi

import (
	"encoding/json"
	"testing"
)

// TestParseAccessLogEntry_SystemLogsEnvelope exercises the 4.2.16
// POST /system/logs envelope that the v0.5.0 poller consumes. The
// payload is taken verbatim from the verified production call on
// Apr 18 2026 (the LEF Ash Smith PIN tap) so the parser stays
// locked to the real shape; any UniFi firmware change that alters
// the field names will immediately fail this test rather than
// silently regress to "zero taps".
func TestParseAccessLogEntry_SystemLogsEnvelope(t *testing.T) {
	hit := mustHit(t, `{
		"_id": "73118",
		"_source": {
			"actor": {
				"id":           "cust-17",
				"display_name": "Ash Smith"
			},
			"authentication": {
				"credential_provider": "PIN_CODE"
			},
			"event": {
				"published": 1744979880123,
				"result":    "ACCESS",
				"type":      "access.door.unlock",
				"log_key":   "opaque-log-key"
			},
			"target": [
				{"type":"door",   "id":"door-front", "display_name":"Front Door"},
				{"type":"nfc_id", "id":"04A1B2C3D4"},
				{"type":"UAH",    "id":"hub-1"}
			]
		}
	}`)

	ev := parseAccessLogEntry(hit)

	if ev.LogID != "73118" {
		t.Errorf("LogID = %q, want %q", ev.LogID, "73118")
	}
	if ev.ActorID != "cust-17" || ev.ActorName != "Ash Smith" {
		t.Errorf("actor = %q/%q, want cust-17/Ash Smith", ev.ActorID, ev.ActorName)
	}
	if ev.AuthType != "PIN_CODE" {
		t.Errorf("AuthType = %q, want PIN_CODE", ev.AuthType)
	}
	if ev.Result != "ACCESS" {
		t.Errorf("Result = %q, want ACCESS (upper-cased)", ev.Result)
	}
	if ev.DoorID != "door-front" || ev.DoorName != "Front Door" {
		t.Errorf("door = %q/%q, want door-front/Front Door", ev.DoorID, ev.DoorName)
	}
	if ev.CredentialID != "04A1B2C3D4" {
		t.Errorf("CredentialID (nfc_id) = %q, want 04A1B2C3D4", ev.CredentialID)
	}
	if ev.Timestamp == "" {
		t.Error("Timestamp empty; should be parsed from event.published")
	}
}

// TestParseAccessLogEntry_NfcTap covers the NFC-tap variant (as
// opposed to PIN above) since the poller drops auth types that are
// neither NFC nor PIN_CODE and we want to be sure both survive.
func TestParseAccessLogEntry_NfcTap(t *testing.T) {
	hit := mustHit(t, `{
		"_id": "73120",
		"_source": {
			"actor": {"id":"cust-42","display_name":"Lillian Bakri"},
			"authentication": {"credential_provider":"NFC"},
			"event": {"published": 1744980000000, "result":"ACCESS", "type":"access.door.unlock"},
			"target": [
				{"type":"door","id":"door-front","display_name":"Front Door"},
				{"type":"nfc_id","id":"DEADBEEF"}
			]
		}
	}`)

	ev := parseAccessLogEntry(hit)
	if ev.AuthType != "NFC" {
		t.Fatalf("AuthType = %q, want NFC", ev.AuthType)
	}
	if ev.CredentialID != "DEADBEEF" {
		t.Errorf("CredentialID = %q, want DEADBEEF", ev.CredentialID)
	}
	if ev.LogID != "73120" {
		t.Errorf("LogID = %q, want 73120", ev.LogID)
	}
}

// TestParseAccessLogEntry_MissingSource tolerates a malformed hit
// that lacks _source entirely — the parser should fall back to
// treating the hit itself as source (so the test doesn't panic)
// and produce an event with an empty payload the poller will filter
// out on the auth-type check.
func TestParseAccessLogEntry_MissingSource(t *testing.T) {
	hit := mustHit(t, `{"_id":"99999"}`)

	ev := parseAccessLogEntry(hit)
	if ev.LogID != "99999" {
		t.Errorf("LogID = %q, want 99999", ev.LogID)
	}
	if ev.AuthType != "" || ev.DoorID != "" || ev.ActorID != "" {
		t.Errorf("expected empty fields on hit with no _source; got auth=%q door=%q actor=%q",
			ev.AuthType, ev.DoorID, ev.ActorID)
	}
	if ev.Timestamp == "" {
		t.Error("Timestamp should default to now, not be empty")
	}
}

// TestParseLogIDInt covers the dedup key helper the poller uses to
// advance its monotonic cursor. Non-numeric ids must report !ok so
// the caller falls back to timestamp-based cursoring.
func TestParseLogIDInt(t *testing.T) {
	cases := []struct {
		in   string
		n    int64
		ok   bool
		note string
	}{
		{"73118", 73118, true, "plain numeric"},
		{"0", 0, true, "zero"},
		{"", 0, false, "empty"},
		{"abc", 0, false, "letters"},
		{"12a", 0, false, "mixed"},
		{"999999999999999999", 999999999999999999, true, "large but in-range"},
	}
	for _, tc := range cases {
		n, ok := parseLogIDInt(tc.in)
		if n != tc.n || ok != tc.ok {
			t.Errorf("%s: parseLogIDInt(%q) = (%d, %v), want (%d, %v)",
				tc.note, tc.in, n, ok, tc.n, tc.ok)
		}
	}
}

func mustHit(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal sample hit: %v", err)
	}
	return m
}
