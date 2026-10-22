package launch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshx"
)

// While a chat turn runs a terminal opens no dialog and clears no refusal:
// it needs a login the helper already holds.
func TestTerminalClientNoSignInWhileBusy(t *testing.T) {
	oldC, oldK := sshConnect, connectCached
	t.Cleanup(func() { sshConnect, connectCached = oldC, oldK })
	dialogs := 0
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		dialogs++
		return nil, errors.New("dialog")
	}
	connectCached = func(context.Context, string) (*ssh.Client, error) { return nil, errors.New("no login") }
	cc := newConnCache(nil)
	cc.markDenied("login")
	_, err := cc.terminalClient(context.Background(), "login", false)
	if !errors.Is(err, ErrTerminalSignInLater) {
		t.Fatalf("err = %v", err)
	}
	if dialogs != 0 {
		t.Errorf("%d sign-ins attempted while busy", dialogs)
	}
	if cc.deniedMsg("login") == "" {
		t.Error("the refusal was cleared while busy")
	}
}

// Without a running turn an explicit request clears an old refusal and signs in.
func TestTerminalClientSignsInWhenIdle(t *testing.T) {
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	calls := 0
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		calls++
		return nil, fmt.Errorf("ssh login: %w", sshx.ErrPasswordRejected)
	}
	cc := newConnCache(nil)
	cc.markDenied("login")
	if _, err := cc.terminalClient(context.Background(), "login", true); !errors.Is(err, sshx.ErrPasswordRejected) {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (the old refusal cleared, one attempt)", calls)
	}
	if cc.deniedMsg("login") == "" {
		t.Error("the new rejection isn't recorded")
	}
}

// Connects to one host are serialized: a request that waited while another
// one's password was rejected fails with the refusal and never prompts.
func TestTerminalClientWaiterGetsDenial(t *testing.T) {
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	var mu sync.Mutex
	calls := 0
	started := make(chan struct{})
	release := make(chan struct{})
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(started)
			<-release
		}
		return nil, fmt.Errorf("ssh login: %w", sshx.ErrPasswordRejected)
	}
	cc := newConnCache(nil)
	errs := make(chan error, 2)
	go func() { _, err := cc.terminalClient(context.Background(), "login", true); errs <- err }()
	<-started
	go func() { _, err := cc.terminalClient(context.Background(), "login", true); errs <- err }()
	time.Sleep(50 * time.Millisecond) // the second request is waiting for the slot
	close(release)
	e1, e2 := <-errs, <-errs
	rejected := 0
	for _, e := range []error{e1, e2} {
		if e == nil {
			t.Error("a request succeeded")
		} else if errors.Is(e, sshx.ErrPasswordRejected) {
			rejected++
		}
	}
	if rejected != 1 {
		t.Errorf("%d requests got the rejection itself (%v / %v), want 1: the other gets the refusal message", rejected, e1, e2)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("sign-in attempts = %d, want 1", calls)
	}
}

// A waiting request ends with its context.
func TestTerminalConnectWaitCanceled(t *testing.T) {
	k := termConnectKey{newConnCache(nil), "h"}
	rel, waited, err := acquireTermConnect(context.Background(), k)
	if err != nil || waited {
		t.Fatal(err, waited)
	}
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := acquireTermConnect(ctx, k); err == nil {
		t.Error("wait didn't end with the context")
	}
}
