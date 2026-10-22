package sshx

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSocketNameCarriesHelperVersion(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/run/user/1000", "labmill", "ssh-v"+HelperVersion+".sock")
	if got != want {
		t.Fatalf("SocketPath = %q, want %q", got, want)
	}
}

func serveAt(t *testing.T, sock string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: time.Minute}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("helper did not stop")
		}
	})
	waitForSocket(t, sock)
}

// TestHelperVersionsCoexist: helpers of two versions run side by side in one
// directory (separate sockets and locks), and a client for the current
// version never connects to the older helper's socket.
func TestHelperVersionsCoexist(t *testing.T) {
	root, err := os.MkdirTemp("", "hv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("XDG_RUNTIME_DIR", root)
	dir := filepath.Join(root, "labmill")

	// "Old" helper: the unversioned name every pre-versioning helper uses.
	oldSock := filepath.Join(dir, "ssh.sock")
	serveAt(t, oldSock)

	// The current helper starts although the old one holds ssh.lock.
	cur, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(cur) != dir || cur == oldSock {
		t.Fatalf("current socket %q should be a different file in %q", cur, dir)
	}
	serveAt(t, cur)
	for _, name := range []string{"ssh.lock", "ssh-v" + HelperVersion + ".lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("lock file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(oldSock); err != nil {
		t.Errorf("the old helper's socket vanished: %v", err)
	}
}

// TestNewClientNeverUsesOldSocket: with only an old helper listening, the
// current client finds nothing (ConnectCached refuses, Connect spawns a new
// helper) and the old helper sees no connection.
func TestNewClientNeverUsesOldSocket(t *testing.T) {
	root, err := os.MkdirTemp("", "hv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("XDG_RUNTIME_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, "labmill"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldSock := filepath.Join(root, "labmill", "ssh.sock")
	ln, err := net.Listen("unix", oldSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()

	if _, err := ConnectCached(context.Background(), "hpcx", ConnectOptions{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("ConnectCached err = %v, want ErrNotConnected", err)
	}
	spawned := 0
	_, err = Connect(context.Background(), "hpcx", &fakePrompter{}, ConnectOptions{
		Spawn: func() error { spawned++; return nil }, Wait: 100 * time.Millisecond})
	if err == nil || spawned == 0 || !strings.Contains(err.Error(), "ssh-v"+HelperVersion+".sock") {
		t.Fatalf("Connect: err = %v, spawned = %d (want a spawn and the versioned socket in the error)", err, spawned)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the old helper's socket got %d connections", n)
	}
}
