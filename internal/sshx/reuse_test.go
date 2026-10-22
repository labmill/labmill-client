package sshx

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// stallConn is a transport that can stop answering without closing (a link
// that went stale while the laptop slept): reads block until Close, writes
// are swallowed.
type stallConn struct {
	net.Conn
	stalled atomic.Bool
	closed  chan struct{}
	once    sync.Once
}

func (c *stallConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.stalled.Load() {
		<-c.closed
		return 0, net.ErrClosed
	}
	return n, err
}

func (c *stallConn) Write(p []byte) (int, error) {
	if c.stalled.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// reuseEnv is a helper whose upstream logins run over stallConns, with a
// keepalive interval of an hour (no background keepalive during a test) and
// a short keepalive timeout. probes counts the reuse keepalives sent.
type reuseEnv struct {
	co     ConnectOptions
	probes atomic.Int64

	mu    sync.Mutex
	conns []*stallConn
}

func (e *reuseEnv) stallFirst() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.conns[0].stalled.Store(true)
}

func newReuseEnv(t *testing.T) *reuseEnv {
	t.Helper()
	e := &reuseEnv{}
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, _ sshconfig.Paths, _ string, pr Prompter, opts LoginOptions) (*ssh.Client, error) {
		var d net.Dialer
		raw, err := d.DialContext(ctx, "tcp", srv.Addr)
		if err != nil {
			return nil, err
		}
		c := &stallConn{Conn: raw, closed: make(chan struct{})}
		e.mu.Lock()
		e.conns = append(e.conns, c)
		e.mu.Unlock()
		opts.User = "alice"
		return Login(ctx, c, srv.Addr, pr, opts)
	}
	dir, err := os.MkdirTemp("", "yh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s", "ssh.sock")
	// Reset after the helper stopped (cleanups run last-in first-out).
	t.Cleanup(func() { probeHook = nil })
	probeHook = func() { e.probes.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: time.Minute, Paths: p, Dial: dial,
			KeepaliveInterval: time.Hour, KeepaliveTimeout: 200 * time.Millisecond,
			Login: LoginOptions{KnownHosts: kh}})
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
	e.co = ConnectOptions{SocketPath: sock, Spawn: noSpawn}
	return e
}

// TestHelperReuseSkipsRecentKeepalive: a login whose keepalive answered
// within the keepalive interval is handed out without another keepalive (a
// slow but working link is never re-checked, so never dropped, while rclone
// opens connection after connection).
func TestHelperReuseSkipsRecentKeepalive(t *testing.T) {
	e := newReuseEnv(t)
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c1, err := Connect(context.Background(), "hpcx", pr, e.co)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	for i := 0; i < 3; i++ {
		cl, err := ConnectCached(context.Background(), "hpcx", e.co)
		if err != nil {
			t.Fatalf("ConnectCached %d: %v", i, err)
		}
		cl.Close()
	}
	c2, err := Connect(context.Background(), "hpcx", pr, e.co)
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	if n := e.probes.Load(); n != 1 {
		t.Fatalf("reuse keepalives = %d, want 1 (the first reuse; later ones within the interval are skipped)", n)
	}
	if n := pr.count(AskPassword); n != 1 {
		t.Fatalf("password asked %d times, want 1", n)
	}
}

// TestHelperDropsStaleLogin: a held upstream login that no longer answers is
// not handed to a new client. ConnectCached reports no login (the monitor
// never prompts), the clients of the stale login are closed, and Connect
// logs in again, with its prompts.
func TestHelperDropsStaleLogin(t *testing.T) {
	e := newReuseEnv(t)
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c1, err := Connect(context.Background(), "hpcx", pr, e.co)
	if err != nil {
		t.Fatal(err)
	}
	e.stallFirst()
	if _, err := ConnectCached(context.Background(), "hpcx", e.co); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("ConnectCached on a stale login: %v, want ErrNotConnected", err)
	}
	waited := make(chan struct{})
	go func() { c1.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the client of the stale login was not closed")
	}
	c2, err := Connect(context.Background(), "hpcx", pr, e.co)
	if err != nil {
		t.Fatalf("Connect after the stale login: %v", err)
	}
	defer c2.Close()
	if n := pr.count(AskPassword); n != 2 {
		t.Fatalf("password asked %d times, want 2", n)
	}
	s, err := c2.NewSession()
	if err != nil {
		t.Fatalf("session on the new login: %v", err)
	}
	s.Close()
}

// TestHelperLocalKeepaliveFollowsUpstream: a local client's keepalive is
// answered once the upstream login answered, and not while it is stale (so
// the token source can tell a stale login from a slow command).
func TestHelperLocalKeepaliveFollowsUpstream(t *testing.T) {
	e := newReuseEnv(t)
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c1, err := Connect(context.Background(), "hpcx", pr, e.co)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	ask := func() error {
		res := make(chan error, 1)
		go func() {
			_, _, err := c1.SendRequest("keepalive@openssh.com", true, nil)
			res <- err
		}()
		select {
		case err := <-res:
			return err
		case <-time.After(150 * time.Millisecond):
			return errors.New("no answer")
		}
	}
	if err := ask(); err != nil {
		t.Fatalf("keepalive on a live login: %v", err)
	}
	e.stallFirst()
	if err := ask(); err == nil {
		t.Fatal("keepalive answered while the upstream login is stale")
	}
}
