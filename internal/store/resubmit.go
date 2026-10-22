package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The exactly-once protocol of an automatic resubmission (M61.3). The job
// keeps one Labmill identity; its scheduler ID changes. Before anything is
// submitted the caller claims the old ID in the database (ClaimResubmit), so
// no second caller, thread or Labmill process submits for the same ended job;
// the new ID and the history event are saved in one transaction
// (FinishResubmit). A claim that is neither finished nor released is what a
// crash leaves behind: nothing submits for that ID again.
const (
	EventResubmitting     = "resubmitting"      // text: "<old ID> <payload>"
	EventResubmitDone     = "resubmit-done"     // text: "<old ID>"
	EventResubmitReleased = "resubmit-released" // text: "<old ID>"
)

// ErrJobChanged is returned (wrapped) when a job's scheduler ID or state
// changed since the caller looked, so its action must not be applied.
var ErrJobChanged = errors.New("store: job changed meanwhile")

// ResubmitClaim is an open claim: a resubmission started and not finished.
type ResubmitClaim struct {
	Old     string    // the scheduler ID being replaced
	Payload string    // what the claimant stored with it
	At      time.Time // when it was made
}

// OpenResubmitClaim finds the claim in evs (a job's events, oldest first)
// that was neither finished nor released.
func OpenResubmitClaim(evs []Event) (ResubmitClaim, bool) {
	var open ResubmitClaim
	var have bool
	for _, e := range evs {
		switch e.Kind {
		case EventResubmitting:
			old, payload, _ := strings.Cut(e.Text, " ")
			open, have = ResubmitClaim{Old: old, Payload: payload, At: e.Time}, true
		case EventResubmitDone, EventResubmitReleased:
			if have && strings.TrimSpace(e.Text) == open.Old {
				have = false
			}
		}
	}
	return open, have
}

func txEvents(ctx context.Context, tx *sql.Tx, jobID int64) ([]Event, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, job_id, time, kind, text FROM events WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&e.ID, &e.JobID, &t, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		e.Time = fromTS(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClaimResubmit claims the replacement of job id's scheduler ID old, in one
// transaction. It succeeds (true) only when the job still has RemoteID ==
// old, is submitted, pending or running (not canceled or finished) and has
// no open claim; it then records a claim event with payload.
func (s *Store) ClaimResubmit(ctx context.Context, id int64, old, payload string) (bool, error) {
	if old == "" || strings.ContainsAny(old, " \n") {
		return false, fmt.Errorf("store: bad scheduler ID %q", old)
	}
	claimed := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: job %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if cur.RemoteID != old || (cur.State != StateSubmitted && cur.State != StatePending && cur.State != StateRunning) {
			return nil
		}
		evs, err := txEvents(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, open := OpenResubmitClaim(evs); open {
			return nil
		}
		if err := addEvent(ctx, tx, id, EventResubmitting, old+" "+payload); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: claiming the resubmission of job %d: %w", id, err)
	}
	return claimed, nil
}

// ReleaseResubmit gives up the open claim on old: nothing was submitted, so
// a later try may claim again.
func (s *Store) ReleaseResubmit(ctx context.Context, id int64, old string) error {
	return s.AddEvent(ctx, id, EventResubmitReleased, old)
}

// FinishResubmit completes a claim in one transaction: the job's scheduler
// ID becomes newID (state submitted, Message msg, and Script script when
// not empty), and the history event (kind, text) and the claim's closing
// event are recorded. It fails with ErrJobChanged, saving nothing, when the
// job no longer has RemoteID == old or was finished or canceled meanwhile.
func (s *Store) FinishResubmit(ctx context.Context, id int64, old, newID, script, msg, kind, text string) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if cur.RemoteID == newID {
			return nil // another caller saved this very ID already
		}
		if cur.RemoteID != old || (cur.State != StateSubmitted && cur.State != StatePending && cur.State != StateRunning) {
			return fmt.Errorf("job %d: %w", id, ErrJobChanged)
		}
		if script == "" {
			script = cur.Script
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET remote_id=?, state=?, script=?, message=?, updated_at=? WHERE id=?`,
			newID, string(StateSubmitted), script, msg, ts(time.Now()), id); err != nil {
			return err
		}
		if cur.State != StateSubmitted {
			if err := addEvent(ctx, tx, id, "state", fmt.Sprintf("%s -> %s", cur.State, StateSubmitted)); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, id, kind, text); err != nil {
			return err
		}
		return addEvent(ctx, tx, id, EventResubmitDone, old)
	})
	if err != nil {
		return fmt.Errorf("store: saving the resubmission of job %d: %w", id, err)
	}
	return nil
}
