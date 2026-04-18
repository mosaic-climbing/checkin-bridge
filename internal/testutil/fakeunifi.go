package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// FakeUniFi simulates the UniFi Access REST API for testing.
//
// The handler set covers both the original status-sync paths (ListUsers,
// UpdateUserStatus, UnlockDoor) and the C2 provisioning paths
// (CreateUser, AssignAccessPolicies, AssignNFCCard, NFC enrollment
// session lifecycle, FetchNFCCardByToken).
//
// The recorders exposed as exported slices are read-by-assertion in
// tests: e.g. `if len(fake.UsersCreated) != 1 { ... }`. Each slice is
// guarded by the shared mu mutex.
type FakeUniFi struct {
	Server        *httptest.Server
	mu            sync.Mutex
	Unlocks       []UnlockRequest
	StatusUpdates []StatusUpdate
	Users         []map[string]any

	// C2 provisioning recorders / state.

	// UsersCreated captures every POST /users body.
	UsersCreated []CreatedUser
	// UserPatches captures every PUT /users/:id body (any field).
	UserPatches []UserPatch
	// AccessPolicyAssignments captures every PUT /users/:id/access_policies.
	AccessPolicyAssignments []AccessPolicyAssignment
	// NFCCardAssignments captures every PUT /users/:id/nfc_cards.
	NFCCardAssignments []NFCCardAssignment
	// Sessions is the enrollment-session state. Test code can preload
	// Sessions[id] with a Token value to simulate "a card was tapped
	// before the test polled", or leave Token empty to simulate the
	// "still waiting" state.
	Sessions map[string]*EnrollmentSession
	// NextSessionID controls what StartNFCEnrollment returns. Tests can
	// set this to a fixed value for assertions; otherwise each
	// StartNFCEnrollment call picks an auto-numbered id.
	NextSessionID string
	// CardOwners maps NFC token → owner info. Tests preload this to
	// simulate FetchNFCCardByToken responses; absence produces a 404.
	CardOwners map[string]CardOwner
	// DeletedSessions records every DELETE /credentials/nfc_cards/sessions/:id call.
	DeletedSessions []string
}

type UnlockRequest struct {
	DoorID     string
	MemberName string
}

// StatusUpdate records a PUT /users/:id call that sets status.
//
// Retained as a separate slice (in addition to UserPatches) because the
// legacy statusync tests assert against it by count.
type StatusUpdate struct {
	UserID string
	Status string
}

// CreatedUser captures a POST /users body.
type CreatedUser struct {
	ID        string // assigned by the fake
	FirstName string
	LastName  string
	Email     string
}

// UserPatch captures a PUT /users/:id body. Only the fields the bridge
// actually sends are modelled here.
type UserPatch struct {
	UserID    string
	FirstName string
	LastName  string
	Email     string
	Status    string
}

// AccessPolicyAssignment captures a PUT /users/:id/access_policies body.
type AccessPolicyAssignment struct {
	UserID    string
	PolicyIDs []string
}

// NFCCardAssignment captures a PUT /users/:id/nfc_cards body.
type NFCCardAssignment struct {
	UserID   string
	Token    string
	ForceAdd bool
}

// EnrollmentSession is the fake's model of a §6.2/§6.3 session.
type EnrollmentSession struct {
	DeviceID string
	Token    string // if empty, GetNFCEnrollmentStatus reports "pending"
	CardID   string
	Status   string // e.g. "pending" / "completed"
}

// CardOwner is the fake's response payload for §6.7
// GET /credentials/nfc_cards/tokens/:token.
type CardOwner struct {
	Token    string
	CardID   string
	UserID   string
	UserName string
}

func NewFakeUniFi() *FakeUniFi {
	f := &FakeUniFi{
		Sessions:   make(map[string]*EnrollmentSession),
		CardOwners: make(map[string]CardOwner),
	}
	mux := http.NewServeMux()

	// PUT /api/v1/developer/doors/:id/unlock
	mux.HandleFunc("/api/v1/developer/doors/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			f.mu.Lock()
			f.Unlocks = append(f.Unlocks, UnlockRequest{DoorID: r.URL.Path})
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS", "data": []any{}})
	})

	// GET  /api/v1/developer/users  (list)
	// POST /api/v1/developer/users  (create — §3.2)
	mux.HandleFunc("/api/v1/developer/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			users := f.Users
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS", "data": users})
		case http.MethodPost:
			var body struct {
				FirstName string `json:"first_name"`
				LastName  string `json:"last_name"`
				Email     string `json:"user_email"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			id := newUserID(len(f.UsersCreated))
			f.UsersCreated = append(f.UsersCreated, CreatedUser{
				ID:        id,
				FirstName: body.FirstName,
				LastName:  body.LastName,
				Email:     body.Email,
			})
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"code": "SUCCESS",
				"data": map[string]any{
					"id":         id,
					"first_name": body.FirstName,
					"last_name":  body.LastName,
					"user_email": body.Email,
				},
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Per-user routes:
	//   PUT /users/:id                         — user patch (§3.3)
	//   PUT /users/:id/access_policies         — assign policies (§3.6)
	//   PUT /users/:id/nfc_cards               — assign NFC card (§3.7)
	//   GET /users/:id                         — fetch one
	//
	// Hand-rolled dispatch because the stdlib ServeMux doesn't path-param.
	mux.HandleFunc("/api/v1/developer/users/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/developer/users/")
		parts := strings.SplitN(rest, "/", 2)
		userID := parts[0]
		sub := ""
		if len(parts) == 2 {
			sub = parts[1]
		}

		switch {
		case sub == "access_policies" && r.Method == http.MethodPut:
			var body struct {
				IDs []string `json:"access_policy_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.AccessPolicyAssignments = append(f.AccessPolicyAssignments, AccessPolicyAssignment{
				UserID: userID, PolicyIDs: body.IDs,
			})
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS"})

		case sub == "nfc_cards" && r.Method == http.MethodPut:
			var body struct {
				Token    string `json:"token"`
				ForceAdd bool   `json:"force_add"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.NFCCardAssignments = append(f.NFCCardAssignments, NFCCardAssignment{
				UserID: userID, Token: body.Token, ForceAdd: body.ForceAdd,
			})
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS"})

		case sub == "" && r.Method == http.MethodPut:
			// Patch: record every field the bridge might send. The original
			// StatusUpdates slice is kept in sync for the legacy path.
			var raw map[string]any
			_ = json.NewDecoder(r.Body).Decode(&raw)
			patch := UserPatch{UserID: userID}
			if v, ok := raw["first_name"].(string); ok {
				patch.FirstName = v
			}
			if v, ok := raw["last_name"].(string); ok {
				patch.LastName = v
			}
			if v, ok := raw["user_email"].(string); ok {
				patch.Email = v
			}
			if v, ok := raw["status"].(string); ok {
				patch.Status = v
			}
			f.mu.Lock()
			f.UserPatches = append(f.UserPatches, patch)
			if patch.Status != "" {
				f.StatusUpdates = append(f.StatusUpdates, StatusUpdate{
					UserID: userID, Status: patch.Status,
				})
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS"})

		case sub == "" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS", "data": map[string]any{}})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})

	// NFC enrollment sessions (§6.2, §6.3, §6.4).
	mux.HandleFunc("/api/v1/developer/credentials/nfc_cards/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			DeviceID string `json:"device_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		sid := f.NextSessionID
		if sid == "" {
			sid = newSessionID(len(f.Sessions))
		}
		f.Sessions[sid] = &EnrollmentSession{
			DeviceID: body.DeviceID,
			Status:   "pending",
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"code": "SUCCESS",
			"data": map[string]any{"session_id": sid, "status": "pending"},
		})
	})
	mux.HandleFunc("/api/v1/developer/credentials/nfc_cards/sessions/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sid := strings.TrimPrefix(r.URL.Path, "/api/v1/developer/credentials/nfc_cards/sessions/")
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			sess, ok := f.Sessions[sid]
			f.mu.Unlock()
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			status := sess.Status
			if status == "" {
				status = "pending"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"code": "SUCCESS",
				"data": map[string]any{
					"session_id": sid,
					"token":      sess.Token,
					"card_id":    sess.CardID,
					"status":     status,
				},
			})
		case http.MethodDelete:
			f.mu.Lock()
			delete(f.Sessions, sid)
			f.DeletedSessions = append(f.DeletedSessions, sid)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// NFC card token lookup (§6.7).
	mux.HandleFunc("/api/v1/developer/credentials/nfc_cards/tokens/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token := strings.TrimPrefix(r.URL.Path, "/api/v1/developer/credentials/nfc_cards/tokens/")
		f.mu.Lock()
		owner, ok := f.CardOwners[token]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": "SUCCESS",
			"data": map[string]any{
				"token":     owner.Token,
				"card_id":   owner.CardID,
				"user_id":   owner.UserID,
				"user_name": owner.UserName,
			},
		})
	})

	f.Server = httptest.NewTLSServer(mux)
	return f
}

// CompleteSession flips an in-progress enrollment session to "completed"
// with the given token/cardID. Lets tests simulate "card was tapped"
// without racing the polling loop.
func (f *FakeUniFi) CompleteSession(sessionID, token, cardID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sess, ok := f.Sessions[sessionID]; ok {
		sess.Token = token
		sess.CardID = cardID
		sess.Status = "completed"
	}
}

// AddCardOwner preloads a §6.7 response so FetchNFCCardByToken returns
// that owner. Empty UserID models "card enrolled but unbound".
func (f *FakeUniFi) AddCardOwner(token string, owner CardOwner) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner.Token == "" {
		owner.Token = token
	}
	f.CardOwners[token] = owner
}

func (f *FakeUniFi) Close() { f.Server.Close() }
func (f *FakeUniFi) BaseURL() string { return f.Server.URL + "/api/v1/developer" }
func (f *FakeUniFi) UnlockCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Unlocks)
}
func (f *FakeUniFi) StatusUpdateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.StatusUpdates)
}

func newUserID(n int) string {
	return "fake-user-" + strconv.Itoa(n)
}
func newSessionID(n int) string {
	return "fake-session-" + strconv.Itoa(n)
}
