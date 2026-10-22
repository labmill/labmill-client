package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

func rf(paths ...string) []transfer.RemoteFile {
	var out []transfer.RemoteFile
	for _, p := range paths {
		out = append(out, transfer.RemoteFile{Path: p, Size: 10})
	}
	return out
}

// selectResults leaves out private names, environments and caches.
func TestSelectResults(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		keep []string
		omit int
	}{
		{"plain", []string{"out/a.csv", "log.txt"}, []string{"out/a.csv", "log.txt"}, 0},
		{"home inside the job folder", []string{"home/.cache/huggingface/token", "home/.netrc", "home/.env", "home/.bash_history", "out/r.csv"}, []string{"out/r.csv"}, 4},
		{"secret names", []string{"id_rsa", "my.pem", "credentials.json", "a/secret.txt", "ok.txt"}, []string{"ok.txt"}, 4},
		{"venv by pyvenv.cfg", []string{"env/pyvenv.cfg", "env/bin/python", "env/lib/x.py", "out/a"}, []string{"out/a"}, 3},
		{"conda env", []string{"c/conda-meta/history", "c/bin/python", "out/a"}, []string{"out/a"}, 2},
		{"conda env at the root", []string{"conda-meta/x.json", "bin/python", "a"}, nil, 3},
		{"caches", []string{"x/__pycache__/m.pyc", "node_modules/p/i.js", "lib/python3/site-packages/p.py", "r/.git/HEAD", "r/data.bin"}, []string{"r/data.bin"}, 4},
		{"venv sibling is kept", []string{"env/pyvenv.cfg", "envx/a.txt"}, []string{"envx/a.txt"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, omit := selectResults(rf(c.in...))
			var got []string
			for _, f := range kept {
				got = append(got, f.Path)
			}
			if strings.Join(got, ",") != strings.Join(c.keep, ",") || omit != c.omit {
				t.Errorf("kept %v, left out %d; want %v, %d", got, omit, c.keep, c.omit)
			}
		})
	}
}

// Private files in the job folder are left out and the message says how many;
// too many files are not downloaded.
func TestAutoFetchPrivacyAndFileLimit(t *testing.T) {
	h := newHarness(t)
	var got []string
	h.env.Download = func(_ context.Context, _ transfer.Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
		got = paths
		return nil, nil
	}
	h.env.ListRemote = func(context.Context, transfer.Remote) ([]transfer.RemoteFile, error) {
		return rf("home/.netrc", "home/.cache/huggingface/token", "home/.env", "out/r.csv"), nil
	}
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})
	r := h.env.AutoFetch(ctx, id, nil)
	if strings.Join(got, ",") != "out/r.csv" || !strings.Contains(r.Text, "3 files were left out") {
		t.Errorf("paths %v, text %q", got, r.Text)
	}

	h = newHarness(t)
	var many []transfer.RemoteFile
	for i := 0; i <= AutoFetchMaxFiles; i++ {
		many = append(many, transfer.RemoteFile{Path: fmt.Sprintf("o/f%05d", i), Size: 1})
	}
	h.env.ListRemote = func(context.Context, transfer.Remote) ([]transfer.RemoteFile, error) { return many, nil }
	h.env.Download = func(context.Context, transfer.Remote, []string, string, func(transfer.Progress)) ([]string, error) {
		t.Error("downloaded too many files")
		return nil, nil
	}
	id, _ = h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})
	r = h.env.AutoFetch(ctx, id, nil)
	if !strings.Contains(r.Text, "2001 files, more than the 2000-file limit") {
		t.Errorf("text %q", r.Text)
	}
}

// A postponed attempt (no sign-in, busy, quitting) is not the final outcome:
// the job stays pending, the user is told about the sign-in once, and a later
// attempt that works downloads it, once.
func TestAutoFetchPostponedThenRetried(t *testing.T) {
	old := autoFetchBackoff
	autoFetchBackoff = 0
	t.Cleanup(func() { autoFetchBackoff = old })
	h := newHarness(t)
	failWith := ErrSignInNeeded
	var downloads int
	h.env.ListRemote = func(context.Context, transfer.Remote) ([]transfer.RemoteFile, error) {
		if failWith != nil {
			return nil, fmt.Errorf("%w: no open sign-in", failWith)
		}
		return rf("o/a"), nil
	}
	h.env.Download = func(_ context.Context, _ transfer.Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
		downloads++
		return []string{filepath.Join(localDir, paths[0])}, nil
	}
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})

	r := h.env.AutoFetch(ctx, id, nil)
	if !r.Retry || !strings.Contains(r.Text, "sign in") {
		t.Fatalf("first: %+v", r)
	}
	if p := h.env.PendingAutoFetch(ctx); len(p) != 1 || p[0] != id {
		t.Errorf("pending = %v", p)
	}
	if r = h.env.AutoFetch(ctx, id, nil); !r.Retry || r.Text != "" {
		t.Errorf("second attempt must be silent: %+v", r)
	}
	failWith = ErrTryLater
	if r = h.env.AutoFetch(ctx, id, nil); !r.Retry || r.Text != "" {
		t.Errorf("busy attempt must be silent: %+v", r)
	}
	failWith = nil
	if r = h.env.AutoFetch(ctx, id, nil); !r.Downloaded || downloads != 1 {
		t.Fatalf("retry: %+v downloads %d", r, downloads)
	}
	if p := h.env.PendingAutoFetch(ctx); len(p) != 0 {
		t.Errorf("still pending: %v", p)
	}
	if r = h.env.AutoFetch(ctx, id, nil); r.Text != "" || downloads != 1 {
		t.Errorf("repeated: %+v", r)
	}
}

// A canceled context (Labmill quitting) leaves the job pending, not handled.
func TestAutoFetchCanceledStaysPending(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.env.ListRemote = func(context.Context, transfer.Remote) ([]transfer.RemoteFile, error) { return rf("o/a"), nil }
	h.env.Download = func(context.Context, transfer.Remote, []string, string, func(transfer.Progress)) ([]string, error) {
		cancel()
		return nil, context.Canceled
	}
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})
	r := h.env.AutoFetch(ctx, id, nil)
	if !r.Retry || r.Text != "" {
		t.Errorf("got %+v", r)
	}
	if p := h.env.PendingAutoFetch(context.Background()); len(p) != 1 {
		t.Errorf("pending = %v", p)
	}
}

// job_status calls OnJobEnded when it is the one that saw the job end.
func TestJobStatusCallsOnJobEnded(t *testing.T) {
	h := newHarness(t)
	var ended []int64
	h.env.OnJobEnded = func(id int64) { ended = append(ended, id) }
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateRunning})
	h.be.status.State = store.StateRunning
	if _, err := h.run(t, "job_status", map[string]any{"job_id": id}); err != nil || len(ended) != 0 {
		t.Fatalf("running: %v %v", err, ended)
	}
	h.be.status.State = store.StateCompleted
	if _, err := h.run(t, "job_status", map[string]any{"job_id": id}); err != nil || len(ended) != 1 || ended[0] != id {
		t.Fatalf("completed: %v %v", err, ended)
	}
	if _, err := h.run(t, "job_status", map[string]any{"job_id": id}); err != nil || len(ended) != 1 {
		t.Errorf("a job that was already finished called it again: %v", ended)
	}
}

// Downloads of one job never run side by side.
func TestDownloadsOfOneJobAreSerialized(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	active, maxActive := 0, 0
	h.env.Download = func(_ context.Context, _ transfer.Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
		mu.Lock()
		active++
		maxActive = max(maxActive, active)
		mu.Unlock()
		for i := 0; i < 200000; i++ { // a moment of work
			_ = errors.New("x")
		}
		mu.Lock()
		active--
		mu.Unlock()
		return []string{filepath.Join(localDir, paths[0])}, nil
	}
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.run(t, "fetch_results", map[string]any{"job_id": id, "paths": []string{"out/a.txt"}})
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Errorf("%d downloads of one job ran at once", maxActive)
	}
}

func TestExistingParent(t *testing.T) {
	d := t.TempDir()
	if got := existingParent(filepath.Join(d, "a", "b", "c")); got != d {
		t.Errorf("existingParent = %q, want %q", got, d)
	}
	if got := existingParent(d); got != d {
		t.Errorf("existingParent(existing) = %q", got)
	}
}

// A job waiting for a sign-in is retried at the next check, then only after
// the back-off, and the log gets the postponement once.
func TestAutoFetchBackoffAndQuietLog(t *testing.T) {
	h := newHarness(t)
	lists := 0
	h.env.ListRemote = func(context.Context, transfer.Remote) ([]transfer.RemoteFile, error) {
		lists++
		return nil, fmt.Errorf("%w: none", ErrSignInNeeded)
	}
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})
	var log []string
	logf := func(s string) { log = append(log, s) }
	for i := 0; i < 5; i++ {
		if r := h.env.AutoFetch(ctx, id, logf); !r.Retry {
			t.Fatalf("attempt %d: %+v", i, r)
		}
	}
	if lists != 2 {
		t.Errorf("listed %d times in 5 quick checks, want 2 (first try and one retry)", lists)
	}
	if len(log) != 2 {
		t.Errorf("log = %q, want the start and the postponement once", log)
	}
}

// Waiting for another download of the same job ends when ctx does.
func TestLockJobHonorsContext(t *testing.T) {
	h := newHarness(t)
	if err := lockJob(context.Background(), h.env.Store, 5, "x"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lockJob(ctx, h.env.Store, 5, "x"); err == nil {
		t.Error("lock taken twice")
	}
	unlockJob(h.env.Store, 5, "x")
	if err := lockJob(context.Background(), h.env.Store, 5, "x"); err != nil {
		t.Error(err)
	}
	unlockJob(h.env.Store, 5, "x")
}
