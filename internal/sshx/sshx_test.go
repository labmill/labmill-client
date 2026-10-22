package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// fakePrompter answers by question kind (optionally per host) and records
// everything.
type fakePrompter struct {
	mu        sync.Mutex
	answers   map[QuestionKind]string
	perHost   map[string]map[QuestionKind]string
	asked     []Question
	notified  []string
	cancelErr error
}

func (f *fakePrompter) Ask(_ context.Context, q Question) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, q)
	if f.cancelErr != nil {
		return "", f.cancelErr
	}
	if h, ok := f.perHost[q.Host]; ok {
		if a, ok := h[q.Kind]; ok {
			return a, nil
		}
	}
	return f.answers[q.Kind], nil
}

func (f *fakePrompter) Notify(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, msg)
}

func (f *fakePrompter) count(k QuestionKind) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.asked {
		if q.Kind == k {
			n++
		}
	}
	return n
}

func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func knownHostsWith(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func echoHandler(args string, _ io.Reader, stdout, _ io.Writer) int {
	fmt.Fprint(stdout, args)
	return 0
}

func TestIsDuoMenuAndNotEnrolled(t *testing.T) {
	for _, q := range []string{"Passcode or option (1-3): ", "passcode or option (1-2):", "Passcode or option (1-3)"} {
		if !IsDuoMenu(q) {
			t.Errorf("IsDuoMenu(%q) = false", q)
		}
	}
	for _, q := range []string{"Password: ", "Verification code: ", ""} {
		if IsDuoMenu(q) {
			t.Errorf("IsDuoMenu(%q) = true", q)
		}
	}
	if !IsNotEnrolled(sshtest.NotEnrolledInstruction) {
		t.Error("IsNotEnrolled(not-enrolled instruction) = false")
	}
	if IsNotEnrolled(fmt.Sprintf(sshtest.DuoInstruction, "alice")) {
		t.Error("IsNotEnrolled(menu) = true")
	}
}

func TestLoginPasswordAndPush(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "s3cret", Duo: sshtest.DuoApprove, PushDelay: 50 * time.Millisecond,
		Commands: map[string]sshtest.Handler{"echo": echoHandler}})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskUsername: "alice", AskPassword: "s3cret", AskHostKey: "yes"}}
	kh := knownHostsWith(t)
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{KnownHosts: kh})
	if err != nil {
		t.Fatalf("Login: %v (asked %+v)", err, pr.asked)
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Output("echo hello")
	if err != nil || string(out) != "hello" {
		t.Fatalf("exec = %q, %v", out, err)
	}
	if pr.count(AskUsername) != 1 || pr.count(AskPassword) != 1 {
		t.Errorf("asked %+v", pr.asked)
	}
	if pr.count(AskPasscode) != 0 {
		t.Error("asked for a passcode although push was used")
	}
	if len(pr.notified) == 0 || !strings.Contains(strings.ToLower(strings.Join(pr.notified, " ")), "phone") {
		t.Errorf("no push notification: %v", pr.notified)
	}
	for _, q := range pr.asked {
		if q.Host == "" {
			t.Errorf("question without host: %+v", q)
		}
	}
}

func TestLoginKnownUserSkipsUsernamePrompt(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNone})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if pr.count(AskUsername) != 0 || pr.count(AskHostKey) != 0 {
		t.Errorf("asked %+v", pr.asked)
	}
}

func TestLoginPasscode(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, Passcode: "123456"})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskPasscode: "123456"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh, PreferPasscode: true})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if pr.count(AskPasscode) != 1 {
		t.Errorf("asked %+v", pr.asked)
	}
}

func TestLoginPushTimeout(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNever})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	start := time.Now()
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh,
		PushTimeout: 300 * time.Millisecond, TwoFactorHelp: "Set up Duo at https://duo.example.edu"})
	if !errors.Is(err, ErrTwoFactor) {
		t.Fatalf("err = %v, want ErrTwoFactor", err)
	}
	if !strings.Contains(err.Error(), "https://duo.example.edu") {
		t.Errorf("help missing from %q", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("push timeout took %v", time.Since(start))
	}
}

func TestLoginNotEnrolled(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNotEnrolled})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh, TwoFactorHelp: "HELP-TEXT"})
	if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), "HELP-TEXT") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "nope"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if err == nil || errors.Is(err, ErrTwoFactor) {
		t.Fatalf("err = %v, want a non-2FA auth failure", err)
	}
}

func TestLoginCanceledDialog(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	cancelled := errors.New("user closed the dialog")
	pr := &fakePrompter{cancelErr: cancelled}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if err == nil {
		t.Fatal("login succeeded with a canceled dialog")
	}
}

func TestHostKeyTOFU(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	kh := filepath.Join(t.TempDir(), "known_hosts") // does not exist yet

	// "no" refuses.
	prNo := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskHostKey: "no"}}
	if _, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, prNo, LoginOptions{User: "alice", KnownHosts: kh}); err == nil {
		t.Fatal("login succeeded after refusing the host key")
	}

	// "yes" trusts and records it.
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskHostKey: "yes"}}
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	var hk Question
	for _, q := range pr.asked {
		if q.Kind == AskHostKey {
			hk = q
		}
	}
	if !strings.Contains(hk.Prompt, "SHA256:") {
		t.Errorf("host key prompt lacks fingerprint: %q", hk.Prompt)
	}
	b, err := os.ReadFile(kh)
	if err != nil || !strings.Contains(string(b), "ssh-ed25519") {
		t.Fatalf("known_hosts = %q, %v", b, err)
	}

	// Next login: no host key question.
	pr2 := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err = Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr2, LoginOptions{User: "alice", KnownHosts: kh})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if pr2.count(AskHostKey) != 0 {
		t.Error("asked about a host key that is already known")
	}
}

func TestHostKeyMismatch(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	other := sshtest.New(t, sshtest.Options{User: "x", Password: "y"})
	// known_hosts claims srv's address has other's key.
	line := strings.Replace(other.KnownHostsLine(), other.Addr[strings.LastIndex(other.Addr, ":")+1:], srv.Addr[strings.LastIndex(srv.Addr, ":")+1:], 1)
	kh := knownHostsWith(t, line)
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskHostKey: "yes"}}
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("err = %v, want ErrHostKeyMismatch", err)
	}
	if pr.count(AskHostKey) != 0 || pr.count(AskPassword) != 0 {
		t.Errorf("asked %+v despite mismatch", pr.asked)
	}
}

func TestDialProxyJump(t *testing.T) {
	jump := sshtest.New(t, sshtest.Options{User: "alice", Password: "jumppw", Duo: sshtest.DuoApprove, AllowTCPForward: true})
	target := sshtest.New(t, sshtest.Options{User: "alice", Password: "targetpw",
		Commands: map[string]sshtest.Handler{"hostname": func(string, io.Reader, io.Writer, io.Writer) int { return 0 }}})

	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	jh, jp, _ := net.SplitHostPort(jump.Addr)
	th, tp, _ := net.SplitHostPort(target.Addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{
		{Alias: "jump", HostName: jh, User: "alice", Port: atoi(t, jp)},
		{Alias: "target", HostName: th, User: "alice", Port: atoi(t, tp), ProxyJump: "jump"},
	}, "linux"); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(p.SSHDir, "known_hosts")
	if err := os.WriteFile(kh, []byte(jump.KnownHostsLine()+"\n"+target.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Both servers listen on 127.0.0.1, so answer password prompts in hop order.
	seq := &seqPrompter{passwords: []string{"jumppw", "targetpw"}}
	c, err := Dial(context.Background(), p, "target", seq, LoginOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run("hostname"); err != nil {
		t.Fatal(err)
	}
	if jump.Logins() != 1 || target.Logins() != 1 {
		t.Errorf("logins: jump=%d target=%d", jump.Logins(), target.Logins())
	}
	if got := target.Execs(); len(got) != 1 || got[0] != "hostname" {
		t.Errorf("target execs = %v", got)
	}
}

func TestDialProxyJumpCycle(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{
		{Alias: "a", HostName: "127.0.0.1", Port: 1, ProxyJump: "b"},
		{Alias: "b", HostName: "127.0.0.1", Port: 1, ProxyJump: "a"},
	}, "linux"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, p, "a", &seqPrompter{}, LoginOptions{}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a cycle error", err)
	}
}

// seqPrompter answers password prompts from a list, in order.
type seqPrompter struct {
	mu        sync.Mutex
	passwords []string
	i         int
}

func (s *seqPrompter) Ask(_ context.Context, q Question) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q.Kind != AskPassword {
		return "", nil
	}
	if s.i >= len(s.passwords) {
		return "", errors.New("no more passwords")
	}
	s.i++
	return s.passwords[s.i-1], nil
}

func (s *seqPrompter) Notify(string) {}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParseJump(t *testing.T) {
	got, err := parseJump("a, bob@b:2222 ,c")
	if err != nil || len(got) != 3 || got[0] != (hopSpec{name: "a"}) || got[1] != (hopSpec{name: "b", user: "bob", port: 2222}) || got[2].name != "c" {
		t.Fatalf("parseJump = %+v, %v", got, err)
	}
	if got, err := parseJump("none"); err != nil || len(got) != 0 {
		t.Errorf("none = %+v, %v", got, err)
	}
	if _, err := parseJump("a:xyz"); err == nil {
		t.Error("bad port accepted")
	}
}

func TestHostKeyFileModes(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	kh := filepath.Join(t.TempDir(), "sub", "known_hosts")
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskHostKey: "Y"}}
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if fi, err := os.Stat(kh); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts: %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(kh)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir: %v, %v", fi, err)
	}
}

func TestLoginNoticesNameTheProduct(t *testing.T) {
	for _, tc := range []struct {
		name, product string
		want, notWant string
	}{
		{"generic", "", "If your phone shows a sign-in request, approve it.", "Duo"},
		{"named", "Duo", "If your phone shows a sign-in request from Duo, approve it.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, PushDelay: 20 * time.Millisecond})
			pr := &fakePrompter{answers: map[QuestionKind]string{AskUsername: "alice", AskPassword: "pw", AskHostKey: "yes"}}
			c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{KnownHosts: knownHostsWith(t), TwoFactorName: tc.product})
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			all := strings.Join(pr.notified, "\n")
			if !strings.Contains(all, tc.want) {
				t.Errorf("notices %q lack %q", all, tc.want)
			}
			if !strings.Contains(all, "Approve the "+SignInRequest(tc.product)+" for") {
				t.Errorf("notices %q lack the approve line", all)
			}
			if tc.notWant != "" && strings.Contains(all, tc.notWant) {
				t.Errorf("notices %q mention %q", all, tc.notWant)
			}
		})
	}
}

func TestPasscodePromptNamesTheProduct(t *testing.T) {
	for _, tc := range []struct{ product, want string }{{"", "Passcode:"}, {"Duo", "Duo passcode:"}} {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, Passcode: "123456"})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskUsername: "alice", AskPassword: "pw", AskPasscode: "123456", AskHostKey: "yes"}}
		c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{KnownHosts: knownHostsWith(t), PreferPasscode: true, TwoFactorName: tc.product})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		got := ""
		for _, q := range pr.asked {
			if q.Kind == AskPasscode {
				got = q.Prompt
			}
		}
		if got != tc.want {
			t.Errorf("product %q: prompt = %q, want %q", tc.product, got, tc.want)
		}
	}
}

// A password rejected without any 2FA step carries the lockout warning, and the
// site's help when it has one.
func TestLoginWrongPasswordMessage(t *testing.T) {
	for _, help := range []string{"", "Reset it at https://reset.example.edu"} {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "nope"}}
		kh := knownHostsWith(t, srv.KnownHostsLine())
		_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh, TwoFactorHelp: help})
		if !errors.Is(err, ErrAuthDenied) || errors.Is(err, ErrTwoFactor) {
			t.Fatalf("help %q: err = %v, want ErrAuthDenied only", help, err)
		}
		if !strings.Contains(err.Error(), PasswordCareNote) {
			t.Errorf("help %q: care note missing from %q", help, err)
		}
		if got := strings.Contains(err.Error(), help); !got {
			t.Errorf("help %q missing from %q", help, err)
		}
		if help == "" && strings.Contains(err.Error(), "\n\n") {
			t.Errorf("empty help left a blank line: %q", err)
		}
	}
}

// A 2FA failure keeps its own message: no password warning.
func TestLoginTwoFactorNoPasswordNote(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNever})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh,
		PushTimeout: 300 * time.Millisecond, TwoFactorHelp: "HELP"})
	if !errors.Is(err, ErrTwoFactor) || strings.Contains(err.Error(), PasswordCareNote) {
		t.Fatalf("err = %v", err)
	}
}
