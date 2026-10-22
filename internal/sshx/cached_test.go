package sshx

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"labmill-client/internal/sshx/sshtest"
)

// TestConnectCached: without an open login the helper refuses without
// logging in upstream; after a normal login the cached-only client works
// and still no second upstream login happens.
func TestConnectCached(t *testing.T) {
	srv, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw",
		Commands: map[string]sshtest.Handler{"echo": echoHandler}}, time.Minute)
	ctx := context.Background()

	if _, err := ConnectCached(ctx, "hpcx", ConnectOptions{SocketPath: sock}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("before login: err = %v, want ErrNotConnected", err)
	}
	if srv.Logins() != 0 {
		t.Fatalf("a cached-only connect logged in upstream (%d logins)", srv.Logins())
	}

	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c1, err := Connect(ctx, "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c1.Close()

	c2, err := ConnectCached(ctx, "hpcx", ConnectOptions{SocketPath: sock})
	if err != nil {
		t.Fatalf("after login: %v", err)
	}
	defer c2.Close()
	s, err := c2.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.Stdout = &out
	if err := s.Run("echo cached"); err != nil || out.String() != "cached" {
		t.Fatalf("echo = %q, %v", out.String(), err)
	}
	if srv.Logins() != 1 {
		t.Errorf("upstream logins = %d, want 1", srv.Logins())
	}

	// Another alias the helper never logged in to is refused, too.
	if _, err := ConnectCached(ctx, "other", ConnectOptions{SocketPath: sock}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("unknown alias: err = %v", err)
	}
}

// TestConnectCachedNoHelper: nothing listening means ErrNotConnected and no
// helper is started.
func TestConnectCachedNoHelper(t *testing.T) {
	dir := t.TempDir()
	spawned := false
	_, err := ConnectCached(context.Background(), "hpcx", ConnectOptions{SocketPath: filepath.Join(dir, "none.sock"),
		Spawn: func() error { spawned = true; return nil }})
	if !errors.Is(err, ErrNotConnected) || spawned {
		t.Fatalf("err = %v, spawned = %v", err, spawned)
	}
}

func TestSplitHelperUser(t *testing.T) {
	if a, c := splitHelperUser("hpcx"); a != "hpcx" || c {
		t.Errorf("plain = %q %v", a, c)
	}
	if a, c := splitHelperUser(cachedOnlyPrefix + "hpcx"); a != "hpcx" || !c {
		t.Errorf("cached = %q %v", a, c)
	}
}
