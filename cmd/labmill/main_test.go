package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"labmill-client/internal/site"
)

func TestRunDispatch(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"version"}, nil, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "Labmill ") {
		t.Errorf("version: %d %q", code, out.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"help"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "Usage") {
		t.Errorf("help: %d %q", code, out.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"frobnicate"}, nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("unknown: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"shell"}, nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "Usage: Labmill shell") {
		t.Errorf("shell without cluster: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"ssh-exec"}, nil, &out, &errb); code == 0 {
		t.Error("ssh-exec without alias succeeded")
	}
}

// The first-run form needs a login host for an institution
// without known hosts, and shows the home-institution note when given (M55).
func TestSetupFormHomeInstitution(t *testing.T) {
	g := site.Generic()
	if err := hostsGiven(g, ""); err == nil {
		t.Error("generic with no hosts accepted")
	}
	if err := hostsGiven(g, " login.example.edu "); err != nil {
		t.Errorf("login host: %v", err)
	}
	if got := welcomeText("choose it"); !strings.HasPrefix(got, "choose it\n") {
		t.Errorf("welcomeText = %q", got)
	}
	if strings.Contains(welcomeText(""), "\n\n") {
		t.Error("welcomeText has a blank note")
	}
}
