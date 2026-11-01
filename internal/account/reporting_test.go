package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// fakeOutput is a backend that can read output files.
type fakeOutput struct {
	backend.Backend
	stdout, stderr []string
	calls          int
	head           int
	err            error
}

func (f *fakeOutput) Output(_ context.Context, _ string, head, _ int) ([]string, []string, error) {
	f.calls++
	f.head = head
	return f.stdout, f.stderr, f.err
}

var yes, no = true, false

func uploader(st *store.Store, p string) *Uploader {
	return &Uploader{Store: st, LinkPath: p, Institution: func(string) string { return "Example University" },
		Words: func(store.Job) []string { return []string{"someone", "login1"} }}
}

// secrets are texts that must never reach the server, whatever was collected.
var secrets = []string{"secret-project-name", "private-project", "mygroup", "hunter2", "sk-live", "someone", "login1",
	"ada@example.edu", "10.1.2.3", "/home/", "hf_supersecret"}

func noSecrets(t *testing.T, what, body string) {
	t.Helper()
	for _, bad := range secrets {
		if strings.Contains(body, bad) {
			t.Errorf("%s holds %q: %s", what, bad, body)
		}
	}
}

func collect(t *testing.T, st *store.Store, id int64, be *fakeOutput) {
	t.Helper()
	j, err := st.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := CollectOutput(context.Background(), st, be, j, NewRedactor(j.Name, "someone", "login1")); err != nil {
		t.Fatal(err)
	}
}

func TestFullReportIsRedacted(t *testing.T) {
	f := &fakeAPI{contributes: &yes}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	id := addJob(t, st, store.StateFailed, "OUT_OF_MEMORY")
	be := &fakeOutput{stdout: []string{"start secret-project-name on login1.example.edu", "[... 7 lines left out ...]", "done"},
		stderr: []string{"Traceback: /home/someone/run.py line 3", "mail ada@example.edu from 10.1.2.3", "token=hunter2"}}
	collect(t, st, id, be)
	collect(t, st, id, be) // once per run
	if be.calls != 1 || be.head != 100 {
		t.Errorf("output read %d times, head %d", be.calls, be.head)
	}
	st.UpdateJob(context.Background(), id, func(j *store.Job) error {
		j.Facts = store.Facts{RemoteID: "4242", ExitCode: 1, ElapsedSec: 100, CPUSec: 200, MaxRSSBytes: 3 << 30,
			AllocTRES: "billing=8,cpu=8,gres/gpu:a100=1,gres/gpu=1,mem=32G,node=1"}
		return nil
	})
	if n, err := uploader(st, p).Pass(context.Background()); err != nil || n != 1 {
		t.Fatalf("pass: %d %v", n, err)
	}
	body := f.posts[len(f.posts)-1]
	noSecrets(t, "report", body)
	var got JobIn
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Script, "python train.py") || !strings.Contains(got.Script, "#SBATCH --job-name <name>") && strings.Contains(got.Script, "#SBATCH") {
		t.Errorf("script = %q", got.Script)
	}
	if len(got.StdoutLines) != 3 || len(got.StderrLines) != 3 || got.StdoutLines[1] != "[... 7 lines left out ...]" {
		t.Errorf("output = %q / %q", got.StdoutLines, got.StderrLines)
	}
	var alloc, usage map[string]any
	json.Unmarshal(got.Alloc, &alloc)
	json.Unmarshal(got.Usage, &usage)
	if alloc["cpus"] != 8.0 || alloc["mem_mb"] != 32768.0 || alloc["gpus"] != 1.0 || alloc["gpu_type"] != "a100" || alloc["nodes"] != 1.0 {
		t.Errorf("alloc = %v", alloc)
	}
	if usage["cpu_eff"] != 0.25 || usage["max_rss_mb"] != 3072.0 || usage["source"] != "sacct" {
		t.Errorf("usage = %v", usage)
	}
}

func TestPrivateJobSendsOnlyFacts(t *testing.T) {
	f := &fakeAPI{contributes: &yes}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	id := addJob(t, st, store.StateFailed, "")
	st.AddEvent(context.Background(), id, EventPrivate, "")
	be := &fakeOutput{stdout: []string{"x"}}
	collect(t, st, id, be)
	if be.calls != 0 {
		t.Error("output of a private job was read")
	}
	pr := &Problems{Store: st, LinkPath: p, Version: "1.0", OS: "linux"}
	pr.Record(KindSbatch, "submit_job", "bigcluster", id, "sbatch: error: bad")
	if n, err := uploader(st, p).Pass(context.Background()); err != nil || n != 1 {
		t.Fatalf("pass: %d %v", n, err)
	}
	var got map[string]any
	json.Unmarshal([]byte(f.posts[0]), &got)
	for _, k := range []string{"script", "stdout_lines", "stderr_lines"} {
		if _, ok := got[k]; ok {
			t.Errorf("a private job sent %s", k)
		}
	}
	if got["shared"] != false {
		t.Errorf("shared = %v", got["shared"])
	}
	if len(f.events) != 0 {
		t.Errorf("a problem of a private job was sent: %v", f.events)
	}
}

func TestFullyPrivateAccountSendsOnlyFacts(t *testing.T) {
	for _, c := range []*bool{&no, nil} { // off, and an older server that does not say
		f := &fakeAPI{contributes: c}
		srv := httptest.NewServer(f.handler(t))
		st := newStore(t)
		p := link(t, srv.URL, time.Now().Add(-time.Minute))
		id := addJob(t, st, store.StateFailed, "")
		collect(t, st, id, &fakeOutput{stdout: []string{"some output"}})
		(&Problems{Store: st, LinkPath: p, Version: "1.0", OS: "linux"}).Record(KindModel, "", "", 0, "relay: 502")
		if n, err := uploader(st, p).Pass(context.Background()); err != nil || n != 1 {
			t.Fatalf("pass: %d %v", n, err)
		}
		if strings.Contains(f.posts[0], "script") || strings.Contains(f.posts[0], "some output") || len(f.events) != 0 {
			t.Errorf("contributes %v: sent %s, events %v", c, f.posts[0], f.events)
		}
		if due, _ := st.DueClientEvents(context.Background(), time.Now().Add(time.Hour), 10); len(due) != 0 {
			t.Error("the dropped problem report is still queued")
		}
		srv.Close()
	}
}

func TestStateChangeUpdatesTheReport(t *testing.T) {
	f := &fakeAPI{contributes: &yes}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, store.Job{Name: "job1", Cluster: "bigcluster", Backend: "slurm", Script: "#!/bin/bash\necho hi"})
	st.UpdateJob(ctx, id, func(j *store.Job) error { j.State, j.RemoteID = store.StateRunning, "77"; return nil })
	u := uploader(st, p)
	if n, _ := u.Pass(ctx); n != 1 {
		t.Fatalf("running: sent %d", n)
	}
	if n, _ := u.Pass(ctx); n != 0 {
		t.Fatal("an unchanged report was sent again")
	}
	st.UpdateJob(ctx, id, func(j *store.Job) error { j.State = store.StateCompleted; return nil })
	if n, _ := u.Pass(ctx); n != 1 {
		t.Fatalf("completed: sent %d", n)
	}
	var a, b JobIn
	json.Unmarshal([]byte(f.posts[0]), &a)
	json.Unmarshal([]byte(f.posts[1]), &b)
	if a.State != "RUNNING" || b.State != "COMPLETED" || a.ClientJobRef != b.ClientJobRef || b.EndedAt == nil {
		t.Errorf("reports: %s %s, refs %s %s", a.State, b.State, a.ClientJobRef, b.ClientJobRef)
	}
}

func TestProblemsQueuedOfflineAndRetried(t *testing.T) {
	f := &fakeAPI{contributes: &yes, eventFails: 1}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	now := time.Now()
	pr := &Problems{Store: st, LinkPath: p, Version: "1.2.3", OS: "darwin", Now: func() time.Time { return now },
		Words: func(store.Job) []string { return []string{"someone"} }}
	pr.Record(KindConnection, "cluster_info", "bigcluster", 0, "ssh: dial tcp 10.1.2.3:22: i/o timeout for someone@login.example.edu")
	pr.Record(KindCrash, "", "", 0, "panic: boom\ngoroutine 1:\nlabmill-client/internal/app.(*toolset).run(...)")
	u := uploader(st, p)
	u.Now = func() time.Time { return now }
	if _, err := u.Pass(context.Background()); err == nil {
		t.Fatal("a server error was not reported")
	}
	if len(f.events) != 0 {
		t.Fatal("sent despite the error")
	}
	u.Pass(context.Background())
	if len(f.events) != 0 {
		t.Fatal("retried before the backoff ended")
	}
	now = now.Add(Backoff(1) + time.Second)
	if _, err := u.Pass(context.Background()); err != nil || len(f.events) != 1 {
		t.Fatalf("retry: %v, %d batches", err, len(f.events))
	}
	noSecrets(t, "problem report", f.events[0])
	var body struct{ Events []ClientEvent }
	json.Unmarshal([]byte(f.events[0]), &body)
	if len(body.Events) != 2 || body.Events[0].Kind != KindConnection || body.Events[0].HPCSystemID == "" ||
		body.Events[0].SessionID != pr.Session() || body.Events[1].Kind != KindCrash || body.Events[0].ClientVersion != "1.2.3" {
		t.Errorf("events = %+v", body.Events)
	}
	if u.Pass(context.Background()); len(f.events) != 1 {
		t.Error("a sent problem report was sent again")
	}
}

func TestProblemsNotRecordedWhenLoggedOut(t *testing.T) {
	st := newStore(t)
	(&Problems{Store: st, LinkPath: t.TempDir() + "/none.json"}).Record(KindTool, "x", "", 0, "boom")
	if due, _ := st.DueClientEvents(context.Background(), time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Error("recorded while logged out")
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor("someone", "bigproj", "ab")
	for in, want := range map[string]string{
		"open /scratch/someone/bigproj/x.fa failed":        "open <path> failed",
		"user someone on login1.cluster.example.edu":       "user <name> on <host>",
		"export API_KEY=abc123 and --password hunter2":     "export <secret> and --<secret>",
		"from 192.168.1.20 mail a.b@example.org":           "from <ip> mail <email>",
		"job 12345678 of bigproj":                          "job <num> of <name>",
		"plain line with ab inside":                        "plain line with ab inside",
		"key lmt_abcdefghijklmnopqrstuvwxyz is the token!": "key <secret> is the <secret>",
	} {
		if got := r.Line(in); got != want {
			t.Errorf("Line(%q) = %q, want %q", in, got, want)
		}
	}
	s := r.Script("#!/bin/bash\n#SBATCH --job-name=bigproj-run\n#SBATCH --account=lab_someone\n#SBATCH --mem=4G\ncd /data/x/y")
	if strings.Contains(s, "bigproj") || strings.Contains(s, "lab_") || !strings.Contains(s, "--mem=4G") || !strings.Contains(s, "cd <path>") {
		t.Errorf("script = %q", s)
	}
	if m := r.Message(strings.Repeat("x\n", 3000)); len(m) > maxMessage {
		t.Errorf("message is %d bytes", len(m))
	}
	if l := r.Line(strings.Repeat("é", 400)); len(l) > maxLineLen || !strings.HasPrefix(l, "é") {
		t.Errorf("long line cut to %d bytes", len(l))
	}
}

func TestToolKind(t *testing.T) {
	for _, c := range [][3]string{
		{"submit_job", "sbatch: error: Batch job submission failed: Invalid partition", KindSbatch},
		{"submit_job", "ssh: handshake failed: ssh: unable to authenticate", KindAuth},
		{"cluster_info", "dial tcp 10.0.0.1:22: connect: connection refused", KindConnection},
		{"upload_data", "rclone: copy failed", KindTransfer},
		{"draft_job", "That job setup isn't valid", KindTool},
	} {
		if got := ToolKind(c[0], c[1]); got != c[2] {
			t.Errorf("ToolKind(%s, %q) = %s, want %s", c[0], c[1], got, c[2])
		}
	}
}

func TestCollectOutputError(t *testing.T) {
	st := newStore(t)
	id := addJob(t, st, store.StateCompleted, "")
	j, _ := st.Job(context.Background(), id)
	be := &fakeOutput{err: errors.New("tail: no such file")}
	if err := CollectOutput(context.Background(), st, be, j, NewRedactor()); err == nil {
		t.Error("error lost")
	}
	if be.head != 0 {
		t.Errorf("a completed job read %d first lines", be.head)
	}
	evs, _ := st.Events(context.Background(), id, 0)
	for _, e := range evs {
		if e.Kind == EventOutput {
			t.Error("an output event after a failed read")
		}
	}
}

func TestHugeOutputStaysBelowTheLimit(t *testing.T) {
	f := &fakeAPI{contributes: &yes}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	id := addJob(t, st, store.StateCompleted, "")
	marks := strings.Repeat("<", 299)
	lines := make([]string, 601)
	for i := range lines {
		lines[i] = marks
	}
	collect(t, st, id, &fakeOutput{stdout: lines, stderr: lines})
	if n, err := uploader(st, p).Pass(context.Background()); err != nil || n != 1 {
		t.Fatalf("pass: %d %v", n, err)
	}
	if b := f.posts[0]; len(b) > maxReportBytes || !strings.Contains(b, "stdout_lines") {
		t.Errorf("report is %d bytes", len(b))
	}
}
