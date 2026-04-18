package store

import "testing"

// TestDoorPolicy_EvaluateAccess covers every branch of EvaluateAccess in one
// table. The function is pure (no DB, no network), so we only need direct
// struct literals — no Store, no context, no fixtures.
//
// Ordering of cases mirrors the order of the if-branches in EvaluateAccess so
// failures are easier to diagnose.
func TestDoorPolicy_EvaluateAccess(t *testing.T) {
	t.Parallel()

	// An "active, ACTIVE" member that would pass basic gating — individual
	// cases override fields as needed.
	activeMember := &Member{
		NfcUID:      "AABBCC",
		CustomerID:  "cust-1",
		FirstName:   "Test",
		LastName:    "User",
		BadgeStatus: "ACTIVE",
		BadgeName:   "Standard",
		Active:      true,
	}

	cases := []struct {
		name       string
		policy     *DoorPolicy
		member     *Member
		wantOK     bool
		wantReason string
	}{
		{
			name:       "nil policy is treated as open",
			policy:     nil,
			member:     activeMember,
			wantOK:     true,
			wantReason: "",
		},
		{
			name: "open policy allows any active member",
			policy: &DoorPolicy{
				DoorID: "front",
				Policy: "open",
			},
			member:     activeMember,
			wantOK:     true,
			wantReason: "",
		},
		{
			name: "open policy allows even inactive members (no gating)",
			// Rationale: "open" literally bypasses all checks. The handler
			// still records the event; the policy just doesn't deny. This
			// pins that behavior so future edits don't quietly change it.
			policy: &DoorPolicy{Policy: "open"},
			member: &Member{
				Active:      false,
				BadgeStatus: "EXPIRED",
			},
			wantOK:     true,
			wantReason: "",
		},
		{
			name:       "staff_only denies any member (no per-member check)",
			policy:     &DoorPolicy{DoorID: "staff-lounge", Policy: "staff_only"},
			member:     activeMember,
			wantOK:     false,
			wantReason: "staff-only door",
		},
		{
			name:   "membership policy denies inactive account",
			policy: &DoorPolicy{Policy: "membership"},
			member: &Member{
				Active:      false,
				BadgeStatus: "ACTIVE",
				BadgeName:   "Standard",
			},
			wantOK:     false,
			wantReason: "account inactive",
		},
		{
			name:   "membership policy denies frozen badge",
			policy: &DoorPolicy{Policy: "membership"},
			member: &Member{
				Active:      true,
				BadgeStatus: "FROZEN",
				BadgeName:   "Standard",
			},
			wantOK:     false,
			wantReason: "membership frozen",
		},
		{
			name:   "membership policy denies expired badge",
			policy: &DoorPolicy{Policy: "membership"},
			member: &Member{
				Active:      true,
				BadgeStatus: "EXPIRED",
				BadgeName:   "Standard",
			},
			wantOK:     false,
			wantReason: "membership expired",
		},
		{
			name:   "membership policy denies pending_sync",
			policy: &DoorPolicy{Policy: "membership"},
			member: &Member{
				Active:      true,
				BadgeStatus: "PENDING_SYNC",
				BadgeName:   "Standard",
			},
			wantOK:     false,
			wantReason: "pending initial sync",
		},
		{
			name:   "membership policy denies deleted",
			policy: &DoorPolicy{Policy: "membership"},
			member: &Member{
				Active:      true,
				BadgeStatus: "DELETED",
				BadgeName:   "Standard",
			},
			wantOK:     false,
			wantReason: "membership deleted",
		},
		{
			name:   "membership policy falls through for unknown badge status",
			policy: &DoorPolicy{Policy: "membership"},
			member: &Member{
				Active:      true,
				BadgeStatus: "WEIRD_STATE",
				BadgeName:   "Standard",
			},
			wantOK:     false,
			wantReason: "badge status: WEIRD_STATE",
		},
		{
			name: "empty allowed_badges list allows any active member",
			policy: &DoorPolicy{
				Policy:        "membership",
				AllowedBadges: "",
			},
			member:     activeMember,
			wantOK:     true,
			wantReason: "",
		},
		{
			name: "allowed_badges with matching badge allows",
			policy: &DoorPolicy{
				Policy:        "membership",
				AllowedBadges: "Standard,Premium",
			},
			member:     activeMember, // BadgeName = "Standard"
			wantOK:     true,
			wantReason: "",
		},
		{
			name: "allowed_badges is case-insensitive",
			policy: &DoorPolicy{
				Policy:        "membership",
				AllowedBadges: "STANDARD,premium",
			},
			member: &Member{
				Active:      true,
				BadgeStatus: "ACTIVE",
				BadgeName:   "Standard",
			},
			wantOK:     true,
			wantReason: "",
		},
		{
			name: "allowed_badges with whitespace around entries still matches",
			// AllowedBadgeList() trims whitespace per-entry, so "  Premium  "
			// should still match a member with BadgeName "Premium".
			policy: &DoorPolicy{
				Policy:        "membership",
				AllowedBadges: "Standard ,  Premium  ",
			},
			member: &Member{
				Active:      true,
				BadgeStatus: "ACTIVE",
				BadgeName:   "Premium",
			},
			wantOK:     true,
			wantReason: "",
		},
		{
			name: "allowed_badges with no match denies",
			policy: &DoorPolicy{
				Policy:        "membership",
				AllowedBadges: "Premium,Founders",
			},
			member: &Member{
				Active:      true,
				BadgeStatus: "ACTIVE",
				BadgeName:   "Standard",
			},
			wantOK:     false,
			wantReason: "membership type not allowed for this door: Standard",
		},
		{
			name: "allowed_badges denial reason includes empty badge name when member has none",
			policy: &DoorPolicy{
				Policy:        "membership",
				AllowedBadges: "Standard",
			},
			member: &Member{
				Active:      true,
				BadgeStatus: "ACTIVE",
				BadgeName:   "",
			},
			wantOK:     false,
			wantReason: "membership type not allowed for this door: ",
		},
		{
			name: "basic-membership denial takes precedence over badge allowlist",
			// If the member is inactive, EvaluateAccess should return the
			// member's deny reason, not the badge mismatch. This pins the
			// ordering: basic gating first, badge allowlist second.
			policy: &DoorPolicy{
				Policy:        "membership",
				AllowedBadges: "Standard",
			},
			member: &Member{
				Active:      true,
				BadgeStatus: "FROZEN",
				BadgeName:   "SomeOtherBadge",
			},
			wantOK:     false,
			wantReason: "membership frozen",
		},
	}

	for _, tc := range cases {
		tc := tc // pin for parallel sub-tests
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotOK, gotReason := tc.policy.EvaluateAccess(tc.member)
			if gotOK != tc.wantOK {
				t.Errorf("EvaluateAccess() allowed = %v, want %v (reason=%q)", gotOK, tc.wantOK, gotReason)
			}
			if gotReason != tc.wantReason {
				t.Errorf("EvaluateAccess() reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestDoorPolicy_AllowedBadgeList pins the parsing helper used by
// EvaluateAccess so badge-list edge cases don't regress silently.
func TestDoorPolicy_AllowedBadgeList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty string returns nil", "", nil},
		{"single entry", "Standard", []string{"Standard"}},
		{"comma-separated", "Standard,Premium", []string{"Standard", "Premium"}},
		{"trims whitespace", " Standard , Premium ", []string{"Standard", "Premium"}},
		{"skips empty entries", "Standard,,Premium,", []string{"Standard", "Premium"}},
		{"only whitespace and commas is empty", " , , ", []string{}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &DoorPolicy{AllowedBadges: tc.input}
			got := p.AllowedBadgeList()
			if len(got) != len(tc.want) {
				t.Fatalf("AllowedBadgeList() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("AllowedBadgeList()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
