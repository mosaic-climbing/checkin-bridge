package statusync

import (
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

func ua(first, last, email string) unifi.UniFiUser {
	return unifi.UniFiUser{FirstName: first, LastName: last, Email: email}
}

func rp(id, first, last, email string) *redpoint.Customer {
	return &redpoint.Customer{ID: id, FirstName: first, LastName: last, Email: email}
}

// Email-branch: the straightforward case — one Redpoint row for this
// email, bind immediately.
func TestDecideFromEmail_SingleHit(t *testing.T) {
	d, stop := decideFromEmailResults(
		ua("Alex", "Smith", "alex@example.com"),
		[]*redpoint.Customer{rp("rp-1", "Alex", "Smith", "alex@example.com")},
	)
	if !stop {
		t.Fatal("stop = false; want true (email branch should own the decision)")
	}
	if d.Matched == nil || d.Matched.ID != "rp-1" {
		t.Errorf("Matched = %+v", d.Matched)
	}
	if d.Source != MatchSourceEmail {
		t.Errorf("Source = %q, want %q", d.Source, MatchSourceEmail)
	}
}

// Email-branch: zero Redpoint rows — caller should fall through to the
// name scan, so stop == false and decision is zero.
func TestDecideFromEmail_ZeroHits_FallsThrough(t *testing.T) {
	d, stop := decideFromEmailResults(
		ua("Alex", "Smith", "alex@example.com"),
		nil,
	)
	if stop {
		t.Error("stop = true; want false (0 email results should fall through)")
	}
	if d.Matched != nil || d.PendingReason != "" {
		t.Errorf("decision = %+v; want zero", d)
	}
}

// Email-branch: household collision — parent's email is on the child's
// account too, UA user's name disambiguates cleanly to one row. This is
// the whole reason C2 exists.
func TestDecideFromEmail_HouseholdCollision_NameDisambiguates(t *testing.T) {
	d, stop := decideFromEmailResults(
		ua("Robin", "Lee", "jamie@example.com"),
		[]*redpoint.Customer{
			rp("rp-parent", "Jamie", "Lee", "jamie@example.com"),
			rp("rp-child", "Robin", "Lee", "jamie@example.com"),
		},
	)
	if !stop {
		t.Fatal("stop = false; want true")
	}
	if d.Matched == nil || d.Matched.ID != "rp-child" {
		t.Errorf("Matched = %+v, want rp-child", d.Matched)
	}
	if d.Source != MatchSourceEmailAndName {
		t.Errorf("Source = %q, want %q", d.Source, MatchSourceEmailAndName)
	}
}

// Email-branch: household collision but the UA user is the parent —
// disambiguation still lands on the right row.
func TestDecideFromEmail_HouseholdCollision_ParentWins(t *testing.T) {
	d, stop := decideFromEmailResults(
		ua("Jamie", "Lee", "jamie@example.com"),
		[]*redpoint.Customer{
			rp("rp-parent", "Jamie", "Lee", "jamie@example.com"),
			rp("rp-child", "Robin", "Lee", "jamie@example.com"),
		},
	)
	if !stop {
		t.Fatal("stop = false; want true")
	}
	if d.Matched == nil || d.Matched.ID != "rp-parent" {
		t.Errorf("Matched = %+v, want rp-parent", d.Matched)
	}
	if d.Source != MatchSourceEmailAndName {
		t.Errorf("Source = %q", d.Source)
	}
}

// Email-branch: multiple rows, none match the UA user's name. This is
// the "someone's email reused across unrelated accounts" path — we
// never auto-bind, we always surface as ambiguous_email with all ids
// so staff can pick.
func TestDecideFromEmail_MultipleRows_NoNameMatch_Ambiguous(t *testing.T) {
	d, stop := decideFromEmailResults(
		ua("Chris", "Evans", "shared@example.com"),
		[]*redpoint.Customer{
			rp("rp-a", "Alex", "Smith", "shared@example.com"),
			rp("rp-b", "Jamie", "Lee", "shared@example.com"),
		},
	)
	if !stop {
		t.Fatal("stop = false; want true (ambiguity is a terminal decision)")
	}
	if d.Matched != nil {
		t.Errorf("Matched = %+v; want nil (ambiguous)", d.Matched)
	}
	if d.PendingReason != store.PendingReasonAmbiguousEmail {
		t.Errorf("PendingReason = %q, want %q", d.PendingReason, store.PendingReasonAmbiguousEmail)
	}
	if len(d.Candidates) != 2 {
		t.Errorf("Candidates = %v; want both ids", d.Candidates)
	}
}

// Email-branch: two rows share both the email AND the UA user's name.
// Vanishingly rare but correctness matters — we must not pick one.
// Surface as ambiguous_email with all ids.
func TestDecideFromEmail_MultipleRows_MultipleNameMatches_Ambiguous(t *testing.T) {
	d, stop := decideFromEmailResults(
		ua("Alex", "Smith", "shared@example.com"),
		[]*redpoint.Customer{
			rp("rp-a", "Alex", "Smith", "shared@example.com"),
			rp("rp-b", "Alex", "Smith", "shared@example.com"),
		},
	)
	if !stop {
		t.Fatal("stop = false; want true")
	}
	if d.Matched != nil {
		t.Errorf("Matched = %+v; want nil (2 name hits is still ambiguous)", d.Matched)
	}
	if d.PendingReason != store.PendingReasonAmbiguousEmail {
		t.Errorf("PendingReason = %q", d.PendingReason)
	}
}

// Email-branch: case/whitespace insensitivity on the name comparator.
// Redpoint may store "alex smith " where UA has "Alex Smith"; these
// must be treated as the same person for disambiguation.
func TestDecideFromEmail_NameComparisonIsCaseInsensitive(t *testing.T) {
	d, stop := decideFromEmailResults(
		ua("alex", "SMITH", "shared@example.com"),
		[]*redpoint.Customer{
			rp("rp-a", "Alex", " smith ", "shared@example.com"),
			rp("rp-b", "Jamie", "Lee", "shared@example.com"),
		},
	)
	if !stop {
		t.Fatal("stop = false; want true")
	}
	if d.Matched == nil || d.Matched.ID != "rp-a" {
		t.Errorf("Matched = %+v, want rp-a (case/whitespace insensitive)", d.Matched)
	}
	if d.Source != MatchSourceEmailAndName {
		t.Errorf("Source = %q", d.Source)
	}
}

// Name-branch: unique name hit.
func TestDecideFromName_SingleHit(t *testing.T) {
	d := decideFromNameResults(
		ua("Alex", "Smith", ""),
		[]*redpoint.Customer{rp("rp-1", "Alex", "Smith", "alex@example.com")},
	)
	if d.Matched == nil || d.Matched.ID != "rp-1" {
		t.Errorf("Matched = %+v", d.Matched)
	}
	if d.Source != MatchSourceName {
		t.Errorf("Source = %q, want %q", d.Source, MatchSourceName)
	}
}

// Name-branch: multiple hits → pending with all candidates.
func TestDecideFromName_Ambiguous(t *testing.T) {
	d := decideFromNameResults(
		ua("Alex", "Smith", ""),
		[]*redpoint.Customer{
			rp("rp-1", "Alex", "Smith", "a@example.com"),
			rp("rp-2", "Alex", "Smith", "b@example.com"),
		},
	)
	if d.Matched != nil {
		t.Errorf("Matched = %+v; want nil", d.Matched)
	}
	if d.PendingReason != store.PendingReasonAmbiguousName {
		t.Errorf("PendingReason = %q, want %q", d.PendingReason, store.PendingReasonAmbiguousName)
	}
	if len(d.Candidates) != 2 {
		t.Errorf("Candidates = %v", d.Candidates)
	}
}

// Name-branch: zero hits. Reason code depends on whether the UA user
// had an email — the partitioning matters for staff UI triage.
func TestDecideFromName_ZeroHits_NoEmail(t *testing.T) {
	d := decideFromNameResults(ua("Alex", "Smith", ""), nil)
	if d.Matched != nil {
		t.Errorf("Matched = %+v", d.Matched)
	}
	if d.PendingReason != store.PendingReasonNoEmail {
		t.Errorf("PendingReason = %q, want %q", d.PendingReason, store.PendingReasonNoEmail)
	}
}

func TestDecideFromName_ZeroHits_WithEmail(t *testing.T) {
	d := decideFromNameResults(ua("Alex", "Smith", "alex@example.com"), nil)
	if d.PendingReason != store.PendingReasonNoMatch {
		t.Errorf("PendingReason = %q, want %q", d.PendingReason, store.PendingReasonNoMatch)
	}
}

// namesMatch: the workhorse — important enough to test directly because
// the "empty-string never matches" rule is load-bearing for safety.
func TestNamesMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Alex", "Alex", true},
		{"alex", "ALEX", true},
		{" Alex ", "alex", true},
		{"Alex", "Alexander", false}, // no prefix match
		{"", "", false},               // empty never matches
		{"", "Alex", false},
		{"Alex", "", false},
		{"   ", "Alex", false}, // whitespace-only is effectively empty
	}
	for _, c := range cases {
		if got := namesMatch(c.a, c.b); got != c.want {
			t.Errorf("namesMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// fullNamesMatch: both first and last must match. Either-side miss
// should block the bind.
func TestFullNamesMatch(t *testing.T) {
	cases := []struct {
		first, last, uaFirst, uaLast string
		want                         bool
	}{
		{"Alex", "Smith", "Alex", "Smith", true},
		{"Alex", "Smith", "alex", "smith", true},
		{"Alex", "Smith", "Alex", "Jones", false}, // last mismatch
		{"Alex", "Smith", "Jamie", "Smith", false}, // first mismatch
		{"", "Smith", "Alex", "Smith", false},      // missing first
		{"Alex", "", "Alex", "Smith", false},       // missing last
	}
	for _, c := range cases {
		got := fullNamesMatch(
			&redpoint.Customer{FirstName: c.first, LastName: c.last},
			unifi.UniFiUser{FirstName: c.uaFirst, LastName: c.uaLast},
		)
		if got != c.want {
			t.Errorf("fullNamesMatch(%q %q, %q %q) = %v, want %v",
				c.first, c.last, c.uaFirst, c.uaLast, got, c.want)
		}
	}
}

// hasMatchableSignal: staff-provisioned users with just an NFC card and
// no name/email should be pending without a Redpoint query. (The bridge
// shouldn't hammer Redpoint with empty searches — the upstream's
// semantics for blank filters are unspecified and prior experience with
// the email filter shows this is a real risk.)
func TestHasMatchableSignal(t *testing.T) {
	cases := []struct {
		u    unifi.UniFiUser
		want bool
	}{
		{ua("Alex", "Smith", "alex@example.com"), true},
		{ua("Alex", "Smith", ""), true},
		{ua("", "", "alex@example.com"), true},
		{ua("Alex", "", ""), false},           // only first name — not enough
		{ua("", "Smith", ""), false},          // only last name — not enough
		{ua("", "", ""), false},
		{ua("   ", "   ", "   "), false},
	}
	for _, c := range cases {
		if got := hasMatchableSignal(c.u); got != c.want {
			t.Errorf("hasMatchableSignal(%+v) = %v, want %v", c.u, got, c.want)
		}
	}
}
