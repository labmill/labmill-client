package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Upload is one queued job report for the user's Labmill account
// (internal/account). One row per job and remote ID, so a report is queued
// once; a resubmitted job (new remote ID) is a new report.
type Upload struct {
	JobID     int64
	RemoteID  string
	Ref       string // client_job_ref: random, so the API stores it once
	Cluster   string
	Payload   string // JSON report without hpc_system_id
	Attempts  int
	NextAt    time.Time
	SentAt    time.Time // zero while not sent
	LastError string
}

// Never is the NextAt of a report that will not be sent again (refused by the API).
var Never = time.Unix(0, math.MaxInt64).UTC()

const uploadCols = `job_id, remote_id, ref, cluster, payload, attempts, next_at, sent_at, last_error`

func scanUpload(r scanner) (Upload, error) {
	var u Upload
	var next, sent int64
	if err := r.Scan(&u.JobID, &u.RemoteID, &u.Ref, &u.Cluster, &u.Payload, &u.Attempts, &next, &sent, &u.LastError); err != nil {
		return Upload{}, err
	}
	u.NextAt = fromTS(next)
	if sent != 0 {
		u.SentAt = fromTS(sent)
	}
	return u, nil
}

// HasUpload reports whether a report for job jobID with remoteID is queued or sent.
func (s *Store) HasUpload(ctx context.Context, jobID int64, remoteID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM uploads WHERE job_id = ? AND remote_id = ?`, jobID, remoteID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: reading uploads: %w", err)
	}
	return true, nil
}

// AddUpload queues u (due at once). A second report for the same job and
// remote ID is ignored; added says whether u was queued.
func (s *Store) AddUpload(ctx context.Context, u Upload) (added bool, err error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO uploads (job_id, remote_id, ref, cluster, payload, next_at)
		VALUES (?,?,?,?,?,?)`, u.JobID, u.RemoteID, u.Ref, u.Cluster, u.Payload, ts(u.NextAt))
	if err != nil {
		return false, fmt.Errorf("store: queuing report for job %d: %w", u.JobID, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DueUploads returns at most limit unsent reports whose NextAt is not after now, oldest first.
func (s *Store) DueUploads(ctx context.Context, now time.Time, limit int) ([]Upload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+uploadCols+` FROM uploads WHERE sent_at = 0 AND next_at <= ?
		ORDER BY next_at, job_id LIMIT ?`, ts(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading uploads: %w", err)
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading uploads: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Uploads returns every queued and sent report, oldest job first.
func (s *Store) Uploads(ctx context.Context) ([]Upload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+uploadCols+` FROM uploads ORDER BY job_id, remote_id`)
	if err != nil {
		return nil, fmt.Errorf("store: reading uploads: %w", err)
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading uploads: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UploadSent marks a report as sent.
func (s *Store) UploadSent(ctx context.Context, jobID int64, remoteID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE uploads SET sent_at = ?, last_error = '', attempts = attempts + 1
		WHERE job_id = ? AND remote_id = ?`, ts(at), jobID, remoteID)
	if err != nil {
		return fmt.Errorf("store: marking report for job %d sent: %w", jobID, err)
	}
	return nil
}

// UploadFailed records a failed send: the next try is not before next (Never: no more tries).
func (s *Store) UploadFailed(ctx context.Context, jobID int64, remoteID string, next time.Time, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE uploads SET attempts = attempts + 1, next_at = ?, last_error = ?
		WHERE job_id = ? AND remote_id = ? AND sent_at = 0`, ts(next), why, jobID, remoteID)
	if err != nil {
		return fmt.Errorf("store: recording failed report for job %d: %w", jobID, err)
	}
	return nil
}
