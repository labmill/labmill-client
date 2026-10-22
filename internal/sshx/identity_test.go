package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// writeKey writes a new unencrypted ed25519 private key in OpenSSH format
// and returns its public key.
func writeKey(t *testing.T, path string) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	sg, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return sg.PublicKey()
}

// TestDialAllIdentityFiles: three IdentityFile lines, the first unauthorized,
// the second missing, the third garbage, the fourth authorized: Dial offers
// every key in order, skips the unusable files silently, and logs in without
// asking for a password.
func TestDialAllIdentityFiles(t *testing.T) {
	dir := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(dir, ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeKey(t, filepath.Join(p.SSHDir, "id_first"))
	garbage := filepath.Join(p.SSHDir, "id_garbage")
	if err := os.WriteFile(garbage, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	authorized := writeKey(t, filepath.Join(p.SSHDir, "id_last"))

	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw",
		AuthorizedKeys: []ssh.PublicKey{authorized}})
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "box", HostName: h, User: "alice", Port: atoi(t, port),
		IdentityFiles: []string{"~/.ssh/id_first", filepath.Join(p.SSHDir, "id_missing"), garbage, "~/.ssh/id_last"}}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Dial(context.Background(), p, "box", pr, LoginOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if n := pr.count(AskPassword); n != 0 {
		t.Errorf("asked for a password %d times; the key should have logged in", n)
	}
}

// TestDialIdentityFilesFallBackToPassword: keys that the server doesn't
// accept don't break the password login.
func TestDialIdentityFilesFallBackToPassword(t *testing.T) {
	dir := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(dir, ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeKey(t, filepath.Join(p.SSHDir, "id_a"))
	other := writeKey(t, filepath.Join(dir, "other"))
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", AuthorizedKeys: []ssh.PublicKey{other}})
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "box", HostName: h, User: "alice", Port: atoi(t, port),
		IdentityFiles: []string{"~/.ssh/id_a"}}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Dial(context.Background(), p, "box", pr, LoginOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if n := pr.count(AskPassword); n != 1 {
		t.Errorf("password asked %d times, want 1", n)
	}
}

// TestDialSkipsGroupReadableKey: an authorized key with mode 0644 is not
// used (like OpenSSH's UNPROTECTED PRIVATE KEY FILE), so Dial falls back to
// the password.
func TestDialSkipsGroupReadableKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	dir := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(dir, ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(p.SSHDir, "id_open")
	pub := writeKey(t, keyPath)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", AuthorizedKeys: []ssh.PublicKey{pub}})
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "box", HostName: h, User: "alice", Port: atoi(t, port),
		IdentityFiles: []string{"~/.ssh/id_open"}}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Dial(context.Background(), p, "box", pr, LoginOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if n := pr.count(AskPassword); n != 1 {
		t.Errorf("password asked %d times, want 1 (the 0644 key must not be used)", n)
	}
	if got := strings.Join(pr.notified, "\n"); !strings.Contains(got, "Skipped key ~/.ssh/id_open: permissions too open; using password and 2FA instead.") {
		t.Errorf("no notice about the skipped key: %q", got)
	}
	// A second login in the same process doesn't repeat the notice.
	pr2 := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c2, err := Dial(context.Background(), p, "box", pr2, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	if got := strings.Join(pr2.notified, "\n"); strings.Contains(got, "Skipped key") {
		t.Errorf("notice repeated: %q", got)
	}
	// Fixing the mode makes the same key usable.
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := identitySigners(p, sshconfig.Host{IdentityFiles: []string{"~/.ssh/id_open"}}, nil); len(got) != 1 {
		t.Errorf("0600 key signers = %d, want 1", len(got))
	}
}

// TestIdentitySignersDedupe: the same key via two files, or already in the
// caller's signers, is offered once.
func TestIdentitySignersDedupe(t *testing.T) {
	dir := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(dir, ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(p.SSHDir, "id_a")
	writeKey(t, a)
	data, _ := os.ReadFile(a)
	b := filepath.Join(p.SSHDir, "id_b") // a copy of id_a
	if err := os.WriteFile(b, data, 0o600); err != nil {
		t.Fatal(err)
	}
	writeKey(t, filepath.Join(p.SSHDir, "id_c"))
	cfg := sshconfig.Host{IdentityFiles: []string{a, b, filepath.Join(p.SSHDir, "id_c")}}
	got := identitySigners(p, cfg, nil)
	if len(got) != 2 {
		t.Fatalf("signers = %d, want 2", len(got))
	}
	// A caller signer that equals id_a removes it from the files' keys.
	if got2 := identitySigners(p, cfg, got[:1]); len(got2) != 1 {
		t.Errorf("with existing signer: %d, want 1", len(got2))
	}
}

// TestDialCapsOfferedKeys: seven IdentityFiles where only the seventh is
// authorized. The server allows 6 attempts, so offering all keys would use
// them up before the password; Dial offers 5 and falls back to the password.
// Login with all seven signers (no cap) fails, which shows the cap is needed.
func TestDialCapsOfferedKeys(t *testing.T) {
	dir := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(dir, ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var files []string
	var signers []ssh.Signer
	var last ssh.PublicKey
	for i := 0; i < 7; i++ {
		f := filepath.Join(p.SSHDir, "id_"+string(rune('a'+i)))
		last = writeKey(t, f)
		files = append(files, f)
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sg, err := ssh.ParsePrivateKey(data)
		if err != nil {
			t.Fatal(err)
		}
		signers = append(signers, sg)
	}
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw",
		AuthorizedKeys: []ssh.PublicKey{last}, MaxAuthTries: 6})
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "box", HostName: h, User: "alice", Port: atoi(t, port),
		IdentityFiles: files}}, "linux"); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(p.SSHDir, "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Dial(context.Background(), p, "box", pr, LoginOptions{})
	if err != nil {
		t.Fatalf("Dial with the cap: %v", err)
	}
	c.Close()
	if n := pr.count(AskPassword); n != 1 {
		t.Errorf("password asked %d times, want 1", n)
	}

	// Without the cap the tries run out before the password.
	conn, err := net.Dial("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	pr2 := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	if c2, err := Login(context.Background(), conn, srv.Addr, pr2, LoginOptions{User: "alice", KnownHosts: kh, Signers: signers}); err == nil {
		c2.Close()
		t.Error("Login with 7 offered keys succeeded; the server's MaxAuthTries should have stopped it")
	}
}

// TestDialRetriesWithoutKeys: a hardened server (MaxAuthTries 3) hangs up on
// 4 unauthorized keys before keyboard-interactive runs; Dial dials that hop
// again without keys and logs in with the password.
func TestDialRetriesWithoutKeys(t *testing.T) {
	dir := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(dir, ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var files []string
	for i := 0; i < 4; i++ {
		f := filepath.Join(p.SSHDir, "id_"+string(rune('a'+i)))
		writeKey(t, f)
		files = append(files, f)
	}
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", MaxAuthTries: 3})
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "box", HostName: h, User: "alice", Port: atoi(t, port),
		IdentityFiles: files}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Dial(context.Background(), p, "box", pr, LoginOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	if n := pr.count(AskPassword); n != 1 {
		t.Errorf("password asked %d times, want 1", n)
	}
}

func TestShortHostAndPushNotice(t *testing.T) {
	for in, want := range map[string]string{
		"files.cl.example.edu": "files.cl", "login.example.edu": "login",
		"example.edu": "example.edu", "127.0.0.1": "127.0.0.1", "box": "box",
	} {
		if got := shortHost(in); got != want {
			t.Errorf("shortHost(%q) = %q, want %q", in, got, want)
		}
	}
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	if err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()), LoginOptions{PushTimeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range pr.notified {
		if strings.Contains(m, "Approve the sign-in request for 127.0.0.1 on your phone") {
			found = true
		}
	}
	if !found {
		t.Errorf("notices = %v, want the push notice to name the host", pr.notified)
	}
}
