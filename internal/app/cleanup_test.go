package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/rules"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

func TestCleanupRootSafety(t *testing.T) {
	plain := &rules.GlobalScratch{Path: "/scratch"}
	ph := &rules.GlobalScratch{Path: "/scratch/<group>"}
	tests := []struct {
		name string
		g    *rules.GlobalScratch
		dir  string
		ok   bool
	}{
		{"job folder", plain, "/scratch/me/labmill/seg", true},
		{"not a Labmill job folder", plain, "/scratch/me/job", false},
		{"Labmill job folder at the minimum depth", plain, "/scratch/labmill/job", true},
		{"placeholder job folder", ph, "/scratch/lab/me/labmill/seg", true},
		{"root itself", plain, "/scratch", false},
		{"one level below root", plain, "/scratch/me", false},
		{"placeholder group folder", ph, "/scratch/lab/me", false},
		{"outside", plain, "/home/me/labmill/seg", false},
		{"sibling prefix", plain, "/scratchy/me/labmill/seg", false},
		{"dot dot", plain, "/scratch/me/../../etc/x", false},
		{"dot dot inside", plain, "/scratch/me/labmill/../..", false},
		{"unclean", plain, "/scratch/me//labmill/seg", false},
		{"trailing slash", plain, "/scratch/me/labmill/seg/", false},
		{"relative", plain, "scratch/me/labmill/seg", false},
		{"the Labmill parent", plain, "/scratch/me/labmill", false},
		{"no profile", nil, "/scratch/me/labmill/seg", false},
	}
	for _, tc := range tests {
		if err := cleanupRoot(tc.g, tc.dir); (err == nil) != tc.ok {
			t.Errorf("%s: cleanupRoot(%q) = %v, want ok=%v", tc.name, tc.dir, err, tc.ok)
		}
	}
}

func TestCleanablePath(t *testing.T) {
	tests := []struct {
		rel string
		ok  bool
	}{
		{"results/out.csv", true},
		{"a b/c d.txt", true},
		{"-rf", true}, // quoted after "--"
		{"", false}, {".", false}, {"/etc/passwd", false}, {"../x", false}, {"a/../b", false}, {"a//b", false},
		{"a\nb", false},
		{"venv/lib/python3/site-packages/x.py", false},
		{"env/.conda/x", false}, {"envs/a/x", false}, {"conda-meta/x.json", false},
		{"__pycache__/x.pyc", false}, {".cache/x", false},
	}
	for _, tc := range tests {
		if got := cleanablePath(tc.rel); got != tc.ok {
			t.Errorf("cleanablePath(%q) = %v, want %v", tc.rel, got, tc.ok)
		}
	}
}

func TestCleanupCommands(t *testing.T) {
	dir := "/scratch/me/labmill/it's"
	abs := cleanupAbs(dir, []string{"a b", "-x"})
	if got := backend.Quote(cleanupRmArgv(abs)); got != `rm -f -- '/scratch/me/labmill/it'\''s/a b' '/scratch/me/labmill/it'\''s/-x'` {
		t.Errorf("rm line = %s", got)
	}
	if got := strings.Join(cleanupRmdirArgv([]string{"/d/a/b", "/d/a"}), " "); got != "rmdir --ignore-fail-on-non-empty -- /d/a/b /d/a" {
		t.Errorf("rmdir = %s", got)
	}
	// Batches by quoted size: 3 words of 10 bytes (+ quotes) fit 2 per 30 bytes.
	words := []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"}
	if got := batches(words, 24); len(got) != 2 || len(got[0]) != 2 {
		t.Errorf("batches = %v", got)
	}
	if got := batches([]string{strings.Repeat("x", 100)}, 10); len(got) != 1 {
		t.Errorf("oversize word batches = %v", got)
	}
	if got := parentDirs("/j", []string{"/j/a/b/f", "/j/a/g", "/j/top"}); strings.Join(got, " ") != "/j/a/b /j/a" {
		t.Errorf("parentDirs = %v", got)
	}
}

// cleanupFixture is a Slurm job in the shared scratch that was downloaded,
// on a fake login node.
type cleanupFixture struct {
	h   *harness
	fr  *fakeRunner
	id  int64
	dir string
	mu  sync.Mutex
	rm  [][]string        // the files of each rm call
	sum map[string]string // absolute path -> SHA-256 on the fake cluster
	// findOut is what the fake cluster's look for environments and symbolic
	// links prints.
	findOut string
	// squeueOut is what squeue --me prints ("<id> <work dir>" lines);
	// squeueFail makes it exit non-zero.
	squeueOut  string
	squeueFail bool
	// noMe makes squeue --me fail (Slurm before 20.02); realDir is what
	// readlink -f prints for the job folder.
	noMe    bool
	realDir string
}

func sumOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func newCleanupFixture(t *testing.T, planScratch string, dir string) *cleanupFixture {
	t.Helper()
	f := &cleanupFixture{sum: map[string]string{}, dir: dir}
	f.fr = &fakeRunner{fn: func(_ context.Context, argv []string) (backend.Result, error) {
		a := argv[4:] // after: timeout -k <kill> <secs>
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case a[0] == "readlink":
			return backend.Result{Stdout: f.realDir + "\n"}, nil
		case a[0] == "id":
			return backend.Result{Stdout: "me\n"}, nil
		case a[0] == "squeue":
			if f.squeueFail || (f.noMe && a[1] == "--me") {
				return backend.Result{ExitCode: 1, Stderr: "slurm down"}, nil
			}
			return backend.Result{Stdout: f.squeueOut}, nil
		case a[0] == "find" && len(a) > 2 && a[2] == "-name":
			return backend.Result{Stdout: f.findOut}, nil
		case a[0] == "sha256sum":
			var b strings.Builder
			for _, p := range a[2:] {
				if s, ok := f.sum[p]; ok {
					b.WriteString(s + "  " + p + "\n")
				}
			}
			return backend.Result{Stdout: b.String()}, nil
		case a[0] == "rm":
			f.rm = append(f.rm, a[3:])
		}
		return backend.Result{}, nil
	}}
	h := remoteHarness(t, f.fr)
	h.env.Site = SiteInfo{Clusters: map[string]ClusterHints{"hpcone": {Slurm: rules.SlurmHints{GlobalScratch: &rules.GlobalScratch{Path: "/scratch"}}}}}
	h.tools = map[string]agent.Tool{}
	for _, tool := range Tools(h.env) {
		h.tools[tool.Name] = tool
	}
	f.h = h
	ctx := context.Background()
	id, err := h.env.Store.CreateJob(ctx, store.Job{Name: "seg", Cluster: "hpcone", Backend: "slurm", RemoteDir: dir, RemoteID: "9", State: store.StateFetched, Downloaded: true})
	if err != nil {
		t.Fatal(err)
	}
	f.id = id
	if planScratch != "" {
		b, _ := json.Marshal(placementPlan{Scratch: planScratch})
		h.env.Store.AddEvent(ctx, id, eventPlan, string(b))
	}
	return f
}

func (f *cleanupFixture) downloaded(t *testing.T, rels ...string) {
	t.Helper()
	recs := make([]downloadedRecord, len(rels))
	for i, r := range rels {
		recs[i] = downloadedRecord{r, sumOf(r)}
		if _, ok := f.sum[f.dir+"/"+r]; !ok {
			f.sum[f.dir+"/"+r] = sumOf(r) // the cluster still has the verified file
		}
	}
	b, _ := json.Marshal(recs)
	if err := f.h.env.Store.AddEvent(context.Background(), f.id, eventDownloaded, string(b)); err != nil {
		t.Fatal(err)
	}
}

func (f *cleanupFixture) calls() [][]string {
	f.fr.mu.Lock()
	defer f.fr.mu.Unlock()
	return append([][]string(nil), f.fr.calls...)
}

func jobIn(id int64) json.RawMessage { return mustJSON(map[string]any{"job_id": id}) }

func TestCleanupOfferOnlyForVerifiedGlobalDownload(t *testing.T) {
	dir := "/scratch/me/labmill/seg"
	tests := []struct {
		name    string
		plan    string
		dl      bool
		cleaned bool
		want    bool
	}{
		{"global, verified", scratchGlobal, true, false, true},
		{"job folder is not offered", scratchJobFolder, true, false, false},
		{"no plan is not offered", "", true, false, false},
		{"node-local is cleaned by the job", scratchLocal, true, false, false},
		{"nothing verified recorded", scratchGlobal, false, false, false},
		{"already cleaned", scratchGlobal, true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCleanupFixture(t, tc.plan, dir)
			if tc.dl {
				f.downloaded(t, "out/a.csv")
			}
			if tc.cleaned {
				f.h.env.Store.AddEvent(context.Background(), f.id, eventCleaned, `{}`)
			}
			offer := (&toolset{env: f.h.env}).cleanupOffer(context.Background(), f.id)
			if (offer != "") != tc.want {
				t.Fatalf("offer = %q, want offer=%v", offer, tc.want)
			}
			if tc.want && !strings.Contains(offer, "clean up job #") {
				t.Errorf("offer = %q", offer)
			}
		})
	}
}

func TestCleanupOfferNotForUndownloadedJob(t *testing.T) {
	f := newCleanupFixture(t, scratchGlobal, "/scratch/me/labmill/seg")
	f.downloaded(t, "a")
	f.h.env.Store.UpdateJob(context.Background(), f.id, func(j *store.Job) error { j.Downloaded = false; return nil })
	if o := (&toolset{env: f.h.env}).cleanupOffer(context.Background(), f.id); o != "" {
		t.Errorf("offer = %q", o)
	}
}

// Only a download whose every file matched by SHA-256 is recorded.
func TestRecordDownloadedNeedsFullVerification(t *testing.T) {
	tests := []struct {
		name string
		v    transfer.Verification
		want bool
	}{
		{"all verified", transfer.Verification{Verified: 2}, true},
		{"size only", transfer.Verification{Unverified: 2, SizeOnly: "no sha256sum"}, false},
		{"one unreadable", transfer.Verification{Verified: 1, Failed: []string{"b"}}, false},
		{"mismatch", transfer.Verification{Verified: 1, Mismatched: []string{"/x/b"}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCleanupFixture(t, scratchGlobal, "/scratch/me/labmill/seg")
			ts := &toolset{env: f.h.env}
			local := t.TempDir()
			os.MkdirAll(local+"/out", 0o755)
			os.WriteFile(local+"/out/a.csv", []byte("A"), 0o644)
			os.WriteFile(local+"/b", []byte("B"), 0o644)
			ts.recordDownloaded(context.Background(), fetchPlan{job: store.Job{ID: f.id}, localDir: local}, []string{local + "/out/a.csv", local + "/b"}, tc.v)
			got, _ := ts.pendingDownloads(context.Background(), f.id)
			if (len(got) == 2) != tc.want {
				t.Errorf("recorded %v, want recorded=%v", got, tc.want)
			}
			if tc.want && (got[0].Path != "out/a.csv" || got[0].SHA256 != sumOf("A") || got[1].Path != "b") {
				t.Errorf("recorded %v", got)
			}
		})
	}
}

func TestCleanupRefusesOutsideScratch(t *testing.T) {
	for _, dir := range []string{"/home/alice/labmill/seg", "/scratch", "/scratch/me", "/storage/x/labmill/seg"} {
		f := newCleanupFixture(t, scratchJobFolder, dir)
		f.downloaded(t, "a")
		_, err := f.h.tools["cleanup_job"].Propose(jobIn(f.id))
		if err == nil || !strings.Contains(err.Error(), "shared scratch") {
			t.Errorf("%s: err = %v", dir, err)
		}
		if len(f.calls()) != 0 {
			t.Errorf("%s: ran %v", dir, f.calls())
		}
	}
}

func TestCleanupApprovedRunExactCommands(t *testing.T) {
	dir := "/scratch/me/labmill/seg"
	f := newCleanupFixture(t, scratchJobFolder, dir) // not offered automatically, but the user may ask
	ctx := context.Background()
	// Staged inputs and a helper file; the cluster still has input a as
	// uploaded, but the job changed input b.
	f.h.env.Store.SetManifest(ctx, f.id, []store.ManifestEntry{{Path: "in/a.dat", Size: 1, SHA256: sumOf("A")}, {Path: "in/b.dat", Size: 1, SHA256: sumOf("B")}})
	b, _ := json.Marshal([]jobFile{{Name: "labmill-run.sh", Content: "echo hi\n"}})
	f.h.env.Store.AddEvent(ctx, f.id, eventFiles, string(b))
	f.sum[dir+"/in/a.dat"] = sumOf("A")
	f.sum[dir+"/in/b.dat"] = sumOf("changed")
	f.sum[dir+"/labmill-run.sh"] = sumOf("echo hi\n")
	f.sum[dir+"/out/rewritten.csv"] = sumOf("replaced by a later run") // downloaded, but changed since
	f.downloaded(t, "out/r 1.csv", "out/rewritten.csv", "venv/lib/python3/site-packages/x.py", "myenv/bin/tool", "link/data.csv", "-dash", "deep/a/b/f.txt")
	// The cluster has an environment myenv and a symlinked folder link.
	f.findOut = dir + "/myenv/pyvenv.cfg\n" + dir + "/link\n"

	tool := f.h.tools["cleanup_job"]
	p, err := tool.Propose(jobIn(f.id))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"out/r 1.csv", dir, "rmdir --ignore-fail-on-non-empty -- " + dir, "Software environments", "SHA-256 on the cluster"} {
		if !strings.Contains(p.Details, want) {
			t.Errorf("details lack %q:\n%s", want, p.Details)
		}
	}
	if strings.Contains(p.Details, "site-packages/") || strings.Contains(p.Details, "-empty -delete") {
		t.Errorf("details wrong:\n%s", p.Details)
	}
	// No approval: nothing runs.
	if _, err := tool.Run(ctx, jobIn(f.id), agent.Decision{}); err == nil || len(f.calls()) != 0 {
		t.Fatalf("ran without approval: %v %v", err, f.calls())
	}
	out, err := tool.Run(ctx, jobIn(f.id), agent.Decision{Approved: true, Proposal: p})
	if err != nil {
		t.Fatal(err)
	}
	// myenv/bin/tool and link/data.csv are skipped on the cluster's evidence;
	// input b and rewritten.csv changed on the cluster.
	if len(f.rm) != 1 {
		t.Fatalf("rm calls = %v", f.rm)
	}
	got := strings.Join(f.rm[0], "|")
	want := strings.Join([]string{dir + "/-dash", dir + "/deep/a/b/f.txt", dir + "/in/a.dat", dir + "/labmill-run.sh", dir + "/out/r 1.csv"}, "|")
	if got != want {
		t.Errorf("rm files = %q\nwant       %q", got, want)
	}
	calls := f.calls()
	tail := func(i int) string { return strings.Join(calls[len(calls)-i][4:], " ") }
	if tail(1) != "rmdir --ignore-fail-on-non-empty -- "+dir {
		t.Errorf("last call = %v", calls[len(calls)-1])
	}
	if tail(2) != "rmdir --ignore-fail-on-non-empty -- "+dir+"/deep/a/b "+dir+"/deep/a "+dir+"/deep "+dir+"/in "+dir+"/out" {
		t.Errorf("rmdir folders = %s", tail(2))
	}
	if !strings.Contains(out, "Removed 5 working files") || !strings.Contains(out, "Left in place") {
		t.Errorf("out = %q", out)
	}
	// The cleaned event; a second call finds nothing, asks nothing, runs nothing.
	if _, ok, _ := (&toolset{env: f.h.env}).latestEvent(ctx, f.id, eventCleaned); !ok {
		t.Error("no cleaned event")
	}
	n := len(f.calls())
	if _, err := tool.Propose(jobIn(f.id)); err != agent.ErrNoApproval {
		t.Errorf("second propose = %v", err)
	}
	out, err = tool.Run(ctx, jobIn(f.id), agent.Decision{})
	if err != nil || !strings.HasPrefix(out, "Nothing to remove") || len(f.calls()) != n {
		t.Errorf("second run = %q, %v, calls %d->%d", out, err, n, len(f.calls()))
	}
}

// A job that is still running, being resubmitted, or whose folder another
// active job uses is never cleaned.
func TestCleanupBlockedWhileInUse(t *testing.T) {
	dir := "/scratch/me/labmill/seg"
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(f *cleanupFixture)
		blame string
	}{
		{"running", func(f *cleanupFixture) {
			f.h.env.Store.UpdateJob(ctx, f.id, func(j *store.Job) error { j.State = store.StateRunning; return nil })
		}, "hasn't ended"},
		{"pending", func(f *cleanupFixture) {
			f.h.env.Store.UpdateJob(ctx, f.id, func(j *store.Job) error { j.State = store.StatePending; return nil })
		}, "hasn't ended"},
		{"open resubmission claim", func(f *cleanupFixture) {
			f.h.env.Store.AddEvent(ctx, f.id, store.EventResubmitting, "9 {}")
		}, "being resubmitted"},
		{"another active job in the folder", func(f *cleanupFixture) {
			f.h.env.Store.CreateJob(ctx, store.Job{Name: "sub", Cluster: "hpcone", RemoteDir: dir + "/sub", State: store.StateRunning})
		}, "still active in the same folder"},
		{"another active job in the very folder", func(f *cleanupFixture) {
			f.h.env.Store.CreateJob(ctx, store.Job{Name: "twin", Cluster: "hpcone", RemoteDir: dir, State: store.StatePending})
		}, "still active in the same folder"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCleanupFixture(t, scratchGlobal, dir)
			f.downloaded(t, "a")
			tc.setup(f)
			_, err := f.h.tools["cleanup_job"].Propose(jobIn(f.id))
			if err == nil || !strings.Contains(err.Error(), tc.blame) {
				t.Fatalf("err = %v, want %q", err, tc.blame)
			}
			if o := (&toolset{env: f.h.env}).cleanupOffer(ctx, f.id); o != "" {
				t.Errorf("offer = %q", o)
			}
			if len(f.calls()) != 0 {
				t.Errorf("ran %v", f.calls())
			}
		})
	}
	// Ended neighbours and jobs elsewhere don't block.
	f := newCleanupFixture(t, scratchGlobal, dir)
	f.downloaded(t, "a")
	f.h.env.Store.CreateJob(ctx, store.Job{Name: "old", Cluster: "hpcone", RemoteDir: dir + "/old", State: store.StateFetched})
	f.h.env.Store.CreateJob(ctx, store.Job{Name: "other", Cluster: "hpcone", RemoteDir: "/scratch/me/labmill/other", State: store.StateRunning})
	if _, err := f.h.tools["cleanup_job"].Propose(jobIn(f.id)); err != nil {
		t.Errorf("ended neighbours blocked: %v", err)
	}
}

// A downloaded file whose cluster copy no longer has the verified SHA-256 is
// kept; nothing but exact files is removed.
func TestCleanupKeepsChangedDownloadedFile(t *testing.T) {
	dir := "/scratch/me/labmill/seg"
	f := newCleanupFixture(t, scratchGlobal, dir)
	f.sum[dir+"/a"] = sumOf("different")
	f.downloaded(t, "a", "b")
	tool := f.h.tools["cleanup_job"]
	p, _ := tool.Propose(jobIn(f.id))
	if _, err := tool.Run(context.Background(), jobIn(f.id), agent.Decision{Approved: true, Proposal: p}); err != nil {
		t.Fatal(err)
	}
	if len(f.rm) != 1 || strings.Join(f.rm[0], "|") != dir+"/b" {
		t.Errorf("rm = %v", f.rm)
	}
}

func TestCleanupChangedListIsRefused(t *testing.T) {
	f := newCleanupFixture(t, scratchGlobal, "/scratch/me/labmill/seg")
	f.downloaded(t, "a")
	tool := f.h.tools["cleanup_job"]
	p, _ := tool.Propose(jobIn(f.id))
	f.downloaded(t, "b") // another download after the user looked
	if _, err := tool.Run(context.Background(), jobIn(f.id), agent.Decision{Approved: true, Proposal: p}); err == nil || len(f.calls()) != 0 {
		t.Errorf("err = %v, calls %v", err, f.calls())
	}
}

func TestCleanupSymlinkedJobFolderAborts(t *testing.T) {
	dir := "/scratch/me/labmill/seg"
	f := newCleanupFixture(t, scratchGlobal, dir)
	f.downloaded(t, "a")
	f.findOut = dir + "\n"
	tool := f.h.tools["cleanup_job"]
	p, _ := tool.Propose(jobIn(f.id))
	_, err := tool.Run(context.Background(), jobIn(f.id), agent.Decision{Approved: true, Proposal: p})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") || len(f.rm) != 0 {
		t.Errorf("err = %v, rm %v", err, f.rm)
	}
}

// "Approve everything automatically" skips the question and runs the same
// commands.
func TestCleanupAutoApprove(t *testing.T) {
	f := newCleanupFixture(t, scratchGlobal, "/scratch/me/labmill/seg")
	f.downloaded(t, "a.txt")
	tool := f.h.tools["cleanup_job"]
	p, err := tool.Propose(jobIn(f.id))
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	auto := &agent.AutoApprover{Next: approverFunc(func(agent.Proposal) agent.Decision { asked++; return agent.Decision{} }), On: func() bool { return true }}
	d, err := auto.Approve(context.Background(), p)
	if err != nil || !d.Approved || !d.Auto || asked != 0 {
		t.Fatalf("decision %+v, %v, asked %d", d, err, asked)
	}
	d.Proposal = p
	if _, err := tool.Run(context.Background(), jobIn(f.id), d); err != nil || len(f.rm) != 1 {
		t.Errorf("err = %v, rm %v", err, f.rm)
	}
}

func TestCleanupNotForOtherOrNoClusterFolder(t *testing.T) {
	f := newCleanupFixture(t, scratchGlobal, "/scratch/me/labmill/seg")
	id, _ := f.h.env.Store.CreateJob(context.Background(), store.Job{Name: "r", Cluster: "second", RemoteDir: "/scratch/me/labmill/r", Downloaded: true})
	if _, err := f.h.tools["cleanup_job"].Propose(jobIn(id)); err == nil {
		t.Error("job on a cluster without a shared scratch accepted")
	}
	id, _ = f.h.env.Store.CreateJob(context.Background(), store.Job{Name: "n", Cluster: "hpcone"})
	if _, err := f.h.tools["cleanup_job"].Propose(jobIn(id)); err == nil {
		t.Error("job without folder accepted")
	}
}
