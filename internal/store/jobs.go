package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Job represents a background task (ingest, sync, etc.)
type Job struct {
	ID        string `db:"id"         json:"id"`
	Type      string `db:"type"       json:"type"`       // "ingest", "cache_sync", "status_sync", "directory_sync"
	Status    string `db:"status"     json:"status"`     // "pending", "running", "completed", "failed"
	Progress  string `db:"progress"   json:"progress"`   // JSON progress data
	Result    string `db:"result"     json:"result"`     // JSON result data
	Error     string `db:"error"      json:"error"`
	CreatedAt string `db:"created_at" json:"createdAt"`
	UpdatedAt string `db:"updated_at" json:"updatedAt"`
}

// CreateJob creates a new background job record.
func (s *Store) CreateJob(ctx context.Context, id, jobType string) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO jobs (id, type, status) VALUES (?, ?, 'running')
    `, id, jobType)
	return err
}

// UpdateJobProgress updates a job's progress JSON.
func (s *Store) UpdateJobProgress(ctx context.Context, id string, progress any) error {
	data, _ := json.Marshal(progress)
	_, err := s.db.ExecContext(ctx, `
        UPDATE jobs SET progress = ?, updated_at = ? WHERE id = ?
    `, string(data), time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// CompleteJob marks a job as completed with result data.
func (s *Store) CompleteJob(ctx context.Context, id string, result any) error {
	data, _ := json.Marshal(result)
	_, err := s.db.ExecContext(ctx, `
        UPDATE jobs SET status = 'completed', result = ?, updated_at = ? WHERE id = ?
    `, string(data), time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// FailJob marks a job as failed.
func (s *Store) FailJob(ctx context.Context, id, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE jobs SET status = 'failed', error = ?, updated_at = ? WHERE id = ?
    `, errMsg, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// GetJob returns a single job by ID.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	var j Job
	err := s.db.GetContext(ctx, &j, `SELECT * FROM jobs WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &j, err
}

// RecentJobs returns the most recent jobs.
func (s *Store) RecentJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 20
	}
	var jobs []Job
	err := s.db.SelectContext(ctx, &jobs, `SELECT * FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	return jobs, err
}

// ActiveJob returns the most recent running job of a given type, if any.
func (s *Store) ActiveJob(ctx context.Context, jobType string) (*Job, error) {
	var j Job
	err := s.db.GetContext(ctx, &j, `SELECT * FROM jobs WHERE type = ? AND status = 'running' ORDER BY created_at DESC LIMIT 1`, jobType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &j, err
}

// LastJobByType returns the most recent job of a given type regardless of
// status — running, completed, or failed. Used by the /ui/sync page to
// render "Last run: N ago · ✓/✗" pills next to each sync trigger so
// staff can see at a glance whether a scheduled sync has been fired
// recently (and whether it succeeded). Returns (nil, nil) if no job of
// that type has ever run.
func (s *Store) LastJobByType(ctx context.Context, jobType string) (*Job, error) {
	var j Job
	err := s.db.GetContext(ctx, &j, `
		SELECT * FROM jobs WHERE type = ?
		ORDER BY created_at DESC LIMIT 1`, jobType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &j, err
}

// CleanOldJobs deletes completed/failed jobs older than the given duration.
func (s *Store) CleanOldJobs(ctx context.Context, olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-olderThan).UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, `
        DELETE FROM jobs WHERE status IN ('completed', 'failed') AND updated_at < ?
    `, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
