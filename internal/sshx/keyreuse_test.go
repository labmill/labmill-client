package sshx

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
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

// newKeyPair returns a fresh ed25519 key: the private key in OpenSSH PEM
// (encrypted when pass != ""), and the public key.
func newKeyPair(t *testing.T, pass string) ([]byte, ssh.PublicKey) {
	t.Helper()
	return newKeyPairOf(t, "ed25519", pass)
}

// newKeyPairOf is newKeyPair for "ed25519", "rsa" or "ecdsa".
func newKeyPairOf(t *testing.T, kind, pass string) ([]byte, ssh.PublicKey) {
	t.Helper()
	var priv, pub any
	switch kind {
	case "rsa":
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		priv, pub = k, &k.PublicKey
	case "ecdsa":
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		priv, pub = k, &k.PublicKey
	default:
		pk, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		priv, pub = k, pk
	}
	var blk *pem.Block
	var err error
	if pass == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "home-key")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "home-key", []byte(pass))
	}
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(blk), sp
}

// homeWith starts a "home" login host whose ~/.ssh holds the key pair
// <name> (private data priv, .pub line of pub) and returns a client on it.
func homeWith(t *testing.T, name string, priv []byte, pub ssh.PublicKey) *ssh.Client {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, name), string(priv))
	writeFile(t, filepath.Join(dir, name+".pub"), keyLine(pub, "alice@home"))
	c, _ := execClient(t, map[string]sshtest.Handler{"sh": shHandler(t, home)})
	return c
}

// reuseGuide builds a guide for the key-only host "orca" with the home
// connection cl (nil: no home connection).
func reuseGuide(p sshconfig.Paths, srv *sshtest.Server, pr Prompter, cl *ssh.Client) *KeyGuide {
	g := &KeyGuide{Paths: p, Alias: "orca", Host: hostOf(srv), SiteName: "Example University", Prompter: pr,
		QueryOpts: LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}}}
	if cl != nil {
		g.Home = &HomeKeys{Alias: "home", Host: "login.home.example.edu", Client: func(context.Context) (*ssh.Client, func()) {
			return cl, func() {}
		}}
	}
	return g
}

func dialOrca(p sshconfig.Paths, srv *sshtest.Server, pr Prompter) func(context.Context) (*ssh.Client, error) {
	return func(ctx context.Context) (*ssh.Client, error) {
		return Dial(ctx, p, "orca", pr, LoginOptions{KeyOnlyHosts: []string{hostOf(srv)}})
	}
}

// noSecrets fails when a question or notice shows any of the private key's
// text.
func noSecrets(t *testing.T, pr *scriptPrompter, priv []byte) {
	t.Helper()
	body := ""
	for _, l := range strings.Split(string(priv), "\n") {
		if len(l) > 20 && !strings.HasPrefix(l, "-----") {
			body = l
			break
		}
	}
	if body == "" {
		t.Fatal("test key has no body line")
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	var all []string
	for _, q := range pr.asked {
		all = append(all, q.Prompt, q.Data)
	}
	all = append(all, pr.notified...)
	for _, s := range all {
		if strings.Contains(s, body) || strings.Contains(s, "PRIVATE KEY") {
			t.Errorf("private key text shown to the user/Claude: %.60q", s)
		}
	}
}

func TestKeyGuideLocalKeyAccepted(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	priv, pub := newKeyPair(t, "")
	writeFile(t, filepath.Join(p.SSHDir, "work_key"), string(priv))
	writeFile(t, filepath.Join(p.SSHDir, "work_key.pub"), keyLine(pub, "me"))
	srv.Authorize(pub)
	pr := &scriptPrompter{onAsk: func(q Question) (string, error) { return "", errors.New("unexpected " + q.Prompt) }}
	g := reuseGuide(p, srv, pr, nil)
	c, err := g.Run(context.Background(), dialOrca(p, srv, pr))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.Close()
	h, _, _ := sshconfig.Lookup(p, "orca")
	if len(h.IdentityFiles) != 1 || h.IdentityFiles[0] != "~/.ssh/work_key" {
		t.Errorf("IdentityFile = %v", h.IdentityFiles)
	}
	if len(pr.notified) != 1 || !strings.Contains(pr.notified[0], "work_key") {
		t.Errorf("notices = %q, want one line naming the key", pr.notified)
	}
}

func TestKeyGuideLocalKeyNotAccepted(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	priv, pub := newKeyPair(t, "")
	writeFile(t, filepath.Join(p.SSHDir, "work_key"), string(priv))
	writeFile(t, filepath.Join(p.SSHDir, "work_key.pub"), keyLine(pub, "me"))
	pr := guideAnswers{create: "no"}.prompter()
	g := reuseGuide(p, srv, pr, nil)
	if _, err := g.Run(context.Background(), dialOrca(p, srv, pr)); !errors.Is(err, ErrKeySetupDeclined) {
		t.Fatalf("Run = %v, want the create flow (declined)", err)
	}
	if h, _, _ := sshconfig.Lookup(p, "orca"); len(h.IdentityFiles) != 0 {
		t.Errorf("a rejected key was added: %v", h.IdentityFiles)
	}
	if pr.count(AskKeyCreate) != 1 {
		t.Error("the create question was not asked")
	}
}

func copyAnswers(copyAns string, extra func(Question) (string, bool)) *scriptPrompter {
	return &scriptPrompter{onAsk: func(q Question) (string, error) {
		if extra != nil {
			if a, ok := extra(q); ok {
				return a, nil
			}
		}
		switch q.Kind {
		case AskKeyCopy:
			return copyAns, nil
		case AskKeyCreate:
			return "no", nil
		}
		return "", errors.New("unexpected question " + q.Prompt)
	}}
}

func TestKeyGuideCopyHomeKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	priv, pub := newKeyPair(t, "")
	cl := homeWith(t, "id_ed25519", priv, pub)
	p, srv := keyOnlyEnv(t, pub)
	// A file already named like the copy must stay as it is.
	existing := filepath.Join(p.SSHDir, "id_ed25519_home")
	writeFile(t, existing, "KEEP")
	pr := copyAnswers("yes", nil)
	g := reuseGuide(p, srv, pr, cl)
	c, err := g.Run(context.Background(), dialOrca(p, srv, pr))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.Close()
	if b, _ := os.ReadFile(existing); string(b) != "KEEP" {
		t.Error("an existing file was overwritten")
	}
	copied := filepath.Join(p.SSHDir, "id_ed25519_home_2")
	got, err := os.ReadFile(copied)
	if err != nil || string(got) != string(priv) {
		t.Fatalf("copied key missing or different: %v", err)
	}
	if fi, _ := os.Stat(copied); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if fi, err := os.Stat(copied + ".pub"); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf(".pub = %v, %v", fi, err)
	}
	h, _, _ := sshconfig.Lookup(p, "orca")
	if len(h.IdentityFiles) != 1 || h.IdentityFiles[0] != "~/.ssh/id_ed25519_home_2" {
		t.Errorf("IdentityFile = %v", h.IdentityFiles)
	}
	// The question names file, fingerprint, source, destination, reason, risk.
	var q Question
	for _, a := range pr.asked {
		if a.Kind == AskKeyCopy {
			q = a
		}
	}
	for _, want := range []string{"id_ed25519", ssh.FingerprintSHA256(pub), "login.home.example.edu:~/.ssh/id_ed25519", "~/.ssh/id_ed25519_home_2", "already accepts", "copy of a private key"} {
		if !strings.Contains(q.Prompt, want) {
			t.Errorf("copy question lacks %q:\n%s", want, q.Prompt)
		}
	}
	noSecrets(t, pr, priv)
	if pr.count(AskKeyCreate) != 0 {
		t.Error("create flow ran after a successful copy")
	}
}

func TestKeyGuideCopyFingerprintMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	_, pub := newKeyPair(t, "")
	otherPriv, _ := newKeyPair(t, "") // the private file belongs to another key
	cl := homeWith(t, "id_ed25519", otherPriv, pub)
	p, srv := keyOnlyEnv(t, pub)
	pr := copyAnswers("yes", nil)
	g := reuseGuide(p, srv, pr, cl)
	if _, err := g.Run(context.Background(), dialOrca(p, srv, pr)); !errors.Is(err, ErrKeySetupDeclined) {
		t.Fatalf("Run = %v, want the create flow (declined)", err)
	}
	if _, err := os.Stat(filepath.Join(p.SSHDir, "id_ed25519_home")); err == nil {
		t.Error("a key that does not match was written")
	}
	found := false
	for _, n := range pr.notified {
		if strings.Contains(n, "does not match") {
			found = true
		}
	}
	if !found {
		t.Errorf("no refusal notice: %q", pr.notified)
	}
	noSecrets(t, pr, otherPriv)
}

func TestKeyGuideCopyDeclinedGoesToCreate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	priv, pub := newKeyPair(t, "")
	cl := homeWith(t, "id_ed25519", priv, pub)
	p, srv := keyOnlyEnv(t, pub)
	pr := &scriptPrompter{}
	pr.onAsk = func(q Question) (string, error) {
		switch q.Kind {
		case AskKeyCopy:
			return "no", nil
		case AskKeyCreate:
			return "yes", nil
		case AskNewPassphrase:
			return "", nil
		case AskKeyUpload:
			authorizeLine(t, srv, q.Data)
			return "yes", nil
		}
		return "", errors.New("unexpected " + q.Prompt)
	}
	g := reuseGuide(p, srv, pr, cl)
	c, err := g.Run(context.Background(), dialOrca(p, srv, pr))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.Close()
	if _, err := os.Stat(filepath.Join(p.SSHDir, "id_ed25519_home")); err == nil {
		t.Error("the key was copied after declining")
	}
	var up Question
	for _, q := range pr.asked {
		if q.Kind == AskKeyCreate {
			up = q
		}
	}
	if !strings.Contains(up.Prompt, "No SSH key of yours is registered at Example University yet") {
		t.Errorf("create text = %q", up.Prompt)
	}
}

func TestKeyGuideNoHomeConnection(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	pr := guideAnswers{create: "no"}.prompter()
	g := reuseGuide(p, srv, pr, nil)
	g.Home = &HomeKeys{Alias: "home", Host: "login.home.example.edu", Client: func(context.Context) (*ssh.Client, func()) { return nil, nil }}
	if _, err := g.Run(context.Background(), dialOrca(p, srv, pr)); !errors.Is(err, ErrKeySetupDeclined) {
		t.Fatalf("Run = %v", err)
	}
	if pr.count(AskKeyCopy) != 0 || len(pr.notified) != 0 {
		t.Errorf("home step was not silent: asked %v notified %q", pr.asked, pr.notified)
	}
}

func TestKeyGuideCopyEncryptedKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	priv, pub := newKeyPair(t, "s3cret-pass")
	cl := homeWith(t, "id_ed25519", priv, pub)
	p, srv := keyOnlyEnv(t, pub)
	pr := copyAnswers("yes", func(q Question) (string, bool) {
		if q.Kind == AskKeyPassphrase {
			return "s3cret-pass", true
		}
		return "", false
	})
	g := reuseGuide(p, srv, pr, cl)
	c, err := g.Run(context.Background(), dialOrca(p, srv, pr))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.Close()
	if pr.count(AskKeyPassphrase) != 1 {
		t.Errorf("passphrase asked %d times, want 1", pr.count(AskKeyPassphrase))
	}
	noSecrets(t, pr, priv)
	for _, q := range pr.asked {
		if strings.Contains(q.Prompt+q.Data, "s3cret-pass") {
			t.Error("passphrase shown in a question")
		}
	}
}

func TestPrivateKeyPublic(t *testing.T) {
	priv, pub := newKeyPair(t, "")
	if got, enc, err := privateKeyPublic(priv); err != nil || enc || string(got.Marshal()) != string(pub.Marshal()) {
		t.Errorf("plain: %v enc=%v", err, enc)
	}
	encPriv, encPub := newKeyPair(t, "pw")
	if got, enc, err := privateKeyPublic(encPriv); err != nil || !enc || string(got.Marshal()) != string(encPub.Marshal()) {
		t.Errorf("encrypted: %v enc=%v", err, enc)
	}
	if _, _, err := privateKeyPublic([]byte("not a key")); err == nil {
		t.Error("garbage accepted")
	}
	// A legacy PEM key with a passphrase does not show its public key.
	if _, _, err := privateKeyPublic(legacyEncryptedKey(t)); err == nil {
		t.Error("a legacy encrypted key was accepted without a way to check it")
	}
}

func legacyEncryptedKey(t *testing.T) []byte {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rk), []byte("pw"), x509.PEMCipherAES128)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(blk)
}

func TestKeyGuideCopyKeyTypes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	for _, kind := range []string{"rsa", "ecdsa"} {
		t.Run(kind, func(t *testing.T) {
			priv, pub := newKeyPairOf(t, kind, "")
			cl := homeWith(t, "id_"+kind, priv, pub)
			p, srv := keyOnlyEnv(t, pub)
			pr := copyAnswers("yes", nil)
			c, err := reuseGuide(p, srv, pr, cl).Run(context.Background(), dialOrca(p, srv, pr))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			c.Close()
			if _, err := os.Stat(filepath.Join(p.SSHDir, "id_"+kind+"_home")); err != nil {
				t.Errorf("copy missing: %v", err)
			}
		})
	}
}

func TestKeyGuideCopyLegacyEncryptedRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	_, pub := newKeyPair(t, "")
	// The private file is a legacy encrypted key whose public key cannot be
	// read: the copy is refused.
	cl := homeWith(t, "id_ed25519", legacyEncryptedKey(t), pub)
	p, srv := keyOnlyEnv(t, pub)
	pr := copyAnswers("yes", nil)
	if _, err := reuseGuide(p, srv, pr, cl).Run(context.Background(), dialOrca(p, srv, pr)); !errors.Is(err, ErrKeySetupDeclined) {
		t.Fatalf("Run = %v, want the create flow (declined)", err)
	}
	if _, err := os.Stat(filepath.Join(p.SSHDir, "id_ed25519_home")); err == nil {
		t.Error("an unverifiable key was written")
	}
}

func TestKeyGuideCopyOversized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	_, pub := newKeyPair(t, "")
	cl := homeWith(t, "id_ed25519", []byte(strings.Repeat("A", 3<<20)), pub)
	p, srv := keyOnlyEnv(t, pub)
	pr := copyAnswers("yes", nil)
	done := make(chan error, 1)
	go func() {
		_, err := reuseGuide(p, srv, pr, cl).Run(context.Background(), dialOrca(p, srv, pr))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrKeySetupDeclined) {
			t.Fatalf("Run = %v, want the create flow (declined)", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("an oversized key file blocked the copy")
	}
	found := false
	for _, n := range pr.notified {
		if strings.Contains(n, "too large") {
			found = true
		}
	}
	if !found {
		t.Errorf("no size notice: %q", pr.notified)
	}
	if _, err := os.Stat(filepath.Join(p.SSHDir, "id_ed25519_home")); err == nil {
		t.Error("an oversized file was written")
	}
}

// A home key that is already on this computer is not offered again.
func TestKeyGuideHomeKeyAlreadyLocalSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	priv, pub := newKeyPair(t, "")
	cl := homeWith(t, "id_ed25519", priv, pub)
	p, srv := keyOnlyEnv(t, pub)
	writeFile(t, filepath.Join(p.SSHDir, "id_ed25519"), string(priv))
	writeFile(t, filepath.Join(p.SSHDir, "id_ed25519.pub"), keyLine(pub, "x"))
	pr := copyAnswers("yes", nil)
	// Dial already offered that key (the connect function says it failed).
	_, err := reuseGuide(p, srv, pr, cl).Run(context.Background(), func(context.Context) (*ssh.Client, error) { return nil, ErrKeyRejected })
	if !errors.Is(err, ErrKeySetupDeclined) {
		t.Fatalf("Run = %v", err)
	}
	if pr.count(AskKeyCopy) != 0 {
		t.Error("a key already on this computer was offered for copying")
	}
}

// A path through a jump host that is not key-only is not checked (QueryKeys
// would sign in to the jump host): one line says so and the create flow runs.
func TestKeyGuideSkipsCheckBehindPasswordJump(t *testing.T) {
	p, srv := keyOnlyEnv(t)
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{
		{Alias: "orca", HostName: h, User: "alice", Port: atoi(t, port), ProxyJump: "jump"},
		{Alias: "jump", HostName: "jump.example.edu", User: "alice", Port: 1},
	}, "linux"); err != nil {
		t.Fatal(err)
	}
	priv, pub := newKeyPair(t, "")
	writeFile(t, filepath.Join(p.SSHDir, "work_key"), string(priv))
	writeFile(t, filepath.Join(p.SSHDir, "work_key.pub"), keyLine(pub, "me"))
	pr := guideAnswers{create: "no"}.prompter()
	g := reuseGuide(p, srv, pr, nil)
	if _, err := g.Run(context.Background(), func(context.Context) (*ssh.Client, error) { return nil, ErrKeyRejected }); !errors.Is(err, ErrKeySetupDeclined) {
		t.Fatalf("Run = %v", err)
	}
	if len(pr.notified) != 1 || !strings.Contains(pr.notified[0], "jump.example.edu") {
		t.Errorf("notices = %q, want one line naming the jump host", pr.notified)
	}
	// With the jump host key-only the check is not skipped.
	if hop, err := nonKeyOnlyJump(p, "orca", LoginOptions{KeyOnlyHosts: []string{"jump.example.edu"}}); err != nil || hop != "" {
		t.Errorf("key-only jump: %q, %v", hop, err)
	}
	if hop, _ := nonKeyOnlyJump(p, "orca", LoginOptions{}); hop != "jump.example.edu" {
		t.Errorf("password jump: %q", hop)
	}
}

// A key that was copied but cannot be recorded (the alias is in the user's
// own ssh config) stops with the manual instruction, not a new key.
func TestKeyGuideCopyUserOwnedAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	priv, pub := newKeyPair(t, "")
	cl := homeWith(t, "id_ed25519", priv, pub)
	p, srv := keyOnlyEnv(t, pub)
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := os.WriteFile(p.UserConfig(), []byte("Host mine\n    HostName "+h+"\n    User alice\n    Port "+port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := copyAnswers("yes", nil)
	g := reuseGuide(p, srv, pr, cl)
	g.Alias = "mine"
	_, err := g.Run(context.Background(), func(context.Context) (*ssh.Client, error) { return nil, ErrKeyRejected })
	if !errors.Is(err, ErrKeySetupDeclined) || !strings.Contains(err.Error(), "IdentityFile ~/.ssh/id_ed25519_home") {
		t.Fatalf("Run = %v", err)
	}
	if pr.count(AskKeyCreate) != 0 {
		t.Error("fell through into creating a key")
	}
}
