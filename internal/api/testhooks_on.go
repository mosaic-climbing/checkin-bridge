//go:build devhooks

package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// testHooksCompiled is true in the devhooks build. Tests can use it to
// select assertions appropriate for this build tag.
const testHooksCompiled = true

// registerTestHooks installs the /test-checkin simulation route, but ONLY
// if s.enableTestHooks is also true. That second gate gives operators a
// kill-switch: a devhooks binary accidentally deployed to prod won't
// expose the route unless BRIDGE_ENABLE_TEST_HOOKS=true is also set.
//
// The handler writes an ACCESS event directly into the checkin dispatch
// path, which (depending on configuration) may call Redpoint's create-
// check-in mutation and/or fire an unlock pulse. Ship only to dev/test.
//
// See S5 in docs/architecture-review.md.
func (s *Server) registerTestHooks(shortTimeout time.Duration) {
	if !s.enableTestHooks {
		return
	}
	// Registered on the control plane (controlMux) — same threat-model
	// reasoning as POST /unlock and friends. The control plane is bound to
	// loopback by default, so the dev-hook is reachable only from the
	// bridge host even if the binary slips into prod with the wrong build
	// tag and the wrong env var set.
	s.controlMux.HandleFunc("POST /test-checkin", withTimeout(shortTimeout, s.handleTestCheckin))
	s.logger.Warn("/test-checkin route ENABLED — devhooks build with EnableTestHooks=true. " +
		"Do not ship this configuration to production.")
}

func (s *Server) handleTestCheckin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CardUID  string `json:"cardUid"`
		DoorID   string `json:"doorId"`
		DoorName string `json:"doorName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.CardUID == "" {
		writeError(w, http.StatusBadRequest, "cardUid is required")
		return
	}
	if body.DoorName == "" {
		body.DoorName = "Test Door"
	}

	event := unifi.AccessEvent{
		EventType:    "test",
		CredentialID: body.CardUID,
		DoorID:       body.DoorID,
		DoorName:     body.DoorName,
		AuthType:     "NFC",
		ActorName:    "Test",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Result:       "ACCESS",
	}

	s.handler.HandleEvent(r.Context(), event)

	writeJSON(w, map[string]any{
		"success": true,
		"message": "test check-in processed – check logs",
		"stats":   s.handler.GetStats(),
	})
}
