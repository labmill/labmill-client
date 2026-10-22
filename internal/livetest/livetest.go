// Package livetest is test-only support for the opt-in live checks against
// the owner's already-authenticated OpenSSH ControlMaster. Production code
// must not import it.
package livetest

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"testing"

	"labmill-client/internal/backend"
)

// Runner runs commands on the cluster through `ssh -o BatchMode=yes <alias>`,
// which can never prompt.
type Runner struct{ Alias string }

// Run implements backend.Runner.
func (l Runner) Run(ctx context.Context, argv []string, stdin io.Reader) (backend.Result, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", l.Alias, backend.Quote(argv))
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code, err = ee.ExitCode(), nil
	}
	return backend.Result{Stdout: out.String(), Stderr: errb.String(), ExitCode: code}, err
}

// Require skips the test unless LABMILL_LIVE=1 and a live ControlMaster exists
// for the SSH alias named in LABMILL_LIVE_ALIAS (no default; skips when unset). It returns the alias.
func Require(t *testing.T) string {
	t.Helper()
	if os.Getenv("LABMILL_LIVE") != "1" {
		t.Skip("set LABMILL_LIVE=1 to run against the real cluster")
	}
	alias := os.Getenv("LABMILL_LIVE_ALIAS")
	if alias == "" {
		t.Skip("set LABMILL_LIVE_ALIAS to the SSH alias of a live ControlMaster")
	}
	if err := exec.Command("ssh", "-O", "check", alias).Run(); err != nil {
		t.Skipf("no live ControlMaster for %s", alias)
	}
	return alias
}

// RequireSubmit is Require plus LABMILL_LIVE_SUBMIT=1: tests that submit small
// jobs stay off in plain live runs, which are read-only.
func RequireSubmit(t *testing.T) string {
	t.Helper()
	alias := Require(t)
	if os.Getenv("LABMILL_LIVE_SUBMIT") != "1" {
		t.Skip("set LABMILL_LIVE_SUBMIT=1 to submit small test jobs")
	}
	return alias
}
