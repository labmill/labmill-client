//go:build !race

// This end-to-end test is excluded from -race builds: rclone v1.75.1's
// external-ssh connection pooling has an internal data race
// (backend/sftp/ssh_external.go sshSessionExternal.exited reads
// cmd.ProcessState while exec.Cmd.Wait writes it), which fires during real
// transfers and isn't ours to fix here (see docs/dev/plan.md, M8 follow-ups:
// report upstream). Run it with `go test ./internal/e2e/`.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/agent"
	"labmill-client/internal/agent/agenttest"
	"labmill-client/internal/app"
	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
	"labmill-client/internal/sshx/sshtest"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// TestMain doubles as `labmill ssh-exec` when rclone re-executes the test binary.
func TestMain(m *testing.M) {
	os.Unsetenv("SSH_AUTH_SOCK") // the developer's agent stays out of the tests
	if os.Getenv("LABMILL_E2E_SSHEXEC") == "1" {
		code, err := sshx.Exec(context.Background(), os.Getenv("LABMILL_E2E_ALIAS"), os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
			sshx.ExecOptions{Connect: sshx.ConnectOptions{SocketPath: os.Getenv("LABMILL_E2E_SOCK"), Spawn: func() error { return fmt.Errorf("no spawn") }}})
		if err != nil {
			fmt.Fprintln(os.Stderr, "ssh-exec:", err)
			if code == 0 {
				code = 255
			}
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// fakeCluster is the scripted scheduler state behind the test SSH server.
type fakeCluster struct {
	mu        sync.Mutex
	submitted []string // sbatch scripts received
	home      string   // the "cluster" home directory (also the SFTP root)
}

func (f *fakeCluster) handlers() map[string]sshtest.Handler {
	return map[string]sshtest.Handler{
		"printenv": func(args string, _ io.Reader, out, _ io.Writer) int { fmt.Fprintln(out, f.home); return 0 },
		"sinfo":    func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
		// submit_job creates the work dir before sbatch; the dir exists already.
		"mkdir": func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
		"sbatch": func(args string, in io.Reader, out, _ io.Writer) int {
			b, _ := io.ReadAll(in)
			f.mu.Lock()
			f.submitted = append(f.submitted, string(b))
			f.mu.Unlock()
			// The "job" writes a result file into its work dir.
			os.MkdirAll(filepath.Join(f.home, "labmill", "seg", "results"), 0o755)
			os.WriteFile(filepath.Join(f.home, "labmill", "seg", "results", "model.pt"), []byte("weights"), 0o644)
			fmt.Fprintln(out, "101")
			return 0
		},
		"squeue": func(string, io.Reader, io.Writer, io.Writer) int { return 0 }, // finished jobs drop out of squeue
		"sacct": func(args string, _ io.Reader, out, _ io.Writer) int {
			fmt.Fprintln(out, "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:05:00|gpu-01")
			return 0
		},
	}
}

type tool struct{ name, input string }

// scriptModel is a fake broker (agenttest) that answers one assistant turn
// per element: a list of tool calls, or (when empty) a final text reply.
func scriptModel(t *testing.T, turns [][]tool) agent.Model {
	b := agenttest.NewFunc(t, func(n int, _ agenttest.Request) (string, error) {
		if n >= len(turns) {
			return "", fmt.Errorf("script exhausted")
		}
		calls := turns[n]
		if len(calls) == 0 {
			return agenttest.TextReply("Done."), nil
		}
		var uses []agenttest.ToolUse
		for i, c := range calls {
			uses = append(uses, agenttest.ToolUse{ID: fmt.Sprintf("t%d_%d", n+1, i), Name: c.name, Input: c.input})
		}
		return agenttest.ToolReply(uses...), nil
	})
	return b.Client(t)
}

// approveAll approves everything and records what was shown.
type approveAll struct {
	mu   sync.Mutex
	seen []agent.Proposal
}

func (a *approveAll) Approve(_ context.Context, p agent.Proposal) (agent.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, p)
	return agent.Decision{Approved: true}, nil
}

type pw struct{}

func (pw) Ask(_ context.Context, q sshx.Question) (string, error) {
	if q.Kind == sshx.AskPassword {
		return "pw", nil
	}
	return "", nil
}
func (pw) Notify(string) {}

// harness is the shared E2E setup: a fake login host behind a real SSH
// server, the credential helper, a local data tree and a configured app.Env

type harness struct {
	cluster *fakeCluster
	st      *store.Store
	env     *app.Env
	home    string // local home with data/mri-2026
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// The fake cluster's home must not be under /tmp: Slurm refuses a
	// node-local work_dir. Use the repo's gitignored tmp/ instead.
	base, err := filepath.Abs("../../tmp/e2e")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	chome, err := os.MkdirTemp(base, "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(chome) })
	cluster := &fakeCluster{home: chome}
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, SFTPRoot: cluster.home, Commands: cluster.handlers()})

	// SSH config with an alias for the fake login host.
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	h, port, _ := net.SplitHostPort(srv.Addr)
	var portN int
	fmt.Sscanf(port, "%d", &portN)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "hpc-login", HostName: h, User: "alice", Port: portN}}, "linux"); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(p.SSHDir, "known_hosts")
	os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600)

	// Helper on a short socket path.
	dir, _ := os.MkdirTemp("", "ye")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	hctx, hcancel := context.WithCancel(context.Background())
	hdone := make(chan struct{})
	go func() {
		sshx.Serve(hctx, sshx.HelperOptions{SocketPath: sock, IdleTimeout: time.Minute, Paths: p, Login: sshx.LoginOptions{KnownHosts: kh, PushTimeout: 5 * time.Second}})
		close(hdone)
	}()
	t.Cleanup(func() { hcancel(); <-hdone })
	var clientMu sync.Mutex
	var client *ssh.Client
	getClient := func(ctx context.Context) (*ssh.Client, error) {
		clientMu.Lock()
		defer clientMu.Unlock()
		if client != nil {
			return client, nil
		}
		var err error
		deadline := time.Now().Add(5 * time.Second)
		for {
			c, e := sshx.Connect(ctx, "hpc-login", pw{}, sshx.ConnectOptions{SocketPath: sock, Spawn: func() error { return fmt.Errorf("no spawn") }})
			if e == nil {
				client = c
				return client, nil
			}
			err = e
			if time.Now().After(deadline) {
				return nil, err
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Setenv("LABMILL_E2E_SSHEXEC", "1")
	t.Setenv("LABMILL_E2E_SOCK", sock)
	t.Setenv("LABMILL_E2E_ALIAS", "hpc-login")

	// Local data.
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "data", "mri-2026"), 0o755)
	os.WriteFile(filepath.Join(home, "data", "mri-2026", "scan1.dcm"), []byte(strings.Repeat("x", 5000)), 0o644)
	os.WriteFile(filepath.Join(home, "data", "mri-2026", "labels.csv"), []byte("id,label\n1,a\n"), 0o644)

	st, err := store.Open(filepath.Join(t.TempDir(), "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{Clusters: []config.Cluster{
		{Name: "hpcone", Type: config.TypeSlurm, SSHHost: "hpc-login"},
	}}
	env := &app.Env{
		Config: cfg,
		Store:  st,
		Backend: func(ctx context.Context, c config.Cluster) (backend.Backend, error) {
			cl, err := getClient(ctx)
			if err != nil {
				return nil, err
			}
			r := &backend.SSHRunner{Client: cl}
			return &slurm.Slurm{Runner: r}, nil
		},
		RemoteHome: func(ctx context.Context, c config.Cluster) (string, error) { return cluster.home, nil },
		Upload:     transfer.Upload,
		Download:   transfer.Download,
		UploadS3:   transfer.UploadS3,
		DownloadS3: transfer.DownloadS3,
		SSHCommand: func(alias string) []string { return []string{os.Args[0]} },
		TransferCheck: func(ctx context.Context, alias string) error {
			_, err := getClient(ctx)
			return err
		},
		LocalRoots: []string{home},
		ResultsDir: filepath.Join(home, "labmill-results"),
	}
	return &harness{cluster: cluster, st: st, env: env, home: home}
}

// runScript drives the agent through the scripted turns with an approve-all
// approver and fails the test on a Send error or any failed tool call.
func (h *harness) runScript(t *testing.T, turns [][]tool, prompt string) *approveAll {
	t.Helper()
	reg := agent.NewRegistry()
	for _, tl := range app.Tools(h.env) {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	ap := &approveAll{}
	model := scriptModel(t, turns)
	a := &agent.Agent{Model: model, Tools: reg, Approver: ap, System: app.SystemPrompt(h.env), MaxTurns: 20}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var events []agent.Event
	a.OnEvent = func(e agent.Event) { events = append(events, e) }
	if _, err := a.Send(ctx, nil, prompt, agent.Typed); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, e := range events {
		if e.Kind == agent.EventToolResult && e.IsError {
			t.Errorf("tool %s failed: %s", e.Tool, e.Text)
		}
	}
	return ap
}

func TestEndToEndSlurm(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	h := newHarness(t)
	cluster, st, home := h.cluster, h.st, h.home
	ctx := context.Background()
	turns := [][]tool{
		{{"find_local_data", `{"query":"mri 2026"}`}},
		{{"stage_data", `{"root":` + jsonStr(home) + `,"paths":["data/mri-2026"],"job_name":"seg"}`}},
		{{"upload_data", `{"job_id":1,"cluster":"hpcone"}`}},
		{{"draft_job", `{"job_id":1,"cluster":"hpcone","spec":{"name":"seg","gpus":1,"command":"python train.py"}}`}},
		{{"submit_job", `{"job_id":1}`}},
		{{"job_status", `{"job_id":1}`}},
		{{"fetch_results", `{"job_id":1,"paths":["results"]}`}},
		{},
	}
	ap := h.runScript(t, turns, "train the segmentation model on my mri scans")

	// The data reached the cluster, the approved script was submitted, results came back.
	if b, err := os.ReadFile(filepath.Join(cluster.home, "labmill", "seg", "data", "mri-2026", "labels.csv")); err != nil || !strings.Contains(string(b), "id,label") {
		t.Errorf("uploaded labels.csv: %q %v", b, err)
	}
	if len(cluster.submitted) == 0 || !strings.Contains(cluster.submitted[0], "python train.py") || !strings.Contains(cluster.submitted[0], "--chdir="+filepath.Join(cluster.home, "labmill", "seg")) {
		t.Errorf("submitted = %q", cluster.submitted)
	}
	if b, err := os.ReadFile(filepath.Join(home, "labmill-results", "seg", "results", "model.pt")); err != nil || string(b) != "weights" {
		t.Errorf("fetched results: %q %v", b, err)
	}
	j, _ := st.Job(ctx, 1)
	if j.State != store.StateFetched || j.RemoteID != "101" {
		t.Errorf("job = %+v", j)
	}
	if len(ap.seen) != 3 { // upload, submit, fetch
		t.Errorf("approvals shown = %d", len(ap.seen))
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }
