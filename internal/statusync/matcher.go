package statusync

import (
	"strings"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// Match-source labels written to ua_user_mappings.matched_by and to
// match_audit.source. These values are what downstream dashboards group
// on, so treat them as part of the storage contract — changes require a
// migration for existing mapping rows.
const (
	// MatchSourceEmail: unique email hit in Redpoint. Highest confidence
	// path that needs zero human involvement.
	MatchSourceEmail = "auto:email"
	// MatchSourceEmailAndName: two or more Redpoint customers share the
	// email (typical household: parent's address on a child's account)
	// and first+last disambiguated to a single row.
	MatchSourceEmailAndName = "auto:email+name"
	// MatchSourceName: UA user had no email (or zero email hits) and the
	// name-only scan returned exactly one Redpoint customer.
	MatchSourceName = "auto:name"
	// MatchSourceEmailRecheck: email match resolved by the mirror-refresh
	// recheck pass — the UA-Hub user's email was missing from the
	// paginated list and only landed in ua_users after a per-user
	// FetchUser hydration pass, which retroactively lets an
	// already-pending row find its single Redpoint customer. Same
	// confidence as MatchSourceEmail; the distinction exists so audits
	// can count how much work the recheck path is saving staff.
	MatchSourceEmailRecheck = "auto:email:recheck"
	// MatchSourceBridgeSync: source label for status/email writebacks
	// the bridge performs on an existing mapping (non-match audit rows).
	MatchSourceBridgeSync = "bridge:sync"
	// MatchSourceBridgeExpiry: source label for auto-deactivation when a
	// pending row's grace window expires without a match.
	MatchSourceBridgeExpiry = "bridge:unmatched-expired"

	// MatchSourceStaff: a staff member picked the binding from the
	// /ui/needs-match panel. The staff UI is currently single-password
	// with no per-user identity; a future enhancement can extend this to
	// "staff:<username>" once sessions carry a user identity. The constant
	// is centralised here so that when the username gate lands, the
	// rewrite is a single line.
	MatchSourceStaff = "staff"
	// MatchSourceStaffSkip: staff-triggered immediate deactivation from
	// /ui/needs-match/{id}/skip. Differentiated from Staff so audit
	// queries can count "manual acknowledge + deactivate" separately
	// from "manual match".
	MatchSourceStaffSkip = "staff:skip"
	// MatchSourceStaffDefer: staff pushed the grace window out via
	// /ui/needs-match/{id}/defer. Written as an audit-trail breadcrumb
	// so the forensic log shows who pushed the ETA out and when.
	MatchSourceStaffDefer = "staff:defer"
	// MatchSourceStaffUnbind: staff-triggered mapping removal from the
	// v0.5.9 member detail panel. The UA-Hub user survives; the mapping
	// row is deleted and the user is re-queued in Needs Match with
	// PendingReasonNoMatch so staff can re-bind them to the correct
	// Redpoint customer. Differentiated from Staff so audit queries
	// can count misassignment recoveries separately from first-time
	// matches.
	MatchSourceStaffUnbind = "staff:unbind"
	// MatchSourceStaffReactivate: staff undid a prior Skip (or an
	// external UA-Hub deactivation) from the v0.5.9 member detail panel,
	// flipping the UA-Hub user status back to ACTIVE. Written as an
	// audit breadcrumb so the forensic log shows who reactivated and
	// when, distinguishing staff reactivation from a fresh auto-match.
	MatchSourceStaffReactivate = "staff:reactivate"
	// MatchSourceStaffReassign: staff reassigned an NFC card from one
	// UA-Hub user to another via the v0.5.9 member detail panel. Two
	// audit rows are written per reassign (one under the old owner's
	// UAUserID with the token in before_val, one under the new owner's
	// UAUserID with the token in after_val) so either user's forensic
	// view surfaces the hand-off.
	MatchSourceStaffReassign = "staff:reassign"
)

// matchDecision describes the result of running the matching tree on a
// single UA user. Exactly one of (Matched, PendingReason) is set:
//
//   - Matched != nil → the bridge will upsert ua_user_mappings with
//     MatchedBy = Source, and drop any existing pending row.
//   - PendingReason != "" → the bridge will upsert ua_user_mappings_pending
//     with that reason + Candidates (customer IDs staff can pick from).
//
// Candidates is informational for the staff UI; it's never used to pick
// a match automatically.
type matchDecision struct {
	Matched       *redpoint.Customer
	Source        string
	PendingReason string
	Candidates    []string
}

// normaliseEmail lowercases and trims. RFC 5321 §2.4 makes the domain
// part case-insensitive, and most real-world consumer mail systems treat
// the local-part the same way, so comparing lowercased forms is the
// practical choice and matches what the Redpoint GraphQL server does on
// its email filter.
func normaliseEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// normaliseName is the conservative version: trim and lowercase, nothing
// more. We intentionally don't fold accents or strip punctuation — a
// bridge-side "O'Connor" == "oconnor" rule would cause false matches
// across very common surnames in the member base and we'd rather kick
// edge cases to staff than silently bind the wrong person to a door key.
func normaliseName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// namesMatch compares two name fragments case-insensitively. Empty-string
// on either side never matches — a UA user who somehow has an empty
// LastName must not collide with a Redpoint row that also happens to be
// missing that field.
func namesMatch(a, b string) bool {
	an := normaliseName(a)
	if an == "" {
		return false
	}
	return an == normaliseName(b)
}

// fullNamesMatch requires both first and last to agree. Either-side
// missing → no match. This is strict on purpose: it's the gate for the
// household-collision disambiguation, and the cost of a false match is
// giving someone else's door key to the wrong person.
func fullNamesMatch(c *redpoint.Customer, ua unifi.UniFiUser) bool {
	return namesMatch(c.FirstName, ua.FirstName) && namesMatch(c.LastName, ua.LastName)
}

// customerIDs flattens a customer slice to its IDs in input order.
func customerIDs(cs []*redpoint.Customer) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// decideFromEmailResults applies the email-first branch of the matching
// tree. The caller has already queried Redpoint for customers whose
// Email matches ua.Email (case-insensitive).
//
// The second return value is "should we stop here":
//   - true  → use the returned decision (Matched or PendingReason set)
//   - false → email produced zero hits, caller should try name-only scan
//
// Table:
//
//	rows | name filter  | decision
//	-----+--------------+-------------------------------------------
//	  0  |       —      | fall through (return _, false)
//	  1  |       —      | auto:email
//	  N  | unique name  | auto:email+name
//	  N  | 0 or 2+ hits | pending(ambiguous_email) with all N ids
//
// The "N rows, 0 name hits" branch is worth calling out: it means the
// UA user's email matches some Redpoint customer(s) but none of them
// have the UA user's name. That's suspicious (reused email across
// unrelated accounts), not routine, so we defer to staff rather than
// guess.
func decideFromEmailResults(ua unifi.UniFiUser, rows []*redpoint.Customer) (matchDecision, bool) {
	if len(rows) == 0 {
		return matchDecision{}, false
	}
	if len(rows) == 1 {
		return matchDecision{Matched: rows[0], Source: MatchSourceEmail}, true
	}
	var nameHits []*redpoint.Customer
	for _, c := range rows {
		if fullNamesMatch(c, ua) {
			nameHits = append(nameHits, c)
		}
	}
	if len(nameHits) == 1 {
		return matchDecision{Matched: nameHits[0], Source: MatchSourceEmailAndName}, true
	}
	return matchDecision{
		PendingReason: store.PendingReasonAmbiguousEmail,
		Candidates:    customerIDs(rows),
	}, true
}

// decideFromNameResults applies the name-only fallback. This runs when
// either the UA user had no email, or the email-search came back empty.
//
// The "reason" picked for pending rows preserves the distinction between
// no-signal (pending:no_email) and signal-but-no-match (pending:no_match
// / pending:ambiguous_name) so the staff UI can partition the queue by
// how much data is available.
//
// Table:
//
//	rows | ua.Email   | decision
//	-----+------------+---------------------------------------
//	  0  |   empty    | pending(no_email)
//	  0  |  non-empty | pending(no_match)
//	  1  |     —      | auto:name
//	  N  |     —      | pending(ambiguous_name) with all N ids
func decideFromNameResults(ua unifi.UniFiUser, rows []*redpoint.Customer) matchDecision {
	if len(rows) == 1 {
		return matchDecision{Matched: rows[0], Source: MatchSourceName}
	}
	if len(rows) > 1 {
		return matchDecision{
			PendingReason: store.PendingReasonAmbiguousName,
			Candidates:    customerIDs(rows),
		}
	}
	reason := store.PendingReasonNoMatch
	if normaliseEmail(ua.Email) == "" {
		reason = store.PendingReasonNoEmail
	}
	return matchDecision{PendingReason: reason}
}

// hasMatchableSignal reports whether a UA user has any data that could
// possibly match a Redpoint row. If not, the caller should skip both
// the email query and the name scan and go straight to pending.
// Avoids hitting upstream with empty queries that might have
// surprising semantics.
func hasMatchableSignal(ua unifi.UniFiUser) bool {
	if normaliseEmail(ua.Email) != "" {
		return true
	}
	if normaliseName(ua.FirstName) != "" && normaliseName(ua.LastName) != "" {
		return true
	}
	return false
}
