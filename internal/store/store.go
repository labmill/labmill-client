// Package store keeps Labmill's local job state in one SQLite database that the
// TUI, GUI and headless Agent processes all open, so a job started in one mode
// can be watched from another without any daemon. The scheduler (Slurm)
// remains the source of truth for live status; this is Labmill's record
// of what it submitted, what data it staged, and what happened.
//
// It uses the pure-Go modernc.org/sqlite driver (no cgo, so cross-compiling
// stays simple), WAL mode and a busy timeout so several processes can read
// and write concurrently.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// JobState is Labmill's view of a job's lifecycle.
type JobState string

const (
	StateDraft     JobState = "draft"     // planned, not yet approved
	StateStaging   JobState = "staging"   // building the manifest / staging folder
	StateUploading JobState = "uploading" // transferring data to the cluster
	StateSubmitted JobState = "submitted" // handed to the scheduler, no status yet
	StatePending   JobState = "pending"   // queued by the scheduler
	StateRunning   JobState = "running"
	StateCompleted JobState = "completed" // finished successfully, results not fetched yet
	StateFetched   JobState = "fetched"   // results downloaded to the laptop
	StateFailed    JobState = "failed"
	StateCanceled  JobState = "canceled"
)

// Terminal reports whether no further transitions are expected (fetched,
// failed, canceled). Completed is not terminal: results still need fetching.
func (s JobState) Terminal() bool {
	switch s {
	case StateFetched, StateFailed, StateCanceled:
		return true
	}
	return false
}

// Valid reports whether s is one of the defined states.
func (s JobState) Valid() bool {
	for _, v := range allStates {
		if s == v {
			return true
		}
	}
	return false
}

// allStates lists every defined state. Valid and the terminal-state SQL
// filter both derive from it, so a new state is added in one place.
var allStates = []JobState{StateDraft, StateStaging, StateUploading, StateSubmitted, StatePending,
	StateRunning, StateCompleted, StateFetched, StateFailed, StateCanceled}

// Job is one Labmill job.
type Job struct {
	ID        int64
	Name      string // short human name, e.g. "train-segmentation"
	Cluster   string // config.Cluster.Name
	Backend   string // "slurm"
	RemoteID  string // scheduler job/workload ID, once submitted
	State     JobState
	Script    string // the sbatch script exactly as approved
	LocalDir  string // where results come back to
	RemoteDir string // where data was uploaded / the job runs
	Message   string // latest human-readable status detail
	// Downloaded is set when the job's results were downloaded to LocalDir
	// (checked); LocalDir alone is only the planned folder. A failed job
	// that was downloaded stays failed, so State can't say it.
	Downloaded bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ManifestEntry is one staged file.
type ManifestEntry struct {
	Path   string // slash-separated path relative to the staging root
	Size   int64
	SHA256 string // lowercase hex
}

// Event is one entry in a job's history (state changes, transfer progress
// milestones, errors). Event IDs increase monotonically across the database,
// so a viewer in another process can tail with Events(jobID, lastSeenID).
type Event struct {
	ID    int64
	JobID int64
	Time  time.Time
	Kind  string // e.g. "state", "upload", "error"
	Text  string
}

// JobFilter selects jobs. Zero value: all jobs.
type JobFilter struct {
	Cluster    string     // "" means any
	States     []JobState // empty means any
	ActiveOnly bool       // exclude terminal states
	Limit      int        // 0 means no limit
}

// ErrNotFound is returned (wrapped) for a missing job.
var ErrNotFound = errors.New("store: not found")

// Store is an open database. Its fields are chosen by the implementation.
type Store struct {
	db *sql.DB
}

// DefaultPath returns $XDG_STATE_HOME/labmill/labmill.db, or
// <home>/.local/state/labmill/labmill.db when XDG_STATE_HOME is unset or relative
// (the XDG spec says to ignore relative values), on every
// OS (one documented location, like config.DefaultPath).
func DefaultPath() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" && filepath.IsAbs(x) { // the XDG spec says to ignore relative values
		return filepath.Join(x, "labmill", "labmill.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: locating home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "labmill", "labmill.db"), nil
}

// schemaVersion is the user_version this build writes.
const schemaVersion = 3

// migrations[i] upgrades a database from version i to i+1.
var migrations = []string{
	`CREATE TABLE jobs (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL,
		cluster    TEXT NOT NULL,
		backend    TEXT NOT NULL,
		remote_id  TEXT NOT NULL,
		state      TEXT NOT NULL,
		script     TEXT NOT NULL,
		local_dir  TEXT NOT NULL,
		remote_dir TEXT NOT NULL,
		message    TEXT NOT NULL,
		created_at INTEGER NOT NULL, -- unix nanoseconds, UTC
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX jobs_created ON jobs(created_at, id);
	CREATE TABLE manifest (
		job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		path   TEXT NOT NULL,
		size   INTEGER NOT NULL,
		sha256 TEXT NOT NULL,
		PRIMARY KEY (job_id, path)
	);
	CREATE TABLE events (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		time   INTEGER NOT NULL,
		kind   TEXT NOT NULL,
		text   TEXT NOT NULL
	);
	CREATE INDEX events_job ON events(job_id, id);`,
	// 2: a table from yeetl's container checks; unused here, kept so the
	// schema versions stay the same.
	`CREATE TABLE probes (
		cluster    TEXT NOT NULL,
		project    TEXT NOT NULL,
		image      TEXT NOT NULL,
		workload   TEXT NOT NULL, -- the probe's remote ID
		state      TEXT NOT NULL, -- running | done | failed
		result     TEXT NOT NULL, -- the parsed result (JSON) or why it failed
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (cluster, project, image)
	);`,
	// 3: the results of the job were downloaded (kept apart from the state,
	// so a failed job that was downloaded can still show as failed).
	`ALTER TABLE jobs ADD COLUMN downloaded INTEGER NOT NULL DEFAULT 0;`,
}

// WAL mode needs shared memory and file locking, so the database must live on
// a local filesystem: DefaultPath (under the home directory) is wrong on a
// home directory mounted over NFS or another network filesystem, where Open
// should be given a local path instead.
//
// Open opens (creating if needed, parent dirs 0700, file 0600) the database at
// path, enables WAL, foreign keys and a 5 s busy timeout, and applies schema
// migrations tracked with PRAGMA user_version. Opening a database written by a
// newer Labmill (higher user_version) is an error rather than a silent
// downgrade.
func Open(path string) (*Store, error) {
	// The file: URI below needs an absolute path: a relative one would be read
	// as a URI authority (Unix) or rooted at the drive (Windows), opening a
	// different file than the one created here.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: creating directory: %w", err)
	}
	// Create the file ourselves so it gets 0600 regardless of umask.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", path, err)
	}
	f.Close()
	if err := tightenPerms(filepath.Dir(path), path); err != nil {
		return nil, err
	}

	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p // file:///C:/dir/x.db
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate") // write transactions take the lock up front
	u := url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // one connection per handle; cross-handle contention is handled by SQLite
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	return s, nil
}

// tightenPerms removes group/other access from an existing database file and
// its directory (Unix only; Windows modes carry no such bits). The directory
// is best effort, since it may belong to someone else; the file must succeed.
// An existing -wal or -shm file gets the database file's mode.
func tightenPerms(dir, file string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(dir, fi.Mode().Perm()&^0o077)
	}
	fi, err := os.Stat(file)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(file, fi.Mode().Perm()&^0o077); err != nil {
			return fmt.Errorf("store: restricting permissions of %s: %w", file, err)
		}
		fi, err = os.Stat(file)
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	// New -wal/-shm files inherit the db file's mode from SQLite; leftovers
	// from an older run are set to it explicitly (best effort).
	for _, suffix := range []string{"-wal", "-shm"} {
		side := file + suffix
		if sfi, err := os.Stat(side); err == nil && sfi.Mode().Perm() != fi.Mode().Perm() {
			// Best effort, like the directory chmod: the file may belong to
			// another user, and that must not stop the store from opening.
			_ = os.Chmod(side, fi.Mode().Perm())
		}
	}
	return nil
}

func (s *Store) migrate() error {
	ctx := context.Background()
	// Cheap unlocked check first: an up-to-date database (the common case)
	// never takes the write lock.
	var cur int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return err
	}
	if cur == schemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil) // immediate: serializes concurrent first-time opens
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this Labmill supports (%d); upgrade labmill", v, schemaVersion)
	}
	for ; v < schemaVersion; v++ {
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("migrating to version %d: %w", v+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) int64 { return t.UTC().UnixNano() }

func fromTS(n int64) time.Time { return time.Unix(0, n).UTC() }

// withTx runs fn in a write transaction (BEGIN IMMEDIATE).
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// requireJob returns an error wrapping ErrNotFound when job id doesn't exist.
func requireJob(ctx context.Context, tx *sql.Tx, id int64) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("job %d: %w", id, ErrNotFound)
	}
	return err
}

func addEvent(ctx context.Context, tx *sql.Tx, jobID int64, kind, text string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events(job_id, time, kind, text) VALUES (?,?,?,?)`,
		jobID, ts(time.Now()), kind, text)
	return err
}

// CreateJob inserts j (ID, CreatedAt and UpdatedAt are set by the store; an
// empty State becomes StateDraft) and records a "state" event. It returns the
// new ID. An invalid State is an error.
func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	if j.State == "" {
		j.State = StateDraft
	}
	if !j.State.Valid() {
		return 0, fmt.Errorf("store: invalid job state %q", j.State)
	}
	now := ts(time.Now())
	var id int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO jobs
			(name, cluster, backend, remote_id, state, script, local_dir, remote_dir, message, downloaded, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			j.Name, j.Cluster, j.Backend, j.RemoteID, string(j.State), j.Script, j.LocalDir, j.RemoteDir, j.Message, j.Downloaded, now, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return addEvent(ctx, tx, id, "state", fmt.Sprintf("created (%s)", j.State))
	})
	if err != nil {
		return 0, fmt.Errorf("store: creating job: %w", err)
	}
	return id, nil
}

const jobCols = `id, name, cluster, backend, remote_id, state, script, local_dir, remote_dir, message, downloaded, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (Job, error) {
	var j Job
	var st string
	var c, u int64
	if err := r.Scan(&j.ID, &j.Name, &j.Cluster, &j.Backend, &j.RemoteID, &st, &j.Script,
		&j.LocalDir, &j.RemoteDir, &j.Message, &j.Downloaded, &c, &u); err != nil {
		return Job{}, err
	}
	j.State = JobState(st)
	j.CreatedAt, j.UpdatedAt = fromTS(c), fromTS(u)
	return j, nil
}

// UpdateJob loads job id, calls fn on it, and saves the result, all in one
// transaction, so concurrent processes never lose each other's updates. ID
// and CreatedAt changes made by fn are ignored; UpdatedAt is set by the store.
// If fn changes State, a "state" event "<old> -> <new>" is recorded in the same
// transaction. An error from fn aborts without saving. An invalid new State is
// an error.
//
// fn runs while this process holds the database write lock shared by every
// Labmill process, so it must be quick and pure: no network or disk I/O, no
// waiting on the user, and no calls back into the Store (with one connection
// per handle that would deadlock). Other processes wait up to the 5 s busy
// timeout and then fail with SQLITE_BUSY.
func (s *Store) UpdateJob(ctx context.Context, id int64, fn func(*Job) error) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		old, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: job %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: loading job %d: %w", id, err)
		}
		j := old
		if err := fn(&j); err != nil {
			return err
		}
		if !j.State.Valid() {
			return fmt.Errorf("store: invalid job state %q", j.State)
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET name=?, cluster=?, backend=?, remote_id=?, state=?,
			script=?, local_dir=?, remote_dir=?, message=?, downloaded=?, updated_at=? WHERE id=?`,
			j.Name, j.Cluster, j.Backend, j.RemoteID, string(j.State), j.Script, j.LocalDir,
			j.RemoteDir, j.Message, j.Downloaded, ts(time.Now()), id)
		if err != nil {
			return fmt.Errorf("store: saving job %d: %w", id, err)
		}
		if j.State != old.State {
			return addEvent(ctx, tx, id, "state", fmt.Sprintf("%s -> %s", old.State, j.State))
		}
		return nil
	})
}

// Job returns job id, or an error wrapping ErrNotFound.
func (s *Store) Job(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("store: job %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: loading job %d: %w", id, err)
	}
	return j, nil
}

// Jobs returns the jobs matching f, newest first (by CreatedAt, then ID).
func (s *Store) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	q := `SELECT ` + jobCols + ` FROM jobs`
	var where []string
	var args []any
	if f.Cluster != "" {
		where = append(where, "cluster = ?")
		args = append(args, f.Cluster)
	}
	if len(f.States) > 0 {
		ph := make([]string, len(f.States))
		for i, st := range f.States {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(ph, ",")+")")
	}
	if f.ActiveOnly {
		var term []string
		for _, st := range allStates {
			if st.Terminal() {
				term = append(term, "?")
				args = append(args, string(st))
			}
		}
		where = append(where, "state NOT IN ("+strings.Join(term, ",")+")")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: listing jobs: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing jobs: %w", err)
	}
	return out, nil
}

// SetManifest replaces job id's manifest with entries.
func (s *Store) SetManifest(ctx context.Context, jobID int64, entries []ManifestEntry) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireJob(ctx, tx, jobID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM manifest WHERE job_id = ?`, jobID); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO manifest(job_id, path, size, sha256) VALUES (?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, e := range entries {
			if _, err := stmt.ExecContext(ctx, jobID, e.Path, e.Size, e.SHA256); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: setting manifest for job %d: %w", jobID, err)
	}
	return nil
}

// Manifest returns job id's manifest sorted by Path.
func (s *Store) Manifest(ctx context.Context, jobID int64) ([]ManifestEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, size, sha256 FROM manifest WHERE job_id = ? ORDER BY path`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: reading manifest for job %d: %w", jobID, err)
	}
	defer rows.Close()
	var out []ManifestEntry
	for rows.Next() {
		var e ManifestEntry
		if err := rows.Scan(&e.Path, &e.Size, &e.SHA256); err != nil {
			return nil, fmt.Errorf("store: reading manifest for job %d: %w", jobID, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading manifest for job %d: %w", jobID, err)
	}
	return out, nil
}

// AddEvent appends an event to job id's history.
func (s *Store) AddEvent(ctx context.Context, jobID int64, kind, text string) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireJob(ctx, tx, jobID); err != nil {
			return err
		}
		return addEvent(ctx, tx, jobID, kind, text)
	})
	if err != nil {
		return fmt.Errorf("store: adding event to job %d: %w", jobID, err)
	}
	return nil
}

// Events returns job id's events with ID > afterID, oldest first.
func (s *Store) Events(ctx context.Context, jobID int64, afterID int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, time, kind, text FROM events WHERE job_id = ? AND id > ? ORDER BY id`, jobID, afterID)
	if err != nil {
		return nil, fmt.Errorf("store: reading events for job %d: %w", jobID, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&e.ID, &e.JobID, &t, &e.Kind, &e.Text); err != nil {
			return nil, fmt.Errorf("store: reading events for job %d: %w", jobID, err)
		}
		e.Time = fromTS(t)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading events for job %d: %w", jobID, err)
	}
	return out, nil
}

// setUserVersionForTest sets the schema version (tests only).
func (s *Store) setUserVersionForTest(v int) error {
	_, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v))
	return err
}
