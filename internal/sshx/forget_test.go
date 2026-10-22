package sshx

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// An older helper refuses the forget request: ForgetLogins goes on when it
// holds no login for the aliases, and refuses when it does.
func TestForgetLoginsOlderHelper(t *testing.T) {
	forgetUnsupportedHook = func() bool { return true }
	t.Cleanup(func() { forgetUnsupportedHook = nil })
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw",
		Commands: map[string]sshtest.Handler{"echo": echoHandler}}, time.Minute)
	ctx := context.Background()
	opts := ConnectOptions{SocketPath: sock}
	hc, err := Hold(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if err := hc.Forget([]string{"hpcx"}); !errors.Is(err, ErrForgetUnsupported) {
		t.Fatalf("Forget = %v, want ErrForgetUnsupported", err)
	}
	// No login for the alias: fine.
	if err := ForgetLogins(ctx, opts, []string{"hpcx"}); err != nil {
		t.Errorf("no login held: %v", err)
	}
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	c, err := Connect(ctx, "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := ForgetLogins(ctx, opts, []string{"hpcx"}); !errors.Is(err, ErrStillLoggedIn) {
		t.Errorf("login held: %v, want ErrStillLoggedIn", err)
	}
	if err := ForgetLogins(ctx, opts, []string{"other"}); err != nil {
		t.Errorf("other alias: %v", err)
	}
}

// A sign-in that is in flight when the helper forgets the alias is
// discarded when it completes: it may carry the old account name.
func TestForgetDiscardsInFlightLogin(t *testing.T) {
	started := make(chan struct{})
	gate := make(chan struct{})
	dial := func(ctx context.Context, p sshconfig.Paths, alias string, pr Prompter, o LoginOptions) (*ssh.Client, error) {
		close(started)
		<-gate
		return Dial(ctx, p, alias, pr, o)
	}
	_, sock, _ := helperEnvDial(t, sshtest.Options{User: "alice", Password: "pw",
		Commands: map[string]sshtest.Handler{"echo": echoHandler}}, time.Minute, dial)
	ctx := context.Background()
	res := make(chan error, 1)
	go func() {
		pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
		c, err := Connect(ctx, "hpcx", pr, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
		if err == nil {
			c.Close()
		}
		res <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("sign-in did not start")
	}
	if err := ForgetLogins(ctx, ConnectOptions{SocketPath: sock}, []string{"hpcx"}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	select {
	case err := <-res:
		if err == nil {
			t.Error("the sign-in that started before the reset was kept")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Connect did not return")
	}
	hc, err := Hold(ctx, ConnectOptions{SocketPath: sock})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if got, _ := hc.LoggedIn(); len(got) != 0 {
		t.Errorf("logged in after reset: %v", got)
	}
}
