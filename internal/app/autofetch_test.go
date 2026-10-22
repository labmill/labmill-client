package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// M60.1: AutoFetch downloads a finished job's results by itself: once per
// job, the files the job produced (not what was uploaded), into the usual
// results folder, with the size guards and plain-language failures.
func TestAutoFetch(t *testing.T) {
	const gib = int64(1) << 30
	results := []transfer.RemoteFile{{Path: "out/a.csv", Size: 1000}, {Path: "slurm-1.out", Size: 50}}
	cases := []struct {
		name       string
		state      store.JobState
		remoteDir  string
		files      []transfer.RemoteFile
		listErr    error
		downloadEr error
		free       int64 // -1: FreeSpace not set
		mismatch   bool
		again      bool // call twice
		wantText   []string
		wantNone   []string
		wantGot    []string // paths handed to Download
		fetched    bool
		retry      bool // postponed, not handled
	}{
		{name: "completed", state: store.StateCompleted, files: results, free: 100 * gib,
			wantText: []string{"Job #1 (train)", "downloaded 2 files", "labmill-results"}, wantGot: []string{"out/a.csv", "slurm-1.out"}, fetched: true},
		{name: "failed job is downloaded too", state: store.StateFailed, files: results, free: -1,
			wantText: []string{"downloaded 2 files"}, wantGot: []string{"out/a.csv", "slurm-1.out"}, fetched: true},
		{name: "uploaded input and scripts are left", state: store.StateCompleted, free: 100 * gib,
			files:    []transfer.RemoteFile{{Path: "in/x.dat", Size: 7}, {Path: "labmill-run.sh", Size: 20}, {Path: "out/a.csv", Size: 1000}, {Path: "in/y.dat", Size: 9}},
			wantText: []string{"downloaded 2 files"}, wantGot: []string{"in/y.dat", "out/a.csv"}, fetched: true},
		{name: "nothing new", state: store.StateCompleted, free: 100 * gib,
			files:    []transfer.RemoteFile{{Path: "in/x.dat", Size: 7}},
			wantText: []string{"weren't downloaded automatically", "besides the ones you uploaded", "Ask me to fetch"}},
		{name: "once only", state: store.StateCompleted, files: results, free: -1, again: true,
			wantText: []string{"downloaded 2 files"}, wantGot: []string{"out/a.csv", "slurm-1.out"}, fetched: true},
		{name: "over 2 GB", state: store.StateCompleted, free: 100 * gib,
			files:    []transfer.RemoteFile{{Path: "big.bin", Size: 2*gib + 1}},
			wantText: []string{"weren't downloaded automatically", "more than the 2 GB limit", "Ask me to fetch"}},
		{name: "exactly 2 GB is fine", state: store.StateCompleted, free: 100 * gib,
			files: []transfer.RemoteFile{{Path: "big.bin", Size: 2 * gib}}, wantText: []string{"downloaded 1 file "}, wantGot: []string{"big.bin"}, fetched: true},
		{name: "not enough free space", state: store.StateCompleted, free: 1000,
			files:    []transfer.RemoteFile{{Path: "a.bin", Size: 950}},
			wantText: []string{"weren't downloaded automatically", "only", "is free on this computer"}},
		{name: "fits in 90 percent of free space", state: store.StateCompleted, free: 1000,
			files: []transfer.RemoteFile{{Path: "a.bin", Size: 900}}, wantText: []string{"downloaded 1 file "}, wantGot: []string{"a.bin"}, fetched: true},
		{name: "listing fails", state: store.StateCompleted, listErr: errors.New("connection reset"), free: -1,
			wantText: []string{"weren't downloaded automatically", "connection reset", "Ask me to fetch"}},
		{name: "sign-in needed", state: store.StateCompleted, listErr: fmt.Errorf("%w: no open sign-in", ErrSignInNeeded), free: -1,
			wantText: []string{"sign in to the cluster", "within a few minutes"}, wantNone: []string{"connection"}, retry: true},
		{name: "download fails", state: store.StateCompleted, files: results, downloadEr: errors.New("disk full"), free: -1,
			wantText: []string{"weren't downloaded automatically", "didn't finish", "disk full", "Ask me to fetch"}, wantGot: []string{"out/a.csv", "slurm-1.out"}},
		{name: "checksum mismatch is not fetched", state: store.StateCompleted, files: results, free: -1, mismatch: true,
			wantText: []string{"downloaded 2 files", "WARNING"}, wantGot: []string{"out/a.csv", "slurm-1.out"}},
		{name: "canceled job is left alone", state: store.StateCanceled, files: results, free: -1},
		{name: "still running is left alone", state: store.StateRunning, files: results, free: -1},
		{name: "job without a folder", state: store.StateCompleted, remoteDir: "-", files: results, free: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			var got []string
			downloads := 0
			h.env.ListRemote = func(context.Context, transfer.Remote) ([]transfer.RemoteFile, error) {
				return c.files, c.listErr
			}
			h.env.Download = func(_ context.Context, src transfer.Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
				downloads++
				got = paths
				if c.mismatch {
					src.OnVerify(transfer.Verification{Mismatched: []string{"out/a.csv.mismatch"}})
				}
				if c.downloadEr != nil {
					return nil, c.downloadEr
				}
				var w []string
				for _, p := range paths {
					w = append(w, filepath.Join(localDir, p))
				}
				return w, nil
			}
			if c.free >= 0 {
				h.env.FreeSpace = func(dir string) (int64, error) { return c.free, nil }
			}
			dir := "/home/alice/train"
			switch c.remoteDir {
			case "-":
				dir = ""
			}
			ctx := context.Background()
			id, err := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: dir, RemoteID: "9", State: c.state})
			if err != nil {
				t.Fatal(err)
			}
			if c.name == "uploaded input and scripts are left" || c.name == "nothing new" {
				h.env.Store.SetManifest(ctx, id, []store.ManifestEntry{{Path: "in/x.dat", Size: 7}, {Path: "in/y.dat", Size: 8}})
				h.env.Store.AddEvent(ctx, id, eventFiles, `[{"name":"labmill-run.sh","content":"x"}]`)
			}
			var log []string
			r := h.env.AutoFetch(ctx, id, func(s string) { log = append(log, s) })
			if c.again {
				if r2 := h.env.AutoFetch(ctx, id, nil); r2.Text != "" || r2.Downloaded {
					t.Errorf("second call did something: %+v", r2)
				}
			}
			if len(c.wantText) == 0 {
				if r.Text != "" || r.Downloaded || downloads != 0 {
					t.Fatalf("expected nothing, got %+v (downloads %d)", r, downloads)
				}
				return
			}
			for _, w := range c.wantText {
				if !strings.Contains(r.Text, w) {
					t.Errorf("text %q lacks %q", r.Text, w)
				}
			}
			for _, w := range c.wantNone {
				if strings.Contains(r.Text, w) {
					t.Errorf("text %q has %q", r.Text, w)
				}
			}
			if strings.Join(got, ",") != strings.Join(c.wantGot, ",") {
				t.Errorf("Download got %v, want %v", got, c.wantGot)
			}
			if c.again && downloads != 1 {
				t.Errorf("downloaded %d times", downloads)
			}
			if r.Downloaded != c.fetched {
				t.Errorf("Downloaded = %v, want %v (%q)", r.Downloaded, c.fetched, r.Text)
			}
			j, _ := h.env.Store.Job(ctx, id)
			switch {
			case c.fetched && c.state == store.StateFailed:
				if j.State != store.StateFailed || j.LocalDir == "" {
					t.Errorf("a failed job must stay failed with its folder recorded: %s %q", j.State, j.LocalDir)
				}
			case (j.State == store.StateFetched) != c.fetched:
				t.Errorf("job state %s, want fetched=%v", j.State, c.fetched)
			}
			if r.Retry != c.retry {
				t.Errorf("Retry = %v, want %v", r.Retry, c.retry)
			}
			_, handled, _ := (&toolset{env: h.env}).latestEvent(ctx, id, eventAutoFetch)
			if handled == c.retry {
				t.Errorf("handled marker = %v for retry=%v", handled, c.retry)
			}
			if len(log) == 0 {
				t.Error("nothing was logged")
			}
		})
	}
}

// A job whose end was already handled (the event is in the history, as after
// a restart) is not downloaded again.
func TestAutoFetchNotRepeatedAfterRestart(t *testing.T) {
	h := newHarness(t)
	called := false
	h.env.ListRemote = func(context.Context, transfer.Remote) ([]transfer.RemoteFile, error) {
		called = true
		return []transfer.RemoteFile{{Path: "a", Size: 1}}, nil
	}
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})
	h.env.Store.AddEvent(ctx, id, eventAutoFetch, "started")
	if r := h.env.AutoFetch(ctx, id, nil); r.Text != "" || called {
		t.Errorf("repeated: %+v called=%v", r, called)
	}
}

// Without listing support the download is skipped with a sentence.
func TestAutoFetchWithoutListing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", RemoteDir: "/home/alice/train", RemoteID: "9", State: store.StateCompleted})
	r := h.env.AutoFetch(ctx, id, nil)
	if !strings.Contains(r.Text, "Ask me to fetch") || r.Downloaded {
		t.Errorf("got %+v", r)
	}
}
