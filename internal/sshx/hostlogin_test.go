package sshx

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// LoginOptions.Hosts replaces the two-factor help for one host (compared
// case-insensitively on its HostName); other hosts keep the default text.
func TestDialPerHostTwoFactorHelp(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNotEnrolled})
	host, port, _ := net.SplitHostPort(srv.Addr)
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "far", HostName: host, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dial := func(hosts map[string]HostLogin) error {
		pr := &scriptPrompter{onAsk: func(q Question) (string, error) {
			if q.Kind == AskPassword {
				return "pw", nil
			}
			return "", nil
		}}
		_, err := Dial(context.Background(), p, "far", pr, LoginOptions{TwoFactorHelp: "HOME-HELP", Hosts: hosts})
		return err
	}
	err := dial(map[string]HostLogin{strings.ToLower(host): {TwoFactorHelp: "FAR-HELP"}})
	if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), "FAR-HELP") || strings.Contains(err.Error(), "HOME-HELP") {
		t.Errorf("per-host help: %v", err)
	}
	// The map key is lowercase; a host name written in capitals still matches.
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "far", HostName: strings.ToUpper(host), User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	err = dial(map[string]HostLogin{strings.ToLower(host): {TwoFactorHelp: "FAR-HELP"}})
	if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), "FAR-HELP") {
		t.Errorf("upper-case HostName: %v", err)
	}
	// No override for this host: the default text.
	err = dial(map[string]HostLogin{"other.example.org": {TwoFactorHelp: "FAR-HELP"}})
	if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), "HOME-HELP") {
		t.Errorf("default help: %v", err)
	}
}
