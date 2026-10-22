package sshx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// helperEnv starts an upstream test server, writes an ssh config alias for
// it, and runs Serve on a temp socket. It returns the server, the socket path
// and the config paths.
func helperEnv(t *testing.T, opts sshtest.Options, idle time.Duration) (*sshtest.Server, string, sshconfig.Paths) {
	return helperEnvDial(t, opts, idle, nil)
}

// helperEnvDial is helperEnv with an injected Dial (nil: the real one).
func helperEnvDial(t *testing.T, opts sshtest.Options, idle time.Duration, dial DialFunc) (*sshtest.Server, string, sshconfig.Paths) {
	t.Helper()
	srv := sshtest.New(t, opts)
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "hpcx", HostName: h, User: opts.User, Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are limited to ~104 bytes; t.TempDir() can be long.
	dir, err := os.MkdirTemp("", "yh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s", "ssh.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: idle, Paths: p, Dial: dial,
			Login: LoginOptions{KnownHosts: filepath.Join(p.SSHDir, "known_hosts"), PushTimeout: 5 * time.Second}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("helper did not stop")
		}
	})
	waitForSocket(t, sock)
	return srv, sock, p
}

func waitForSocket(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper socket %s never came up", sock)
}

func noSpawn() error { return fmt.Errorf("test: spawning not allowed") }

func TestRelayEncoding(t *testing.T) {
	for _, q := range []Question{
		{Kind: AskPassword, Host: "login.example.edu", Prompt: "Password: ", Echo: false},
		{Kind: AskOther, Host: "h", Prompt: "Passcode or option (1-3): ", Echo: true},
		{Kind: AskHostKey, Host: "[127.0.0.1]:2222", Prompt: "SHA256:abc ...", Echo: true},
	} {
		ins, prompt, echo := EncodeRelay(q)
		got, ok := DecodeRelay(ins, prompt, echo)
		if !ok || got != q {
			t.Errorf("round trip %+v -> %+v (%v)", q, got, ok)
		}
	}
	if _, ok := DecodeRelay("Duo two-factor login", "Passcode: ", true); ok {
		t.Error("decoded a non-relay challenge")
	}
}

func TestHelperSharesOneLogin(t *testing.T) {
	srv, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove,
		Commands: map[string]sshtest.Handler{
			"echo": echoHandler,
			"fail": func(args string, _ io.Reader, _, stderr io.Writer) int { fmt.Fprint(stderr, "boom"); return 3 },
		}}, time.Minute)

	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c1, err := Connect(context.Background(), "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatalf("Connect 1: %v", err)
	}
	defer c1.Close()
	if pr.count(AskPassword) != 1 {
		t.Fatalf("password not relayed: %+v", pr.asked)
	}
	if len(pr.notified) == 0 {
		t.Error("Duo push notification not relayed")
	}
	// M43.1: the relayed push notice is cleared when the login ends.
	pr.mu.Lock()
	if n := pr.notified; n[len(n)-1] != "" {
		t.Errorf("the push notice was left showing: %q", n)
	}
	pr.mu.Unlock()

	// A second client (another Labmill process) must not log in again.
	pr2 := &fakePrompter{}
	c2, err := Connect(context.Background(), "hpcx", pr2, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatalf("Connect 2: %v", err)
	}
	defer c2.Close()
	if len(pr2.asked) != 0 {
		t.Errorf("second client was prompted: %+v", pr2.asked)
	}
	// A helper that already holds the login shows no notice at all.
	if len(pr2.notified) != 0 {
		t.Errorf("second client was notified: %q", pr2.notified)
	}
	if srv.Logins() != 1 {
		t.Errorf("upstream logins = %d, want 1", srv.Logins())
	}

	// exec with stdout, stderr and exit status through the helper.
	s, err := c2.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	s.Stdout, s.Stderr = &out, &errb
	if err := s.Run("echo via helper"); err != nil || out.String() != "via helper" {
		t.Fatalf("echo = %q, %v", out.String(), err)
	}
	s, _ = c1.NewSession()
	errb.Reset()
	s.Stderr = &errb
	err = s.Run("fail now")
	var exitErr interface{ ExitStatus() int }
	if ee, ok := err.(interface{ ExitStatus() int }); ok {
		exitErr = ee
	}
	if exitErr == nil || exitErr.ExitStatus() != 3 || errb.String() != "boom" {
		t.Errorf("fail = %v (stderr %q)", err, errb.String())
	}
}

func TestHelperSFTP(t *testing.T) {
	root := t.TempDir()
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", SFTPRoot: root}, time.Minute)
	c, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatalf("sftp over helper: %v", err)
	}
	defer sc.Close()
	f, err := sc.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "data")
	f.Close()
	b, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil || string(b) != "data" {
		t.Fatalf("uploaded file = %q, %v", b, err)
	}
}

func TestHelperDirectTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.WriteString(c, "hi from inside")
			c.Close()
		}
	}()
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", AllowTCPForward: true}, time.Minute)
	c, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn, err := c.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(conn)
	if string(b) != "hi from inside" {
		t.Errorf("got %q", b)
	}
}

func TestHelperIdleExit(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	_ = srv
	dir, _ := os.MkdirTemp("", "yh")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ssh.sock")
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), HelperOptions{SocketPath: sock, IdleTimeout: 200 * time.Millisecond})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit when idle")
	}
}

func TestHelperSocketPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw"}, time.Minute)
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("socket dir mode = %v, %v", di.Mode().Perm(), err)
	}
	si, err := os.Stat(sock)
	if err != nil || si.Mode().Perm()&0o077 != 0 {
		t.Errorf("socket mode = %v, %v", si.Mode().Perm(), err)
	}
}

func TestHelperRefusesSecondInstance(t *testing.T) {
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw"}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Serve(ctx, HelperOptions{SocketPath: sock}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second Serve = %v, want an 'already running' error", err)
	}
}

func TestConnectSpawnsHelper(t *testing.T) {
	_, sockReal, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw"}, time.Minute)
	// Point Connect at a socket nobody listens on yet; Spawn makes it appear
	// by proxying to the real helper socket.
	dir, _ := os.MkdirTemp("", "yh")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "late.sock")
	spawned := false
	spawn := func() error {
		spawned = true
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					up, err := net.Dial("unix", sockReal)
					if err != nil {
						c.Close()
						return
					}
					go io.Copy(up, c)
					io.Copy(c, up)
					c.Close()
				}()
			}
		}()
		t.Cleanup(func() { ln.Close() })
		return nil
	}
	c, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: spawn, Wait: 3 * time.Second})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	c.Close()
	if !spawned {
		t.Error("Spawn not called")
	}
}
