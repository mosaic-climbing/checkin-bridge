package unifi

// UniFi Access — provisioning endpoints for the C2 "new member" flow.
//
// Every method here targets the UniFi Access Developer API documented at
// assets.identity.ui.com/unifi-access/api_reference.pdf. Section numbers
// below correspond to that document. The bridge orchestrates these calls
// from the staff UI (/ui/members/new) so a staff member can create a UA-Hub
// user, bind a policy, and enroll an NFC card end-to-end without touching
// UA-Hub's native admin.
//
// Error mode: every method returns the UA-Hub response body verbatim in
// its error string on non-2xx. The API produces useful JSON error payloads
// (`{"code":"USER.EMAIL_ALREADY_EXISTS", ...}`) and the staff UI surfaces
// them directly, so operators can tell "this email is taken" from "reader
// unavailable" at a glance.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ─── §3.2 User Registration / §3.3 Update User ──────────────────────

// CreateUser creates a new UA-Hub user (§3.2). Returns the new user's ID
// on success. The `user_email` field populates on UA-Hub firmware
// 1.22.16+; older firmware accepts the call but silently drops the email,
// which is why the C2 provisioning flow falls back to a follow-up
// UpdateUser call when RequireMinimumUAHubVersion is false.
func (c *Client) CreateUser(ctx context.Context, firstName, lastName, email string) (string, error) {
	url := fmt.Sprintf("%s/users", c.baseURL)
	body, _ := json.Marshal(map[string]any{
		"first_name": firstName,
		"last_name":  lastName,
		"user_email": email,
	})
	resp, respBody, err := c.doJSON(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("create user HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Response shape: {"data": {"id": "...", ...}} or bare {"id": "..."}
	var wrapped struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(respBody, &wrapped)
	if id := stringFromAny(wrapped.Data["id"]); id != "" {
		return id, nil
	}
	var flat map[string]any
	_ = json.Unmarshal(respBody, &flat)
	if id := stringFromAny(flat["id"]); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("create user: no id in response body %q", string(respBody))
}

// UserPatch is the set of fields UpdateUser will send. Only non-zero
// fields are marshalled — omitting a field leaves UA-Hub's current value
// alone. This matches the UA-Hub API's partial-update semantics (§3.3).
type UserPatch struct {
	FirstName string
	LastName  string
	Email     string
	Status    string // "ACTIVE" | "DEACTIVATED"
}

// UpdateUser PUTs a partial update to a UA-Hub user (§3.3). Returns nil
// on success. Unlike UpdateUserStatus (which is kept around as the
// single-purpose helper the legacy statusync path uses), this is a
// general-purpose patch used by the new matching algorithm when it needs
// to mirror an email AND flip a status in the same call, avoiding two
// separate round-trips.
func (c *Client) UpdateUser(ctx context.Context, userID string, patch UserPatch) error {
	if userID == "" {
		return fmt.Errorf("UpdateUser: userID empty")
	}
	fields := map[string]any{}
	if patch.FirstName != "" {
		fields["first_name"] = patch.FirstName
	}
	if patch.LastName != "" {
		fields["last_name"] = patch.LastName
	}
	if patch.Email != "" {
		fields["user_email"] = patch.Email
	}
	if patch.Status != "" {
		fields["status"] = patch.Status
	}
	if len(fields) == 0 {
		return nil // nothing to do; not an error, just a no-op
	}
	url := fmt.Sprintf("%s/users/%s", c.baseURL, userID)
	body, _ := json.Marshal(fields)
	resp, respBody, err := c.doJSON(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("update user HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ─── §3.6 Assign Access Policy to User ──────────────────────────────

// AssignAccessPolicies binds the given access-policy IDs to a UA-Hub user
// (§3.6). UA-Hub creates users with no policies attached by default, so
// this is mandatory during the provisioning flow — without it the user
// exists but every tap denies.
func (c *Client) AssignAccessPolicies(ctx context.Context, userID string, policyIDs []string) error {
	if userID == "" {
		return fmt.Errorf("AssignAccessPolicies: userID empty")
	}
	url := fmt.Sprintf("%s/users/%s/access_policies", c.baseURL, userID)
	body, _ := json.Marshal(map[string]any{"access_policy_ids": policyIDs})
	resp, respBody, err := c.doJSON(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("assign access policies: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("assign access policies HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ─── §3.7 Assign NFC Card to User ──────────────────────────────────

// AssignNFCCard binds an already-enrolled NFC token to a UA-Hub user
// (§3.7). If forceAdd is true, the UA-Hub API reassigns the card if it's
// currently bound to a different user — used for the "swap a lost card"
// path. The bridge enforces its own uniqueness pre-check via
// FetchNFCCardByToken before calling this with forceAdd=true, so the
// staff member explicitly confirms the reassignment.
func (c *Client) AssignNFCCard(ctx context.Context, userID, token string, forceAdd bool) error {
	if userID == "" || token == "" {
		return fmt.Errorf("AssignNFCCard: userID and token required")
	}
	url := fmt.Sprintf("%s/users/%s/nfc_cards", c.baseURL, userID)
	body, _ := json.Marshal(map[string]any{
		"token":     token,
		"force_add": forceAdd,
	})
	resp, respBody, err := c.doJSON(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("assign NFC card: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("assign NFC card HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ─── §6.2 / §6.3 / §6.4 NFC Card Enrollment Session ────────────────

// StartNFCEnrollment begins an enrollment session on the given reader
// device (§6.2). The reader enters enrollment mode; the next card tap is
// captured and bound to the session instead of triggering an access
// check. Returns the session_id the caller will poll with
// GetNFCEnrollmentStatus.
func (c *Client) StartNFCEnrollment(ctx context.Context, deviceID string) (string, error) {
	if deviceID == "" {
		return "", fmt.Errorf("StartNFCEnrollment: deviceID required")
	}
	url := fmt.Sprintf("%s/credentials/nfc_cards/sessions", c.baseURL)
	body, _ := json.Marshal(map[string]any{
		"device_id":     deviceID,
		"reset_ua_card": false,
	})
	resp, respBody, err := c.doJSON(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("start NFC enrollment: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("start NFC enrollment HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var wrapped struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(respBody, &wrapped)
	for _, k := range []string{"session_id", "id"} {
		if v := stringFromAny(wrapped.Data[k]); v != "" {
			return v, nil
		}
	}
	var flat map[string]any
	_ = json.Unmarshal(respBody, &flat)
	for _, k := range []string{"session_id", "id"} {
		if v := stringFromAny(flat[k]); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("start NFC enrollment: no session_id in response body %q", string(respBody))
}

// NFCEnrollmentStatus is the result of a GetNFCEnrollmentStatus call. If
// the tap hasn't happened yet, Token is empty and the caller polls again.
// Once the card is tapped, Token carries the card's UA-Hub identifier —
// that's the same string the bridge passes to AssignNFCCard to bind it to
// a user.
type NFCEnrollmentStatus struct {
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
	CardID    string `json:"card_id"`
	Status    string `json:"status"` // e.g. "pending", "active", "completed"
}

// GetNFCEnrollmentStatus polls an enrollment session (§6.3). Returns a
// status with empty Token if the tap hasn't been detected yet; the UI
// is expected to poll every ~500ms with a 30s timeout.
func (c *Client) GetNFCEnrollmentStatus(ctx context.Context, sessionID string) (*NFCEnrollmentStatus, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("GetNFCEnrollmentStatus: sessionID required")
	}
	url := fmt.Sprintf("%s/credentials/nfc_cards/sessions/%s", c.baseURL, sessionID)
	resp, respBody, err := c.doJSON(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch NFC enrollment status: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch NFC enrollment status HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Accept either {"data": {...}} or the bare object.
	var wrapped struct {
		Data NFCEnrollmentStatus `json:"data"`
	}
	if err := json.Unmarshal(respBody, &wrapped); err == nil && (wrapped.Data.SessionID != "" || wrapped.Data.Token != "" || wrapped.Data.Status != "") {
		if wrapped.Data.SessionID == "" {
			wrapped.Data.SessionID = sessionID
		}
		return &wrapped.Data, nil
	}
	var flat NFCEnrollmentStatus
	if err := json.Unmarshal(respBody, &flat); err != nil {
		return nil, fmt.Errorf("unmarshal NFC enrollment status: %w", err)
	}
	if flat.SessionID == "" {
		flat.SessionID = sessionID
	}
	return &flat, nil
}

// DeleteNFCEnrollmentSession cleans up an abandoned or completed
// enrollment session (§6.4). Called when the staff member cancels the
// flow or the poll hits its timeout.
func (c *Client) DeleteNFCEnrollmentSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("DeleteNFCEnrollmentSession: sessionID required")
	}
	url := fmt.Sprintf("%s/credentials/nfc_cards/sessions/%s", c.baseURL, sessionID)
	resp, respBody, err := c.doJSON(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("delete NFC enrollment session: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("delete NFC enrollment session HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ─── §6.7 Fetch NFC Card by Token ───────────────────────────────────

// NFCCardOwner captures just enough of the §6.7 response to drive the
// staff-UI "this card is already bound to Jamie Lee — reassign?" prompt.
// If UserID is empty, the card is enrolled but unbound, which is the
// expected state right after §6.3 completes and before §3.7 runs.
type NFCCardOwner struct {
	Token     string `json:"token"`
	CardID    string `json:"card_id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
}

// FetchNFCCardByToken looks up the current owner of an NFC card (§6.7).
// Returns (nil, nil) if the API reports the token is unknown (HTTP 404
// from UA-Hub) — the card was never enrolled. Returns a non-nil result
// with empty UserID if the card is enrolled but unbound. Any other
// non-2xx is surfaced as an error.
//
// Used by the provisioning flow immediately before AssignNFCCard to
// detect the "card already belongs to someone else" case and refuse to
// auto-reassign unless the staff member explicitly ticks "reassign this
// card".
func (c *Client) FetchNFCCardByToken(ctx context.Context, token string) (*NFCCardOwner, error) {
	if token == "" {
		return nil, fmt.Errorf("FetchNFCCardByToken: token required")
	}
	url := fmt.Sprintf("%s/credentials/nfc_cards/tokens/%s", c.baseURL, token)
	resp, respBody, err := c.doJSON(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch NFC card: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch NFC card HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var wrapped struct {
		Data map[string]any `json:"data"`
	}
	var flat map[string]any
	var obj map[string]any
	if err := json.Unmarshal(respBody, &wrapped); err == nil && len(wrapped.Data) > 0 {
		obj = wrapped.Data
	} else if err := json.Unmarshal(respBody, &flat); err == nil {
		obj = flat
	}
	if obj == nil {
		return nil, fmt.Errorf("unmarshal NFC card: empty body")
	}

	owner := &NFCCardOwner{
		Token:  stringFromAny(obj["token"]),
		CardID: stringFromAny(obj["card_id"]),
	}
	// The API may return the bound user either as a flat "user_id" field
	// or nested under "user": {...}. Support both.
	if uid := stringFromAny(obj["user_id"]); uid != "" {
		owner.UserID = uid
		owner.UserName = strings.TrimSpace(stringFromAny(obj["user_name"]))
	}
	if userObj, ok := obj["user"].(map[string]any); ok {
		if uid := stringFromAny(userObj["id"]); uid != "" {
			owner.UserID = uid
			name := strings.TrimSpace(stringFromAny(userObj["first_name"]) + " " + stringFromAny(userObj["last_name"]))
			if name != "" {
				owner.UserName = name
			} else if n := stringFromAny(userObj["name"]); n != "" {
				owner.UserName = n
			}
		}
	}
	if owner.Token == "" {
		owner.Token = token
	}
	return owner, nil
}

// ─── internal: HTTP helper ─────────────────────────────────────────

// doJSON centralises the common request/response plumbing for the
// provisioning endpoints. It always returns the response (so callers can
// inspect the status code) and the already-read response body (limited
// to 10MB). The caller owns interpretation of status and body; this
// helper only does transport.
func (c *Client) doJSON(ctx context.Context, method, url string, body []byte) (*http.Response, []byte, error) {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	return resp, respBody, nil
}
