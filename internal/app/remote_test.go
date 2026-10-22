package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/skill"
	"labmill-client/internal/sshx/sshtest"
)

// fakeRunner records argv and answers with fn (default: exit 0, no output).
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(ctx context.Context, argv []string) (backend.Result, error)
	// allowStdin accepts a script on stdin and records it in stdins.
	allowStdin bool
	stdins     []string
}

func (f *fakeRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (backend.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), argv...))
	f.mu.Unlock()
	if stdin != nil {
		if !f.allowStdin {
			return backend.Result{}, errors.New("remote_command must not pass stdin")
		}
		b, _ := io.ReadAll(stdin)
		f.mu.Lock()
		f.stdins = append(f.stdins, string(b))
		f.mu.Unlock()
	}
	if f.fn != nil {
		return f.fn(ctx, argv)
	}
	return backend.Result{}, nil
}

// shellRunner runs the command line the way sshd does (`sh -c <Quote(argv)>`)
// on this machine, in home with HOME=home, so the quoting and the guard
// script are exercised for real.
type shellRunner struct{ home string }

func (s shellRunner) Run(ctx context.Context, argv []string, _ io.Reader) (backend.Result, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", backend.Quote(argv))
	cmd.Dir = s.home
	cmd.Env = []string{"HOME=" + s.home, "PATH=" + os.Getenv("PATH")}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := backend.Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode, err = ee.ExitCode(), nil
	}
	return res, err
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not available", n)
		}
	}
}

func remoteHarness(t *testing.T, r backend.Runner) *harness {
	t.Helper()
	h := newHarness(t)
	h.env.LoginRunner = func(_ context.Context, c config.Cluster) (backend.Runner, error) {
		if c.SSHHost == "" {
			return nil, errors.New("no host")
		}
		return r, nil
	}
	h.env.HostName = func(alias string) string {
		if alias == "hpclogin" {
			return "login.example.edu"
		}
		return ""
	}
	return h
}

func TestClassifyRemote(t *testing.T) {
	ro, gt := remoteReadOnly, remoteGated
	cases := []struct {
		argv []string
		want remoteClass
	}{
		{[]string{"squeue", "--me"}, ro},
		{[]string{"squeue", "-u", "x; rm -rf ~"}, ro}, // just an argument to squeue
		{[]string{"squeue; rm -rf ~"}, gt},            // one word: not a known command
		{[]string{"squeue", "-i", "5"}, gt},
		{[]string{"squeue", "--iterate=5"}, gt},
		{[]string{"squeue", "--it=5"}, gt}, // GNU abbreviation
		{[]string{"sinfo", "-s"}, ro},
		{[]string{"sacct", "-j", "123", "--format=JobID,State"}, ro},
		{[]string{"sshare"}, ro},
		{[]string{"seff", "123"}, ro},
		{[]string{"scontrol", "show", "job", "123"}, ro},
		{[]string{"scontrol", "-o", "show", "node", "n1"}, ro},
		{[]string{"scontrol", "show", "partition"}, ro},
		{[]string{"scontrol", "update", "JobId=1", "TimeLimit=5"}, gt},
		{[]string{"scontrol", "show", "job", "1", "2"}, gt},
		{[]string{"scontrol", "hold", "1"}, gt},
		{[]string{"sacctmgr", "show", "assoc", "user=alice"}, ro},
		{[]string{"sacctmgr", "-n", "list", "account"}, ro},
		{[]string{"sacctmgr", "-i", "delete", "user", "bob"}, gt},
		{[]string{"sacctmgr", "modify", "user", "bob"}, gt},
		{[]string{"module", "avail", "cuda"}, ro},
		{[]string{"module", "-t", "spider"}, ro},
		{[]string{"module", "spider", "gcc/12.2"}, ro},
		{[]string{"module", "-t", "avail"}, ro},
		{[]string{"module", "keyword", "aligner"}, ro},
		{[]string{"module", "-r", "spider", ".*torch.*"}, ro},
		{[]string{"module", "--terse", "avail"}, ro},
		{[]string{"module", "-t", "list"}, ro},
		{[]string{"module", "--width=80", "avail"}, ro},
		{[]string{"module", "-w", "avail", "save"}, gt},
		{[]string{"module", "--width", "avail", "initadd", "x"}, gt},
		{[]string{"module", "-r", "show", ".*torch.*"}, gt},
		{[]string{"module", "avail", "./x"}, gt},
		{[]string{"module", "-r", "load", "foo"}, gt},
		{[]string{"module", "load", "cuda"}, gt},
		{[]string{"command", "-v", "samtools"}, ro},
		{[]string{"command", "-V", "gcc", "python3"}, ro},
		{[]string{"command", "samtools"}, gt},
		{[]string{"command", "-p", "ls"}, gt},
		{[]string{"command", "-v"}, gt},
		{[]string{"command", "-v", "-p"}, gt},
		{[]string{"module", "avail", "--debug"}, gt},
		{[]string{"ls"}, ro},
		{[]string{"ls", "-la", "~"}, ro},
		{[]string{"ls", "labmill/job1"}, ro},
		// no path limits: any path the account can read runs at once
		{[]string{"ls", "../.."}, ro},
		{[]string{"ls", "labmill/../.."}, ro},
		{[]string{"ls", "/etc"}, ro},
		{[]string{"ls", "~bob"}, ro},
		{[]string{"ls", "/storage/lab/labmill/job1"}, ro},
		{[]string{"ls", "/storage/lab/labmill/job10"}, ro},
		{[]string{"ls", "/storage/lab/labmill/job1/../job2"}, ro},
		{[]string{"cat", "slurm-1.out"}, ro},
		{[]string{"head", "-n", "5", "x.log"}, ro},
		{[]string{"head", "-n5", "x.log"}, ro},
		{[]string{"head", "-5", "x.log"}, ro},
		{[]string{"tail", "-n", "20", "x.log"}, ro},
		{[]string{"tail", "-f", "x.log"}, gt},
		{[]string{"tail", "-F", "x.log"}, gt},
		{[]string{"tail", "-5f", "x.log"}, gt},
		{[]string{"tail", "--follow=name", "x.log"}, gt},
		{[]string{"tail", "--fo", "x.log"}, gt},
		{[]string{"tail", "--pid=1", "x.log"}, gt},
		{[]string{"wc", "-l", "x.log"}, ro},
		{[]string{"wc", "--files0-from=list"}, gt},
		{[]string{"du", "-sh", "labmill"}, ro},
		{[]string{"du", "-d", "1", "labmill"}, ro},
		{[]string{"du", "-X", "pat", "labmill"}, gt},
		{[]string{"df", "-h", "."}, ro},
		{[]string{"df", "-hT", "-x", "tmpfs", "-x", "devtmpfs"}, ro},
		{[]string{"findmnt", "-t", "lustre,gpfs,beegfs,nfs,nfs4,cifs"}, ro},
		{[]string{"findmnt", "-lo", "TARGET,SOURCE,FSTYPE,SIZE"}, ro},
		{[]string{"findmnt", "--poll"}, gt},
		{[]string{"findmnt", "-F", "/etc/x"}, gt},
		{[]string{"lsblk"}, ro},
		{[]string{"mount"}, ro},
		{[]string{"mount", "/dev/sdb", "/mnt"}, gt},
		{[]string{"stat", "-c", "%s", "x"}, ro},
		{[]string{"file", "x"}, ro},
		{[]string{"file", "-C"}, gt},
		{[]string{"file", "-f", "names"}, gt},
		{[]string{"cat", "--", "-weird"}, ro},
		{[]string{"/usr/bin/ls"}, gt},
		{[]string{"rm", "-rf", "labmill"}, gt},
		{[]string{"sbatch", "job.sh"}, gt},
		{[]string{"python3", "train.py"}, gt},
		{[]string{"bash", "-c", "squeue"}, gt},
		// hidden files are not special to the classifier; a read of a sign-in
		// secret asks first, decided in remotePlan (TestRemoteCommandSecretReadsAsk)
		{[]string{"cat", ".bashrc"}, ro},
		{[]string{"cat", "~/.bashrc"}, ro},
		{[]string{"cat", "~/.ssh/config"}, ro},
		{[]string{"ls", ".config"}, ro},
		{[]string{"cat", "results/.hidden/x"}, ro},
		{[]string{"cat", "my-secret-notes.txt"}, ro},
		{[]string{"cat", "results/out.txt"}, ro},
		// not on the read-only list: these need approval, never a refusal
		{[]string{"vim", "job.sh"}, gt},
		{[]string{"top"}, gt},
		{[]string{"srun", "--pty", "bash"}, gt},
		{[]string{"cp", "x", ".aws/credentials"}, gt},
		{[]string{"bash", "-c", "cat ~/.netrc"}, gt},
		// module names only
		{[]string{"module", "show", "gcc/12.2"}, ro},
		{[]string{"module", "show", "/etc/modulefiles/x"}, gt},
		{[]string{"module", "whatis", "../x"}, gt},
		{[]string{"module", "display", ".hidden"}, gt},
		{[]string{"module", "show", "~/mods/x"}, gt},
		{[]string{"module", "show", "a/../../b"}, gt},
		// sacct -f/--file
		{[]string{"sacct", "-f", "/tmp/acct"}, gt},
		{[]string{"sacct", "--file=/tmp/acct"}, gt},
		{[]string{"sacct", "--fi", "/tmp/acct"}, gt},
		{[]string{"sacct", "--format=JobID,State"}, ro},
		// long options with a value in the next word
		{[]string{"head", "--lines", "5", "x.log"}, ro},
		{[]string{"head", "--lines=5", "x.log"}, ro},
		{[]string{"head", "--line", "5", "x.log"}, gt},
		{[]string{"head", "--line=5", "x.log"}, ro},
		{[]string{"du", "--exclude=*.tmp", "labmill"}, ro},
		{[]string{"du", "--exclude", "*.tmp", "labmill"}, ro},
		{[]string{"du", "--exclude-from=list", "labmill"}, gt},
		{[]string{"du", "--excl=x", "labmill"}, gt},
		{[]string{"ls", "--sort", "size", "labmill"}, ro},
	}
	for _, c := range cases {
		got, why, _ := classifyRemote(c.argv)
		if got != c.want {
			t.Errorf("%q: class = %v (why %q), want %v", c.argv, got, why, c.want)
		}
		if got == remoteGated && why == "" {
			t.Errorf("%q: gated without a reason", c.argv)
		}
	}
}

func TestClassifyRemoteWire(t *testing.T) {
	// A value of a value flag is not a path, ~ maps to the home folder; the
	// command runs as given, with no guard script around it.
	_, _, w := classifyRemote([]string{"tail", "-n", "5", "~/x.log"})
	want := []string{"tail", "-n", "5", "./x.log"}
	if strings.Join(w, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("wire = %q\nwant %q", w, want)
	}
	// Other paths go through unchanged (the cluster's permissions decide).
	_, _, w = classifyRemote([]string{"cat", "/etc/passwd", "../x", ".ssh/config"})
	want = []string{"cat", "/etc/passwd", "../x", ".ssh/config"}
	if strings.Join(w, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("wire = %q\nwant %q", w, want)
	}
	// A full long option takes the next word as its value.
	_, _, w = classifyRemote([]string{"head", "--lines", "5", "x.log"})
	want = []string{"head", "--lines", "5", "x.log"}
	if strings.Join(w, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("wire = %q\nwant %q", w, want)
	}
	_, _, w = classifyRemote([]string{"module", "avail", "cuda"})
	if strings.Join(w, " ") != "bash -lc "+remoteModuleScript+" labmill-module avail cuda" {
		t.Errorf("module wire = %q", w)
	}
	_, _, w = classifyRemote([]string{"module", "-r", "spider", ".*torch.*"})
	if strings.Join(w, " ") != "bash -lc "+remoteModuleScript+" labmill-module -r spider .*torch.*" {
		t.Errorf("module -r wire = %q", w)
	}
	_, _, w = classifyRemote([]string{"module", "load", "foo"})
	if strings.Join(w, " ") != "bash -lc "+remoteModuleScript+" labmill-module load foo" {
		t.Errorf("gated module wire = %q", w)
	}
	_, _, w = classifyRemote([]string{"ml", "foo"})
	if strings.Join(w, " ") != "bash -lc "+remoteMlScript+" labmill-ml foo" {
		t.Errorf("gated ml wire = %q", w)
	}
	_, _, w = classifyRemote([]string{"command", "-V", "gcc", "python3"})
	if strings.Join(w, " ") != `bash -c command "$@" labmill-command -V -- gcc python3` {
		t.Errorf("command wire = %q", w)
	}
}

// Only malformed requests are refused; nothing about what runs on the cluster.
func TestRemoteCommandRefusals(t *testing.T) {
	fr := &fakeRunner{}
	h := remoteHarness(t, fr)
	cases := []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"cluster": "hpcone", "argv": []string{"ls", "a\nb"}}, "put multi-line shell code in `script` instead"},
		{map[string]any{"cluster": "hpcone", "argv": []string{"ls", "a\x00b"}}, "control character"},
		{map[string]any{"cluster": "hpcone", "argv": []string{"ls", "a‮b"}}, "control character"},
		{map[string]any{"cluster": "hpcone", "argv": []string{}}, "must name a command"},
		{map[string]any{"cluster": "hpcone", "argv": []string{""}}, "must name a command"},
		{map[string]any{"cluster": "hpcone", "argv": make([]string, 65)}, "at most 64"},
		{map[string]any{"cluster": "hpcone", "argv": []string{"echo", strings.Repeat("x", 4097)}}, "longer than 4096"},
		{map[string]any{"cluster": "hpcone", "argv": []string{"squeue"}, "timeout_seconds": 601}, "between 1 and 600"},
		{map[string]any{"cluster": "nope", "argv": []string{"squeue"}}, "no cluster called"},
	}
	for _, c := range cases {
		if argv, ok := c.in["argv"].([]string); ok && len(argv) == 65 {
			for i := range argv {
				argv[i] = "x"
			}
		}
		b, _ := json.Marshal(c.in)
		tool := h.tools["remote_command"]
		_, err := tool.Propose(b)
		if err == nil || errors.Is(err, agent.ErrNoApproval) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: Propose err = %v, want %q", c.in["argv"], err, c.want)
		}
		// Even an "approved" call must not run.
		if _, err := tool.Run(context.Background(), b, agent.Decision{Approved: true}); err == nil {
			t.Errorf("%v: Run succeeded", c.in["argv"])
		}
	}
	if len(fr.calls) != 0 {
		t.Errorf("refused commands ran: %q", fr.calls)
	}
}

func TestRemoteCommandApproval(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "Submitted batch job 42\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	in := json.RawMessage(`{"cluster":"hpcone","argv":["sbatch","job.sh"]}`)
	p, err := tool.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := "Run on hpcone: sbatch job.sh"
	wantDetails := "Command: sbatch job.sh\n" +
		"Where: the login node of hpcone, hpclogin (login.example.edu)\n" +
		"Runs exactly as: timeout -k 5 60 sbatch job.sh\n" +
		"It is stopped after 60 seconds. It runs without a terminal and without input, so it can't ask questions.\n\n" +
		"This command can change files on the cluster. Approve it only if you want it to run.\n" +
		"Why Labmill asks: it is not on Labmill's list of read-only commands."
	if p.Tool != "remote_command" || p.Summary != wantSummary || p.Details != wantDetails || p.Editable {
		t.Errorf("proposal =\n%q\n%q\nwant\n%q\n%q", p.Summary, p.Details, wantSummary, wantDetails)
	}
	// Without approval a gated command never runs.
	if _, err := tool.Run(context.Background(), in, agent.Decision{}); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Errorf("unapproved Run err = %v", err)
	}
	// A tampered approval is refused.
	bad := p
	bad.Details = strings.Replace(bad.Details, "sbatch job.sh", "sbatch other.sh", 1)
	if _, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: bad}); err == nil {
		t.Error("tampered approval ran")
	}
	if len(fr.calls) != 0 {
		t.Fatalf("ran before approval: %q", fr.calls)
	}
	out, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.calls) != 1 || backend.Quote(fr.calls[0]) != "timeout -k 5 60 sbatch job.sh" {
		t.Errorf("ran %q", fr.calls)
	}
	want := remoteSubmitHint + "\nRan on hpcone (hpclogin): sbatch job.sh\nExit code: 0\nOutput:\nSubmitted batch job 42"
	if out != want {
		t.Errorf("result = %q, want %q", out, want)
	}

	// A custom timeout is shown exactly.
	p, err = tool.Propose(json.RawMessage(`{"cluster":"hpcone","argv":["scancel","12"],"timeout_seconds":120}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Details, "Runs exactly as: timeout -k 5 120 scancel 12\n") ||
		!strings.Contains(p.Details, "stopped after 120 seconds") || !strings.Contains(p.Details, remoteChangeNote) {
		t.Errorf("scancel details:\n%s", p.Details)
	}
	// A long command is shortened in the summary only.
	long := strings.Repeat("a", 300)
	p, err = tool.Propose(json.RawMessage(`{"cluster":"hpcone","argv":["echo","` + long + `"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.Summary, "…") || len([]rune(p.Summary)) > len("Run on hpcone: ")+remoteMaxSummary+1 || !strings.Contains(p.Details, long) {
		t.Errorf("long summary = %q", p.Summary)
	}
}

// hardSamples are paths (relative to the home folder) that hold sign-in
// secrets: hardSecretPath flags them, and a remote cat of them asks first.
var hardSamples = []string{
	".ssh/id_rsa", ".ssh", ".ssh/config", "id_ed25519.pub", ".config/tool/authentication.json", ".netrc", ".aws/credentials",
	".kube/config", ".kube/cache/x", "server.pem", "certs/tls.KEY", "x.p12", ".gnupg/pubring.kbx", ".git-credentials",
	".cache/huggingface/token", ".huggingface/token", ".config/gh/hosts.yml", ".config/rclone/rclone.conf", ".vault-token",
	".s3cfg", ".boto", ".pypirc", ".npmrc", ".config/gcloud/credentials.db", ".config/gcloud", ".azure/accessTokens.json",
	".bash_history", ".zsh_history", ".docker/config.json", ".pgpass",
}

// notSecretSamples are not hard secrets.
var notSecretSamples = []string{
	".bashrc", ".config/app/settings.txt", "results/.cache/x", "my-secrets.txt", "aws-credentials.csv", "id_list.txt",
	".env", "project/.env.local", "login.keychain-db", ".aws/config", ".cache/huggingface/hub/model.bin",
	"results/out.txt", "notes.txt", "keys.txt", "keyring/x", "slurm-1.out", "config", "labmill/job1/run.sh", "a.b/c",
	".key", "--api-key", ".local/bin/tool",
}

func TestSecretRules(t *testing.T) {
	for _, p := range hardSamples {
		if !hardSecretPath(p) || !hardSecretPath("/home/alice/"+p) || !hardSecretPath("~/"+p) {
			t.Errorf("hardSecretPath(%q) = false", p)
		}
	}
	for _, p := range notSecretSamples {
		if hardSecretPath(p) {
			t.Errorf("hardSecretPath(%q) = true", p)
		}
	}
	// One generated sample per hardSecrets entry, so a new entry is covered
	// without editing hardSamples. Entries must be plain names (letters,
	// digits, '.', '_', '-'), the form the matcher and its docs assume.
	plain := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	hs := hardSecrets
	entry := func(e, sample string) {
		if !plain.MatchString(e) {
			t.Errorf("hardSecrets entry %q isn't a plain name", e)
		}
		if !hardSecretPath("/home/alice/work/" + sample) {
			t.Errorf("hardSecretPath(%q) = false for its own entry", sample)
		}
	}
	for _, d := range hs.dirs {
		entry(d, d+"/inner.txt")
	}
	for _, b := range hs.bases {
		entry(b, b)
	}
	for _, pr := range hs.pairs {
		entry(pr[0], pr[0]+"/"+pr[1])
		entry(pr[1], pr[0]+"/"+pr[1])
	}
	for _, sfx := range hs.suffixes {
		entry(sfx, "server"+sfx)
	}
	for _, pre := range hs.prefixes {
		entry(pre, pre+".pub")
	}
	entry(hs.history, ".bash"+hs.history)
}

// secretArg adds the remote exemptions and printsContent limits the check.
func TestSecretArgAndPrintsContent(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"cat", "~/.ssh/id_rsa"}, true},
		{[]string{"cat", "~/.ssh/config"}, false},
		{[]string{"cat", "~/.ssh/config.d/labmill"}, false},
		{[]string{"cat", "~/.ssh/authorized_keys"}, false},
		{[]string{"cat", "~/.ssh/known_hosts"}, false},
		{[]string{"cat", "~/.ssh/id_rsa.pub"}, false},
		{[]string{"cat", ".bashrc"}, false},
	} {
		_, got := secretArg(c.argv)
		if got != c.want {
			t.Errorf("secretArg(%q) = %v", c.argv, got)
		}
	}
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"cat", "x"}, true}, {[]string{"head", "x"}, true}, {[]string{"tail", "x"}, true},
		{[]string{"file", "-z", "x"}, true}, {[]string{"file", "x"}, false},
		{[]string{"ls", "x"}, false}, {[]string{"stat", "x"}, false}, {[]string{"du", "x"}, false}, {[]string{"wc", "x"}, false},
	} {
		if got := printsContent(c.argv); got != c.want {
			t.Errorf("printsContent(%q) = %v", c.argv, got)
		}
	}
}

// File content that assigns a credential is redacted before it reaches Claude.
func TestRemoteCommandRedactsContent(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "name = run1\napi_key = sk-live-123\nPASSWORD: hunter2\n"}, nil
	}}
	h := remoteHarness(t, fr)
	out, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"cat", "settings.ini"}})
	if err != nil || strings.Contains(out, "sk-live-123") || strings.Contains(out, "hunter2") || !strings.Contains(out, "name = run1") || !strings.Contains(out, "«redacted»") {
		t.Errorf("out = %q, %v", out, err)
	}
	// Other commands' output is left alone (scontrol show config has "AuthType = ...").
	fr.fn = func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "AuthType = auth/munge\n"}, nil
	}
	out, err = h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"scontrol", "show", "config"}})
	if err != nil || !strings.Contains(out, "auth/munge") {
		t.Errorf("out = %q, %v", out, err)
	}
	// ls on a key path runs at once.
	if _, err := h.tools["remote_command"].Propose(json.RawMessage(`{"cluster":"hpcone","argv":["ls","-l","~/.ssh/id_rsa"]}`)); !errors.Is(err, agent.ErrNoApproval) {
		t.Errorf("ls on a key path: %v", err)
	}
}

// Nothing is refused for being hidden, secret-looking or interactive: reads
// run at once, anything else asks, and the approval shows the exact line.
func TestRemoteCommandNothingRefused(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "ok\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	for _, argv := range []string{
		`["cat","~/.ssh/config"]`, `["cat",".bashrc"]`, `["ls","/etc"]`, `["head","../../x"]`,
		`["cat","~/.ssh/known_hosts"]`, `["cat","~/.ssh/id_ed25519.pub"]`,
	} {
		in := json.RawMessage(`{"cluster":"hpcone","argv":` + argv + `}`)
		if _, err := tool.Propose(in); !errors.Is(err, agent.ErrNoApproval) {
			t.Errorf("%s: Propose err = %v, want ErrNoApproval", argv, err)
		}
		if out, err := tool.Run(context.Background(), in, agent.Decision{}); err != nil || !strings.Contains(out, "Exit code: 0") {
			t.Errorf("%s: %q, %v", argv, out, err)
		}
	}
	if got := backend.Quote(fr.calls[0]); got != "timeout -k 5 60 cat ./.ssh/config" {
		t.Errorf("ran %q", got)
	}
	for _, argv := range []string{
		`["vim","job.sh"]`, `["srun","--pty","bash"]`,
		`["cp","x","/home/alice/.aws/credentials"]`, `["rm","-rf","/tmp/x"]`,
	} {
		in := json.RawMessage(`{"cluster":"hpcone","argv":` + argv + `}`)
		p, err := tool.Propose(in)
		if err != nil || errors.Is(err, agent.ErrNoApproval) {
			t.Fatalf("%s: Propose err = %v, want a proposal", argv, err)
		}
		var words []string
		var v struct{ Argv []string }
		json.Unmarshal(in, &v)
		words = v.Argv
		if !strings.Contains(p.Details, "Command: "+backend.Quote(words)+"\n") || !strings.Contains(p.Details, "Runs exactly as: timeout -k 5 60 ") {
			t.Errorf("%s: details lack the exact line:\n%s", argv, p.Details)
		}
		if _, err := tool.Run(context.Background(), in, agent.Decision{}); err == nil {
			t.Errorf("%s: ran without approval", argv)
		}
	}
	// The quoting in the approval is the quoting that runs.
	in := json.RawMessage(`{"cluster":"hpcone","argv":["bash","-c","echo $HOME > '/tmp/my file'"]}`)
	p, _ := tool.Propose(in)
	if !strings.Contains(p.Details, `Runs exactly as: timeout -k 5 60 bash -c 'echo $HOME > '\''/tmp/my file'\'''`) {
		t.Errorf("details:\n%s", p.Details)
	}
}

// A read-only command that names a sign-in secret asks first, with a warning
// that the content goes to Claude; it is never refused.
func TestRemoteCommandSecretReadsAsk(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "KEY\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	for _, argv := range []string{
		`["cat","~/.ssh/id_rsa"]`, `["cat",".ssh/id_ed25519"]`, `["head","-n","3","~/.config/tool/authentication.json"]`,
		`["cat","/home/alice/.netrc"]`, `["cat",".aws/credentials"]`, `["cat",".cache/huggingface/token"]`,
		`["tail","~/.bash_history"]`, `["cat","server.pem"]`,
	} {
		in := json.RawMessage(`{"cluster":"hpcone","argv":` + argv + `}`)
		p, err := tool.Propose(in)
		if err != nil || errors.Is(err, agent.ErrNoApproval) {
			t.Fatalf("%s: Propose err = %v, want a proposal", argv, err)
		}
		if !strings.Contains(p.Details, "This file's content will be sent to Claude.") || strings.Contains(p.Details, remoteChangeNote) ||
			!strings.Contains(p.Details, "Runs exactly as: timeout -k 5 60 ") {
			t.Errorf("%s: details:\n%s", argv, p.Details)
		}
		if _, err := tool.Run(context.Background(), in, agent.Decision{}); err == nil {
			t.Errorf("%s: ran without approval", argv)
		}
		if out, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p}); err != nil || !strings.Contains(out, "KEY") {
			t.Errorf("%s: approved run: %q, %v", argv, out, err)
		}
	}
	// Ordinary files, host names and public keys run at once.
	for _, argv := range []string{`["cat","~/.ssh/config"]`, `["cat",".bashrc"]`, `["cat","~/.ssh/known_hosts"]`, `["cat","~/.ssh/id_rsa.pub"]`, `["cat","keys.txt"]`} {
		if _, err := tool.Propose(json.RawMessage(`{"cluster":"hpcone","argv":` + argv + `}`)); !errors.Is(err, agent.ErrNoApproval) {
			t.Errorf("%s: Propose err = %v, want ErrNoApproval", argv, err)
		}
	}
}

// The approval decision sits in one function: setting off/on by read-only,
// write and secret-printing commands.
func TestRemoteNeedsApproval(t *testing.T) {
	ro := remoteCmd{class: remoteReadOnly}
	write := remoteCmd{class: remoteGated}
	secret := remoteCmd{class: remoteGated, secret: "~/.ssh/id_rsa"}
	for _, c := range []struct {
		name string
		rc   remoteCmd
		auto bool
		want bool
	}{
		{"read-only, setting off", ro, false, false},
		{"write, setting off", write, false, true},
		{"secret, setting off", secret, false, true},
		{"read-only, setting on", ro, true, false},
		{"write, setting on", write, true, false},
		{"secret, setting on", secret, true, true},
	} {
		if got := remoteNeedsApproval(c.rc, c.auto); got != c.want {
			t.Errorf("%s: remoteNeedsApproval = %v, want %v", c.name, got, c.want)
		}
	}
}

// With "Run commands on the cluster without asking" on, a write runs at once
// (read at every call, so turning it off asks again), a secret read still
// asks, and malformed argv is still rejected.
func TestRemoteCommandAutoRun(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "done\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	on := false
	h.env.AutoRunRemote = func() bool { return on }
	write := json.RawMessage(`{"cluster":"hpcone","argv":["mkdir","-p","out"]}`)
	secret := json.RawMessage(`{"cluster":"hpcone","argv":["cat","~/.ssh/id_rsa"]}`)

	if _, err := tool.Propose(write); err != nil || errors.Is(err, agent.ErrNoApproval) {
		t.Fatalf("setting off: Propose err = %v, want a proposal", err)
	}
	on = true
	if _, err := tool.Propose(write); !errors.Is(err, agent.ErrNoApproval) {
		t.Fatalf("setting on: Propose err = %v, want ErrNoApproval", err)
	}
	if out, err := tool.Run(context.Background(), write, agent.Decision{}); err != nil || !strings.Contains(out, "done") {
		t.Errorf("setting on: Run without a decision = %q, %v", out, err)
	}
	if p, err := tool.Propose(secret); err != nil || errors.Is(err, agent.ErrNoApproval) || !strings.Contains(p.Details, "sent to Claude") {
		t.Errorf("setting on, secret read: %v, %q", err, p.Details)
	}
	if _, err := tool.Run(context.Background(), secret, agent.Decision{}); err == nil {
		t.Error("setting on: a secret read ran without approval")
	}
	if _, err := tool.Propose(json.RawMessage(`{"cluster":"hpcone","argv":[]}`)); err == nil {
		t.Error("setting on: empty argv accepted")
	}
	if _, err := tool.Propose(json.RawMessage(`{"cluster":"hpcone","argv":["ls","a\u0000b"]}`)); err == nil {
		t.Error("setting on: argv with a control character accepted")
	}
	on = false
	if _, err := tool.Propose(write); err != nil {
		t.Errorf("setting turned off again: Propose err = %v, want a proposal", err)
	}
}

func TestRemoteCommandCutAt4MB(t *testing.T) {
	var big strings.Builder
	for i := 0; big.Len() < 200<<10; i++ {
		fmt.Fprintf(&big, "row %06d\n", i)
	}
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: big.String() + backend.TruncatedMarker}, nil
	}}
	h := remoteHarness(t, fr)
	out, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"sacct"}})
	if err != nil {
		t.Fatal(err)
	}
	head := "Output (cut at 4 MB on the cluster; only the first 64 KB are shown):\nrow 000000\n"
	if !strings.Contains(out, head) || strings.Contains(out, "[output truncated]") || len(out) > remoteMaxStdout+1024 {
		t.Errorf("result (%d bytes) starts:\n%.300s", len(out), out)
	}
}

func TestRemoteCommandReadOnlyRunsAtOnce(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "PARTITION AVAIL\nshare up\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	in := json.RawMessage(`{"cluster":"hpcone","argv":["sinfo","-s"]}`)
	if _, err := tool.Propose(in); !errors.Is(err, agent.ErrNoApproval) {
		t.Fatalf("Propose err = %v, want ErrNoApproval", err)
	}
	out, err := tool.Run(context.Background(), in, agent.Decision{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "share up") || backend.Quote(fr.calls[0]) != "timeout -k 5 60 sinfo -s" {
		t.Errorf("out = %q, ran %q", out, fr.calls)
	}
	if err := agent.NewRegistry().Register(tool); err != nil {
		t.Errorf("remote_command not registrable: %v", err)
	}
}

func TestRemoteCommandOutputCap(t *testing.T) {
	var big strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&big, "line %05d\n", i)
	}
	big.WriteString("\x1b[31mred\x1b[0m end\n")
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: big.String(), Stderr: strings.Repeat("warn\n", 10000), ExitCode: 3}, nil
	}}
	h := remoteHarness(t, fr)
	out, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"sacct"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > remoteMaxStdout+remoteMaxStderr+1024 {
		t.Errorf("result is %d bytes", len(out))
	}
	for _, want := range []string{"Exit code: 3", "Output (only the last 64 KB):", "line 19999", "red end", "Errors (only the last 16 KB):\nwarn"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q", want)
		}
	}
	if strings.Contains(out, "line 00000") || strings.Contains(out, "\x1b") {
		t.Error("result kept the head or an escape sequence")
	}
	// Kept output starts at a line boundary.
	_, rest, _ := strings.Cut(out, "Output (only the last 64 KB):\n")
	if !strings.HasPrefix(rest, "line ") {
		t.Errorf("kept output starts with %q", rest[:20])
	}
}

// Whatever the cluster prints, the result has no control, escape or format
// characters (the same cleaning as job_logs, and textsafe.Clean before it).
func TestRemoteCommandCleansOutput(t *testing.T) {
	nasty := "ok\x1b[2J\x1b]0;title\x07 ‮gnp.exe‬\r\nnul\x00 bell\x07 c1\u009b31m del\x7f zw​\ttab\n"
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: nasty, Stderr: "err\x1b[31m red\x1b[0m\r\n"}, nil
	}}
	h := remoteHarness(t, fr)
	out, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"sacct"}})
	if err != nil {
		t.Fatal(err)
	}
	if cleanText(out) != out {
		t.Errorf("result not clean per cleanText: %q", out)
	}
	for _, r := range out {
		if r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '‮' || r == '‬' || r == '​') {
			t.Errorf("result keeps %U: %q", r, out)
		}
	}
	for _, want := range []string{"Output:\nok ", "gnp.exe", "nul bell c1", "tab", "Errors:\nerr red"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "title") {
		t.Errorf("OSC payload kept: %q", out)
	}
}

func TestTailCap(t *testing.T) {
	if s, cut := tailCap("abc", 10); s != "abc" || cut {
		t.Errorf("short: %q %v", s, cut)
	}
	if s, cut := tailCap("one\ntwo\nthree\n", 9); s != "three\n" || !cut {
		t.Errorf("lines: %q %v", s, cut)
	}
	if s, _ := tailCap(strings.Repeat("é", 10), 5); !strings.HasPrefix(s, "é") || len(s) > 5 {
		t.Errorf("runes: %q", s)
	}
}

func TestRemoteCommandTimeout(t *testing.T) {
	old := remoteSlack
	remoteSlack = 10 * time.Millisecond
	t.Cleanup(func() { remoteSlack = old })
	fr := &fakeRunner{fn: func(ctx context.Context, _ []string) (backend.Result, error) {
		<-ctx.Done()
		return backend.Result{}, ctx.Err()
	}}
	h := remoteHarness(t, fr)
	start := time.Now()
	_, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"sacct"}, "timeout_seconds": 1})
	if err == nil || !strings.Contains(err.Error(), "didn't finish within 1 seconds") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
	if backend.Quote(fr.calls[0]) != "timeout -k 5 1 sacct" {
		t.Errorf("ran %q", fr.calls)
	}
	// The remote timeout's own exit status is explained.
	fr.fn = func(context.Context, []string) (backend.Result, error) { return backend.Result{ExitCode: 124}, nil }
	out, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"sacct"}, "timeout_seconds": 2})
	if err != nil || !strings.Contains(out, "Exit code: 124 (stopped after 2 seconds") {
		t.Errorf("124: %q %v", out, err)
	}
}

func TestRemoteCommandRealTimeout(t *testing.T) {
	requireTools(t, "sh", "timeout", "sleep")
	h := remoteHarness(t, shellRunner{home: t.TempDir()})
	tool := h.tools["remote_command"]
	in, _ := json.Marshal(map[string]any{"cluster": "hpcone", "argv": []string{"sleep", "30"}, "timeout_seconds": 1})
	p, err := tool.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
	if err != nil || !strings.Contains(out, "Exit code: 124") {
		t.Errorf("out = %q, err = %v", out, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}

// TestRemoteCommandRealReads runs read-only commands for real through a
// POSIX shell: any path the account can read works, hidden files included.
func TestRemoteCommandRealReads(t *testing.T) {
	requireTools(t, "sh", "timeout", "cat", "ls", "head")
	home := t.TempDir()
	other := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "notes.txt"), "hello\nsecond\n")
	write(filepath.Join(home, ".ssh", "config"), "Host x\n")
	write(filepath.Join(other, "other.txt"), "outside data\n")
	h := remoteHarness(t, shellRunner{home: home})
	run := func(argv ...string) (string, error) {
		return h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": argv})
	}
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"cat", "notes.txt"}, "hello\nsecond"},
		{[]string{"cat", "~/notes.txt"}, "hello"},
		{[]string{"head", "-n", "1", "notes.txt"}, "Output:\nhello"},
		{[]string{"cat", "~/.ssh/config"}, "Host x"},
		{[]string{"cat", filepath.Join(other, "other.txt")}, "outside data"},
		{[]string{"ls", "-a"}, ".ssh"},
	} {
		out, err := run(c.argv...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%q: %q, %v", c.argv, out, err)
		}
	}
	// A missing file is the cluster's own error, with its exit code.
	out, err := run("cat", "missing.txt")
	if err != nil || !strings.Contains(out, "Exit code: 1") {
		t.Errorf("missing: %q, %v", out, err)
	}
}

// TestRemoteCommandQuotingOverSSH sends tricky words through a real SSH
// session (sshtest) and lets a POSIX shell split the received command line,
// as the login shell does: every word must arrive intact.
func TestRemoteCommandQuotingOverSSH(t *testing.T) {
	requireTools(t, "sh")
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Commands: map[string]sshtest.Handler{
		"timeout": func(args string, _ io.Reader, stdout, stderr io.Writer) int {
			cmd := exec.Command("sh", "-c", "printf '<%s>\\n' "+args)
			cmd.Stdout, cmd.Stderr = stdout, stderr
			if err := cmd.Run(); err != nil {
				return 1
			}
			return 0
		},
	}})
	cfg := &ssh.ClientConfig{
		User: "alice",
		Auth: []ssh.AuthMethod{ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = "pw"
			}
			return ans, nil
		})},
		HostKeyCallback: ssh.FixedHostKey(srv.HostKey),
		Timeout:         5 * time.Second,
	}
	cl, err := ssh.Dial("tcp", srv.Addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	h := remoteHarness(t, &backend.SSHRunner{Client: cl})
	words := []string{"-u", "x; rm -rf ~", "it's", "$HOME", "`id`", "a  b", "*", "|", "&&", ">out", "é"}
	out, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": append([]string{"squeue"}, words...)})
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, w := range append([]string{"-k", "5", "60", "squeue"}, words...) {
		want.WriteString("<" + w + ">\n")
	}
	if !strings.Contains(out, strings.TrimRight(want.String(), "\n")) {
		t.Errorf("words did not arrive intact:\n%s\nwant\n%s", out, want.String())
	}
	if execs := srv.Execs(); len(execs) != 1 || !strings.HasPrefix(execs[0], "timeout -k 5 60 squeue -u 'x; rm -rf ~' 'it'\\''s' '$HOME'") {
		t.Errorf("exec = %q", execs)
	}
	if srv.PTYs() != 0 {
		t.Error("remote_command asked for a terminal")
	}
}

func TestSystemPromptMentionsRemoteCommand(t *testing.T) {
	h := newHarness(t)
	sp := SystemPrompt(h.env)
	if !strings.Contains(sp, "remote_command runs one command") || strings.Contains(sp, "You have no shell. ") {
		t.Errorf("system prompt doesn't explain remote_command")
	}
	if !strings.Contains(SkillPreamble, "remote_command") {
		t.Error("SkillPreamble doesn't mention remote_command")
	}
}

// With the setting on, commands outside the read-only list that name a
// sign-in secret file still ask (and say the output goes to Claude), and
// their output is redacted; an ordinary write runs at once and says so.
func TestRemoteCommandAutoRunSecretsAndNote(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "password=hunter2\n"}, nil
	}}
	h := remoteHarness(t, fr)
	h.env.AutoRunRemote = func() bool { return true }
	tool := h.tools["remote_command"]
	for _, argv := range []string{
		`["bash","-c","cat ~/.config/tool/authentication.json"]`,
		`["base64",".ssh/id_ed25519"]`,
		`["grep","","~/.netrc"]`,
	} {
		in := json.RawMessage(`{"cluster":"hpcone","argv":` + argv + `}`)
		p, err := tool.Propose(in)
		if err != nil || errors.Is(err, agent.ErrNoApproval) {
			t.Fatalf("%s: Propose err = %v, want a proposal", argv, err)
		}
		if !strings.Contains(p.Details, "This file's content will be sent to Claude.") {
			t.Errorf("%s: details lack the secret note:\n%s", argv, p.Details)
		}
		if _, err := tool.Run(context.Background(), in, agent.Decision{}); err == nil {
			t.Errorf("%s: ran without approval", argv)
		}
		out, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
		if err != nil || strings.Contains(out, "hunter2") || strings.Contains(out, remoteAutoNote) {
			t.Errorf("%s: approved run: %q, %v (want redacted, no auto note)", argv, out, err)
		}
	}
	in := json.RawMessage(`{"cluster":"hpcone","argv":["mkdir","out"]}`)
	if out, err := tool.Run(context.Background(), in, agent.Decision{}); err != nil || !strings.HasPrefix(out, remoteAutoNote+"\n") {
		t.Errorf("auto-run write: %q, %v (want the auto note first)", out, err)
	}
	// Name tricks the secret check can't see still get their output
	// redacted when nobody approved the command.
	glob := json.RawMessage(`{"cluster":"hpcone","argv":["bash","-c","cat ~/.config/tool/auth*"]}`)
	if out, err := tool.Run(context.Background(), glob, agent.Decision{}); err != nil || strings.Contains(out, "hunter2") {
		t.Errorf("auto-run glob read: %q, %v (want redacted)", out, err)
	}
	ro := json.RawMessage(`{"cluster":"hpcone","argv":["ls"]}`)
	if out, err := tool.Run(context.Background(), ro, agent.Decision{}); err != nil || strings.Contains(out, remoteAutoNote) {
		t.Errorf("read-only run: %q, %v (no auto note expected)", out, err)
	}
}

func TestRemoteScript(t *testing.T) {
	fr := &fakeRunner{allowStdin: true, fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "ok\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	script := "set -e\nfor i in 1 2; do\n  echo \"it's $i\"\ndone\n"
	in, _ := json.Marshal(map[string]any{"cluster": "hpcone", "script": script})

	p, err := tool.Propose(in)
	if err != nil || errors.Is(err, agent.ErrNoApproval) {
		t.Fatalf("Propose err = %v, want a proposal", err)
	}
	if !strings.Contains(p.Details, "Runs exactly as: timeout -k 5 60 bash -s\n") || !strings.Contains(p.Details, "in full:\n"+script+"\n--- end of script ---") ||
		!strings.Contains(p.Details, "Why Labmill asks: it is a script") || !strings.HasPrefix(p.Summary, "Run on hpcone: a script via bash -s") {
		t.Errorf("proposal:\n%s\n%s", p.Summary, p.Details)
	}
	if _, err := tool.Run(context.Background(), in, agent.Decision{}); err == nil {
		t.Error("unapproved script ran")
	}
	// The approved script travels on stdin; argv holds only the interpreter.
	out, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if len(fr.calls) != 1 || backend.Quote(fr.calls[0]) != "timeout -k 5 60 bash -s" || len(fr.stdins) != 1 || fr.stdins[0] != script {
		t.Fatalf("calls %q stdins %q", fr.calls, fr.stdins)
	}
	// tcsh safety: no script text reaches the command line.
	for _, w := range fr.calls[0] {
		if strings.ContainsAny(w, "\n'\"$") {
			t.Errorf("script leaked into argv word %q", w)
		}
	}
	// A changed script is not the approved one.
	in2, _ := json.Marshal(map[string]any{"cluster": "hpcone", "script": script + "echo more\n"})
	if _, err := tool.Run(context.Background(), in2, agent.Decision{Approved: true, Proposal: p}); err == nil {
		t.Error("tampered script ran")
	}
	// Another interpreter.
	in3, _ := json.Marshal(map[string]any{"cluster": "hpcone", "argv": []string{"sh", "-s"}, "script": "echo hi\n"})
	p3, err := tool.Propose(in3)
	if err != nil || !strings.Contains(p3.Details, "Runs exactly as: timeout -k 5 60 sh -s\n") {
		t.Errorf("sh -s: %v\n%s", err, p3.Details)
	}
	// Refusals: size and NUL (JSON can't carry bad UTF-8).
	for _, sc := range []string{strings.Repeat("x", remoteMaxScript+1), "a\x00b"} {
		b, _ := json.Marshal(map[string]any{"cluster": "hpcone", "script": sc})
		if _, err := tool.Propose(b); err == nil || errors.Is(err, agent.ErrNoApproval) {
			t.Errorf("script %.10q accepted", sc)
		}
	}
	// Even a read-only-looking script is gated.
	b, _ := json.Marshal(map[string]any{"cluster": "hpcone", "script": "squeue --me\n"})
	if _, err := tool.Propose(b); err != nil && errors.Is(err, agent.ErrNoApproval) {
		t.Errorf("read-only-looking script ran at once: %v", err)
	}
}

func TestRemoteScriptAutoRunAndSecrets(t *testing.T) {
	fr := &fakeRunner{allowStdin: true, fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "password=hunter2\n"}, nil
	}}
	h := remoteHarness(t, fr)
	on := false
	h.env.AutoRunRemote = func() bool { return on }
	tool := h.tools["remote_command"]
	plain, _ := json.Marshal(map[string]any{"cluster": "hpcone", "script": "mkdir -p out\necho x > out/a\n"})
	if _, err := tool.Propose(plain); err != nil {
		t.Fatalf("setting off: %v", err)
	}
	on = true
	if _, err := tool.Propose(plain); !errors.Is(err, agent.ErrNoApproval) {
		t.Fatalf("setting on: Propose err = %v, want ErrNoApproval", err)
	}
	out, err := tool.Run(context.Background(), plain, agent.Decision{})
	if err != nil || !strings.HasPrefix(out, remoteAutoNote+"\n") || strings.Contains(out, "hunter2") || len(fr.stdins) != 1 {
		t.Errorf("auto-run script: %q, %v, %q", out, err, fr.stdins)
	}
	// A secret file named inside the script still asks.
	sec, _ := json.Marshal(map[string]any{"cluster": "hpcone", "script": "echo start\ncat ~/.ssh/id_rsa\n"})
	p, err := tool.Propose(sec)
	if err != nil || !strings.Contains(p.Details, "sent to Claude") {
		t.Fatalf("secret script: %v, %q", err, p.Details)
	}
	if _, err := tool.Run(context.Background(), sec, agent.Decision{}); err == nil {
		t.Error("secret script ran without approval")
	}
}

// The secret-file check covers argv as well as the script: with the
// auto-run setting on, argv naming a key still asks.
func TestRemoteScriptSecretInArgv(t *testing.T) {
	fr := &fakeRunner{allowStdin: true}
	h := remoteHarness(t, fr)
	h.env.AutoRunRemote = func() bool { return true }
	tool := h.tools["remote_command"]
	in := json.RawMessage(`{"cluster":"hpcone","argv":["cat",".ssh/id_rsa"],"script":" "}`)
	p, err := tool.Propose(in)
	if err != nil || errors.Is(err, agent.ErrNoApproval) || !strings.Contains(p.Details, remoteSecretNote) {
		t.Fatalf("Propose: %v, %q", err, p.Details)
	}
	if _, err := tool.Run(context.Background(), in, agent.Decision{}); err == nil || len(fr.calls) != 0 {
		t.Errorf("ran without approval: %v %q", err, fr.calls)
	}
	h.env.AutoRunRemote = func() bool { return false }
	// A long interpreter argv keeps the summary short.
	long := json.RawMessage(`{"cluster":"hpcone","argv":["bash","-s","` + strings.Repeat("a", 300) + `"],"script":"echo hi"}`)
	p, err = tool.Propose(long)
	if err != nil || len([]rune(p.Summary)) > len("Run on hpcone: ")+remoteMaxSummary+1 {
		t.Errorf("summary too long: %v %d", err, len([]rune(p.Summary)))
	}
}

func TestRemoteSubmitHint(t *testing.T) {
	fr := &fakeRunner{allowStdin: true}
	h := remoteHarness(t, fr)
	h.env.AutoRunRemote = func() bool { return true }
	tool := h.tools["remote_command"]
	submits := []string{
		`{"cluster":"hpcone","argv":["sbatch","job.sh"]}`,
		`{"cluster":"hpcone","argv":["srun","hostname"]}`,
		`{"cluster":"hpcone","argv":["bash","-c","cd out && sbatch job.sh"]}`,
		`{"cluster":"hpcone","script":"cd out\nexport A=1\nA=2 sbatch job.sh\n"}`,
		`{"cluster":"hpcone","script":"if true; then\nenv A=1 sbatch j.sh\nfi\n! command sbatch k.sh\n"}`,
	}
	for _, in := range submits {
		out, err := tool.Run(context.Background(), json.RawMessage(in), agent.Decision{})
		if err != nil || !strings.Contains(out, remoteSubmitHint+"\n") {
			t.Errorf("%s: %q, %v (want the submit hint)", in, out, err)
		}
	}
	for _, in := range []string{
		`{"cluster":"hpcone","argv":["squeue","--me"]}`,
		`{"cluster":"hpcone","argv":["ls","sbatch"]}`,
		`{"cluster":"hpcone","argv":["scancel","12"]}`,
		`{"cluster":"hpcone","script":"echo sbatch\ncat sbatch.log\n"}`,
	} {
		out, err := tool.Run(context.Background(), json.RawMessage(in), agent.Decision{})
		if err != nil || strings.Contains(out, remoteSubmitHint) {
			t.Errorf("%s: %q, %v (no hint expected)", in, out, err)
		}
	}
}

func TestSystemPromptSteersToTypedTools(t *testing.T) {
	h := newHarness(t)
	sp := SystemPrompt(h.env)
	for _, want := range []string{"draft_job (files)", "only with submit_job", "its \"script\" field"} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	d := h.tools["remote_command"].Description
	for _, want := range []string{"`script`", "Submit only with submit_job", "draft_job (its `files`)", "job_status and job_logs", "fetch_results"} {
		if !strings.Contains(d, want) {
			t.Errorf("remote_command description lacks %q", want)
		}
	}
}

// The prompt tells Claude to look at the module system first when a Slurm job
// needs a named program, and gives the fall-back order.
func TestSystemPromptModulesFirst(t *testing.T) {
	h := newHarness(t)
	set, err := skill.Load(fstest.MapFS{
		"slurm-user/SKILL.md": {Data: []byte("---\nname: slurm-user\ndescription: Slurm things.\n---\nbody\n")},
	}, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	h.env.Skills = set
	sp := SystemPrompt(h.env)
	for _, want := range []string{
		"module spider <name>",
		"module purge and pinned module load <name>/<version>",
		"site skill's other installs, then a conda or pip environment, then a container",
		"older than one year",
		"node-local scratch for a single-node job",
		"after more than two failed preemptible attempts",
		"only after it has really ended",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}
