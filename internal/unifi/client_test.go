package unifi

import (
	"encoding/json"
	"testing"
)

func TestParseUniFiUser_NfcCards(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "user-1",
		"first_name": "Alice",
		"last_name": "Smith",
		"email": "alice@example.com",
		"status": "ACTIVE",
		"nfc_cards": [
			{"token": "04A3B2C1D2E3F4", "type": "ua_card"}
		]
	}`)

	user := parseUniFiUser(raw)
	if user.ID != "user-1" {
		t.Errorf("ID = %q, want user-1", user.ID)
	}
	if user.FirstName != "Alice" {
		t.Errorf("FirstName = %q, want Alice", user.FirstName)
	}
	if user.Email != "alice@example.com" {
		t.Errorf("Email = %q", user.Email)
	}
	if len(user.NfcTokens) != 1 || user.NfcTokens[0] != "04A3B2C1D2E3F4" {
		t.Errorf("NfcTokens = %v, want [04A3B2C1D2E3F4]", user.NfcTokens)
	}
}

func TestParseUniFiUser_Credentials(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "user-2",
		"name": "Bob Jones",
		"credentials": [
			{"type": "nfc", "token": "DEADBEEF"},
			{"type": "pin", "token": "1234"}
		]
	}`)

	user := parseUniFiUser(raw)
	if user.FullName() != "Bob Jones" {
		t.Errorf("FullName = %q, want 'Bob Jones'", user.FullName())
	}
	if len(user.NfcTokens) != 1 || user.NfcTokens[0] != "DEADBEEF" {
		t.Errorf("NfcTokens = %v, want [DEADBEEF]", user.NfcTokens)
	}
}

func TestParseUniFiUser_NfcCredential(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "user-3",
		"first_name": "Carol",
		"nfc_credential": {"token": "AABB1122"}
	}`)

	user := parseUniFiUser(raw)
	if len(user.NfcTokens) != 1 || user.NfcTokens[0] != "AABB1122" {
		t.Errorf("NfcTokens = %v, want [AABB1122]", user.NfcTokens)
	}
}

func TestParseUniFiUser_TopLevelNfcToken(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "user-4",
		"name": "Dave",
		"nfc_token": "11223344"
	}`)

	user := parseUniFiUser(raw)
	if len(user.NfcTokens) != 1 || user.NfcTokens[0] != "11223344" {
		t.Errorf("NfcTokens = %v, want [11223344]", user.NfcTokens)
	}
}

func TestParseUniFiUser_NoNfc(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "user-5",
		"name": "Eve",
		"credentials": [
			{"type": "pin", "token": "9999"}
		]
	}`)

	user := parseUniFiUser(raw)
	if len(user.NfcTokens) != 0 {
		t.Errorf("NfcTokens = %v, want empty", user.NfcTokens)
	}
}

func TestParseUniFiUser_MultipleNfcCards(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "user-6",
		"nfc_cards": [
			{"token": "AAAA1111"},
			{"token": "BBBB2222"}
		]
	}`)

	user := parseUniFiUser(raw)
	if len(user.NfcTokens) != 2 {
		t.Errorf("NfcTokens count = %d, want 2", len(user.NfcTokens))
	}
}

func TestParseUniFiUser_CredentialCardId(t *testing.T) {
	// Some firmware versions use "card_id" instead of "token"
	raw := json.RawMessage(`{
		"id": "user-7",
		"credentials": [
			{"type": "ua_card", "card_id": "CCCC3333"}
		]
	}`)

	user := parseUniFiUser(raw)
	if len(user.NfcTokens) != 1 || user.NfcTokens[0] != "CCCC3333" {
		t.Errorf("NfcTokens = %v, want [CCCC3333]", user.NfcTokens)
	}
}

func TestParseUniFiUser_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not json`)
	user := parseUniFiUser(raw)
	if user.ID != "" {
		t.Error("should return empty user for invalid JSON")
	}
}

func TestFullName(t *testing.T) {
	tests := []struct {
		user UniFiUser
		want string
	}{
		{UniFiUser{Name: "Display Name"}, "Display Name"},
		{UniFiUser{FirstName: "First", LastName: "Last"}, "First Last"},
		{UniFiUser{FirstName: "First"}, "First"},
		{UniFiUser{LastName: "Last"}, "Last"},
		{UniFiUser{Name: "Preferred", FirstName: "First", LastName: "Last"}, "Preferred"},
	}

	for _, tt := range tests {
		if got := tt.user.FullName(); got != tt.want {
			t.Errorf("FullName() = %q, want %q", got, tt.want)
		}
	}
}

func TestExtractNfcTokens_NfcCardStringArray(t *testing.T) {
	// Edge case: nfc_cards as array of strings
	obj := map[string]any{
		"nfc_cards": []any{"TOKEN1", "TOKEN2"},
	}
	tokens := extractNfcTokens(obj)
	if len(tokens) != 2 {
		t.Errorf("tokens = %v, want 2 items", tokens)
	}
}

func TestStringFromAny(t *testing.T) {
	if got := stringFromAny("hello"); got != "hello" {
		t.Errorf("stringFromAny(string) = %q", got)
	}
	if got := stringFromAny(42); got != "" {
		t.Errorf("stringFromAny(int) = %q, want empty", got)
	}
	if got := stringFromAny(nil); got != "" {
		t.Errorf("stringFromAny(nil) = %q, want empty", got)
	}
}
