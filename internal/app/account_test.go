package app

import (
	"context"
	"labmill-client/internal/config"
	"strings"
	"testing"
)

func TestDraftJobAccount(t *testing.T) {
	draft := func(accts []string, spec map[string]any) string {
		h := newHarness(t)
		h.be.accounts = accts
		spec["name"], spec["command"], spec["work_dir"] = "acct-job", "true", "/home/alice/w"
		out, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": spec})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := draft([]string{"lab1"}, map[string]any{}); !strings.Contains(out, "#SBATCH --account=lab1\n") {
		t.Errorf("one account not set: %s", out)
	}
	if out := draft([]string{"lab1", "lab2"}, map[string]any{}); !strings.Contains(out, "#SBATCH --account=\n") {
		t.Errorf("two accounts must stay unset: %s", out)
	}
	if out := draft([]string{"lab1", "lab2"}, map[string]any{"account": "lab2"}); !strings.Contains(out, "--account=lab2\n") {
		t.Errorf("named account lost: %s", out)
	}
}

func TestClusterInfoListsAccounts(t *testing.T) {
	h := newHarness(t)
	h.be.accounts = []string{"lab1", "lab2"}
	out, err := h.run(t, "cluster_info", map[string]any{"cluster": "hpcone"})
	if err != nil || !strings.Contains(out, "Accounts: lab1, lab2") {
		t.Errorf("out = %q, %v", out, err)
	}
	h.be.accounts = []string{"lab1"}
	if out, _ = h.run(t, "cluster_info", map[string]any{"cluster": "hpcone"}); strings.Contains(out, "Accounts:") {
		t.Errorf("one account listed: %s", out)
	}
}

func TestAccountRemembered(t *testing.T) {
	h := newHarness(t)
	h.be.accounts = []string{"lab1", "lab2"}
	var saved []string
	h.env.RememberAccount = func(_ context.Context, c config.Cluster, a string) error {
		saved = append(saved, c.Name+"="+a)
		cc, _ := h.env.Config.Cluster(c.Name)
		cc.Account = a
		h.env.Config.SetAccount(c.Name, a)
		return nil
	}
	draft := func(spec map[string]any) (string, error) {
		spec["name"], spec["command"], spec["work_dir"] = "acct-job", "true", "/home/alice/w"
		return h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": spec})
	}
	out, err := draft(map[string]any{"account": "lab2"})
	if err != nil || !strings.Contains(out, "--account=lab2\n") || !strings.Contains(out, "saved account lab2") {
		t.Fatalf("%v %s", err, out)
	}
	// Next time no account is named: the saved one is used, nothing is saved again.
	out, err = draft(map[string]any{})
	if err != nil || !strings.Contains(out, "--account=lab2\n") || len(saved) != 1 {
		t.Fatalf("%v %s %v", err, out, saved)
	}
	if _, err := draft(map[string]any{"account": "other"}); err == nil || !strings.Contains(err.Error(), "lab1, lab2") {
		t.Errorf("unknown account: %v", err)
	}
}
