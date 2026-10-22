package sshx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// shortSock returns a socket path short enough for AF_UNIX.
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s", "ssh.sock")
}

// serveBare runs a helper with no reachable upstream (Dial counts calls and
// fails) and returns Serve's result channel and the dial counter.
func serveBare(t *testing.T, sock string, idle time.Duration) (<-chan error, *atomic.Int32, context.CancelFunc) {
	t.Helper()
	var dials atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, HelperOptions{SocketPath: sock, IdleTimeout: idle,
			Dial: func(context.Context, sshconfig.Paths, string, Prompter, LoginOptions) (*ssh.Client, error) {
				dials.Add(1)
				return nil, errors.New("test: no upstream")
			}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	waitForSocket(t, sock)
	return done, &dials, cancel
}

// TestHoldKeepsHelperAlive: while a hold client is connected the helper does
// not idle-expire; after it closes, the normal idle expiry applies. Holding
// never dials upstream.
func TestHoldKeepsHelperAlive(t *testing.T) {
	sock := shortSock(t)
	idle := 150 * time.Millisecond
	done, dials, _ := serveBare(t, sock, idle)
	// waitForSocket's probe connection closed: the idle timer runs again.
	hc, err := Hold(context.Background(), ConnectOptions{SocketPath: sock})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("helper exited while held (err %v)", err)
	case <-time.After(5 * idle):
	}
	if got, err := hc.LoggedIn(); err != nil || len(got) != 0 {
		t.Errorf("LoggedIn = %v, %v; want none", got, err)
	}
	hc.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not idle-expire after the hold closed")
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("holding dialed upstream %d times", n)
	}
}

// TestHoldNoHelper: nothing listening means ErrNoHelper and no helper is
// started.
func TestHoldNoHelper(t *testing.T) {
	spawned := false
	_, err := Hold(context.Background(), ConnectOptions{SocketPath: filepath.Join(t.TempDir(), "none.sock"),
		Spawn: func() error { spawned = true; return nil }})
	if !errors.Is(err, ErrNoHelper) || spawned {
		t.Fatalf("err = %v, spawned = %v; want ErrNoHelper and no spawn", err, spawned)
	}
}

// TestHoldLoggedIn: the hold client sees the helper's logins without causing
// one.
func TestHoldLoggedIn(t *testing.T) {
	srv, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw",
		Commands: map[string]sshtest.Handler{"echo": echoHandler}}, time.Minute)
	ctx := context.Background()
	hc, err := Hold(ctx, ConnectOptions{SocketPath: sock})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer hc.Close()
	if got, err := hc.LoggedIn(); err != nil || len(got) != 0 {
		t.Fatalf("before login: %v, %v", got, err)
	}
	if srv.Logins() != 0 {
		t.Fatalf("holding logged in upstream (%d logins)", srv.Logins())
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Connect(ctx, "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	if got, err := hc.LoggedIn(); err != nil || !slices.Equal(got, []string{"hpcx"}) {
		t.Errorf("after login: %v, %v; want [hpcx]", got, err)
	}
	// The hold connection carries no channels.
	if _, err := hc.cl.NewSession(); err == nil {
		t.Error("a session opened on a hold connection")
	}
	if srv.Logins() != 1 {
		t.Errorf("upstream logins = %d, want 1", srv.Logins())
	}
}

// TestForgetLogins: the helper drops its login for a forgotten alias (so the
// next connect signs in again, with the settings as they are then) and
// leaves the others; with no helper running it is a no-op.
func TestForgetLogins(t *testing.T) {
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw",
		Commands: map[string]sshtest.Handler{"echo": echoHandler}}, time.Minute)
	ctx := context.Background()
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Connect(ctx, "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	hc, err := Hold(ctx, ConnectOptions{SocketPath: sock})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if err := ForgetLogins(ctx, ConnectOptions{SocketPath: sock}, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := hc.LoggedIn(); !slices.Equal(got, []string{"hpcx"}) {
		t.Fatalf("after forgetting another alias: %v", got)
	}
	if err := ForgetLogins(ctx, ConnectOptions{SocketPath: sock}, []string{"hpcx"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := hc.LoggedIn(); len(got) != 0 {
		t.Errorf("after forgetting hpcx: %v", got)
	}
	if err := ForgetLogins(ctx, ConnectOptions{SocketPath: shortSock(t)}, []string{"hpcx"}); err != nil {
		t.Errorf("no helper: %v", err)
	}
}

// TestKeeperAttachesLater: the Keeper waits for a helper, attaches when one
// appears, keeps it alive, and lets go when its context ends.
func TestKeeperAttachesLater(t *testing.T) {
	sock := shortSock(t)
	k := &Keeper{Options: ConnectOptions{SocketPath: sock}, Interval: 30 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { k.Run(ctx); close(ran) }()

	time.Sleep(100 * time.Millisecond)
	if _, held := k.LoggedIn(); held {
		t.Fatal("held before any helper ran")
	}
	idle := 150 * time.Millisecond
	done, dials, _ := serveBare(t, sock, idle)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, held := k.LoggedIn(); held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Keeper never attached to the helper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("helper idle-expired while the Keeper held it")
	case <-time.After(5 * idle):
	}
	cancel()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("Keeper.Run did not return after cancel")
	}
	if _, held := k.LoggedIn(); held {
		t.Error("still reports held after Run returned")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not idle-expire after the Keeper stopped")
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("the Keeper dialed upstream %d times", n)
	}
}

// TestKeeperBacksOffAfterHandshakeError: an error other than ErrNoHelper
// (an older helper) is retried after ErrorBackoff, not every Interval; no
// helper is retried every Interval.
func TestKeeperBacksOffAfterHandshakeError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		many bool
	}{
		{"old helper", errors.New("handshake failed"), false},
		{"no helper", ErrNoHelper, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			k := &Keeper{Interval: 5 * time.Millisecond, ErrorBackoff: time.Hour,
				dial: func(context.Context, ConnectOptions) (*HoldConn, error) {
					calls.Add(1)
					return nil, tc.err
				}}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			k.Run(ctx)
			if n := calls.Load(); tc.many && n < 3 || !tc.many && n != 1 {
				t.Errorf("dial called %d times", n)
			}
		})
	}
}
