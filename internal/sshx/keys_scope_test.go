package sshx

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// useAgent makes agentDial serve kr for the test and counts the connections.
func useAgent(t *testing.T, kr agent.Agent) *int {
	t.Helper()
	old := agentDial
	t.Cleanup(func() { agentDial = old })
	calls := new(int)
	agentDial = func() (net.Conn, error) {
		*calls++
		a, b := net.Pipe()
		go func() { agent.ServeAgent(kr, b); b.Close() }()
		return a, nil
	}
	return calls
}

// TestAgentSignersStuckAgent: an agent that never answers does not hang the
// login (3 s deadline), and canceling ctx ends the wait at once.
func TestAgentSignersStuckAgent(t *testing.T) {
	old := agentDial
	t.Cleanup(func() { agentDial = old })
	var ends []net.Conn
	agentDial = func() (net.Conn, error) {
		a, b := net.Pipe() // b is never read: the request blocks
		ends = append(ends, b)
		return a, nil
	}
	start := time.Now()
	sigs, closeAgent := agentSigners(context.Background())
	closeAgent()
	if len(sigs) != 0 || time.Since(start) > 6*time.Second {
		t.Errorf("stuck agent: %d signers after %v", len(sigs), time.Since(start))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	agentSigners(ctx)
	if time.Since(start) > 2*time.Second {
		t.Errorf("a canceled ctx did not end the wait (%v)", time.Since(start))
	}
	for _, c := range ends {
		c.Close()
	}
}

// TestPasswordHostGetsNoDefaultOrAgentKeys: on a password+2FA host only
// IdentityFile keys are offered; the default key file and the agent's keys
// stay home even when the server would accept them.
func TestPasswordHostGetsNoDefaultOrAgentKeys(t *testing.T) {
	home := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(home, ".ssh")}
	kp, err := GenerateKey(p, "box", "")
	if err != nil {
		t.Fatal(err)
	}
	defPub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.PublicKey))
	_, priv, _ := ed25519Key(t)
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	agSigner, _ := ssh.NewSignerFromKey(priv)
	calls := useAgent(t, kr)
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", AuthorizedKeys: []ssh.PublicKey{defPub, agSigner.PublicKey()}})
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "box", HostName: h, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Dial(context.Background(), p, "box", pr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if pr.count(AskPassword) != 1 {
		t.Errorf("password asked %d times; the default or agent key was offered to a password host", pr.count(AskPassword))
	}
	if *calls != 0 {
		t.Errorf("the agent was contacted %d times for a password host", *calls)
	}
}

// TestEncryptedKeyInAgentSkipsPassphrase: on a key-only host an encrypted
// key whose .pub is in the agent is not asked for.
func TestEncryptedKeyInAgentSkipsPassphrase(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	kp, err := GenerateKey(p, "orca", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(kp.Path)
	raw, err := ssh.ParseRawPrivateKeyWithPassphrase(data, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: raw}); err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.PublicKey))
	srv.Authorize(pk)
	useAgent(t, kr)
	pr := &scriptPrompter{}
	c, err := Dial(context.Background(), p, "orca", pr, LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	if n := pr.count(AskKeyPassphrase); n != 0 {
		t.Errorf("passphrase asked %d times although the agent holds the key", n)
	}
}

// TestKeyOnlyTooManyKeys: a key-only server that hangs up after the first
// refused key gives ErrKeyRejected with a hint, not a retry without keys.
func TestKeyOnlyTooManyKeys(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := sshtest.New(t, sshtest.Options{User: "alice", KeyOnly: true, MaxAuthTries: 1})
	h, port, _ := net.SplitHostPort(srv.Addr)
	var ids []string
	for _, n := range []string{"k1", "k2", "k3"} {
		writeKey(t, filepath.Join(p.SSHDir, n))
		ids = append(ids, "~/.ssh/"+n)
	}
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "orca", HostName: h, User: "alice", Port: atoi(t, port), IdentityFiles: ids}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Dial(context.Background(), p, "orca", &scriptPrompter{}, LoginOptions{KeyOnlyHosts: []string{h}})
	if !errors.Is(err, ErrKeyRejected) || !strings.Contains(err.Error(), "too many keys") {
		t.Errorf("Dial = %v, want ErrKeyRejected with a too-many-keys hint", err)
	}
}

// TestKeyGuideUserOwnedAlias: an alias that only the user's own ssh config
// defines is not edited; the user gets a notice with the line to add and the
// upload step still follows.
func TestKeyGuideUserOwnedAlias(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeKey(t, filepath.Join(p.SSHDir, "id_ed25519")) // so the new key gets its own name
	if err := os.WriteFile(p.UserConfig(), []byte("Host mine\n    HostName login.example.edu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := guideAnswers{create: "yes"}.prompter()
	g := &KeyGuide{Paths: p, Alias: "mine", Host: "login.example.edu", Prompter: pr}
	calls := 0
	_, err := g.Run(context.Background(), func(context.Context) (*ssh.Client, error) {
		if calls++; calls == 1 {
			return nil, ErrKeyRejected
		}
		return nil, errors.New("stop")
	})
	if err == nil || err.Error() != "stop" {
		t.Fatalf("Run = %v", err)
	}
	found := false
	for _, n := range pr.notified {
		if strings.Contains(n, `"IdentityFile ~/.ssh/id_ed25519_labmill_mine"`) && strings.Contains(n, `"Host mine"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no notice with the IdentityFile line: %q", pr.notified)
	}
	if pr.count(AskKeyUpload) != 1 {
		t.Errorf("upload step skipped: %+v", pr.asked)
	}
}
