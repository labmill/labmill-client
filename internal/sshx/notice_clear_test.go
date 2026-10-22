package sshx

import (
	"context"
	"testing"
	"time"

	"labmill-client/internal/sshx/sshtest"
)

// lastNoticeCleared checks the notice sequence is a push notice followed by
// the empty (clear) message (M43.1).
func lastNoticeCleared(t *testing.T, pr *fakePrompter) {
	t.Helper()
	pr.mu.Lock()
	defer pr.mu.Unlock()
	n := pr.notified
	if len(n) < 2 || n[0] == "" || n[len(n)-1] != "" {
		t.Errorf("notices = %q, want a push notice then a clear", n)
	}
}

func TestLoginClearsNoticeOnEveryOutcome(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, PushDelay: 30 * time.Millisecond})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
		c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine())})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		lastNoticeCleared(t, pr)
	})
	t.Run("timeout", func(t *testing.T) {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNever})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
		_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine()), PushTimeout: 300 * time.Millisecond})
		if err == nil {
			t.Fatal("no error")
		}
		lastNoticeCleared(t, pr)
	})
	t.Run("cancel", func(t *testing.T) {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoNever})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(300 * time.Millisecond); cancel() }()
		_, err := Login(ctx, dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine()), PushTimeout: 30 * time.Second})
		if err == nil {
			t.Fatal("no error")
		}
		lastNoticeCleared(t, pr)
	})
	t.Run("passcode typed instead", func(t *testing.T) {
		srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, Passcode: "123456"})
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw", AskPasscode: "123456"}}
		c, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: knownHostsWith(t, srv.KnownHostsLine()), PreferPasscode: true})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		pr.mu.Lock()
		defer pr.mu.Unlock()
		if len(pr.notified) > 0 && pr.notified[len(pr.notified)-1] != "" {
			t.Errorf("notice left showing: %q", pr.notified)
		}
	})
}

func TestClearingPrompterTracksOnlySignInNotices(t *testing.T) {
	pr := &fakePrompter{}
	cp := &clearingPrompter{Prompter: pr}
	cp.Notify("Created the SSH key x")
	if cp.shown.Load() {
		t.Error("informational notice counted as a sign-in wait")
	}
	cp.Notify("Approve the sign-in request for h on your phone…")
	if !cp.shown.Load() {
		t.Error("push notice not tracked")
	}
	cp.Notify("")
	if cp.shown.Load() {
		t.Error("clear not tracked")
	}
}
