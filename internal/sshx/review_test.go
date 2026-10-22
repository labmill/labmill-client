package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// Regression tests for the M5a review findings.

func loginWithKnownHosts(t *testing.T, srv *sshtest.Server, pr *fakePrompter, kh string, o LoginOptions) error {
	t.Helper()
	o.User, o.KnownHosts = "alice", kh
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, o)
	if err == nil {
		c.Close()
	}
	return err
}

// A server with ecdsa+ed25519 keys must present the key known_hosts holds, in
// plain and hashed form.
func TestHostKeyAlgorithmsFromKnownHosts(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", HostKeyTypes: []string{"ed25519", "ecdsa"}})
	for name, line := range map[string]string{
		"ed25519":        srv.KnownHostsLineFor("ed25519"),
		"ed25519 hashed": srv.KnownHostsLineHashed("ed25519"),
		"ecdsa":          srv.KnownHostsLineFor("ecdsa"),
		"ecdsa hashed":   srv.KnownHostsLineHashed("ecdsa"),
	} {
		t.Run(name, func(t *testing.T) {
			pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
			if err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, line), LoginOptions{}); err != nil {
				t.Fatalf("Login: %v", err)
			}
			if pr.count(AskHostKey) != 0 {
				t.Errorf("host key question for a known host: %+v", pr.asked)
			}
		})
	}
}

func TestKnownHostAlgorithmsNothingKnown(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	got, err := knownHostAlgorithms(filepath.Join(t.TempDir(), "nope"), srv.Addr, nil)
	if err != nil || got != nil {
		t.Errorf("missing file: %v, %v", got, err)
	}
	got, err = knownHostAlgorithms(knownHostsWith(t), srv.Addr, nil)
	if err != nil || got != nil {
		t.Errorf("empty file: %v, %v", got, err)
	}
	got, err = knownHostAlgorithms(knownHostsWith(t, srv.KnownHostsLine()), srv.Addr, nil)
	if err != nil || len(got) < 2 || got[0] != "ssh-ed25519" {
		t.Errorf("known ed25519: %v, %v", got, err)
	}
	for _, a := range got {
		if strings.Contains(a, "-cert-") {
			t.Errorf("certificate algorithm %q offered", a)
		}
	}
}

func TestHostKeyMismatchWithSeveralKeyTypes(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", HostKeyTypes: []string{"ed25519", "ecdsa"}})
	other := sshtest.New(t, sshtest.Options{User: "x", Password: "y"})
	line := strings.Replace(other.KnownHostsLine(), other.Addr[strings.LastIndex(other.Addr, ":")+1:], srv.Addr[strings.LastIndex(srv.Addr, ":")+1:], 1)
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskHostKey: "yes"}}
	err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, line), LoginOptions{})
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("err = %v, want ErrHostKeyMismatch", err)
	}
	if !strings.Contains(err.Error(), "contact") {
		t.Errorf("mismatch error not actionable: %q", err)
	}
}

func TestLoginAutoPush(t *testing.T) {
	t.Run("never approved", func(t *testing.T) {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoAutoPushNever})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
		start := time.Now()
		err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()),
			LoginOptions{PushTimeout: 300 * time.Millisecond, TwoFactorHelp: "HELP-TEXT"})
		if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), "HELP-TEXT") {
			t.Fatalf("err = %v, want ErrTwoFactor with help", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("took %v", time.Since(start))
		}
		if len(pr.notified) == 0 {
			t.Error("user was not told to approve the push")
		}
	})
	t.Run("approved", func(t *testing.T) {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoAutoPushApprove, PushDelay: 50 * time.Millisecond})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
		if err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()), LoginOptions{PushTimeout: 5 * time.Second}); err != nil {
			t.Fatalf("Login: %v", err)
		}
	})
}

// Only prompts that ask for a password get the password; a hidden
// "Verification code" prompt goes to the user without echo.
func TestHiddenNonPasswordPrompt(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", ExtraHiddenPrompt: "Verification code: ", ExtraHiddenAnswer: "424242"})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskOther: "424242"}}
	if err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()), LoginOptions{}); err != nil {
		t.Fatalf("Login: %v (asked %+v)", err, pr.asked)
	}
	if pr.count(AskPassword) != 1 || pr.count(AskOther) != 1 {
		t.Fatalf("asked %+v", pr.asked)
	}
	for _, q := range pr.asked {
		if q.Kind == AskOther && (q.Echo || !strings.Contains(q.Prompt, "Verification code")) {
			t.Errorf("bad other question %+v", q)
		}
	}
}

func TestAppendKnownHostAddsMissingNewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte("a ssh-ed25519 AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendKnownHost(p, "b ssh-ed25519 BBBB"); err != nil {
		t.Fatal(err)
	}
	if err := appendKnownHost(p, "c ssh-ed25519 CCCC"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if want := "a ssh-ed25519 AAAA\nb ssh-ed25519 BBBB\nc ssh-ed25519 CCCC\n"; string(b) != want {
		t.Errorf("known_hosts = %q, want %q", b, want)
	}
}

func TestDuoPushOption(t *testing.T) {
	tests := []struct {
		in   string
		opt  string
		ok   bool
		name string
	}{
		{"1. Duo Push to X\n2. Phone call\n", "1", true, "first"},
		{"1. Phone call to X\n 2. duo push to Y\n3. SMS", "2", true, "second, case-insensitive"},
		{"1. Phone call\n2. SMS passcodes\n", "", false, "no push option"},
		{"Enter a passcode", "1", true, "unparseable falls back to 1"},
	}
	for _, tc := range tests {
		opt, ok := duoPushOption(tc.in)
		if ok != tc.ok || (ok && opt != tc.opt) {
			t.Errorf("%s: duoPushOption = %q, %v", tc.name, opt, ok)
		}
	}
}

func TestParseJumpForms(t *testing.T) {
	tests := []struct {
		in   string
		want []hopSpec
	}{
		{"ssh://bob@jump.example.edu:2222", []hopSpec{{name: "jump.example.edu", user: "bob", port: 2222}}},
		{"ssh://a/,ssh://b", []hopSpec{{name: "a"}, {name: "b"}}},
		{"[::1]:2200", []hopSpec{{name: "::1", port: 2200}}},
		{"bob@[fe80::1]", []hopSpec{{name: "fe80::1", user: "bob"}}},
		{"::1", []hopSpec{{name: "::1"}}},
	}
	for _, tc := range tests {
		got, err := parseJump(tc.in)
		if err != nil || len(got) != len(tc.want) {
			t.Errorf("parseJump(%q) = %+v, %v", tc.in, got, err)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseJump(%q)[%d] = %+v, want %+v", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// OpenSSH resolves a hop's own ProxyJump only for the first hop of the list.
func TestFlattenFirstHopOnly(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{
		{Alias: "x", HostName: "x.example", ProxyJump: "w"},
		{Alias: "w", HostName: "w.example"},
		{Alias: "y", HostName: "y.example", ProxyJump: "z"},
		{Alias: "z", HostName: "z.example"},
		{Alias: "t", HostName: "t.example", ProxyJump: "x,y"},
	}, "linux"); err != nil {
		t.Fatal(err)
	}
	var chain []hopSpec
	if err := flatten(p, hopSpec{name: "t"}, nil, &chain); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range chain {
		names = append(names, h.name)
	}
	if got := strings.Join(names, ","); got != "w,x,y" {
		t.Errorf("chain = %s, want w,x,y", got)
	}
}

// slowPrompter delays answers to one kind of question.
type slowPrompter struct {
	*fakePrompter
	kind  QuestionKind
	delay time.Duration
}

func (s *slowPrompter) Ask(ctx context.Context, q Question) (string, error) {
	if q.Kind == s.kind {
		time.Sleep(s.delay)
	}
	return s.fakePrompter.Ask(ctx, q)
}

// The push timer measures server time only: a user who takes longer than
// PushTimeout to type a passcode must not trip it.
func TestPushTimerPausesWhileUserAnswers(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, Passcode: "424242"})
	pr := &slowPrompter{
		fakePrompter: &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskPasscode: "424242"}},
		kind:         AskPasscode, delay: 600 * time.Millisecond,
	}
	o := LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine()), PreferPasscode: true, PushTimeout: 200 * time.Millisecond}
	c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, o)
	if err != nil {
		t.Fatalf("Login: %v (asked %+v)", err, pr.asked)
	}
	c.Close()
	if pr.count(AskPasscode) != 1 {
		t.Errorf("asked %+v", pr.asked)
	}
}

// OpenSSH+PAM puts the Duo menu inside the question with an empty instruction.
func TestDuoMenuInQuestion(t *testing.T) {
	tests := []struct {
		name, menu, push string
		wantPasscode     bool
	}{
		{"push first", "1. Duo Push\n", "1", false},
		{"push second", " 1. Phone call to XXX\n 2. Duo Push to iPhone\n 3. SMS passcodes\n", "2", false},
		{"no push option", " 1. Phone call to XXX\n 2. SMS passcodes\n", "1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, PushDelay: 20 * time.Millisecond,
				MenuInQuestion: true, DuoMenu: tc.menu, PushOption: tc.push, Passcode: "777"})
			pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskPasscode: "777"}}
			if err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()), LoginOptions{PushTimeout: 5 * time.Second}); err != nil {
				t.Fatalf("Login: %v (asked %+v)", err, pr.asked)
			}
			if got := pr.count(AskPasscode) == 1; got != tc.wantPasscode {
				t.Errorf("passcode asked = %v, want %v (asked %+v)", got, tc.wantPasscode, pr.asked)
			}
			for _, m := range pr.notified {
				if tc.wantPasscode && strings.HasPrefix(m, "Approve the ") {
					t.Errorf("told to approve a push that was never sent: %v", pr.notified)
				}
			}
		})
	}
}

func TestNotEnrolledInQuestion(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, MenuInQuestion: true,
		DuoMenu: "You are not enrolled. Enroll at https://duo.example.edu/enroll\n"})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()), LoginOptions{TwoFactorHelp: "HELP"})
	if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), "HELP") {
		t.Fatalf("err = %v, want ErrTwoFactor", err)
	}
}

// A denied push is reported as a 2FA failure; a plain wrong password is not.
func TestRejectedAfterPush(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoDeny, PushDelay: 20 * time.Millisecond})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	err := loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()), LoginOptions{TwoFactorHelp: "HELP", PushTimeout: 5 * time.Second})
	if !errors.Is(err, ErrTwoFactor) || !strings.Contains(err.Error(), "denied or timed out") || !strings.Contains(err.Error(), "HELP") {
		t.Fatalf("denied push: err = %v", err)
	}

	srv = sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove})
	pr = &fakePrompter{answers: map[QuestionKind]string{AskPassword: "wrong"}}
	err = loginWithKnownHosts(t, srv, pr, knownHostsWith(t, srv.KnownHostsLine()), LoginOptions{PushTimeout: 5 * time.Second})
	if err == nil || errors.Is(err, ErrTwoFactor) {
		t.Fatalf("wrong password: err = %v, want a non-2FA failure", err)
	}
}

func TestNormalizeRemote(t *testing.T) {
	var nilTCP *net.TCPAddr
	for name, r := range map[string]net.Addr{"nil": nil, "typed nil": nilTCP, "pipe": pipeAddr()} {
		got := normalizeRemote("example.org:2222", r)
		if _, _, err := net.SplitHostPort(got.String()); err != nil {
			t.Errorf("%s: %q is not host:port", name, got)
		}
	}
	if got := normalizeRemote("10.0.0.1:22", pipeAddr()).String(); got != "10.0.0.1:22" {
		t.Errorf("derived = %q", got)
	}
}

func pipeAddr() net.Addr {
	a, b := net.Pipe()
	defer b.Close()
	defer a.Close()
	return a.RemoteAddr()
}

// Login over a non-TCP transport (net.Pipe splice) must still verify host
// keys against known_hosts, for both the known and the unknown case.
func TestLoginOverNetPipe(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	splice := func() net.Conn {
		a, b := net.Pipe()
		tcp := dialTCP(t, srv.Addr)
		go func() { io.Copy(tcp, b); tcp.Close() }()
		go func() { io.Copy(b, tcp); b.Close() }()
		return a
	}
	t.Run("known", func(t *testing.T) {
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
		o := LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine())}
		c, err := Login(context.Background(), splice(), srv.Addr, pr, o)
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		c.Close()
		if pr.count(AskHostKey) != 0 {
			t.Errorf("asked about a known host: %+v", pr.asked)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		other := sshtest.New(t, sshtest.Options{User: "x", Password: "y"})
		line := strings.Replace(other.KnownHostsLine(), other.Addr[strings.LastIndex(other.Addr, ":")+1:], srv.Addr[strings.LastIndex(srv.Addr, ":")+1:], 1)
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskHostKey: "yes"}}
		o := LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, line)}
		_, err := Login(context.Background(), splice(), srv.Addr, pr, o)
		if !errors.Is(err, ErrHostKeyMismatch) {
			t.Fatalf("err = %v, want ErrHostKeyMismatch", err)
		}
	})
	t.Run("unknown trusts once", func(t *testing.T) {
		kh := filepath.Join(t.TempDir(), "known_hosts")
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskHostKey: "yes"}}
		c, err := Login(context.Background(), splice(), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		c.Close()
		if b, _ := os.ReadFile(kh); !strings.Contains(string(b), "ssh-ed25519") {
			t.Errorf("known_hosts = %q", b)
		}
	})
}
