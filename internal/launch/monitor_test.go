package launch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
)

// statusBackend answers Status from a map (remote ID -> status) and counts
// the calls; a missing ID is an error.
type statusBackend struct {
	backend.Backend // nil: only Status is used
	mu              sync.Mutex
	status          map[string]backend.Status
	calls           map[string]int
	block           chan struct{} // when set, Status waits for it or ctx
}

func (b *statusBackend) Status(ctx context.Context, id string) (backend.Status, error) {
	b.mu.Lock()
	b.calls[id]++
	st, ok := b.status[id]
	block := b.block
	b.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return backend.Status{}, ctx.Err()
		}
	}
	if !ok {
		return backend.Status{}, errors.New("no such job")
	}
	return st, nil
}

func (b *statusBackend) set(id string, st backend.Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status[id] = st
}

func (b *statusBackend) count(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[id]
}

type noticeLog struct {
	mu    sync.Mutex
	texts []string
	ch    chan string
}

func (n *noticeLog) notify(s string) {
	n.mu.Lock()
	n.texts = append(n.texts, s)
	n.mu.Unlock()
	if n.ch != nil {
		n.ch <- s
	}
}

func (n *noticeLog) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.texts...)
}

type monitorFixture struct {
	db      *store.Store
	be      *statusBackend
	notices *noticeLog
	m       *jobMonitor
	asked   map[string]int // backend lookups per cluster
	mu      sync.Mutex
}

func newMonitorFixture(t *testing.T) *monitorFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "labmill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &monitorFixture{
		db:      db,
		be:      &statusBackend{status: map[string]backend.Status{}, calls: map[string]int{}},
		notices: &noticeLog{},
		asked:   map[string]int{},
	}
	cfg := &config.Config{Clusters: []config.Cluster{
		{Name: "hpcone", Type: config.TypeSlurm, SSHHost: "hpclogin"},
		{Name: "offline", Type: config.TypeSlurm, SSHHost: "nowhere"},
	}}
	f.m = &jobMonitor{store: db, cfg: cfg, notify: f.notices.notify, timeout: time.Second,
		backend: func(_ context.Context, c config.Cluster) (backend.Backend, bool) {
			f.mu.Lock()
			f.asked[c.Name]++
			f.mu.Unlock()
			if c.Name == "offline" {
				return nil, false
			}
			return f.be, true
		}}
	return f
}

func (f *monitorFixture) job(t *testing.T, j store.Job) int64 {
	t.Helper()
	id, err := f.db.CreateJob(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *monitorFixture) state(t *testing.T, id int64) store.Job {
	t.Helper()
	j, err := f.db.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestMonitorUpdatesStatesAndNotifiesOnce(t *testing.T) {
	f := newMonitorFixture(t)
	ctx := context.Background()
	run := f.job(t, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "100", State: store.StatePending, RemoteDir: "/home/u/labmill/train"})
	fail := f.job(t, store.Job{Name: "bad", Cluster: "hpcone", Backend: "slurm", RemoteID: "101", State: store.StateRunning})
	draft := f.job(t, store.Job{Name: "draft", Cluster: "hpcone", Backend: "slurm", State: store.StateDraft})
	done := f.job(t, store.Job{Name: "old", Cluster: "hpcone", Backend: "slurm", RemoteID: "99", State: store.StateCompleted})
	noRemote := f.job(t, store.Job{Name: "odd", Cluster: "hpcone", Backend: "slurm", State: store.StateSubmitted})

	f.be.set("100", backend.Status{State: store.StateRunning})
	f.be.set("101", backend.Status{State: store.StateFailed, Reason: "TIMEOUT"})
	f.m.check(ctx)

	if j := f.state(t, run); j.State != store.StateRunning {
		t.Errorf("job %d state = %s, want running", run, j.State)
	}
	if j := f.state(t, fail); j.State != store.StateFailed || j.Message != "TIMEOUT" {
		t.Errorf("failed job = %+v", j)
	}
	if f.be.count("99") != 0 || f.state(t, done).State != store.StateCompleted {
		t.Error("a completed job was checked again")
	}
	if f.state(t, draft).State != store.StateDraft || f.state(t, noRemote).State != store.StateSubmitted {
		t.Error("an unsubmitted job changed")
	}
	got := f.notices.all()
	if len(got) != 1 || !strings.Contains(got[0], "Job #2 (bad) on hpcone failed (TIMEOUT). Ask me to look at the logs.") {
		t.Fatalf("notices = %q", got)
	}

	// The running job finishes: one notice; later checks don't repeat it and
	// don't ask about it any more.
	f.be.set("100", backend.Status{State: store.StateCompleted})
	f.m.check(ctx)
	f.m.check(ctx)
	got = f.notices.all()
	if len(got) != 2 || got[1] != "Job #1 (train) on hpcone finished. Ask me to fetch the results." {
		t.Fatalf("notices = %q", got)
	}
	if n := f.be.count("100"); n != 2 {
		t.Errorf("Status calls for the finished job = %d, want 2 (one per check until it finished)", n)
	}
	if n := f.be.count("101"); n != 1 {
		t.Errorf("Status calls for the failed job = %d, want 1", n)
	}
}

func TestMonitorSkipsJobChangedMeanwhile(t *testing.T) {
	f := newMonitorFixture(t)
	ctx := context.Background()
	id := f.job(t, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "7", State: store.StateRunning})
	j := f.state(t, id)
	// job_status (or another Labmill) saved "completed" after the monitor read
	// the job; the monitor's late result must not notify or overwrite it.
	if err := f.db.UpdateJob(ctx, id, func(j *store.Job) error { j.State = store.StateFetched; return nil }); err != nil {
		t.Fatal(err)
	}
	f.m.apply(ctx, j, backend.Status{State: store.StateCompleted})
	if got := f.notices.all(); len(got) != 0 {
		t.Errorf("notices = %q", got)
	}
	if s := f.state(t, id).State; s != store.StateFetched {
		t.Errorf("state = %s", s)
	}
}

func TestMonitorSkipsClustersWithoutConnection(t *testing.T) {
	f := newMonitorFixture(t)
	a := f.job(t, store.Job{Name: "a", Cluster: "offline", Backend: "slurm", RemoteID: "1", State: store.StateRunning})
	f.job(t, store.Job{Name: "b", Cluster: "offline", Backend: "slurm", RemoteID: "2", State: store.StateRunning})
	f.job(t, store.Job{Name: "c", Cluster: "gone", Backend: "slurm", RemoteID: "3", State: store.StateRunning})
	f.m.check(context.Background())
	if f.state(t, a).State != store.StateRunning || len(f.notices.all()) != 0 {
		t.Error("an unreachable cluster's job changed")
	}
	if f.asked["offline"] != 1 {
		t.Errorf("backend lookups for offline = %d, want 1 per check", f.asked["offline"])
	}
}

func TestMonitorStatusTimeout(t *testing.T) {
	f := newMonitorFixture(t)
	f.m.timeout = 50 * time.Millisecond
	f.be.block = make(chan struct{}) // never answers
	f.job(t, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "1", State: store.StateRunning})
	f.job(t, store.Job{Name: "b", Cluster: "hpcone", Backend: "slurm", RemoteID: "2", State: store.StateRunning})
	start := time.Now()
	f.m.check(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("check took %v; each call should time out after 50ms", d)
	}
	if f.be.count("1") != 1 || f.be.count("2") != 1 {
		t.Errorf("calls = %d, %d; want one each", f.be.count("1"), f.be.count("2"))
	}
}

// fakeTicks is a clock for jobMonitor.start: each pause returns a channel
// the test fires by hand.
type fakeTicks struct {
	pauses chan chan time.Time
}

func (c *fakeTicks) after(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.pauses <- ch
	return ch
}

func TestMonitorRunAndClose(t *testing.T) {
	f := newMonitorFixture(t)
	f.notices.ch = make(chan string, 4)
	f.job(t, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "5", State: store.StateRunning})
	f.be.set("5", backend.Status{State: store.StateCompleted})
	clock := &fakeTicks{pauses: make(chan chan time.Time, 4)}
	f.m.start(clock.after)

	// Nothing happens before the first pause ends.
	tick := <-clock.pauses
	if f.be.count("5") != 0 {
		t.Fatal("checked before the first tick")
	}
	tick <- time.Now()
	select {
	case n := <-f.notices.ch:
		if !strings.Contains(n, "finished") {
			t.Errorf("notice = %q", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice after a tick")
	}
	// The next pause starts only after the check ended (no overlap).
	<-clock.pauses

	closed := make(chan struct{})
	go func() { f.m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the monitor")
	}
	f.m.Close() // idempotent
	if f.be.count("5") != 1 {
		t.Errorf("Status calls = %d, want 1", f.be.count("5"))
	}
}

func TestMonitorCloseCancelsRunningCheck(t *testing.T) {
	f := newMonitorFixture(t)
	f.m.timeout = time.Minute
	f.be.block = make(chan struct{})
	f.job(t, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "1", State: store.StateRunning})
	clock := &fakeTicks{pauses: make(chan chan time.Time, 4)}
	f.m.start(clock.after)
	(<-clock.pauses) <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for f.be.count("1") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { f.m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for a blocked Status call")
	}
}

func TestFinishedNotice(t *testing.T) {
	j := store.Job{ID: 12, Name: "seg", Cluster: "hpcone"}
	cases := []struct {
		st   backend.Status
		dir  string
		want string
	}{
		{backend.Status{State: store.StateCompleted}, "/r", "Job #12 (seg) on hpcone finished. Ask me to fetch the results."},
		{backend.Status{State: store.StateCompleted}, "", "Job #12 (seg) on hpcone finished. Ask me to show its log."},
		{backend.Status{State: store.StateFailed, Reason: "TIMEOUT"}, "", "Job #12 (seg) on hpcone failed (TIMEOUT). Ask me to look at the logs."},
		{backend.Status{State: store.StateFailed}, "", "Job #12 (seg) on hpcone failed. Ask me to look at the logs."},
		{backend.Status{State: store.StateCanceled}, "", "Job #12 (seg) on hpcone was canceled."},
		{backend.Status{State: store.StateRunning}, "", ""},
	}
	for _, c := range cases {
		j.RemoteDir = c.dir
		if got := finishedNotice(j, c.st); got != c.want {
			t.Errorf("%s: %q, want %q", c.st.State, got, c.want)
		}
	}
}

// promptUI fails the test whenever anything would ask the user.
type promptUI struct {
	fakeUI
	t *testing.T
}

func (p *promptUI) Ask(context.Context, sshx.Question) (string, error) {
	p.t.Error("the monitor asked a login question")
	return "", errors.New("no")
}
func (p *promptUI) Approve(context.Context, agent.Proposal) (agent.Decision, error) {
	p.t.Error("the monitor asked for an approval")
	return agent.Decision{}, errors.New("no")
}

// TestBackendNoLoginNeverLogsIn: with no open login, the background lookup
// gives up without the interactive connect and without asking anything.
func TestBackendNoLoginNeverLogsIn(t *testing.T) {
	oldConnect, oldCached := sshConnect, connectCached
	t.Cleanup(func() {
		sshConnect, connectCached = oldConnect, oldCached
	})
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		t.Error("the interactive connect was used")
		return nil, errors.New("no")
	}
	var cachedAsked []string
	connectCached = func(_ context.Context, alias string) (*ssh.Client, error) {
		cachedAsked = append(cachedAsked, alias)
		return nil, sshx.ErrNotConnected
	}

	cc := newConnCache(&promptUI{t: t})
	slurmC := config.Cluster{Name: "hpcone", Type: config.TypeSlurm, SSHHost: "hpclogin"}
	if be, ok := cc.backendNoLogin(context.Background(), slurmC); ok || be != nil {
		t.Errorf("slurm: got a backend without a login")
	}
	if len(cachedAsked) != 1 || cachedAsked[0] != "hpclogin" {
		t.Errorf("cached-only connects = %q", cachedAsked)
	}

	// A busy cache (a login dialog is open) is skipped without waiting.
	cc.mu.Lock()
	done := make(chan bool)
	go func() { _, ok := cc.backendNoLogin(context.Background(), slurmC); done <- ok }()
	select {
	case ok := <-done:
		if ok {
			t.Error("got a backend while the cache was busy")
		}
	case <-time.After(2 * time.Second):
		t.Error("backendNoLogin waited for the busy cache")
	}
	cc.mu.Unlock()

	// After closeAll nothing is attempted.
	cc.closeAll()
	cachedAsked = nil
	if _, ok := cc.backendNoLogin(context.Background(), slurmC); ok || len(cachedAsked) != 0 {
		t.Errorf("after close: ok=%v asked=%q", ok, cachedAsked)
	}
}

// monitorStepUI records the agent events that reach the UI.
type monitorStepUI struct {
	promptUI
	mu     sync.Mutex
	events []agent.Event
}

func (s *monitorStepUI) Event(e agent.Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

// TestBackgroundMonitorEmitsNoSteps: the monitor's lookups never show a
// "Connecting to …" step (they never log in), so the busy row of a running
// turn is not disturbed by a background check.
func TestBackgroundMonitorEmitsNoSteps(t *testing.T) {
	oldConnect, oldCached := sshConnect, connectCached
	t.Cleanup(func() {
		sshConnect, connectCached = oldConnect, oldCached
	})
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		t.Error("the interactive connect was used")
		return nil, errors.New("no")
	}
	connectCached = func(context.Context, string) (*ssh.Client, error) { return nil, sshx.ErrNotConnected }

	ui := &monitorStepUI{promptUI: promptUI{t: t}}
	cc := newConnCache(ui)
	cc.steps = newStepTracker(ui)
	cc.backendNoLogin(context.Background(), config.Cluster{Name: "hpcone", Type: config.TypeSlurm, SSHHost: "hpclogin"})
	ui.mu.Lock()
	defer ui.mu.Unlock()
	for _, e := range ui.events {
		if e.Kind == agent.EventStep {
			t.Errorf("the background lookup emitted a step: %q", e.Text)
		}
	}
}

// A job the scheduler keeps failing to report is marked unknown after 5
// failed checks and then checked only every 10 minutes; a good answer ends it.
func TestMonitorBacksOffUnknownJobs(t *testing.T) {
	f := newMonitorFixture(t)
	clock := time.Unix(1_000_000, 0)
	f.m.now = func() time.Time { return clock }
	id := f.job(t, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "9", State: store.StateRunning, Message: "ok"})
	for i := 0; i < monitorMaxFailures; i++ {
		f.m.check(context.Background())
		clock = clock.Add(monitorInterval)
	}
	if n := f.be.count("9"); n != monitorMaxFailures {
		t.Fatalf("calls = %d, want %d", n, monitorMaxFailures)
	}
	j := f.state(t, id)
	if j.Message != unknownMessage || j.State != store.StateRunning {
		t.Fatalf("job = %q / %q, want state unchanged and the unknown message", j.State, j.Message)
	}
	// Backing off: nothing for 10 minutes.
	for i := 0; i < 5; i++ {
		f.m.check(context.Background())
		clock = clock.Add(monitorInterval)
	}
	if n := f.be.count("9"); n != monitorMaxFailures {
		t.Errorf("calls during back-off = %d, want %d", n, monitorMaxFailures)
	}
	clock = clock.Add(monitorBackoff)
	f.m.check(context.Background())
	if n := f.be.count("9"); n != monitorMaxFailures+1 {
		t.Errorf("calls after back-off = %d, want %d", n, monitorMaxFailures+1)
	}
	// The scheduler knows it again: the state is saved and the counter resets.
	f.be.set("9", backend.Status{State: store.StateCompleted})
	clock = clock.Add(monitorBackoff)
	f.m.check(context.Background())
	if got := f.state(t, id); got.State != store.StateCompleted || got.Message == unknownMessage {
		t.Errorf("job after recovery = %q / %q", got.State, got.Message)
	}
	if f.m.failing[id] != nil {
		t.Error("failure counter not reset")
	}
}

// Failure counters of jobs that are no longer monitored are dropped.
func TestMonitorPrunesFailingForFinishedJobs(t *testing.T) {
	f := newMonitorFixture(t)
	id := f.job(t, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "9", State: store.StateRunning, Message: "ok"})
	f.m.check(context.Background())
	if f.m.failing[id] == nil {
		t.Fatal("failure not counted")
	}
	if err := f.m.store.UpdateJob(context.Background(), id, func(j *store.Job) error {
		j.State = store.StateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.m.check(context.Background())
	if len(f.m.failing) != 0 {
		t.Errorf("failing = %v, want empty", f.m.failing)
	}
}

func TestRetryNotice(t *testing.T) {
	j := store.Job{ID: 7, Name: "fortran1", Cluster: "second"}
	st := backend.Status{State: store.StateRunning, Reason: "failed 2 times (exit 127: gfortran: command not found), retrying"}
	if got := retryNotice(j, "", st); !strings.Contains(got, "Job #7 (fortran1) on second: its container failed 2 times (exit 127") {
		t.Errorf("notice = %q", got)
	}
	if got := retryNotice(j, "failed once, retrying", st); got != "" {
		t.Errorf("repeated notice = %q", got)
	}
	if got := retryNotice(j, "", backend.Status{State: store.StateRunning}); got != "" {
		t.Errorf("notice without retry = %q", got)
	}
}
