package statusync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// matchOne runs the email→name matching tree against Redpoint for one UA
// user and persists the decision to ua_user_mappings or
// ua_user_mappings_pending.
//
// The function is the seam between the pure decision logic in matcher.go
// and the I/O side-effects (Redpoint lookups + DB writes). Keeping it
// thin and stateful-only makes the matcher independently testable and
// this orchestrator testable with a fake upstream + real SQLite.
//
// Error handling:
//
//   - Redpoint upstream failures return early with a wrapped error so
//     the sync loop can count them in result.Errors without burning a
//     pending-row update on transient flakes.
//   - DB failures inside persistDecision return as errors too. The
//     matching decision in those cases is lost; the next sync run will
//     recompute it. We don't retry inline because statusync already has
//     a daily cadence and the breaker protects the recheck path.
func (s *Syncer) matchOne(ctx context.Context, ua unifi.UniFiUser) (matchDecision, error) {
	// No usable signal — skip both upstream calls. Hitting Redpoint with
	// blank filters would at best waste a roundtrip and at worst land in
	// unspecified-behavior territory (we've already been bitten by the
	// email filter's blank-string semantics; no reason to push our luck).
	if !hasMatchableSignal(ua) {
		d := matchDecision{PendingReason: store.PendingReasonNoEmail}
		return d, s.persistDecision(ctx, ua, d)
	}

	// Email branch — try first if we have one, fall through if it
	// produced zero hits (the caller signals via stop=false).
	if normaliseEmail(ua.Email) != "" {
		rows, err := s.redpoint.CustomersByEmail(ctx, ua.Email, 10)
		if err != nil {
			return matchDecision{}, fmt.Errorf("CustomersByEmail: %w", err)
		}
		if d, stop := decideFromEmailResults(ua, rows); stop {
			return d, s.persistDecision(ctx, ua, d)
		}
	}

	// Name-fallback branch. hasMatchableSignal guarantees at least one
	// signal exists; this check covers the "email set but partial name"
	// case, where we'd rather defer to staff than send a partial query.
	if normaliseName(ua.FirstName) == "" || normaliseName(ua.LastName) == "" {
		reason := store.PendingReasonNoMatch
		if normaliseEmail(ua.Email) == "" {
			reason = store.PendingReasonNoEmail
		}
		d := matchDecision{PendingReason: reason}
		return d, s.persistDecision(ctx, ua, d)
	}

	// Name-fallback goes against the local customer mirror, not Redpoint.
	// Rationale:
	//   - Redpoint's CustomerFilter no longer exposes a "search" field
	//     (schema change observed in production; the previous direct call
	//     returns a GraphQL validation error and consumes the rate-limit
	//     budget on every UA user). The mirror is populated nightly by
	//     the walker and carries first_name/last_name columns that we
	//     can LIKE-scan cheaply.
	//   - The email branch above still queries Redpoint directly because
	//     email is canonical and we want the freshest upstream view; the
	//     name branch was always advisory (multi-hit → pending) so
	//     serving it from the local mirror is a strict improvement on
	//     both correctness and cost.
	//
	// The store returns []store.Customer; decideFromNameResults expects
	// []*redpoint.Customer. We project only the four fields the matcher
	// actually reads (ID, FirstName, LastName, Email via fullNamesMatch
	// and customerIDs). Extra store-side fields like Active or
	// BadgeStatus are intentionally dropped here — the matcher has no
	// business making activation decisions, and the status writeback
	// path fetches a fresh Redpoint view per matched customer.
	storeRows, err := s.store.SearchCustomersByName(ctx, ua.FirstName, ua.LastName)
	if err != nil {
		return matchDecision{}, fmt.Errorf("store.SearchCustomersByName: %w", err)
	}
	rows := make([]*redpoint.Customer, len(storeRows))
	for i := range storeRows {
		rows[i] = &redpoint.Customer{
			ID:        storeRows[i].RedpointID,
			FirstName: storeRows[i].FirstName,
			LastName:  storeRows[i].LastName,
			Email:     storeRows[i].Email,
		}
	}
	d := decideFromNameResults(ua, rows)
	return d, s.persistDecision(ctx, ua, d)
}

// persistDecision writes the matching outcome to the appropriate table.
//
// Invariants:
//
//  1. A UA-Hub user is in AT MOST one of {ua_user_mappings,
//     ua_user_mappings_pending} at rest — we DELETE from the counterpart
//     on every write to prevent the "matched + still pending" inconsistency.
//
//  2. Pending.GraceUntil is anchored to the first time we saw the user
//     in this state; re-observation on subsequent syncs must NOT push
//     the deactivation deadline forward. We enforce that here by reading
//     the existing row's grace_until and passing it through. The
//     staff-side "defer" action is expected to write a fresh future
//     timestamp through the same UpsertPending method, which overrides
//     this preservation (that's the intended escape hatch).
//
//  3. Every mapping write that changes the bound customer emits a
//     match_audit row with field="mapping". Unchanged re-upserts (same
//     UA user, same customer) don't — those would just be noise.
func (s *Syncer) persistDecision(ctx context.Context, ua unifi.UniFiUser, d matchDecision) error {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	if d.Matched != nil {
		prior, err := s.store.GetMapping(ctx, ua.ID)
		if err != nil {
			return fmt.Errorf("GetMapping: %w", err)
		}
		m := &store.Mapping{
			UAUserID:         ua.ID,
			RedpointCustomer: d.Matched.ID,
			MatchedAt:        nowStr,
			MatchedBy:        d.Source,
			// LastEmailSyncedAt intentionally left empty. v0.5.1 decision
			// (see docs/architecture-review.md C2 §Matching): the bridge
			// does NOT push Redpoint email into UA-Hub. Redpoint is the
			// source of truth; UA-Hub email is operator-curated and
			// consumed only as a matching input. TouchMappingEmailSynced
			// is retained for schema back-compat but has no production
			// caller — slated for cleanup in migration 5.
		}
		if err := s.store.UpsertMapping(ctx, m); err != nil {
			return fmt.Errorf("UpsertMapping: %w", err)
		}
		if prior == nil || prior.RedpointCustomer != d.Matched.ID {
			before := ""
			if prior != nil {
				before = prior.RedpointCustomer
			}
			// AppendMatchAudit is best-effort — a failure here leaves
			// the mapping row in place but the audit trail incomplete.
			// Log (via the Syncer's logger) rather than aborting.
			if auditErr := s.store.AppendMatchAudit(ctx, &store.MatchAudit{
				UAUserID:  ua.ID,
				Field:     "mapping",
				BeforeVal: before,
				AfterVal:  d.Matched.ID,
				Source:    d.Source,
				Timestamp: nowStr,
			}); auditErr != nil {
				s.logger.Error("match_audit append failed; mapping persisted without audit trail",
					"uaUserId", ua.ID, "error", auditErr)
			}
		}
		if err := s.store.DeletePending(ctx, ua.ID); err != nil {
			return fmt.Errorf("DeletePending: %w", err)
		}
		return nil
	}

	// Pending path. Preserve any existing grace_until so the deactivation
	// deadline stays anchored to the first observation.
	graceUntil := ""
	existing, err := s.store.GetPending(ctx, ua.ID)
	if err != nil {
		return fmt.Errorf("GetPending: %w", err)
	}
	if existing != nil {
		graceUntil = existing.GraceUntil
	}
	if graceUntil == "" {
		days := s.config.UnmatchedGraceDays
		if days <= 0 {
			days = 7 // matches the config default; defensive against zero-value Config{}
		}
		graceUntil = now.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	}
	p := &store.Pending{
		UAUserID:   ua.ID,
		Reason:     d.PendingReason,
		LastSeen:   nowStr,
		GraceUntil: graceUntil,
		Candidates: strings.Join(d.Candidates, "|"),
	}
	if err := s.store.UpsertPending(ctx, p); err != nil {
		return fmt.Errorf("UpsertPending: %w", err)
	}
	// A pending user must not also hold a mapping row — if a prior run
	// bound them and the current run re-observes them as ambiguous
	// (e.g. the Redpoint side changed shape), clear the mapping so the
	// door stops claiming they're authenticated.
	if err := s.store.DeleteMapping(ctx, ua.ID); err != nil {
		return fmt.Errorf("DeleteMapping: %w", err)
	}
	return nil
}
