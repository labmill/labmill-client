package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// scriptPrompter answers by a function and records the questions.
type scriptPrompter struct {
	mu       sync.Mutex
	onAsk    func(Question) (string, error)
	asked    []Question
	notified []string
}

func (s *scriptPrompter) Ask(_ context.Context, q Question) (string, error) {
	s.mu.Lock()
	s.asked = append(s.asked, q)
	f := s.onAsk
	s.mu.Unlock()
	if f == nil {
		return "", nil
	}
	return f(q)
}

func (s *scriptPrompter) Notify(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notified = append(s.notified, msg)
}

func (s *scriptPrompter) count(k QuestionKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.asked {
		if q.Kind == k {
			n++
		}
	}
	return n
}

// keyOnlyEnv starts a key-only server and writes an ssh config with the host
// "orca" (no IdentityFile) and the server's host key into a fresh home.
func keyOnlyEnv(t *testing.T, authorized ...ssh.PublicKey) (sshconfig.Paths, *sshtest.Server) {
	t.Helper()
	home := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(home, ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := sshtest.New(t, sshtest.Options{User: "alice", KeyOnly: true, AuthorizedKeys: authorized})
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "orca", HostName: h, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func hostOf(srv *sshtest.Server) string {
	h, _, _ := net.SplitHostPort(srv.Addr)
	return h
}

func TestGenerateKey(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	kp, err := GenerateKey(p, "orca", "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(kp.Path) != "id_ed25519" || kp.ConfigPath != "~/.ssh/id_ed25519" {
		t.Errorf("first key = %+v, want the default name", kp)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{p.SSHDir: 0o700, kp.Path: 0o600, kp.Path + ".pub": 0o644} {
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
				t.Errorf("%s mode = %v, %v; want %v", path, fi.Mode().Perm(), err, want)
			}
		}
	}
	data, _ := os.ReadFile(kp.Path)
	sg, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	pubFile, _ := os.ReadFile(kp.Path + ".pub")
	if strings.TrimSpace(string(pubFile)) != kp.PublicKey || !strings.HasPrefix(kp.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(kp.PublicKey, " labmill-orca") {
		t.Errorf("public key line = %q (file %q)", kp.PublicKey, pubFile)
	}
	if got := ssh.FingerprintSHA256(sg.PublicKey()); got != kp.Fingerprint {
		t.Errorf("fingerprint %q != %q", got, kp.Fingerprint)
	}

	// Never overwrite: the next keys get their own names.
	before, _ := os.ReadFile(kp.Path)
	kp2, err := GenerateKey(p, "orca", "")
	if err != nil {
		t.Fatal(err)
	}
	kp3, err := GenerateKey(p, "orca", "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(kp2.Path) != "id_ed25519_labmill_orca" || filepath.Base(kp3.Path) != "id_ed25519_labmill_orca_2" {
		t.Errorf("names = %s, %s", kp2.Path, kp3.Path)
	}
	if after, _ := os.ReadFile(kp.Path); string(after) != string(before) {
		t.Error("an existing key was overwritten")
	}

	// A lone .pub also blocks the name.
	p2 := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	os.MkdirAll(p2.SSHDir, 0o700)
	os.WriteFile(filepath.Join(p2.SSHDir, "id_ed25519.pub"), []byte("x"), 0o644)
	if kp, err := GenerateKey(p2, "orca", ""); err != nil || filepath.Base(kp.Path) != "id_ed25519_labmill_orca" {
		t.Errorf("with a lone .pub: %+v, %v", kp, err)
	}
}

func TestGenerateKeyPassphrase(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	kp, err := GenerateKey(p, "orca", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(kp.Path)
	var missing *ssh.PassphraseMissingError
	if _, err := ssh.ParsePrivateKey(data); !errors.As(err, &missing) {
		t.Fatalf("an encrypted key must need a passphrase, got %v", err)
	}
	if _, err := ssh.ParsePrivateKeyWithPassphrase(data, []byte("s3cret")); err != nil {
		t.Errorf("passphrase does not open the key: %v", err)
	}
}

// TestDialDefaultKey: a host with no IdentityFile line is logged in to with
// ~/.ssh/id_ed25519, without any question.
func TestDialDefaultKey(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	kp, err := GenerateKey(p, "orca", "")
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.PublicKey))
	srv.Authorize(pk)
	pr := &scriptPrompter{}
	c, err := Dial(context.Background(), p, "orca", pr, LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	if len(pr.asked) != 0 {
		t.Errorf("asked %v", pr.asked)
	}
}

// TestDialDefaultKeyOrder: ecdsa and rsa default names are tried too, and a
// group-readable default key is skipped.
func TestDialDefaultKeyUnusable(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	pub := writeKey(t, filepath.Join(p.SSHDir, "id_ecdsa")) // an ed25519 key under another default name
	srv.Authorize(pub)
	pr := &scriptPrompter{}
	c, err := Dial(context.Background(), p, "orca", pr, LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}})
	if err != nil {
		t.Fatalf("Dial with id_ecdsa: %v", err)
	}
	c.Close()
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(filepath.Join(p.SSHDir, "id_ecdsa"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Dial(context.Background(), p, "orca", pr, LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}})
	if !errors.Is(err, ErrNoKey) {
		t.Errorf("Dial with a group-readable key = %v, want ErrNoKey", err)
	}
}

// TestDialKeyOnlyRejected: a key the server does not know is ErrKeyRejected,
// and the password is never asked.
func TestDialKeyOnlyRejected(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	if _, err := GenerateKey(p, "orca", ""); err != nil {
		t.Fatal(err)
	}
	pr := &scriptPrompter{}
	_, err := Dial(context.Background(), p, "orca", pr, LoginOptions{KeyOnlyHosts: []string{strings.ToUpper(hostOf(srv))}})
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("Dial = %v, want ErrKeyRejected", err)
	}
	if pr.count(AskPassword) != 0 {
		t.Error("asked for a password on a key-only host")
	}
	if enc := EncodeRelayError(err); !strings.HasPrefix(enc, relayErrorPrefix+"key:") {
		t.Errorf("relay code of %v = %q", err, enc)
	} else if back, ok := DecodeRelayError(enc); !ok || !errors.Is(back, ErrKeyRejected) {
		t.Errorf("relay round trip lost ErrKeyRejected: %v", back)
	}
}

func TestDialAgent(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	_, priv, err := ed25519Key(t)
	if err != nil {
		t.Fatal(err)
	}
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	sg, _ := ssh.NewSignerFromKey(priv)
	srv.Authorize(sg.PublicKey())
	old := agentDial
	t.Cleanup(func() { agentDial = old })
	agentDial = func() (net.Conn, error) {
		a, b := net.Pipe()
		go func() { agent.ServeAgent(kr, b); b.Close() }()
		return a, nil
	}
	if n := CountUsableKeys(p, "orca"); n != 1 {
		t.Errorf("CountUsableKeys with an agent key = %d, want 1", n)
	}
	pr := &scriptPrompter{}
	c, err := Dial(context.Background(), p, "orca", pr, LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}})
	if err != nil {
		t.Fatalf("Dial through the agent: %v", err)
	}
	c.Close()
	if len(pr.asked) != 0 {
		t.Errorf("asked %v", pr.asked)
	}
}

func TestDialEncryptedKeyPassphrase(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	kp, err := GenerateKey(p, "orca", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.PublicKey))
	srv.Authorize(pk)
	opts := LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}}

	tries := 0
	pr := &scriptPrompter{onAsk: func(q Question) (string, error) {
		if q.Kind != AskKeyPassphrase {
			return "", errors.New("unexpected question")
		}
		if tries++; tries == 1 {
			return "wrong", nil
		}
		return "s3cret", nil
	}}
	c, err := Dial(context.Background(), p, "orca", pr, opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	if tries != 2 {
		t.Errorf("passphrase asked %d times, want 2 (wrong, then right)", tries)
	}
	if !strings.Contains(pr.asked[0].Prompt, "~/.ssh/id_ed25519") {
		t.Errorf("prompt does not name the key: %q", pr.asked[0].Prompt)
	}

	// Empty skips the key: no key is left.
	pr = &scriptPrompter{}
	if _, err := Dial(context.Background(), p, "orca", pr, opts); !errors.Is(err, ErrNoKey) {
		t.Errorf("empty passphrase: %v, want ErrNoKey", err)
	}
	// A canceled dialog aborts.
	cancel := errors.New("canceled")
	pr = &scriptPrompter{onAsk: func(Question) (string, error) { return "", cancel }}
	if _, err := Dial(context.Background(), p, "orca", pr, opts); !errors.Is(err, cancel) {
		t.Errorf("canceled dialog: %v", err)
	}
	// On a host that is not key-only the encrypted key is skipped silently.
	pr = &scriptPrompter{}
	pw := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	h, port, _ := net.SplitHostPort(pw.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "pw", HostName: h, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	kh, _ := os.ReadFile(filepath.Join(p.SSHDir, "known_hosts"))
	os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), append(kh, []byte(pw.KnownHostsLine()+"\n")...), 0o600)
	pr.onAsk = func(q Question) (string, error) {
		if q.Kind == AskKeyPassphrase {
			t.Error("asked for a key passphrase on a password host")
		}
		return "pw", nil
	}
	c, err = Dial(context.Background(), p, "pw", pr, LoginOptions{})
	if err != nil {
		t.Fatalf("password host: %v", err)
	}
	c.Close()
}

// uploadPrompter answers the guide's questions. onUpload runs when the
// public key is shown (it "uploads" the key by authorizing it).
type guideAnswers struct {
	create, passphrase string
	uploads            []string // answers to successive AskKeyUpload questions
	onUpload           func(n int, pub string)
}

func (g guideAnswers) prompter() *scriptPrompter {
	n := 0
	return &scriptPrompter{onAsk: func(q Question) (string, error) {
		switch q.Kind {
		case AskKeyCreate:
			return g.create, nil
		case AskNewPassphrase:
			return g.passphrase, nil
		case AskKeyUpload:
			if g.onUpload != nil {
				g.onUpload(n, q.Data)
			}
			ans := "yes"
			if n < len(g.uploads) {
				ans = g.uploads[n]
			}
			n++
			return ans, nil
		}
		return "", errors.New("unexpected question " + q.Prompt)
	}}
}

func authorizeLine(t *testing.T, srv *sshtest.Server, line string) {
	t.Helper()
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatalf("public key %q: %v", line, err)
	}
	srv.Authorize(pk)
}

// TestKeyGuideNoKey: the whole guided setup against a key-only server with no
// key on the computer: create, show, upload, log in, IdentityFile written.
func TestKeyGuideNoKey(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	pr := guideAnswers{create: "yes", onUpload: func(_ int, pub string) { authorizeLine(t, srv, pub) }}.prompter()
	g := &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), Prompter: pr,
		Upload: KeyUploadInfo{URL: "https://registry.example.edu/keys", Instruction: "Paste the key into the registry."}}
	opts := LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}}
	c, err := g.Run(context.Background(), func(ctx context.Context) (*ssh.Client, error) {
		return Dial(ctx, p, "orca", pr, opts)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.Close()
	var up Question
	for _, q := range pr.asked {
		if q.Kind == AskKeyUpload {
			up = q
		}
	}
	if !strings.HasPrefix(up.Data, "ssh-ed25519 ") || !strings.Contains(up.Prompt, "Paste the key into the registry.") || !strings.Contains(up.Prompt, "https://registry.example.edu/keys") {
		t.Errorf("upload question = %+v", up)
	}
	if _, err := os.Stat(filepath.Join(p.SSHDir, "id_ed25519")); err != nil {
		t.Errorf("key not created: %v", err)
	}
	h, _, err := sshconfig.Lookup(p, "orca")
	if err != nil || len(h.IdentityFiles) != 1 || h.IdentityFiles[0] != "~/.ssh/id_ed25519" {
		t.Errorf("IdentityFile in the ssh config = %v, %v", h.IdentityFiles, err)
	}
	if pr.count(AskKeyCreate) != 1 || pr.count(AskNewPassphrase) != 1 {
		t.Errorf("questions: %+v", pr.asked)
	}
}

// TestKeyGuideKeyNotUploadedYet: the first confirmation comes before the
// registry has the key; the guide asks again, then succeeds.
func TestKeyGuideKeyNotUploadedYet(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	pr := guideAnswers{create: "yes", onUpload: func(n int, pub string) {
		if n == 1 {
			authorizeLine(t, srv, pub)
		}
	}}.prompter()
	g := &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), Prompter: pr}
	opts := LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}}
	c, err := g.Run(context.Background(), func(ctx context.Context) (*ssh.Client, error) {
		return Dial(ctx, p, "orca", pr, opts)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.Close()
	if n := pr.count(AskKeyUpload); n != 2 {
		t.Errorf("upload asked %d times, want 2", n)
	}
	if n := pr.count(AskKeyCreate); n != 1 {
		t.Errorf("a second key was offered (%d create questions)", n)
	}
	last := pr.asked[len(pr.asked)-1]
	if last.Kind != AskKeyUpload || !strings.Contains(last.Prompt, "did not accept the key yet") {
		t.Errorf("second upload question = %+v", last)
	}
}

// TestKeyGuideNeverUploaded: three unconfirmed uploads end in the rejection
// error.
func TestKeyGuideNeverUploaded(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	pr := guideAnswers{create: "yes"}.prompter()
	g := &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), Prompter: pr}
	opts := LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}}
	_, err := g.Run(context.Background(), func(ctx context.Context) (*ssh.Client, error) {
		return Dial(ctx, p, "orca", pr, opts)
	})
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("Run = %v, want ErrKeyRejected", err)
	}
	if n := pr.count(AskKeyUpload); n != maxUploadChecks {
		t.Errorf("upload asked %d times, want %d", n, maxUploadChecks)
	}
}

// TestKeyGuideDeclined: "no" at the create question writes nothing.
func TestKeyGuideDeclined(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	pr := guideAnswers{create: "no"}.prompter()
	g := &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), Prompter: pr}
	_, err := g.Run(context.Background(), func(context.Context) (*ssh.Client, error) {
		return nil, ErrNoKey
	})
	if !errors.Is(err, ErrKeySetupDeclined) {
		t.Errorf("Run = %v, want ErrKeySetupDeclined", err)
	}
	if _, err := os.Stat(filepath.Join(p.SSHDir, "id_ed25519")); err == nil {
		t.Error("a key was created after declining")
	}
	// Declining the upload confirmation also stops, keeping the key.
	pr = guideAnswers{create: "yes", uploads: []string{"no"}}.prompter()
	g = &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), Prompter: pr}
	if _, err := g.Run(context.Background(), func(context.Context) (*ssh.Client, error) { return nil, ErrNoKey }); !errors.Is(err, ErrKeySetupDeclined) {
		t.Errorf("declined upload: %v", err)
	}
}

// TestKeyGuideRejectedExistingKey: an existing key the server does not know
// leads to a new key beside it; the old one is untouched.
func TestKeyGuideRejectedExistingKey(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	writeKey(t, filepath.Join(p.SSHDir, "id_ed25519"))
	old, _ := os.ReadFile(filepath.Join(p.SSHDir, "id_ed25519"))
	pr := guideAnswers{create: "yes", passphrase: "pw1", onUpload: func(_ int, pub string) { authorizeLine(t, srv, pub) }}.prompter()
	g := &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), Prompter: pr}
	// The passphrase prompt is answered "pw1" for the new key and the repeat;
	// later AskKeyPassphrase questions (the Dial) also get it.
	pr2 := pr.onAsk
	pr.onAsk = func(q Question) (string, error) {
		if q.Kind == AskKeyPassphrase {
			return "pw1", nil
		}
		return pr2(q)
	}
	opts := LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}}
	c, err := g.Run(context.Background(), func(ctx context.Context) (*ssh.Client, error) {
		return Dial(ctx, p, "orca", pr, opts)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.Close()
	if now, _ := os.ReadFile(filepath.Join(p.SSHDir, "id_ed25519")); string(now) != string(old) {
		t.Error("the existing key changed")
	}
	if _, err := os.Stat(filepath.Join(p.SSHDir, "id_ed25519_labmill_orca")); err != nil {
		t.Errorf("new key missing: %v", err)
	}
	h, _, _ := sshconfig.Lookup(p, "orca")
	if len(h.IdentityFiles) != 1 || h.IdentityFiles[0] != "~/.ssh/id_ed25519_labmill_orca" {
		t.Errorf("IdentityFile = %v", h.IdentityFiles)
	}
	if n := pr.count(AskNewPassphrase); n != 2 {
		t.Errorf("passphrase asked %d times, want 2 (entry and repeat)", n)
	}
}

func TestKeyGuideOtherErrors(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	writeKey(t, filepath.Join(p.SSHDir, "id_ed25519"))
	boom := errors.New("network down")
	g := &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), Prompter: &scriptPrompter{}}
	if _, err := g.Run(context.Background(), func(context.Context) (*ssh.Client, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("Run = %v, want the connect error", err)
	}
}

func ed25519Key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	t.Helper()
	return ed25519.GenerateKey(rand.Reader)
}
