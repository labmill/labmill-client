package launch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/config"
	"labmill-client/internal/sshx"
)

// A non-password ErrAuthDenied (no password was sent) doesn't arm the guard.
func TestClientGuardNotForPlainAuthDenied(t *testing.T) {
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	calls := 0
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		calls++
		return nil, fmt.Errorf("ssh login to far: %w (none of the offered methods)", sshx.ErrAuthDenied)
	}
	cc := newConnCache(nil)
	for i := 0; i < 2; i++ {
		if _, err := cc.client(context.Background(), "far"); !errors.Is(err, sshx.ErrAuthDenied) {
			t.Fatalf("err = %v", err)
		}
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// A password rejection on a cluster's login host also blocks its transfer
// host and the other hosts of the same institution, not other institutions.
func TestClientGuardMarksSiblingHosts(t *testing.T) {
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	calls := 0
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		calls++
		return nil, fmt.Errorf("ssh login: %w", sshx.ErrPasswordRejected)
	}
	cc := newConnCache(nil)
	clusters := []config.Cluster{
		{Name: "a", SSHHost: "login", FilesHost: "files"},
		{Name: "b", SSHHost: "login2"},
		{Name: "c", Site: "other.example.org", SSHHost: "far"},
	}
	cc.clusters = func() []config.Cluster { return clusters }
	cc.homeDomain = "home.example.edu"
	// A cluster added later (add_cluster) is seen at rejection time.
	clusters = append(clusters, config.Cluster{Name: "d", SSHHost: "login3"})
	if _, err := cc.client(context.Background(), "login"); !errors.Is(err, sshx.ErrPasswordRejected) {
		t.Fatal(err)
	}
	for _, h := range []string{"files", "login2", "login3", "login"} {
		_, err := cc.client(context.Background(), h)
		if err == nil || errors.Is(err, sshx.ErrPasswordRejected) {
			t.Errorf("%s: err = %v, want the guard's refusal", h, err)
			continue
		}
		want := "a sign-in at this institution failed a moment ago; check your user name and password, then ask me to try again"
		if h == "login" {
			want = "sign-in to login failed a moment ago; check your user name and password, then ask me to try again"
		}
		if err.Error() != want {
			t.Errorf("%s: message %q, want %q", h, err, want)
		}
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if _, err := cc.client(context.Background(), "far"); !errors.Is(err, sshx.ErrPasswordRejected) || calls != 2 {
		t.Errorf("other institution: err = %v, calls = %d", err, calls)
	}
}
