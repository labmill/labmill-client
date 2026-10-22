package sshx

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// edPub returns an ed25519 public key; no private key is kept.
func edPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519Key(t)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func rsaPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return sg.PublicKey()
}

// keyOnlyAlias starts a key-only server and an ssh config alias "orca" for it.
func keyOnlyAlias(t *testing.T, opts sshtest.Options) (sshconfig.Paths, *sshtest.Server) {
	t.Helper()
	opts.User, opts.KeyOnly = "alice", true
	srv := sshtest.New(t, opts)
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "orca", HostName: h, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func trustHost(t *testing.T, p sshconfig.Paths, srvs ...*sshtest.Server) {
	t.Helper()
	var b strings.Builder
	for _, s := range srvs {
		b.WriteString(s.KnownHostsLine() + "\n")
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQueryKeys(t *testing.T) {
	edOK, edNo, rsaOK, rsaNo := edPub(t), edPub(t), rsaPub(t), rsaPub(t)
	p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{edOK, rsaOK}})
	trustHost(t, p, srv)
	pr := &scriptPrompter{}
	got, err := QueryKeys(context.Background(), p, "orca", pr, LoginOptions{}, []ssh.PublicKey{edOK, edNo, rsaOK, rsaNo})
	if err != nil {
		t.Fatalf("QueryKeys: %v", err)
	}
	want := []bool{true, false, true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d accepted = %v, want %v", i, got[i], want[i])
		}
	}
	if srv.Logins() != 0 {
		t.Errorf("logins = %d, the query must not complete a login", srv.Logins())
	}
	if len(pr.asked) != 0 {
		t.Errorf("asked %+v for a known host and key-only auth", pr.asked)
	}
}

func TestQueryKeysEmptyAndCap(t *testing.T) {
	ok := edPub(t)
	p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{ok}})
	trustHost(t, p, srv)
	got, err := QueryKeys(context.Background(), p, "orca", &scriptPrompter{}, LoginOptions{}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("no keys: %v, %v", got, err)
	}
	// More than maxRemoteKeys keys: only the first maxRemoteKeys are asked,
	// the rest are reported false (even an authorized last one).
	var pubs []ssh.PublicKey
	for i := 0; i < maxRemoteKeys; i++ {
		pubs = append(pubs, edPub(t))
	}
	pubs = append(pubs, ok)
	got, err = QueryKeys(context.Background(), p, "orca", &scriptPrompter{}, LoginOptions{}, pubs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(pubs) || got[len(got)-1] {
		t.Errorf("got %v", got)
	}
}

func TestQueryKeysUnknownHost(t *testing.T) {
	ok := edPub(t)
	for _, answer := range []string{"yes", "no"} {
		t.Run(answer, func(t *testing.T) {
			p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{ok}})
			pr := &scriptPrompter{onAsk: func(q Question) (string, error) {
				if q.Kind != AskHostKey {
					t.Errorf("unexpected question %+v", q)
				}
				return answer, nil
			}}
			got, err := QueryKeys(context.Background(), p, "orca", pr, LoginOptions{}, []ssh.PublicKey{ok})
			kh, _ := os.ReadFile(filepath.Join(p.SSHDir, "known_hosts"))
			if answer == "yes" {
				if err != nil || !got[0] {
					t.Fatalf("got %v, %v", got, err)
				}
				if !strings.Contains(string(kh), "ssh-ed25519") {
					t.Errorf("known_hosts = %q, want the accepted line", kh)
				}
				// One question for the host, once per... the second connection
				// finds it in known_hosts (there is one key only here).
				if n := pr.count(AskHostKey); n != 1 {
					t.Errorf("asked the host key %d times", n)
				}
			} else {
				if err == nil {
					t.Fatalf("declined host key accepted: %v", got)
				}
				if len(kh) != 0 {
					t.Errorf("known_hosts written despite no: %q", kh)
				}
			}
			if srv.Logins() != 0 {
				t.Errorf("logins = %d", srv.Logins())
			}
		})
	}
}

func TestQueryKeysHostKeyMismatch(t *testing.T) {
	ok := edPub(t)
	p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{ok}})
	other := sshtest.New(t, sshtest.Options{User: "x", Password: "y"})
	line := knownhosts.Line([]string{knownhosts.Normalize(srv.Addr)}, other.HostKey)
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := QueryKeys(context.Background(), p, "orca", &scriptPrompter{}, LoginOptions{}, []ssh.PublicKey{ok})
	if err == nil || !strings.Contains(err.Error(), ErrHostKeyMismatch.Error()) {
		t.Fatalf("err = %v, want host key mismatch", err)
	}
}

func TestQueryKeysProxyJump(t *testing.T) {
	jump := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", AllowTCPForward: true})
	ok := edPub(t)
	target := sshtest.New(t, sshtest.Options{User: "alice", KeyOnly: true, AuthorizedKeys: []ssh.PublicKey{ok}})
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	jh, jp, _ := net.SplitHostPort(jump.Addr)
	th, tp, _ := net.SplitHostPort(target.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{
		{Alias: "jump", HostName: jh, User: "alice", Port: atoi(t, jp)},
		{Alias: "orca", HostName: th, User: "alice", Port: atoi(t, tp), ProxyJump: "jump"},
	}, "linux"); err != nil {
		t.Fatal(err)
	}
	trustHost(t, p, jump, target)
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	got, err := QueryKeys(context.Background(), p, "orca", pr, LoginOptions{}, []ssh.PublicKey{ok, edPub(t)})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0] || got[1] {
		t.Errorf("got %v", got)
	}
	if jump.Logins() != 1 || target.Logins() != 0 {
		t.Errorf("logins: jump=%d target=%d", jump.Logins(), target.Logins())
	}
	if pr.count(AskPassword) != 1 {
		t.Errorf("password asked %d times", pr.count(AskPassword))
	}
}

func TestQueryKeysCanceled(t *testing.T) {
	ok := edPub(t)
	p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{ok}})
	trustHost(t, p, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := QueryKeys(ctx, p, "orca", &scriptPrompter{}, LoginOptions{}, []ssh.PublicKey{ok}); err == nil {
		t.Fatal("canceled context returned no error")
	}
}

// TestQuerySignerNeverSigns: Sign and SignWithAlgorithm only record the
// accepted state and stop.
func TestQuerySignerNeverSigns(t *testing.T) {
	var acc atomic.Bool
	q := querySigner{pub: rsaPub(t), accepted: &acc}
	if sig, err := q.SignWithAlgorithm(nil, []byte("x"), ssh.KeyAlgoRSASHA256); sig != nil || err != errQueryStop || !acc.Load() {
		t.Errorf("SignWithAlgorithm = %v, %v, accepted=%v", sig, err, acc.Load())
	}
	if got := q.Algorithms(); len(got) != 3 || got[0] != ssh.KeyAlgoRSASHA512 {
		t.Errorf("rsa algorithms = %v", got)
	}
	if got := (querySigner{pub: edPub(t)}).Algorithms(); len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Errorf("ed25519 algorithms = %v", got)
	}
}

// --- ListRemoteKeys ---

// keyLine returns the authorized_keys line of pub with comment.
func keyLine(pub ssh.PublicKey, comment string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment + "\n"
}

// shHandler runs the exec'd "sh" command with a real POSIX shell, with HOME
// set to home.
func shHandler(t *testing.T, home string) sshtest.Handler {
	return func(args string, _ io.Reader, stdout, stderr io.Writer) int {
		cmd := exec.Command("sh", "-c", "sh "+args)
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return 1
		}
		return 0
	}
}

// execClient connects to a password server with the given commands.
func execClient(t *testing.T, cmds map[string]sshtest.Handler) (*ssh.Client, *sshtest.Server) {
	t.Helper()
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Commands: cmds})
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	h, port, _ := net.SplitHostPort(srv.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "home", HostName: h, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	trustHost(t, p, srv)
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Dial(context.Background(), p, "home", pr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListRemoteKeys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	home := t.TempDir()
	c, srv := execClient(t, map[string]sshtest.Handler{"sh": shHandler(t, home)})
	ctx := context.Background()

	// No ~/.ssh at all: no keys, no error.
	keys, err := ListRemoteKeys(ctx, c)
	if err != nil || len(keys) != 0 {
		t.Fatalf("no .ssh: %v, %v", keys, err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An empty ~/.ssh (no *.pub, the glob stays literal).
	if keys, err = ListRemoteKeys(ctx, c); err != nil || len(keys) != 0 {
		t.Fatalf("empty .ssh: %v, %v", keys, err)
	}

	ed, rs := edPub(t), rsaPub(t)
	writeFile(t, filepath.Join(sshDir, "id_ed25519"), "PRIVATE ed")
	writeFile(t, filepath.Join(sshDir, "id_ed25519.pub"), keyLine(ed, "alice@laptop"))
	writeFile(t, filepath.Join(sshDir, "id_rsa"), "PRIVATE rsa")
	writeFile(t, filepath.Join(sshDir, "id_rsa.pub"), keyLine(rs, "evil\x1b[31m comment"))
	// A .pub that doesn't parse (with its private sibling).
	writeFile(t, filepath.Join(sshDir, "bad"), "PRIVATE")
	writeFile(t, filepath.Join(sshDir, "bad.pub"), "this is not a key\n")
	// A .pub without its private key.
	writeFile(t, filepath.Join(sshDir, "orphan.pub"), keyLine(edPub(t), "orphan"))
	// A non-.pub file holding a key line must never be listed.
	writeFile(t, filepath.Join(sshDir, "authorized_keys"), keyLine(edPub(t), "authorized"))
	// A symlink escaping ~/.ssh, with a private sibling.
	outside := filepath.Join(home, "outside.pub")
	writeFile(t, outside, keyLine(edPub(t), "outside"))
	writeFile(t, filepath.Join(sshDir, "link"), "PRIVATE")
	if err := os.Symlink(outside, filepath.Join(sshDir, "link.pub")); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	// A symlink that stays inside ~/.ssh is fine.
	if err := os.Symlink(filepath.Join(sshDir, "id_ed25519.pub"), filepath.Join(sshDir, "alias.pub")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sshDir, "alias"), "PRIVATE")

	keys, err = ListRemoteKeys(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range keys {
		names = append(names, k.Name)
	}
	if got := strings.Join(names, ","); got != "alias,id_ed25519,id_rsa" {
		t.Fatalf("names = %s", got)
	}
	k := keys[1]
	if k.Type != "ssh-ed25519" || k.Comment != "alice@laptop" || k.Fingerprint != ssh.FingerprintSHA256(ed) || k.PublicKey == nil {
		t.Errorf("key = %+v", k)
	}
	if strings.ContainsRune(keys[2].Comment, 0x1b) || keys[2].Type != "ssh-rsa" {
		t.Errorf("rsa key = %+v", keys[2])
	}
	for _, e := range srv.Execs() {
		if !strings.HasPrefix(e, "sh -c '") || strings.Count(e, "'") != 2 {
			t.Errorf("exec %q is not one single-quoted sh -c", e)
		}
	}
}

func TestListRemoteKeysCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		n := fmt.Sprintf("key%02d", i)
		writeFile(t, filepath.Join(sshDir, n), "PRIVATE")
		writeFile(t, filepath.Join(sshDir, n+".pub"), keyLine(edPub(t), n))
	}
	c, _ := execClient(t, map[string]sshtest.Handler{"sh": shHandler(t, home)})
	keys, err := ListRemoteKeys(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != maxRemoteKeys || keys[0].Name != "key00" || keys[maxRemoteKeys-1].Name != "key15" {
		t.Errorf("got %d keys, first %q last %q", len(keys), keys[0].Name, keys[len(keys)-1].Name)
	}
}

// TestListRemoteKeysOutput: the parser skips noise (login banners, lines
// without a tab, bad keys, CRLF) and sorts.
func TestListRemoteKeysOutput(t *testing.T) {
	a, b := edPub(t), edPub(t)
	out := "Welcome to the cluster\n" +
		"id_b\t" + keyLine(b, "bee") +
		"junk-without-tab\n" +
		"bad\tssh-ed25519 !!!\n" +
		"../escape\t" + keyLine(a, "x") +
		"id_a\t" + strings.TrimSuffix(keyLine(a, "ay\x07"), "\n") + "\r\n"
	c, _ := execClient(t, map[string]sshtest.Handler{"sh": func(_ string, _ io.Reader, stdout, _ io.Writer) int {
		io.WriteString(stdout, out)
		return 0
	}})
	keys, err := ListRemoteKeys(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Name != "id_a" || keys[1].Name != "id_b" || keys[0].Comment != "ay" {
		t.Errorf("keys = %+v", keys)
	}
}

func TestListRemoteKeysFailure(t *testing.T) {
	c, _ := execClient(t, map[string]sshtest.Handler{"sh": func(_ string, _ io.Reader, _, stderr io.Writer) int {
		io.WriteString(stderr, "Unmatched quote.\n")
		return 3
	}})
	if _, err := ListRemoteKeys(context.Background(), c); err == nil || !strings.Contains(err.Error(), "Unmatched") {
		t.Fatalf("exit status 3: err = %v, want the stderr text", err)
	}
	if _, err := ListRemoteKeys(context.Background(), nil); err == nil {
		t.Fatal("nil client returned no error")
	}
}

func TestListRemoteKeysCanceled(t *testing.T) {
	block := make(chan struct{})
	c, _ := execClient(t, map[string]sshtest.Handler{"sh": func(string, io.Reader, io.Writer, io.Writer) int {
		<-block
		return 0
	}})
	t.Cleanup(func() { close(block) }) // runs before the server's Close
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ListRemoteKeys(ctx, c); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestShQuote(t *testing.T) {
	if got := shQuote(`a'b c`); got != `'a'\''b c'` {
		t.Errorf("shQuote = %s", got)
	}
	if strings.ContainsAny(listKeysScript, "\n!'") {
		t.Error("listKeysScript must be one line without ' or !")
	}
}

// --- LocalKeys ---

func TestLocalKeys(t *testing.T) {
	home := t.TempDir()
	p := sshconfig.Paths{SSHDir: filepath.Join(home, ".ssh")}
	kp, err := GenerateKey(p, "orca", "") // id_ed25519 + .pub
	if err != nil {
		t.Fatal(err)
	}
	work := edPub(t)
	writeFile(t, filepath.Join(p.SSHDir, "work"), "PRIVATE")
	writeFile(t, filepath.Join(p.SSHDir, "work.pub"), keyLine(work, "work"))
	writeFile(t, filepath.Join(p.SSHDir, "nopub"), "PRIVATE") // no .pub: skipped
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "orca", HostName: "orca.example.edu", User: "alice",
		IdentityFiles: []string{"~/.ssh/work", "~/.ssh/nopub"}}}, "linux"); err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519Key(t)
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	agSigner, _ := ssh.NewSignerFromKey(priv)
	// The agent also holds the default key: listed once, as "agent".
	useAgent(t, kr)

	got, err := LocalKeys(context.Background(), p, "orca")
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, k := range got {
		if k.PublicKey == nil || k.Fingerprint != ssh.FingerprintSHA256(k.PublicKey) {
			t.Errorf("bad entry %+v", k)
		}
		src[k.Source] = k.Fingerprint
	}
	if len(got) != 3 || src["agent"] != ssh.FingerprintSHA256(agSigner.PublicKey()) ||
		src["~/.ssh/work"] != ssh.FingerprintSHA256(work) || src["~/.ssh/id_ed25519"] != kp.Fingerprint {
		t.Errorf("keys = %v", src)
	}
}

func TestLocalKeysNoAgentNoFiles(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	useAgent(t, agent.NewKeyring())
	got, err := LocalKeys(context.Background(), p, "orca")
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
	// An agent whose key is also a file: the first (agent) source wins.
	kp, err := GenerateKey(p, "orca", "")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(kp.Path)
	priv, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	useAgent(t, kr)
	got, err = LocalKeys(context.Background(), p, "orca")
	if err != nil || len(got) != 1 || got[0].Source != "agent" {
		t.Errorf("got %+v, %v", got, err)
	}
}
