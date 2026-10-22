package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "labmill.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestStates(t *testing.T) {
	for _, st := range []JobState{StateFetched, StateFailed, StateCanceled} {
		if !st.Terminal() {
			t.Errorf("%s should be terminal", st)
		}
	}
	for _, st := range []JobState{StateDraft, StateRunning, StateCompleted} {
		if st.Terminal() {
			t.Errorf("%s should not be terminal", st)
		}
	}
	if JobState("bogus").Valid() || !StatePending.Valid() {
		t.Error("Valid wrong")
	}
}

func TestOpenCreatesWithPerms(t *testing.T) {
	_, path := openTemp(t)
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db mode %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", di.Mode().Perm())
	}
}

func TestJobLifecycle(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id, err := s.CreateJob(ctx, Job{Name: "train", Cluster: "hpcone", Backend: "slurm", Script: "#!/bin/bash\npython train.py"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != StateDraft || j.Name != "train" || j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() {
		t.Fatalf("job = %+v", j)
	}
	if err := s.UpdateJob(ctx, id, func(j *Job) error {
		j.State = StateSubmitted
		j.RemoteID = "12345"
		j.ID = 999 // ignored
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Job(ctx, id)
	if j.State != StateSubmitted || j.RemoteID != "12345" || j.ID != id {
		t.Fatalf("after update = %+v", j)
	}
	evs, err := s.Events(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != "state" || evs[1].Text != "draft -> submitted" {
		t.Fatalf("events = %+v", evs)
	}
	// fn error aborts
	boom := errors.New("boom")
	if err := s.UpdateJob(ctx, id, func(j *Job) error { j.State = StateFailed; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if j, _ := s.Job(ctx, id); j.State != StateSubmitted {
		t.Error("aborted update was saved")
	}
	// invalid state
	if err := s.UpdateJob(ctx, id, func(j *Job) error { j.State = "bogus"; return nil }); err == nil {
		t.Error("invalid state accepted")
	}
	if _, err := s.CreateJob(ctx, Job{State: "bogus"}); err == nil {
		t.Error("invalid state accepted on create")
	}
	if _, err := s.Job(ctx, 424242); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing job err = %v", err)
	}
	if err := s.UpdateJob(ctx, 424242, func(*Job) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing err = %v", err)
	}
}

func TestJobsFilter(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mk := func(name, cluster string, st JobState) int64 {
		id, err := s.CreateJob(ctx, Job{Name: name, Cluster: cluster, State: st})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := mk("a", "hpcone", StateRunning)
	b := mk("b", "second", StateFetched)
	c := mk("c", "hpcone", StateFailed)
	d := mk("d", "second", StatePending)

	all, _ := s.Jobs(ctx, JobFilter{})
	if len(all) != 4 || all[0].ID != d || all[3].ID != a {
		t.Fatalf("all = %v", ids(all))
	}
	ww, _ := s.Jobs(ctx, JobFilter{Cluster: "hpcone"})
	if len(ww) != 2 || ww[0].ID != c {
		t.Errorf("hpcone = %v", ids(ww))
	}
	active, _ := s.Jobs(ctx, JobFilter{ActiveOnly: true})
	if len(active) != 2 || active[0].ID != d || active[1].ID != a {
		t.Errorf("active = %v", ids(active))
	}
	byState, _ := s.Jobs(ctx, JobFilter{States: []JobState{StateFetched, StateFailed}})
	if len(byState) != 2 || byState[0].ID != c || byState[1].ID != b {
		t.Errorf("byState = %v", ids(byState))
	}
	lim, _ := s.Jobs(ctx, JobFilter{Limit: 1})
	if len(lim) != 1 || lim[0].ID != d {
		t.Errorf("limit = %v", ids(lim))
	}
}

func TestManifest(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id, _ := s.CreateJob(ctx, Job{Name: "m"})
	entries := []ManifestEntry{{Path: "b/x.dcm", Size: 10, SHA256: "bb"}, {Path: "a.csv", Size: 5, SHA256: "aa"}}
	if err := s.SetManifest(ctx, id, entries); err != nil {
		t.Fatal(err)
	}
	got, err := s.Manifest(ctx, id)
	if err != nil || len(got) != 2 || got[0].Path != "a.csv" || got[1].Size != 10 {
		t.Fatalf("manifest = %+v, %v", got, err)
	}
	if err := s.SetManifest(ctx, id, entries[:1]); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Manifest(ctx, id); len(got) != 1 {
		t.Errorf("replace failed: %+v", got)
	}
}

func TestEventsTailAcrossProcesses(t *testing.T) {
	s1, path := openTemp(t)
	s2, err := Open(path) // a second "process"
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx := context.Background()
	id, _ := s1.CreateJob(ctx, Job{Name: "tail"})
	evs, err := s2.Events(ctx, id, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("Events = %v, %v", evs, err)
	}
	last := evs[len(evs)-1].ID
	if err := s1.AddEvent(ctx, id, "upload", "50%"); err != nil {
		t.Fatal(err)
	}
	if err := s1.AddEvent(ctx, id, "upload", "100%"); err != nil {
		t.Fatal(err)
	}
	newer, err := s2.Events(ctx, id, last)
	if err != nil || len(newer) != 2 || newer[0].Text != "50%" || newer[1].Text != "100%" {
		t.Fatalf("tail = %+v, %v", newer, err)
	}
}

func TestConcurrentUpdatesFromTwoHandles(t *testing.T) {
	s1, path := openTemp(t)
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx := context.Background()
	id, _ := s1.CreateJob(ctx, Job{Name: "race", Message: ""})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		st := s1
		if i%2 == 1 {
			st = s2
		}
		go func(st *Store) {
			defer wg.Done()
			if err := st.UpdateJob(ctx, id, func(j *Job) error { j.Message += "x"; return nil }); err != nil {
				t.Error(err)
			}
		}(st)
	}
	wg.Wait()
	j, _ := s1.Job(ctx, id)
	if len(j.Message) != 20 {
		t.Errorf("lost updates: message has %d x's, want 20", len(j.Message))
	}
}

func TestNewerSchemaRejected(t *testing.T) {
	s, path := openTemp(t)
	s.Close()
	// Simulate a newer Labmill: bump user_version far beyond ours.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.setUserVersionForTest(9999); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("opened a database from a newer labmill")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdgs")
	if p, err := DefaultPath(); err != nil || p != filepath.Join("/xdgs", "labmill", "labmill.db") {
		t.Fatalf("%q %v", p, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	home, _ := os.UserHomeDir()
	if p, err := DefaultPath(); err != nil || p != filepath.Join(home, ".local", "state", "labmill", "labmill.db") {
		t.Fatalf("%q %v", p, err)
	}
}

func ids(js []Job) []int64 {
	var out []int64
	for _, j := range js {
		out = append(out, j.ID)
	}
	return out
}

func TestOpenRelativePath(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	s, err := Open(filepath.Join("rel", "y.db"))
	if err != nil {
		t.Fatalf("Open relative: %v", err)
	}
	defer s.Close()
	if _, err := s.CreateJob(context.Background(), Job{Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "rel", "y.db")); err != nil || fi.Size() == 0 {
		t.Fatalf("database not at the relative path: %v", err)
	}
}

func TestDefaultPathIgnoresRelativeXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	home, _ := os.UserHomeDir()
	if p, err := DefaultPath(); err != nil || p != filepath.Join(home, ".local", "state", "labmill", "labmill.db") {
		t.Fatalf("%q %v", p, err)
	}
}

// A database written by a v1 Labmill (no probes table) is upgraded in place:
// its jobs stay, and probes work afterwards.
func TestMigrateV1ToV2KeepsJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO jobs (name, cluster, backend, remote_id, state, script, local_dir, remote_dir, message, created_at, updated_at)
		VALUES ('old-job', 'hpcone', 'slurm', '4242', 'completed', '#!/bin/bash', '', '', '', 1, 1); PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	defer s.Close()
	jobs, err := s.Jobs(context.Background(), JobFilter{})
	if err != nil || len(jobs) != 1 || jobs[0].Name != "old-job" || jobs[0].RemoteID != "4242" || jobs[0].State != StateCompleted {
		t.Fatalf("jobs after upgrade = %+v, %v", jobs, err)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion || schemaVersion != 3 {
		t.Errorf("user_version = %d, %v", v, err)
	}
	if jobs[0].Downloaded {
		t.Error("an upgraded job counts as downloaded")
	}
	if err := s.UpdateJob(context.Background(), jobs[0].ID, func(j *Job) error { j.Downloaded = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Job(context.Background(), jobs[0].ID); err != nil || !j.Downloaded {
		t.Errorf("Downloaded not kept: %+v %v", j, err)
	}
}
