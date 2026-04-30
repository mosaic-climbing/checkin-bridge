// Door-policy CRUD handlers (POST /policies, DELETE /policies/{doorId}).
// Split out of server.go in PR5; the underlying store rows are read by
// handleFragPolicyTable in routes_fragments.go and by EvaluateAccess on
// the tap hot path.

package api

import (
	"net/http"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
)
func (s *Server) handleAddDoorPolicy(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	r.ParseForm()
	policy := &store.DoorPolicy{
		DoorID:        r.FormValue("doorId"),
		DoorName:      r.FormValue("doorName"),
		Policy:        r.FormValue("policy"),
		AllowedBadges: r.FormValue("allowedBadges"),
	}
	if policy.DoorID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Door ID is required"))
		return
	}
	if err := s.store.UpsertDoorPolicy(r.Context(), policy); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed: "+err.Error()))
		return
	}
	s.audit.Log("door_policy_update", r.RemoteAddr, map[string]any{"doorId": policy.DoorID, "policy": policy.Policy})
	ui.RenderFragment(w, ui.AlertFragment(true, "Policy saved for "+policy.DoorName))
}

func (s *Server) handleDeleteDoorPolicy(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	doorID := r.PathValue("doorId")
	if err := s.store.DeleteDoorPolicy(r.Context(), doorID); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed: "+err.Error()))
		return
	}
	s.audit.Log("door_policy_delete", r.RemoteAddr, map[string]any{"doorId": doorID})
	w.WriteHeader(http.StatusOK)
}
