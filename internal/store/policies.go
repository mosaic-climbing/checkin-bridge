package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// DoorPolicy defines access rules for a specific door.
type DoorPolicy struct {
	DoorID        string `db:"door_id"        json:"doorId"`
	DoorName      string `db:"door_name"      json:"doorName"`
	Policy        string `db:"policy"         json:"policy"`         // "membership", "waiver", "staff_only", "open"
	RequireWaiver bool   `db:"require_waiver" json:"requireWaiver"`
	AllowedBadges string `db:"allowed_badges" json:"allowedBadges"` // comma-separated badge names, empty = all
	Notes         string `db:"notes"          json:"notes"`
}

// AllowedBadgeList returns the allowed badges as a slice.
func (p *DoorPolicy) AllowedBadgeList() []string {
	if p.AllowedBadges == "" {
		return nil
	}
	parts := strings.Split(p.AllowedBadges, ",")
	result := make([]string, 0, len(parts))
	for _, s := range parts {
		s = strings.TrimSpace(s)
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}

// EvaluateAccess checks if a member is allowed through a specific door.
func (p *DoorPolicy) EvaluateAccess(member *Member) (allowed bool, reason string) {
	if p == nil || p.Policy == "open" {
		return true, ""
	}

	if p.Policy == "staff_only" {
		return false, "staff-only door"
	}

	// Check basic membership first
	if !member.IsAllowed() {
		return false, member.DenyReason()
	}

	// Check badge restriction
	badges := p.AllowedBadgeList()
	if len(badges) > 0 {
		found := false
		for _, b := range badges {
			if strings.EqualFold(member.BadgeName, b) {
				found = true
				break
			}
		}
		if !found {
			return false, "membership type not allowed for this door: " + member.BadgeName
		}
	}

	return true, ""
}

// GetDoorPolicy returns the policy for a specific door, or nil if no custom policy.
func (s *Store) GetDoorPolicy(ctx context.Context, doorID string) (*DoorPolicy, error) {
	var p DoorPolicy
	err := s.db.GetContext(ctx, &p, `SELECT * FROM door_policies WHERE door_id = ?`, doorID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// AllDoorPolicies returns all configured door policies.
func (s *Store) AllDoorPolicies(ctx context.Context) ([]DoorPolicy, error) {
	var policies []DoorPolicy
	err := s.db.SelectContext(ctx, &policies, `SELECT * FROM door_policies ORDER BY door_name`)
	return policies, err
}

// UpsertDoorPolicy creates or updates a door policy.
func (s *Store) UpsertDoorPolicy(ctx context.Context, p *DoorPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO door_policies (door_id, door_name, policy, require_waiver, allowed_badges, notes)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(door_id) DO UPDATE SET
            door_name      = excluded.door_name,
            policy         = excluded.policy,
            require_waiver = excluded.require_waiver,
            allowed_badges = excluded.allowed_badges,
            notes          = excluded.notes
    `, p.DoorID, p.DoorName, p.Policy, p.RequireWaiver, p.AllowedBadges, p.Notes)
	return err
}

// DeleteDoorPolicy removes a door policy.
func (s *Store) DeleteDoorPolicy(ctx context.Context, doorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM door_policies WHERE door_id = ?`, doorID)
	return err
}
