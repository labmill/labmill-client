package sshx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/sshx/sshtest"
)

// The password-then-"password"-method branch: the server lists both methods,
// the keyboard-interactive password is rejected, and the password is not sent
// a second time; the error is a password rejection with the care note.
func TestLoginWrongPasswordBothMethods(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", PasswordAuth: true})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "nope"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if !errors.Is(err, ErrPasswordRejected) || !errors.Is(err, ErrAuthDenied) || !strings.Contains(err.Error(), PasswordCareNote) {
		t.Fatalf("err = %v", err)
	}
	if n := pr.count(AskPassword); n != 1 {
		t.Errorf("password asked %d times, want 1", n)
	}
}

// A key-only server that the client does not know is key-only: no password is
// ever sent, so the failure is neutral (ErrAuthDenied, no care note, not a
// password rejection).
func TestLoginDeniedWithoutPassword(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", KeyOnly: true})
	pr := &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}
	kh := knownHostsWith(t, srv.KnownHostsLine())
	_, err := Login(context.Background(), dialTCP(t, srv.Addr), srv.Addr, pr, LoginOptions{User: "alice", KnownHosts: kh})
	if !errors.Is(err, ErrAuthDenied) || errors.Is(err, ErrPasswordRejected) {
		t.Fatalf("err = %v, want ErrAuthDenied only", err)
	}
	if strings.Contains(err.Error(), PasswordCareNote) || strings.Contains(err.Error(), "check the username and password") {
		t.Errorf("password advice in %q although no password was sent", err)
	}
	if !strings.Contains(err.Error(), "none of the offered sign-in methods") {
		t.Errorf("neutral text missing from %q", err)
	}
}
