package api

// C2 Layer 4d — staff UI for provisioning a new UA-Hub member.
//
// This is the orchestration layer that ties together the UA-Hub
// provisioning client (internal/unifi/provisioning.go), the Redpoint
// email lookup, and the local mapping store. Spec is in
// docs/architecture-review.md C2 §"New-user provisioning flow"; the
// six routes are registered in server.go's routes() block.
//
// Workflow shape:
//
//   1. GET  /ui/members/new                                   — form page
//   2. GET  /ui/members/new/lookup?email=…&first_name=…&last_name=…
//                                                             — live email validation
//   3. POST /ui/members/new                                   — §3.2 create + §3.6 policy + map row + audit
//   4. POST /ui/members/new/{id}/enroll                       — §6.2 start enrollment session
//   5. GET  /ui/members/new/{id}/enroll/{sid}/poll            — §6.3 poll, §6.7 collision check, §3.7 bind
//   6. DELETE /ui/members/new/{id}/enroll/{sid}               — §6.4 cleanup
//
// All six are gated by requireProvisioning(): if AllowNewMembers=false
// in config the handler returns a 403 + friendly fragment instead of
// running. This matches the defence-in-depth pattern used by the
// devhooks gate on /test-checkin.
//
// Match disambiguation reuses the household-collision logic from
// internal/statusync/matcher.go: email → 1 hit auto-match, N hits +
// name disambiguates → match, otherwise present an error fragment.
// Doing it inline (rather than calling into statusync) keeps the
// matcher decision-tree as the single source of truth for
// non-interactive sync, and the staff UI can be a touch more
// permissive (e.g. match on email + last_name only) later if needed
// without changing sync behaviour.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// requireProvisioning returns true when the caller may proceed. When
// the bridge is configured with AllowNewMembers=false the call is
// blocked with a 403 status + a friendly inline alert so the staff UI
// surfaces the reason instead of the browser default 403 page.
//
// Returning a typed bool (rather than a writeError + status int) lets
// each handler short-circuit with `if !s.requireProvisioning(w) {
// return }` as the very first line.
func (s *Server) requireProvisioning(w http.ResponseWriter) bool {
	if s.allowNewMembers {
		return true
	}
	w.WriteHeader(http.StatusForbidden)
	ui.RenderFragment(w, ui.AlertFragment(false,
		"New-member provisioning is disabled in bridge config (Bridge.AllowNewMembers=false). "+
			"Set BRIDGE_ALLOW_NEW_MEMBERS=true and restart the bridge to enable this flow."))
	return false
}

// handleMembersNewPage serves the form page. We delegate to the same
// UI template renderer the other staff pages use; the page itself
// embeds the HTMX targets pointing at the fragment endpoints below.
//
// The provisioning gate fires here too — visiting the page with the
// flag off shows the same 403 fragment as POSTing would, so staff
// don't get a working-looking form that 403s on submit.
func (s *Server) handleMembersNewPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireProvisioning(w) {
		return
	}
	if s.ui == nil {
		writeError(w, http.StatusServiceUnavailable, "UI not available")
		return
	}
	csrf := ""
	if s.sessions != nil {
		csrf = s.sessions.CSRFTokenFromRequest(r)
	}
	s.ui.ServePage(w, r, "members-new", csrf)
}

// resolveByEmailWithName runs the same email-with-name-disambiguation
// algorithm as the statusync matcher (decideFromEmailResults), but
// returns just the bits the staff UI needs and never falls back to a
// pure name search. The staff form requires the email field, so a
// missing-email branch here would be a bug.
//
// Returns:
//   - resolved: the single Redpoint customer that the form can post,
//     and a label describing why it matched ("auto:email" or
//     "auto:email+name").
//   - rows: every row the email matched, so the UI can show "this
//     email matched 3 customers, name disambiguated to {name}" and
//     keep the operator informed even on a clean match.
//   - err: any non-recoverable client error.
//
// A return of (nil, [], nil) means "no match" — display a blocking
// error to the staff member.
func (s *Server) resolveByEmailWithName(r *http.Request, email, firstName, lastName string) (
	resolved *redpoint.Customer, source string, rows []*redpoint.Customer, err error,
) {
	if s.redpoint == nil {
		return nil, "", nil, fmt.Errorf("redpoint client not configured")
	}
	rows, err = s.redpoint.CustomersByEmail(r.Context(), email, 10)
	if err != nil {
		return nil, "", nil, err
	}
	if len(rows) == 0 {
		return nil, "", nil, nil
	}
	if len(rows) == 1 {
		return rows[0], statusync.MatchSourceEmail, rows, nil
	}
	// Multiple hits — try to disambiguate by full name. Empty first/last
	// is fine: namesMatch returns false on empty so we fall through to
	// the ambiguous-pending response.
	var nameHits []*redpoint.Customer
	fnNorm := strings.ToLower(strings.TrimSpace(firstName))
	lnNorm := strings.ToLower(strings.TrimSpace(lastName))
	for _, c := range rows {
		if fnNorm == "" || lnNorm == "" {
			continue
		}
		if strings.ToLower(strings.TrimSpace(c.FirstName)) == fnNorm &&
			strings.ToLower(strings.TrimSpace(c.LastName)) == lnNorm {
			nameHits = append(nameHits, c)
		}
	}
	if len(nameHits) == 1 {
		return nameHits[0], statusync.MatchSourceEmailAndName, rows, nil
	}
	// Either zero or 2+ name hits. Both are "staff must intervene":
	// the form doesn't let them pick from a list (that's what /ui/needs-match
	// is for), so we return no resolved match — the lookup fragment
	// renders an error message including the collision count.
	return nil, "", rows, nil
}

// handleMembersNewLookup is the live-validation endpoint the form
// debounces every 400ms while the staff member types the email. It
// returns one of:
//   - empty 200 (email field blank → swallow the keystroke)
//   - error fragment ("no Redpoint match for jane@…")
//   - error fragment ("ambiguous email — also fill first+last")
//   - success fragment with the resolved Redpoint customer
//
// The success fragment carries data-redpoint-customer-id on the
// outer alert so the page-level submit can read it later if it ever
// needs the resolved ID without re-running the lookup. Today the
// POST handler re-runs the resolve to stay authoritative.
func (s *Server) handleMembersNewLookup(w http.ResponseWriter, r *http.Request) {
	if !s.requireProvisioning(w) {
		return
	}
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	first := strings.TrimSpace(r.URL.Query().Get("first_name"))
	last := strings.TrimSpace(r.URL.Query().Get("last_name"))

	if email == "" {
		// Quietly clear the lookup slot — the field is empty so we
		// shouldn't render either a success or an error.
		ui.RenderFragment(w, "")
		return
	}

	resolved, _, rows, err := s.resolveByEmailWithName(r, email, first, last)
	if err != nil {
		ui.RenderFragment(w, ui.EmailLookupFragment(false,
			"Lookup failed: "+err.Error(), nil))
		return
	}
	if resolved == nil {
		// Distinguish "no hits" from "ambiguous" so staff can self-correct.
		if len(rows) == 0 {
			ui.RenderFragment(w, ui.EmailLookupFragment(false,
				fmt.Sprintf("No Redpoint customer with email %s. The bridge only creates UA-Hub users for paying members.", email), nil))
			return
		}
		ui.RenderFragment(w, ui.EmailLookupFragment(false,
			fmt.Sprintf("Email %s matches %d Redpoint customers — fill first and last name to pick the right one (household sharing).", email, len(rows)), nil))
		return
	}

	hit := &ui.MemberLookupResult{
		RedpointCustomerID: resolved.ID,
		Name:               strings.TrimSpace(resolved.FirstName + " " + resolved.LastName),
		Email:              resolved.Email,
		Active:             resolved.Active,
		BadgeName:          badgeNameFor(resolved),
		BadgeStatus:        badgeStatusFor(resolved),
	}
	ui.RenderFragment(w, ui.EmailLookupFragment(true, "", hit))
}

// listReaderOptions returns the door list shaped for the reader picker.
// Used by both the post-create fragment and the failed/retry fragment.
// On client failure we return an empty list — the form still renders,
// just with no options, and staff can refresh.
func (s *Server) listReaderOptions(r *http.Request) []ui.DoorOption {
	if s.unifi == nil {
		return nil
	}
	doors, err := s.unifi.ListDoors(r.Context())
	if err != nil {
		s.logger.Warn("members.new: list doors failed", "error", err)
		return nil
	}
	out := make([]ui.DoorOption, len(doors))
	for i, d := range doors {
		out[i] = ui.DoorOption{DeviceID: d.ID, Name: d.Name}
	}
	return out
}

// handleMembersNewCreate runs orchestration steps 2–4: create the
// UA-Hub user (§3.2), attach the default access policy (§3.6), and
// write the bridge's mapping row + audit trail. Step 5 (NFC enrollment)
// runs interactively from the returned fragment.
//
// Order of writes is deliberate:
//
//	a) email lookup       — staff must have entered a valid email
//	b) §3.2 create user   — gives us an UA-Hub user_id
//	c) §3.6 policy attach — without this every tap denies; if this
//	                        fails we DO NOT roll back the user (deleting
//	                        it would lose the audit trail). Instead we
//	                        return an error fragment that lists the
//	                        manual fix-up steps; the user is also
//	                        already deactivated by virtue of having no
//	                        policy.
//	d) audit-log + map    — written even if §3.6 fails so the forensic
//	                        trail records the partial state.
//	e) success fragment   — embeds the reader picker for step 5.
func (s *Server) handleMembersNewCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireProvisioning(w) {
		return
	}
	if s.unifi == nil || s.redpoint == nil || s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"UniFi, Redpoint, or store client not configured — provisioning unavailable."))
		return
	}
	if err := r.ParseForm(); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Bad form: "+err.Error()))
		return
	}
	first := strings.TrimSpace(r.FormValue("first_name"))
	last := strings.TrimSpace(r.FormValue("last_name"))
	email := strings.TrimSpace(r.FormValue("email"))
	if first == "" || last == "" || email == "" {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"First name, last name, and email are all required."))
		return
	}

	// (a) Re-run the email lookup authoritatively. The form's live
	// validation is a UX hint; the server must not trust it.
	resolved, source, _, err := s.resolveByEmailWithName(r, email, first, last)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"Redpoint lookup failed: "+err.Error()))
		return
	}
	if resolved == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			fmt.Sprintf("No clean Redpoint match for %s — refresh the lookup and try again.", email)))
		return
	}
	// Collision: would this Redpoint customer end up bound to two UA
	// users? Same guard as handleFragUnmatchedMatch.
	if existing, lookupErr := s.store.GetMappingByCustomerID(r.Context(), resolved.ID); lookupErr == nil && existing != nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			fmt.Sprintf("Redpoint customer %s is already bound to UA-Hub user %s. Pick a different customer or un-match the existing one first.",
				resolved.ID, existing.UAUserID)))
		return
	}

	// (b) §3.2 create UA-Hub user.
	uaUserID, err := s.unifi.CreateUser(r.Context(), first, last, email)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"UA-Hub create user failed: "+err.Error()))
		return
	}

	// (c) §3.6 attach default access policy. Boot validation guarantees
	// defaultAccessPolicyIDs is non-empty when AllowNewMembers is true.
	policyErr := s.unifi.AssignAccessPolicies(r.Context(), uaUserID, s.defaultAccessPolicyIDs)

	// (d) Audit + mapping (written regardless so the forensic trail
	// captures the partial-failure case).
	if auditErr := s.store.AppendMatchAudit(r.Context(), &store.MatchAudit{
		UAUserID:  uaUserID,
		Field:     "mapping",
		BeforeVal: "",
		AfterVal:  resolved.ID,
		Source:    source,
	}); auditErr != nil {
		s.logger.Error("members.new: audit write failed", "uaUserId", uaUserID, "error", auditErr)
	}
	if mapErr := s.store.UpsertMapping(r.Context(), &store.Mapping{
		UAUserID:         uaUserID,
		RedpointCustomer: resolved.ID,
		MatchedBy:        source,
	}); mapErr != nil {
		s.logger.Error("members.new: mapping write failed", "uaUserId", uaUserID, "error", mapErr)
		ui.RenderFragment(w, ui.AlertFragment(false,
			fmt.Sprintf("Created UA-Hub user %s but failed to write mapping: %s. Pop into the Needs Match panel to fix.",
				uaUserID, mapErr.Error())))
		return
	}
	s.audit.Log("members_new_create", s.clientIP(r), map[string]any{
		"uaUserId":           uaUserID,
		"redpointCustomerId": resolved.ID,
		"firstName":          first,
		"lastName":           last,
		"email":              email,
		"matchSource":        source,
	})

	// Surface (c) failure now that the audit trail is safe.
	if policyErr != nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			fmt.Sprintf("Created UA-Hub user %s and bound to Redpoint %s, but access-policy attach failed: %s. Fix manually in UA-Hub admin or retry.",
				uaUserID, resolved.ID, policyErr.Error())))
		return
	}

	displayName := strings.TrimSpace(first + " " + last)
	ui.RenderFragment(w, ui.PostCreateFragment(uaUserID, displayName, resolved.ID, s.listReaderOptions(r)))
}

// resolveDisplayName returns a printable name for the UA-Hub user. The
// enrollment fragments show this back to staff so they can confirm
// they're enrolling the right person; we look it up from the just-
// written mapping rather than asking the staff to re-enter it.
func (s *Server) resolveDisplayName(r *http.Request, uaUserID string) string {
	if s.store == nil {
		return uaUserID
	}
	mapping, err := s.store.GetMapping(r.Context(), uaUserID)
	if err != nil || mapping == nil || s.redpoint == nil {
		return uaUserID
	}
	cust, err := s.redpoint.GetCustomer(r.Context(), mapping.RedpointCustomer)
	if err != nil || cust == nil {
		return uaUserID
	}
	name := strings.TrimSpace(cust.FirstName + " " + cust.LastName)
	if name == "" {
		return uaUserID
	}
	return name
}

// handleMembersNewEnrollStart runs orchestration step 5: §6.2 starts
// the enrollment session on the chosen reader and returns the
// polling fragment. The fragment self-polls /poll until the tap is
// detected, then either swaps in EnrollmentCompleteFragment or
// EnrollmentFailedFragment.
func (s *Server) handleMembersNewEnrollStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireProvisioning(w) {
		return
	}
	if s.unifi == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "UniFi client not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if uaUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID"))
		return
	}
	if err := r.ParseForm(); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Bad form: "+err.Error()))
		return
	}
	deviceID := strings.TrimSpace(r.FormValue("device_id"))
	if deviceID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Pick a reader before starting enrollment."))
		return
	}

	sessionID, err := s.unifi.StartNFCEnrollment(r.Context(), deviceID)
	if err != nil {
		ui.RenderFragment(w, ui.EnrollmentFailedFragment(uaUserID, s.resolveDisplayName(r, uaUserID),
			"Couldn't start enrollment session: "+err.Error(), s.listReaderOptions(r)))
		return
	}
	s.audit.Log("members_new_enroll_start", s.clientIP(r), map[string]any{
		"uaUserId":  uaUserID,
		"deviceId":  deviceID,
		"sessionId": sessionID,
	})
	ui.RenderFragment(w, ui.EnrollmentPollingFragment(uaUserID, s.resolveDisplayName(r, uaUserID), sessionID))
}

// handleMembersNewEnrollPoll runs orchestration step 6: §6.3 fetches
// the session state. If the tap hasn't happened yet (Token empty), we
// return the same polling fragment so HTMX keeps re-triggering. Once
// a token arrives, we run §6.7 to check whether the card is already
// bound to a different UA user, then §3.7 to bind it. Success →
// EnrollmentCompleteFragment; conflict / error → EnrollmentFailedFragment.
//
// HTMX semantics: every polling tick re-renders this same slot
// (hx-target="#members-new-result"), so the polling fragment effectively
// owns the slot until a terminal state replaces it. The browser closes
// the polling loop the moment the response stops including the
// hx-trigger="every 500ms" attribute.
func (s *Server) handleMembersNewEnrollPoll(w http.ResponseWriter, r *http.Request) {
	if !s.requireProvisioning(w) {
		return
	}
	if s.unifi == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "UniFi client not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	sessionID := r.PathValue("sessionId")
	if uaUserID == "" || sessionID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID or session ID"))
		return
	}

	displayName := s.resolveDisplayName(r, uaUserID)
	status, err := s.unifi.GetNFCEnrollmentStatus(r.Context(), sessionID)
	if err != nil {
		ui.RenderFragment(w, ui.EnrollmentFailedFragment(uaUserID, displayName,
			"Polling enrollment status failed: "+err.Error(), s.listReaderOptions(r)))
		return
	}
	if status == nil || status.Token == "" {
		// Tap hasn't happened yet — re-render the polling fragment so
		// HTMX continues the every-500ms loop.
		ui.RenderFragment(w, ui.EnrollmentPollingFragment(uaUserID, displayName, sessionID))
		return
	}

	// §6.7 collision check: refuse to silently reassign a card that
	// belongs to a different UA-Hub user. The architecture review
	// spells this out as a guardrail — protects against the "swapped a
	// child's card with a parent's by mistake" UI accident.
	if owner, err := s.unifi.FetchNFCCardByToken(r.Context(), status.Token); err == nil && owner != nil &&
		owner.UserID != "" && owner.UserID != uaUserID {
		// Drop the abandoned session so the reader exits enrollment mode.
		_ = s.unifi.DeleteNFCEnrollmentSession(r.Context(), sessionID)
		ui.RenderFragment(w, ui.EnrollmentFailedFragment(uaUserID, displayName,
			fmt.Sprintf("That card is already bound to UA-Hub user %s (%s). Use a fresh card or un-bind that user first.",
				owner.UserID, owner.UserName), s.listReaderOptions(r)))
		return
	}

	// §3.7 bind. forceAdd=false because we just confirmed the card is
	// either unbound or already bound to this same user.
	if err := s.unifi.AssignNFCCard(r.Context(), uaUserID, status.Token, false); err != nil {
		ui.RenderFragment(w, ui.EnrollmentFailedFragment(uaUserID, displayName,
			"Binding card to user failed: "+err.Error(), s.listReaderOptions(r)))
		return
	}

	s.audit.Log("members_new_enroll_complete", s.clientIP(r), map[string]any{
		"uaUserId":  uaUserID,
		"sessionId": sessionID,
		"token":     status.Token,
	})
	ui.RenderFragment(w, ui.EnrollmentCompleteFragment(uaUserID, displayName, status.Token))
}

// handleMembersNewEnrollCancel runs §6.4: the staff cancelled the
// flow before a tap was registered. We delete the UA-Hub session so
// the reader exits enrollment mode (otherwise the next legitimate tap
// gets captured into the orphan session) and re-render the post-create
// reader picker so staff can retry without losing the just-created
// UA-Hub user.
func (s *Server) handleMembersNewEnrollCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireProvisioning(w) {
		return
	}
	if s.unifi == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "UniFi client not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	sessionID := r.PathValue("sessionId")
	if uaUserID == "" || sessionID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID or session ID"))
		return
	}

	if err := s.unifi.DeleteNFCEnrollmentSession(r.Context(), sessionID); err != nil {
		// Don't bail — the user is still created, the reader will exit
		// enrollment mode on its own timeout. Surface the warning so
		// staff sees something didn't go cleanly.
		s.logger.Warn("members.new: delete enrollment session failed",
			"uaUserId", uaUserID, "sessionId", sessionID, "error", err)
	}
	s.audit.Log("members_new_enroll_cancel", s.clientIP(r), map[string]any{
		"uaUserId":  uaUserID,
		"sessionId": sessionID,
	})

	displayName := s.resolveDisplayName(r, uaUserID)
	ui.RenderFragment(w, ui.PostCreateFragment(uaUserID, displayName,
		mappingCustomerID(s, r, uaUserID), s.listReaderOptions(r)))
}

// mappingCustomerID returns the Redpoint customer ID bound to a UA-Hub
// user, or an empty string if the lookup fails. Used as a label for
// the "retry enrollment" fragment so staff can see which customer the
// already-created user is attached to.
func mappingCustomerID(s *Server, r *http.Request, uaUserID string) string {
	if s.store == nil {
		return ""
	}
	m, err := s.store.GetMapping(r.Context(), uaUserID)
	if err != nil || m == nil {
		return ""
	}
	return m.RedpointCustomer
}

// Compile-time assertion that we use unifi/redpoint/store types — keeps
// goimports honest if the import set ever shifts.
var _ = unifi.UserPatch{}
var _ = redpoint.Customer{}
var _ = store.Mapping{}
