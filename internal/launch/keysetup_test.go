package launch

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
	"labmill-client/internal/sshx/sshtest"
)

// keyUI answers the key setup questions and "uploads" the shown public key to
// the test server.
type keyUI struct {
	fakeUI
	srv *sshtest.Server
	t   *testing.T

	mu    sync.Mutex
	kinds []sshx.QuestionKind
	data  string
}

func (k *keyUI) Ask(_ context.Context, q sshx.Question) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kinds = append(k.kinds, q.Kind)
	switch q.Kind {
	case sshx.AskKeyCreate:
		return "yes", nil
	case sshx.AskNewPassphrase:
		return "", nil
	case sshx.AskKeyUpload:
		k.data = q.Data
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(q.Data))
		if err != nil {
			k.t.Errorf("public key %q: %v", q.Data, err)
		} else {
			k.srv.Authorize(pk)
		}
		return "yes", nil
	}
	k.t.Errorf("unexpected question %+v", q)
	return "", errors.New("unexpected")
}

func TestConnectGuidesKeyOnlyHost(t *testing.T) {
	home := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(home, ".ssh")}
	srv := sshtest.New(t, sshtest.Options{User: "alice", KeyOnly: true})
	host, port, _ := net.SplitHostPort(srv.Addr)
	pn, _ := strconv.Atoi(port)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "orca-login", HostName: host, User: "alice", Port: pn}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	calls := 0
	ui := &keyUI{srv: srv, t: t}
	sshConnect = func(ctx context.Context, alias string, pr sshx.Prompter, _ sshx.ConnectOptions) (*ssh.Client, error) {
		calls++
		return sshx.Dial(ctx, p, alias, pr, sshx.LoginOptions{KeyOnlyHosts: []string{host}})
	}
	cc := newConnCache(ui)
	cc.sshPaths = func() (sshconfig.Paths, error) { return p, nil }
	cc.keys = keySiteOf(site.Site{
		Hosts:     []sshconfig.Host{{Alias: "orca-login", HostName: host}},
		Auth:      map[string]string{"orca-login": site.AuthKey},
		KeyUpload: site.KeyUpload{URL: "https://reg.example.edu", Instruction: "Add the key."},
	})
	cl, err := cc.client(context.Background(), "orca-login")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	cl.Close()
	want := []sshx.QuestionKind{sshx.AskKeyCreate, sshx.AskNewPassphrase, sshx.AskKeyUpload}
	if len(ui.kinds) != len(want) {
		t.Fatalf("questions = %v, want %v", ui.kinds, want)
	}
	for i := range want {
		if ui.kinds[i] != want[i] {
			t.Fatalf("questions = %v, want %v", ui.kinds, want)
		}
	}
	if calls != 2 {
		t.Errorf("connected %d times, want 2 (the first try finds no key, the second uses the new one)", calls)
	}
	h, _, _ := sshconfig.Lookup(p, "orca-login")
	if len(h.IdentityFiles) != 1 {
		t.Errorf("IdentityFile not written: %v", h.IdentityFiles)
	}
}

// TestConnectPasswordHostSkipsGuide: a host the site does not list as key-only
// connects directly.
func TestConnectPasswordHostSkipsGuide(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "box", HostName: "box.example.edu", User: "alice"}}, "linux"); err != nil {
		t.Fatal(err)
	}
	cc := newConnCache(&fakeUI{})
	cc.sshPaths = func() (sshconfig.Paths, error) { return p, nil }
	cc.keys = keySite{hostNames: []string{"other.example.edu"}}
	if _, ok := cc.keyGuideFor("box"); ok {
		t.Error("guide offered for a password host")
	}
	cc.keys = keySite{hostNames: []string{"BOX.example.edu"}}
	if _, ok := cc.keyGuideFor("box"); !ok {
		t.Error("guide not offered for a key-only host (case-insensitive)")
	}
	if _, ok := cc.keyGuideFor("unknown-alias"); ok {
		t.Error("guide offered for an alias without a HostName")
	}
}
