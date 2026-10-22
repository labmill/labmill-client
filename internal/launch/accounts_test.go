package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
)

// accountPaths writes Labmill's own file with two hosts and the user's own
// config with one more; the user's config includes Labmill's file.
func accountPaths(t *testing.T) sshconfig.Paths {
	t.Helper()
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	write := func(path, text string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(p.LabmillConfig(), "Host home-login\n    HostName login.home.example.edu\n    User alice\n\nHost home-files\n    HostName files.home.example.edu\n    User alice\n\nHost far-login\n    HostName login.far.example.org\n    User farhat\n")
	write(p.UserConfig(), "Include config.d/labmill\n\nHost mine\n    HostName mine.example.net\n    User carol\n")
	return p
}

func TestKnownUsername(t *testing.T) {
	p := accountPaths(t)
	clusters := []config.Cluster{
		{Name: "nouser", SSHHost: "unknown-alias"},
		{Name: "main", SSHHost: "home-login"},
		{Name: "other", Site: "home.example.edu", SSHHost: "mine"},
		{Name: "farone", Site: "far.example.org", SSHHost: "far-login"},
	}
	tests := []struct {
		name, domain, wantUser, wantFrom string
	}{
		{"home, first cluster with a user", "home.example.edu", "alice", "main"},
		{"another institution", "far.example.org", "farhat", "farone"},
		{"institution without clusters", "none.example.com", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, from := knownUsername(p, clusters, "home.example.edu", tt.domain)
			if u != tt.wantUser || from != tt.wantFrom {
				t.Errorf("got %q from %q, want %q from %q", u, from, tt.wantUser, tt.wantFrom)
			}
		})
	}
	// Order is clusters.yaml order: the user's own entry first wins.
	u, from := knownUsername(p, []config.Cluster{{Name: "other", SSHHost: "mine"}, clusters[1]}, "home.example.edu", "home.example.edu")
	if u != "carol" || from != "other" {
		t.Errorf("order: got %q from %q", u, from)
	}
}

func TestSetClusterAccountEditsOnlyLabmillFile(t *testing.T) {
	p := accountPaths(t)
	c := config.Cluster{Name: "main", SSHHost: "home-login", FilesHost: "home-files"}
	ch, _, err := planClusterAccount(p, c, nil, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if ch.File != p.LabmillConfig() || len(ch.Lines) != 2 || !strings.Contains(ch.Lines[0], "User alice -> bob") {
		t.Errorf("plan = %+v", ch)
	}
	changed, err := setClusterAccount(p, c, nil, "bob", "linux")
	if err != nil || len(changed) != 2 {
		t.Fatalf("changed %v, %v", changed, err)
	}
	for _, a := range []string{"home-login", "home-files"} {
		if h, _, _ := sshconfig.Lookup(p, a); h.User != "bob" {
			t.Errorf("%s user = %q", a, h.User)
		}
	}
	if h, _, _ := sshconfig.Lookup(p, "far-login"); h.User != "farhat" {
		t.Errorf("another entry changed: %q", h.User)
	}

	// An entry of the user's own config is refused and left alone.
	before, _ := os.ReadFile(p.UserConfig())
	if _, err := setClusterAccount(p, config.Cluster{Name: "x", SSHHost: "mine"}, nil, "bob", "linux"); err == nil || !strings.Contains(err.Error(), "own SSH settings") {
		t.Errorf("user entry: %v", err)
	}
	// One refused host changes nothing at all.
	if _, err := setClusterAccount(p, config.Cluster{Name: "y", SSHHost: "far-login", FilesHost: "mine"}, nil, "zed", "linux"); err == nil {
		t.Error("mixed entries accepted")
	}
	if h, _, _ := sshconfig.Lookup(p, "far-login"); h.User != "farhat" {
		t.Errorf("partial change: %q", h.User)
	}
	after, _ := os.ReadFile(p.UserConfig())
	if string(before) != string(after) {
		t.Error("the user's own config was rewritten")
	}
	if _, err := setClusterAccount(p, config.Cluster{Name: "z", SSHHost: "missing"}, nil, "bob", "linux"); err == nil {
		t.Error("missing entry accepted")
	}
}

// An entry that another cluster also uses (login, files, or in its jump
// chain) is never changed: the whole change is refused.
func TestSetClusterAccountRefusesSharedEntries(t *testing.T) {
	p := accountPaths(t)
	// far-login jumps through home-files for the cluster "jumper".
	data, _ := os.ReadFile(p.LabmillConfig())
	if err := os.WriteFile(p.LabmillConfig(), []byte(strings.Replace(string(data), "HostName login.far.example.org", "HostName login.far.example.org\n    ProxyJump home-files", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	clusters := []config.Cluster{
		{Name: "main", SSHHost: "home-login", FilesHost: "home-files"},
		{Name: "sibling", SSHHost: "mine", FilesHost: "home-files"},
		{Name: "twin", SSHHost: "home-login"},
		{Name: "jumper", SSHHost: "far-login"},
	}
	tests := []struct {
		name    string
		cluster config.Cluster
		want    string
	}{
		{"shared login and files entries", clusters[0], "also used by the cluster twin"},
		{"shared login entry", config.Cluster{Name: "f", SSHHost: "far-login"}, "jumper"},
		{"entry used as a jump host", config.Cluster{Name: "files-only", SSHHost: "home-files"}, "sibling, jumper"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := os.ReadFile(p.LabmillConfig())
			_, err := setClusterAccount(p, tt.cluster, clusters, "bob", "linux")
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "Settings") {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			after, _ := os.ReadFile(p.LabmillConfig())
			if string(before) != string(after) {
				t.Error("the file changed")
			}
		})
	}
	// Not shared: changed.
	if _, err := setClusterAccount(p, config.Cluster{Name: "solo", SSHHost: "far-login"}, []config.Cluster{{Name: "main", SSHHost: "home-login"}}, "bob", "linux"); err != nil {
		t.Errorf("unshared: %v", err)
	}
}

func TestWireAccounts(t *testing.T) {
	p := accountPaths(t)
	cfg := &config.Config{Clusters: []config.Cluster{{Name: "main", SSHHost: "home-login"}}}
	cc := newConnCache(nil)
	env := &app.Env{Config: cfg}
	wireAccounts(env, cc, cfg, "home.example.edu", func() (sshconfig.Paths, error) { return p, nil }, "linux")
	if u, from := env.KnownUsername("home.example.edu"); u != "alice" || from != "main" {
		t.Errorf("known = %q %q", u, from)
	}
	cc.denied = map[string]string{"home-login": "denied"}
	var forgotten []string
	old := forgetLogins
	t.Cleanup(func() { forgetLogins = old })
	forgetLogins = func(_ context.Context, a []string) error { forgotten = a; return nil }
	if _, err := env.SetClusterAccount(context.Background(), cfg.Clusters[0], "bob"); err != nil {
		t.Fatal(err)
	}
	if cc.denied["home-login"] == "" {
		t.Error("the per-turn denial was cleared")
	}
	if len(forgotten) != 1 || forgotten[0] != "home-login" {
		t.Errorf("helper asked to forget %v", forgotten)
	}
	// A helper that can't forget: nothing is changed.
	forgetLogins = func(context.Context, []string) error { return errors.New("older version") }
	if _, err := env.SetClusterAccount(context.Background(), cfg.Clusters[0], "carl"); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("helper failure: %v", err)
	}
	if h, _, _ := sshconfig.Lookup(p, "home-login"); h.User != "bob" {
		t.Errorf("user = %q, want bob (unchanged by the failed attempt)", h.User)
	}
}

// markDenied: the refusal asks for the user name too, and the rejection the
// model sees points at set_cluster_account.
func TestPasswordRejectedHintsAtAccountName(t *testing.T) {
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		return nil, fmt.Errorf("ssh login: %w", sshx.ErrPasswordRejected)
	}
	cc := newConnCache(nil)
	_, err := cc.client(context.Background(), "far")
	if !errors.Is(err, sshx.ErrPasswordRejected) || !strings.Contains(err.Error(), "set_cluster_account") {
		t.Errorf("first err = %v", err)
	}
	_, err = cc.client(context.Background(), "far")
	if err == nil || !strings.Contains(err.Error(), "check your user name and password") {
		t.Errorf("second err = %v", err)
	}
}
