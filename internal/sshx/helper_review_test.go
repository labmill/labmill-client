package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// reviewEnv is helperEnv with caller-chosen login options, an optional dial
// counter and the known_hosts content under the caller's control.
func reviewEnv(t *testing.T, so sshtest.Options, login LoginOptions, knownHosts func(*sshtest.Server) string) (*sshtest.Server, string, *atomic.Int32) {
	t.Helper()
	srv := sshtest.New(t, so)
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "hpcx", HostName: h, User: so.User, Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	kh := srv.KnownHostsLine()
	if knownHosts != nil {
		kh = knownHosts(srv)
	}
	khPath := filepath.Join(p.SSHDir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(kh+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	login.KnownHosts = khPath
	dir, err := os.MkdirTemp("", "yh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s", "ssh.sock")
	dials := new(atomic.Int32)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: time.Minute, Paths: p, Login: login,
			Dial: func(ctx context.Context, p sshconfig.Paths, alias string, pr Prompter, o LoginOptions) (*ssh.Client, error) {
				dials.Add(1)
				return Dial(ctx, p, alias, pr, o)
			}})
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
	return srv, sock, dials
}

func connectPW(sock string) (*ssh.Client, error) {
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	return Connect(context.Background(), "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
}

func TestRelayErrorRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		err      error
		code     string
		sentinel error
	}{
		{ErrTwoFactor, "2fa", ErrTwoFactor},
		{ErrHostKeyMismatch, "hostkey", ErrHostKeyMismatch},
		{ErrAuthDenied, "denied", ErrAuthDenied},
		{fmt.Errorf("login: %w (x)", ErrPasswordRejected), "password", ErrPasswordRejected},
		{context.Canceled, "canceled", context.Canceled},
		{errors.New("a: b: c"), "error", nil},
	} {
		ins := EncodeRelayError(tc.err)
		if !strings.HasPrefix(ins, "labmill-relay-error:"+tc.code+":") {
			t.Errorf("%v encoded as %q", tc.err, ins)
		}
		got, ok := DecodeRelayError(ins)
		if !ok || got.Error() != tc.err.Error() {
			t.Errorf("decode %q = %v, %v", ins, got, ok)
		}
		if tc.sentinel != nil && !errors.Is(got, tc.sentinel) {
			t.Errorf("decoded %q does not match its sentinel", ins)
		}
	}
	if _, ok := DecodeRelayError("labmill-relay:0:h"); ok {
		t.Error("decoded a non-error instruction")
	}
}

func TestHelperRelaysLoginFailures(t *testing.T) {
	const help = "Enroll at https://duo.example.edu/enroll"
	t.Run("not enrolled", func(t *testing.T) {
		_, sock, _ := reviewEnv(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNotEnrolled},
			LoginOptions{PushTimeout: 3 * time.Second, TwoFactorHelp: help}, nil)
		_, err := connectPW(sock)
		if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), help) {
			t.Fatalf("err = %v, want ErrTwoFactor with the help text", err)
		}
		if strings.Contains(err.Error(), "helper login") {
			t.Errorf("error carries a helper prefix: %v", err)
		}
	})
	t.Run("no push", func(t *testing.T) {
		_, sock, _ := reviewEnv(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNever},
			LoginOptions{PushTimeout: 300 * time.Millisecond}, nil)
		if _, err := connectPW(sock); !errors.Is(err, ErrTwoFactor) {
			t.Fatalf("err = %v, want ErrTwoFactor", err)
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		_, sock, _ := reviewEnv(t, sshtest.Options{User: "alice", Password: "pw"}, LoginOptions{PushTimeout: time.Second}, nil)
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "wrong"}}
		_, err := Connect(context.Background(), "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
		if !errors.Is(err, ErrAuthDenied) {
			t.Fatalf("err = %v, want ErrAuthDenied", err)
		}
	})
	t.Run("host key mismatch", func(t *testing.T) {
		other := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
		h, _, _ := net.SplitHostPort(other.Addr)
		_, sock, _ := reviewEnv(t, sshtest.Options{User: "alice", Password: "pw"}, LoginOptions{PushTimeout: time.Second},
			func(s *sshtest.Server) string {
				// the right address with somebody else's key
				_, port, _ := net.SplitHostPort(s.Addr)
				line := other.KnownHostsLine()
				_, rest, _ := strings.Cut(line, " ")
				return "[" + h + "]:" + port + " " + rest
			})
		if _, err := connectPW(sock); !errors.Is(err, ErrHostKeyMismatch) {
			t.Fatalf("err = %v, want ErrHostKeyMismatch", err)
		}
	})
}

func TestServeConcurrentStartsOneWinner(t *testing.T) {
	dir, err := os.MkdirTemp("", "yh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s", "ssh.sock")
	const n = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	refused := new(atomic.Int32)
	results := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: time.Minute})
			results <- err
		}()
	}
	close(start)
	deadline := time.After(5 * time.Second)
	for i := 0; i < n-1; i++ {
		select {
		case err := <-results:
			if err == nil || !strings.Contains(err.Error(), "already running") {
				t.Fatalf("loser returned %v, want an 'already running' error", err)
			}
			refused.Add(1)
		case <-deadline:
			t.Fatalf("only %d of %d losers returned", refused.Load(), n-1)
		}
	}
	// The winner is still serving and reachable. A loser can return before
	// the winner has bound the socket, so give it time to come up.
	waitForSocket(t, sock)
	select {
	case err := <-results:
		t.Fatalf("a second helper stopped: %v", err)
	default:
	}
	cancel()
	wg.Wait()
}

func TestServeRefusesSymlinkedDir(t *testing.T) {
	real, err := os.MkdirTemp("", "yh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(real) })
	link := real + "-link"
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable: ", err)
	}
	t.Cleanup(func() { os.Remove(link) })
	err = Serve(context.Background(), HelperOptions{SocketPath: filepath.Join(link, "ssh.sock")})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Serve through a symlinked dir = %v, want a refusal", err)
	}
}

func TestUpstreamDeathDisconnectsLocalClients(t *testing.T) {
	srv, sock, dials := reviewEnv(t, sshtest.Options{User: "alice", Password: "pw"}, LoginOptions{PushTimeout: time.Second}, nil)
	c, err := connectPW(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waited := make(chan struct{})
	go func() { c.Wait(); close(waited) }()
	srv.Close()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("local client was not disconnected after the upstream died")
	}
	// The dead upstream must have been evicted: the next Connect logs in
	// again (it fails here because the test server is gone).
	before := dials.Load()
	if _, err := connectPW(sock); err == nil {
		t.Fatal("Connect reused a dead upstream")
	}
	if dials.Load() != before+1 {
		t.Errorf("dials = %d, want %d (a new login attempt)", dials.Load(), before+1)
	}
}

func TestKeepaliveClosesSilentUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := newKey(t)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(priv)
	go func() {
		// A server that never reads global requests: the keepalive can't be answered.
		_, _, _, err := ssh.NewServerConn(sc, cfg)
		_ = err
	}()
	conn, chans, reqs, err := ssh.NewClientConn(cc, "x", &ssh.ClientConfig{User: "u", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	done := make(chan struct{})
	go keepalive(client, done, 50*time.Millisecond, 100*time.Millisecond, nil)
	waited := make(chan struct{})
	go func() { client.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive did not close an unresponsive upstream")
	}
	close(done)
}

func TestConnectDoesNotSpawnOnOtherDialErrors(t *testing.T) {
	// A path longer than sun_path fails with something other than
	// "nothing listening"; Connect must not spawn.
	sock := "/tmp/" + strings.Repeat("x", 200) + "/ssh.sock"
	spawned := false
	_, err := Connect(context.Background(), "hpcx", &fakePrompter{}, ConnectOptions{SocketPath: sock,
		Spawn: func() error { spawned = true; return nil }, Wait: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("Connect succeeded")
	}
	if spawned && runtime.GOOS != "windows" {
		t.Error("spawned on a non-'nothing listening' error")
	}
}

func TestNothingListening(t *testing.T) {
	_, err := net.Dial("unix", filepath.Join(t.TempDir(), "missing.sock"))
	if err == nil || !nothingListening(err) {
		t.Errorf("missing socket: %v should count as nothing listening", err)
	}
}

func newKey(t *testing.T) (ssh.PublicKey, ssh.Signer, error) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s.PublicKey(), s, nil
}
