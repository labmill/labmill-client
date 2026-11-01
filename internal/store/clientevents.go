package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ClientEvent is one queued problem report for the user's Labmill account
// (internal/account): already redacted, without the server's system id.
type ClientEvent struct {
	ID       int64
	JobID    int64  // 0: no job
	Cluster  string // "" when not known
	Payload  string // JSON
	Attempts int
	NextAt   time.Time
}

// MaxQueuedClientEvents bounds the queue while the computer is offline: newer
// reports are dropped beyond it.
const MaxQueuedClientEvents = 2000

// AddClientEvent queues e (due at e.NextAt). It reports false when the queue is full.
func (s *Store) AddClientEvent(ctx context.Context, e ClientEvent) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO client_events (job_id, cluster, payload, next_at)
		SELECT ?, ?, ?, ? WHERE (SELECT count(*) FROM client_events WHERE sent_at = 0) < ?`,
		e.JobID, e.Cluster, e.Payload, ts(e.NextAt), MaxQueuedClientEvents)
	if err != nil {
		return false, fmt.Errorf("store: queuing a problem report: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DueClientEvents returns at most limit unsent reports due at now, oldest first.
func (s *Store) DueClientEvents(ctx context.Context, now time.Time, limit int) ([]ClientEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, cluster, payload, attempts, next_at FROM client_events
		WHERE sent_at = 0 AND next_at <= ? ORDER BY id LIMIT ?`, ts(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading problem reports: %w", err)
	}
	defer rows.Close()
	var out []ClientEvent
	for rows.Next() {
		var e ClientEvent
		var next int64
		if err := rows.Scan(&e.ID, &e.JobID, &e.Cluster, &e.Payload, &e.Attempts, &next); err != nil {
			return nil, fmt.Errorf("store: reading problem reports: %w", err)
		}
		e.NextAt = fromTS(next)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClientEventsDone marks reports as sent (or, with at = Never in next, as
// not to be sent again: next is ignored when sent is true).
func (s *Store) ClientEventsDone(ctx context.Context, ids []int64, sent bool, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{ts(at)}
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE client_events SET attempts = attempts + 1, next_at = ? WHERE id IN (` + ph + `)`
	if sent {
		q = `UPDATE client_events SET attempts = attempts + 1, sent_at = ? WHERE id IN (` + ph + `)`
	}
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("store: updating problem reports: %w", err)
	}
	return nil
}
