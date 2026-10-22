package launch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/config"
	"labmill-client/internal/sshx"
)

func TestParseShellArgs(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want ShellArgs
		bad  bool
	}{
		{[]string{"hpc"}, ShellArgs{Cluster: "hpc"}, false},
		{[]string{"hpc", "--dir", "/data/x y"}, ShellArgs{"hpc", "/data/x y"}, false},
		{[]string{"--dir=/d", "hpc"}, ShellArgs{"hpc", "/d"}, false},
		{[]string{"-d", "~/p", "hpc"}, ShellArgs{"hpc", "~/p"}, false},
		{nil, ShellArgs{}, true},
		{[]string{"hpc", "--dir"}, ShellArgs{}, true},
		{[]string{"a", "b"}, ShellArgs{}, true},
		{[]string{"--x", "a"}, ShellArgs{}, true},
		{[]string{"hpc", "--dir", "/x\n/y"}, ShellArgs{}, true},
		{[]string{"hp\x00c"}, ShellArgs{}, true},
	} {
		got, err := ParseShellArgs(c.in)
		if (err != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("ParseShellArgs(%q) = %+v, %v", c.in, got, err)
		}
	}
}

func TestShellAlias(t *testing.T) {
	cfg := &config.Config{Clusters: []config.Cluster{
		{Name: "bigcluster", Type: config.TypeSlurm, SSHHost: "login"},
		{Name: "nohost", Type: config.TypeSlurm},
	}}
	if a, err := ShellAlias(cfg, "bigcluster"); err != nil || a != "login" {
		t.Errorf("by name: %q %v", a, err)
	}
	if a, err := ShellAlias(cfg, "login"); err != nil || a != "login" {
		t.Errorf("by alias: %q %v", a, err)
	}
	if _, err := ShellAlias(cfg, "nohost"); err == nil {
		t.Error("cluster without ssh_host accepted")
	}
	if _, err := ShellAlias(cfg, "other"); err == nil || !strings.Contains(err.Error(), "bigcluster") {
		t.Errorf("unknown: %v", err)
	}
	if _, err := ShellAlias(nil, "x"); err == nil {
		t.Error("nil config accepted")
	}
}

func TestRunShellBadArgsAndNoTerminal(t *testing.T) {
	var out, errb bytes.Buffer
	if code := RunShell(context.Background(), nil, strings.NewReader(""), &out, &errb); code != 2 || !strings.Contains(errb.String(), "Usage") {
		t.Errorf("code=%d err=%q", code, errb.String())
	}
}

// With a cluster set up but stdin not a terminal, the shell refuses before
// connecting to anything.
func TestRunShellNoTerminal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := config.Save(filepath.Join(dir, "labmill", "clusters.yaml"), &config.Config{Clusters: []config.Cluster{{Name: "hpc", Type: config.TypeSlurm, SSHHost: "login"}}}); err != nil {
		t.Fatal(err)
	}
	old := sshConnect
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		t.Error("connected without a terminal")
		return nil, errors.New("no")
	}
	t.Cleanup(func() { sshConnect = old })
	var out, errb bytes.Buffer
	if code := RunShell(context.Background(), []string{"hpc"}, strings.NewReader(""), &out, &errb); code != 255 || !strings.Contains(errb.String(), "interactive terminal") {
		t.Errorf("code=%d err=%q", code, errb.String())
	}
	errb.Reset()
	if code := RunShell(context.Background(), []string{"nope"}, strings.NewReader(""), &out, &errb); code != 255 || !strings.Contains(errb.String(), "no cluster named") {
		t.Errorf("unknown cluster: code=%d err=%q", code, errb.String())
	}
}

// The terminal prompter asks each question once, reads one line, and refuses
// the key questions (those need Labmill's guided setup).
func TestTermPrompter(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	io.WriteString(w, "alice\nsecond line\n")
	var out bytes.Buffer
	p := &termPrompter{in: r, out: &out}
	got, err := p.Ask(context.Background(), sshx.Question{Kind: sshx.AskUsername, Host: "login.example.edu", Prompt: "User name:", Echo: true})
	if err != nil || got != "alice" {
		t.Fatalf("got %q, %v", got, err)
	}
	if strings.Count(out.String(), "User name:") != 1 || !strings.Contains(out.String(), "login.example.edu") {
		t.Errorf("prompt shown: %q", out.String())
	}
	// Only one line was consumed; the next question gets the next line.
	if got, err := p.Ask(context.Background(), sshx.Question{Kind: sshx.AskOther, Prompt: "More:", Echo: true}); err != nil || got != "second line" {
		t.Errorf("second: %q, %v", got, err)
	}
	for _, k := range []sshx.QuestionKind{sshx.AskKeyPassphrase, sshx.AskKeyCreate, sshx.AskNewPassphrase, sshx.AskKeyUpload, sshx.AskKeyCopy} {
		if _, err := p.Ask(context.Background(), sshx.Question{Kind: k, Prompt: "x"}); err == nil {
			t.Errorf("kind %d answered", k)
		}
	}
	// A canceled context ends the wait.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Ask(ctx, sshx.Question{Kind: sshx.AskOther, Prompt: "x", Echo: true}); err == nil {
		t.Error("canceled ask succeeded")
	}
}

// An explicit /shell forgets the refusal for its host and the institution's
// other hosts only; other institutions and the canceled-sign-in flag stay.
func TestForgetDenied(t *testing.T) {
	cc := newConnCache(nil)
	cc.clusters = func() []config.Cluster {
		return []config.Cluster{
			{Name: "a", SSHHost: "login", FilesHost: "files"},
			{Name: "c", Site: "other.example.org", SSHHost: "far"},
		}
	}
	cc.homeDomain = "home.example.edu"
	cc.markDenied("login")
	cc.markDenied("far")
	cc.forgetDenied("login")
	for _, h := range []string{"login", "files"} {
		if m := cc.deniedMsg(h); m != "" {
			t.Errorf("%s still refused: %q", h, m)
		}
	}
	if cc.deniedMsg("far") == "" {
		t.Error("other institution's refusal was cleared")
	}
}
