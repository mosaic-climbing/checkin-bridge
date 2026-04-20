package unifimirror

import (
	"context"
	"fmt"

	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// recheckPending promotes pending rows whose UA-Hub user, after the
// current refresh's per-user FetchUser hydration pass, now has an email
// that lands exactly one Redpoint customer in cache.customers.
//
// Why it exists: before v0.5.5 the ingest matcher wrote a pending row
// the first time it saw a UA-Hub user with no email (or no email match)
// and never revisited the decision. At LEF the paginated list
// ListAllUsersWithStatus omits email for ~99.7% of users — that's 339
// pending rows with a blank UA email field while the Redpoint side has
// all the corresponding addresses indexed. The mirror refresh now
// hydrates those missing emails via per-user GET /users/{id}; this
// recheck pass closes the loop by flipping the freshly-hydrated
// single-hit rows into confirmed mappings without making staff open
// each one.
//
// Scope is deliberately narrow: only single-hit email matches are
// promoted. Multi-hit (household collision) and name-only paths are
// left to the existing statusync tree because those decisions have
// subtler disambiguation rules and a wrong automated call would bind
// someone else's door key to the wrong person. A future pass could
// extend this, but the single-hit path alone clears the bulk of the
// backlog.
//
// Returns the count of pending rows resolved in this pass, and the
// first error encountered (subsequent errors are logged and skipped —
// the mirror refresh should not fail just because one row couldn't be
// promoted).
func (s *Syncer) recheckPending(ctx context.Context) (int, error) {
	rows, err := s.store.FindResolvablePending(ctx)
	if err != nil {
		return 0, fmt.Errorf("FindResolvablePending: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	s.logger.Info("pending-mapping recheck candidates identified",
		"count", len(rows))

	resolved := 0
	var firstErr error
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		m := &store.Mapping{
			UAUserID:         r.UAUserID,
			RedpointCustomer: r.RedpointCustomer,
			MatchedBy:        statusync.MatchSourceEmailRecheck,
		}
		if err := s.store.UpsertMapping(ctx, m); err != nil {
			s.logger.Warn("recheck UpsertMapping failed",
				"uaUserId", r.UAUserID,
				"redpointCustomerId", r.RedpointCustomer,
				"error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.store.DeletePending(ctx, r.UAUserID); err != nil {
			// The mapping landed; leaving the pending row would
			// just mean the /ui/needs-match page still shows this
			// user until the next refresh cleans it up. Not fatal.
			s.logger.Warn("recheck DeletePending failed (mapping still committed)",
				"uaUserId", r.UAUserID, "error", err)
		}
		if err := s.store.AppendMatchAudit(ctx, &store.MatchAudit{
			UAUserID: r.UAUserID,
			Field:    "mapping",
			AfterVal: r.RedpointCustomer,
			Source:   statusync.MatchSourceEmailRecheck,
		}); err != nil {
			s.logger.Warn("recheck AppendMatchAudit failed",
				"uaUserId", r.UAUserID, "error", err)
		}
		resolved++
	}

	s.logger.Info("pending-mapping recheck complete",
		"candidates", len(rows),
		"resolved", resolved)
	return resolved, firstErr
}
