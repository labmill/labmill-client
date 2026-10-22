package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// A wrong Duo passcode is a 2FA failure, not a credentials failure.
func TestWrongPasscodeIsTwoFactorError(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, Passcode: "424242"})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskPasscode: "000000"}}
	o := LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine()), PreferPasscode: true, PushTimeout: time.Second}
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, o)
	if !errors.Is(err, ErrTwoFactor) || errors.Is(err, ErrAuthDenied) {
		t.Fatalf("err = %v, want ErrTwoFactor only", err)
	}
	if !strings.Contains(err.Error(), "the passcode was rejected") {
		t.Errorf("err = %v, want the passcode message", err)
	}
}

// A challenge that arrives just as the push timer expires, before its disarm
// signal is queued, must not count as server silence.
func TestPushTimerIgnoresChallengeInFlight(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove})
	var n atomic.Int64
	challengeHook = func(seq int64) {
		if n.Add(1) > 1 {
			time.Sleep(300 * time.Millisecond) // slower than PushTimeout
		}
	}
	t.Cleanup(func() { challengeHook = nil })
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	o := LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine()), PushTimeout: 100 * time.Millisecond}
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, o)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	c.Close()
}

// startServe runs Serve with a fake Dial on a fresh socket.
func startServe(t *testing.T, dial DialFunc) (sock string, stop func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "yh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "s", "ssh.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: time.Minute, Dial: dial}) }()
	var stopped atomic.Bool
	stop = func() {
		if stopped.Swap(true) {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("helper did not stop")
		}
	}
	t.Cleanup(stop)
	waitForSocket(t, sock)
	return sock, stop
}

// When the local client goes away during a login, the upstream login is
// aborted.
func TestClientDisconnectAbortsLogin(t *testing.T) {
	started := make(chan struct{})
	aborted := make(chan struct{})
	sock, _ := startServe(t, func(ctx context.Context, _ sshconfig.Paths, _ string, _ Prompter, _ LoginOptions) (*ssh.Client, error) {
		close(started)
		<-ctx.Done()
		close(aborted)
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Connect(ctx, "hpcx", &fakePrompter{}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("login never started")
	}
	cancel() // closes the local connection
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream login kept running after the client disconnected")
	}
}

// The Prompter's own error (a user cancel) survives Connect.
func TestConnectKeepsPrompterError(t *testing.T) {
	_, sock, _ := reviewEnv(t, sshtest.Options{User: "alice", Password: "pw"}, LoginOptions{PushTimeout: time.Second}, nil)
	errCancel := errors.New("user pressed escape")
	pr := &fakePrompter{cancelErr: errCancel}
	_, err := Connect(context.Background(), "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if !errors.Is(err, errCancel) {
		t.Fatalf("err = %v, want it to wrap the prompter's error", err)
	}
}

// An upstream dialed after Serve began shutting down is closed, not cached.
func TestLateUpstreamClosedAfterShutdown(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	release := make(chan struct{})
	got := make(chan *ssh.Client, 1)
	started := make(chan struct{})
	sock, stop := startServe(t, func(_ context.Context, _ sshconfig.Paths, _ string, pr Prompter, _ LoginOptions) (*ssh.Client, error) {
		close(started)
		<-release // ignores the helper's shutdown on purpose
		c, err := ssh.Dial("tcp", srv.Addr, &ssh.ClientConfig{User: "alice", Auth: []ssh.AuthMethod{ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			return pwAnswers(qs), nil
		})}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
		if err == nil {
			got <- c
		}
		return c, err
	})
	go Connect(context.Background(), "hpcx", &fakePrompter{}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("login never started")
	}
	stop()
	close(release)
	select {
	case c := <-got:
		done := make(chan struct{})
		go func() { c.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the late upstream was left open")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dial never completed")
	}
}

// Connect spawns again once when the first spawned helper never came up
// (it lost the lock to a helper that was still exiting).
func TestConnectRetriesSpawn(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "hpcx", HostName: h, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(p.SSHDir, "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "yh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s", "ssh.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var spawns atomic.Int32
	spawn := func() error {
		if spawns.Add(1) == 2 {
			go func() {
				done <- Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: time.Minute, Paths: p, Login: LoginOptions{KnownHosts: kh, PushTimeout: time.Second}})
			}()
		}
		return nil
	}
	t.Cleanup(func() {
		cancel()
		if spawns.Load() >= 2 {
			<-done
		}
	})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Connect(context.Background(), "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: spawn, Wait: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("Connect: %v (spawns %d)", err, spawns.Load())
	}
	c.Close()
	if spawns.Load() != 2 {
		t.Errorf("spawns = %d, want 2", spawns.Load())
	}
}

// A command that ends immediately must still deliver the exec reply and exit
// status through the helper (the proxy used to close the channel between the
// upstream's reply and its own).
func TestHelperQuickCommandsKeepExecReply(t *testing.T) {
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw",
		Commands: map[string]sshtest.Handler{
			"fail": func(string, io.Reader, io.Writer, io.Writer) int { return 3 },
		}}, time.Minute)
	c, err := connectPW(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 100; i++ {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		err = s.Run("fail")
		s.Close()
		var ee *ssh.ExitError
		if !errors.As(err, &ee) || ee.ExitStatus() != 3 {
			t.Fatalf("run %d: err = %v, want exit status 3", i, err)
		}
	}
}

func pwAnswers(qs []string) []string {
	a := make([]string, len(qs))
	for i := range a {
		a[i] = "pw"
	}
	return a
}

func TestConnectDidNotComeUpNamesHelperLog(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	sock := filepath.Join(t.TempDir(), "h.sock")
	_, err := Connect(context.Background(), "hpcx", &fakePrompter{}, ConnectOptions{
		SocketPath: sock, Spawn: func() error { return nil }, Wait: 100 * time.Millisecond,
	})
	want := filepath.Join(state, "labmill", "ssh-helper.log")
	if err == nil || !strings.Contains(err.Error(), "did not come up") || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want mention of %s", err, want)
	}
}
