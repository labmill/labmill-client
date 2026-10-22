package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshx"
)

// After the server rejects the password for a host, a second login to it in
// the same turn is refused without dialing; the next turn may try again.
func TestClientRefusesRetryAfterPasswordRejected(t *testing.T) {
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	calls := 0
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		calls++
		return nil, fmt.Errorf("ssh login to far: %w (check the username and password)", sshx.ErrPasswordRejected)
	}
	cc := newConnCache(nil)
	if _, err := cc.client(context.Background(), "far"); !errors.Is(err, sshx.ErrPasswordRejected) {
		t.Fatalf("first err = %v", err)
	}
	_, err := cc.client(context.Background(), "far")
	if err == nil || !strings.Contains(err.Error(), "sign-in to far failed a moment ago; check your user name and password, then ask me to try again") {
		t.Fatalf("second err = %v", err)
	}
	if calls != 1 {
		t.Errorf("dialed %d times in one turn, want 1", calls)
	}
	// Another host is unaffected.
	if _, err := cc.client(context.Background(), "other"); !errors.Is(err, sshx.ErrPasswordRejected) {
		t.Errorf("other host err = %v", err)
	}
	cc.newTurn()
	if _, err := cc.client(context.Background(), "far"); !errors.Is(err, sshx.ErrPasswordRejected) || calls != 3 {
		t.Errorf("after newTurn: err = %v, calls = %d", err, calls)
	}
}

// Failures that are not a password rejection (2FA, network) don't arm the guard.
func TestClientGuardOnlyForPasswordRejection(t *testing.T) {
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	calls := 0
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		calls++
		return nil, fmt.Errorf("%w: push denied", sshx.ErrTwoFactor)
	}
	cc := newConnCache(nil)
	for i := 0; i < 2; i++ {
		if _, err := cc.client(context.Background(), "far"); !errors.Is(err, sshx.ErrTwoFactor) {
			t.Fatalf("err = %v", err)
		}
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}
