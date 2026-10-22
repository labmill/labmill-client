package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/livetest"
)

// TestLiveRemoteCommand runs read-only commands through remote_command's
// code path on the real login node (LABMILL_LIVE=1 and the owner's
// ControlMaster; BatchMode, so it never prompts). Nothing is written.
func TestLiveRemoteCommand(t *testing.T) {
	alias := livetest.Require(t)
	h := newHarness(t)
	h.env.Config = &config.Config{Clusters: []config.Cluster{{Name: "hpcone", Type: config.TypeSlurm, SSHHost: alias}}}
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) {
		return livetest.Runner{Alias: alias}, nil
	}
	tool := (&toolset{env: h.env}).remoteCommand()
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"sinfo", "-s"}, "PARTITION"},
		{[]string{"squeue", "--me"}, "JOBID"},
		{[]string{"ls", "-d", "~"}, "Output:\n."}, // a file reader on the login node
		{[]string{"module", "list"}, "odule"},     // module is a shell function: bash -lc finds it
	} {
		in, _ := json.Marshal(map[string]any{"cluster": "hpcone", "argv": c.argv})
		if _, err := tool.Propose(in); !errors.Is(err, agent.ErrNoApproval) {
			t.Fatalf("%q is not read-only: %v", c.argv, err)
		}
		out, err := tool.Run(context.Background(), in, agent.Decision{})
		if err != nil {
			t.Fatalf("%q: %v", c.argv, err)
		}
		if !strings.Contains(out, "Exit code: 0") || !strings.Contains(out, c.want) {
			t.Errorf("%q:\n%s", c.argv, out)
		}
		t.Logf("%q:\n%s", c.argv, out)
	}
}
