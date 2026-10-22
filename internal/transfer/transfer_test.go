package transfer

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
	"labmill-client/internal/sshx/sshtest"
)

// TestMain doubles as `labmill ssh-exec` when re-executed by rclone.
func TestMain(m *testing.M) {
	os.Unsetenv("SSH_AUTH_SOCK") // the developer's agent stays out of the tests
	if os.Getenv("LABMILL_TEST_SSHEXEC") == "1" {
		code, err := sshx.Exec(context.Background(), os.Getenv("LABMILL_TEST_ALIAS"), os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
			sshx.ExecOptions{Connect: sshx.ConnectOptions{SocketPath: os.Getenv("LABMILL_TEST_SOCK"), Spawn: func() error { return fmt.Errorf("no spawn in tests") }}})
		if err != nil {
			fmt.Fprintln(os.Stderr, "ssh-exec:", err)
			if code == 0 {
				code = 255
			}
		}
		os.Exit(code)
	}
	skipShutdown = true // see leakTB: shutting down sftp Fs trips rclone's internal race
	os.Exit(m.Run())
}

// leakTB makes sshtest.New's t.Cleanup a no-op. Servers, helpers and logins
// created by transferEnvSrv live until the test binary exits.
//
// Why: rclone's external-ssh client reads exec.Cmd.ProcessState in
// sshSessionExternal.exited() with no synchronization, while another
// goroutine's cmd.Wait() writes it when the ssh-exec process exits. That is
// a genuine data race inside rclone (v1.75.1), and the race detector reports
// it whenever an ssh-exec process exits while the test binary is still
// running, e.g. because a test's cleanup tore its helper down. Keeping the
// processes alive until os.Exit avoids the report; it does not remove the
// race. It cannot be fixed from Labmill's side. In production it is benign:
// the reader only decides whether to reuse a pooled connection.
type leakTB struct{ testing.TB }

func (leakTB) Cleanup(func()) {}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type pwPrompter struct{}

func (pwPrompter) Ask(_ context.Context, q sshx.Question) (string, error) {
	if q.Kind == sshx.AskPassword {
		return "pw", nil
	}
	return "", nil
}
func (pwPrompter) Notify(string) {}

// transferEnv: sshtest server with SFTP root, helper on a temp socket, one
// interactive login done (so ssh-exec can reuse it), env set for re-exec.
func transferEnv(t *testing.T) (remoteRoot string, r Remote) {
	t.Helper()
	remoteRoot, r, _ = transferEnvSrv(t)
	return remoteRoot, r
}

// transferEnvSrv is transferEnv that also returns the test server (to
// inspect which commands were run on it).
func transferEnvSrv(t *testing.T) (remoteRoot string, r Remote, srv *sshtest.Server) {
	return transferEnvCmds(t, map[string]sshtest.Handler{})
}

// transferEnvCmds is transferEnvSrv with scripted remote commands.
func transferEnvCmds(t *testing.T, cmds map[string]sshtest.Handler) (remoteRoot string, r Remote, srv *sshtest.Server) {
	t.Helper()
	remoteRoot = t.TempDir()
	srv = sshtest.New(leakTB{t}, sshtest.Options{User: "alice", Password: "pw", SFTPRoot: remoteRoot,
		Commands: cmds})
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	h, port, _ := net.SplitHostPort(srv.Addr)
	var portN int
	fmt.Sscanf(port, "%d", &portN)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "hpcx", HostName: h, User: "alice", Port: portN}}, "linux"); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(p.SSHDir, "known_hosts")
	os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600)
	dir, _ := os.MkdirTemp("", "yt")
	sock := filepath.Join(dir, "s.sock")
	// Never cancelled: see leakTB.
	go sshx.Serve(context.Background(), sshx.HelperOptions{SocketPath: sock, IdleTimeout: 10 * time.Minute, Paths: p, Login: sshx.LoginOptions{KnownHosts: kh}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := sshx.Connect(context.Background(), "hpcx", pwPrompter{}, sshx.ConnectOptions{SocketPath: sock, Spawn: func() error { return fmt.Errorf("x") }})
		if err == nil {
			_ = c // never closed: keeps the upstream login alive (see leakTB)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("login via helper: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Setenv("LABMILL_TEST_SSHEXEC", "1")
	t.Setenv("LABMILL_TEST_SOCK", sock)
	t.Setenv("LABMILL_TEST_ALIAS", "hpcx")
	return remoteRoot, Remote{SSHCommand: []string{os.Args[0]}, Dir: "proj/run1"}, srv
}

func TestUploadDownloadViaSSHExec(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	local := t.TempDir()
	write(t, filepath.Join(local, "data", "a.csv"), "x,y\n1,2\n")
	write(t, filepath.Join(local, "data", "deep", "b.bin"), strings.Repeat("b", 100000))
	write(t, filepath.Join(local, "data", "unselected.txt"), "must not be uploaded")
	entries, _, err := BuildManifest(context.Background(), local, []string{"data/a.csv", "data/deep"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot, r := transferEnv(t)
	var last Progress
	if err := Upload(context.Background(), local, entries, r, func(p Progress) { last = p }); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(remoteRoot, "proj", "run1", "data", "deep", "b.bin"))
	if err != nil || len(b) != 100000 {
		t.Fatalf("remote b.bin: %d bytes, %v", len(b), err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "proj", "run1", "data", "unselected.txt")); err == nil {
		t.Error("a file outside the manifest was uploaded")
	}
	if last.Bytes != last.TotalBytes || last.TotalFiles != 2 {
		t.Errorf("final progress = %+v", last)
	}

	// Results come back.
	write(t, filepath.Join(remoteRoot, "proj", "run1", "results", "model.pt"), "weights")
	out := t.TempDir()
	written, err := Download(context.Background(), r, []string{"results"}, out, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "results", "model.pt"))
	if err != nil || string(got) != "weights" || len(written) != 1 {
		t.Fatalf("downloaded %q %v written=%v", got, err, written)
	}
	if _, err := Download(context.Background(), r, []string{"../../etc"}, out, nil); err == nil {
		t.Error("Download outside localDir succeeded")
	}
	// A missing path and an empty directory are errors, not silent successes.
	os.MkdirAll(filepath.Join(remoteRoot, "proj", "run1", "emptydir"), 0o755)
	if _, err := Download(context.Background(), r, []string{"nope"}, t.TempDir(), nil); err == nil {
		t.Error("Download of a missing path succeeded")
	}
	if _, err := Download(context.Background(), r, []string{"emptydir"}, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "nothing found at that path") {
		t.Errorf("Download of an empty dir err = %v", err)
	}
}

func TestSSHExecWithoutLoginFails(t *testing.T) {
	dir, _ := os.MkdirTemp("", "yt")
	t.Cleanup(func() { os.RemoveAll(dir) })
	_, err := sshx.Exec(context.Background(), "hpcx", []string{"-s", "sftp"}, strings.NewReader(""), os.Stdout, os.Stderr,
		sshx.ExecOptions{Connect: sshx.ConnectOptions{SocketPath: filepath.Join(dir, "none.sock"), Spawn: func() error { return fmt.Errorf("no helper") }, Wait: 200 * time.Millisecond}})
	if err == nil {
		t.Fatal("Exec without a helper succeeded")
	}
}

// Files that already exist on the cluster with a different size are reported
// to OnReplace as they are replaced; same-size and new files are not.
func TestUploadReportsReplacedFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	local := t.TempDir()
	write(t, filepath.Join(local, "d", "new.txt"), "brand new")
	write(t, filepath.Join(local, "d", "diff.txt"), "twelve bytes")
	write(t, filepath.Join(local, "d", "same.txt"), "aaaa")
	entries, _, err := BuildManifest(context.Background(), local, []string{"d"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot, r := transferEnv(t)
	base := filepath.Join(remoteRoot, "proj", "run1", "d")
	write(t, filepath.Join(base, "diff.txt"), "old")
	write(t, filepath.Join(base, "same.txt"), "bbbb")
	got := map[string]int64{}
	r.OnReplace = func(rel string, old int64) { got[rel] = old }
	if err := Upload(context.Background(), local, entries, r, nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(got) != 1 || got["d/diff.txt"] != 3 {
		t.Errorf("replaced = %v, want only d/diff.txt (3 bytes)", got)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "diff.txt")); string(b) != "twelve bytes" {
		t.Errorf("remote diff.txt = %q", b)
	}
}
