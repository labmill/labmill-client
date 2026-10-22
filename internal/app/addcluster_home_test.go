package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/config"
)

// M57.2: a cluster the home profile gained after this install was set up.
func homeMissingHarness(t *testing.T) (*harness, *[]string) {
	t.Helper()
	h, applied := addClusterHarness(t)
	h.env.Site.HomeMissing = []HomeCluster{{Name: "extra", Type: "slurm"}}
	h.env.KnownUsername = func(string) (string, string) { return "alice", "hpcone" }
	h.env.PlanAddCluster = func(site, cluster string) (AddClusterPlan, error) {
		return AddClusterPlan{
			Site:    OtherSite{Name: "Home University", Domain: "home.example.edu", Skill: "site-home-example-edu"},
			Cluster: config.Cluster{Name: cluster, Type: config.TypeSlurm, SSHHost: "home-extra"},
			Hosts:   []AddClusterHost{{Alias: "home-extra", HostName: "login.extra.example.edu", Role: "login"}},
			Files:   []string{"/home/u/.config/labmill/clusters.yaml", "/home/u/.ssh/config.d/labmill"},
		}, nil
	}
	h.env.AddCluster = func(_ context.Context, p AddClusterPlan, username string) (config.Cluster, error) {
		*applied = append(*applied, username)
		return p.Cluster, nil // a home cluster carries no `site:`
	}
	return h, applied
}

func TestAddClusterHomeSite(t *testing.T) {
	h, applied := homeMissingHarness(t)
	in := map[string]any{"site": "home.example.edu", "cluster": "extra"}
	tool := h.tools["add_cluster"]
	p, err := tool.Propose(mustJSON(in))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Secret || !tool.NeedsApproval || !strings.Contains(p.Summary, "account alice") || !strings.Contains(p.Details, "login.extra.example.edu") {
		t.Errorf("proposal = %+v", p)
	}
	if _, err := h.run(t, "add_cluster", in); err != nil {
		t.Fatal(err)
	}
	if len(*applied) != 1 || (*applied)[0] != "alice" {
		t.Errorf("applied = %v, want the account already in use", *applied)
	}
	c, ok := h.env.Config.Cluster("extra")
	if !ok || c.Site != "" {
		t.Errorf("session cluster = %+v %v", c, ok)
	}
	if len(h.env.Site.HomeMissing) != 0 {
		t.Errorf("still offered: %+v", h.env.Site.HomeMissing)
	}
	// Configured now: offered no more, refused.
	if _, err := h.run(t, "add_cluster", in); err == nil {
		t.Error("second add succeeded")
	}
}

func TestAddClusterHomeSiteRefusals(t *testing.T) {
	h, applied := homeMissingHarness(t)
	// A home cluster that isn't in the missing list is configured already.
	if _, err := h.run(t, "add_cluster", map[string]any{"site": "home.example.edu", "cluster": "hpcone"}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("configured home cluster: %v", err)
	}
	// An explicit account wins over the one in use.
	if _, err := h.run(t, "add_cluster", map[string]any{"site": "home.example.edu", "cluster": "extra", "username": "bob"}); err != nil {
		t.Fatal(err)
	}
	if len(*applied) != 1 || (*applied)[0] != "bob" {
		t.Errorf("applied = %v", *applied)
	}
	// Without a known account the model must ask.
	h2, _ := homeMissingHarness(t)
	h2.env.KnownUsername = func(string) (string, string) { return "", "" }
	if _, err := h2.run(t, "add_cluster", map[string]any{"site": "home.example.edu", "cluster": "extra"}); err == nil || !strings.Contains(err.Error(), "account name") {
		t.Errorf("no account: %v", err)
	}
}

// A reused SSH entry's account shows in the proposal, and a different typed
// account is refused (the entry's own User would win at sign-in).
func TestAddClusterReusedEntryAccount(t *testing.T) {
	h, applied := homeMissingHarness(t)
	old := h.env.PlanAddCluster
	h.env.PlanAddCluster = func(s, c string) (AddClusterPlan, error) {
		p, err := old(s, c)
		p.Hosts[0].ReusedAlias, p.Hosts[0].ReusedUser = "mine", "carol"
		return p, err
	}
	if _, err := h.tools["add_cluster"].Propose(mustJSON(map[string]any{"site": "home.example.edu", "cluster": "extra", "username": "alice"})); err == nil ||
		!strings.Contains(err.Error(), "carol") || !strings.Contains(err.Error(), "alice") {
		t.Errorf("different account: %v", err)
	}
	p, err := h.tools["add_cluster"].Propose(mustJSON(map[string]any{"site": "home.example.edu", "cluster": "extra", "username": "carol"}))
	if err != nil || !strings.Contains(p.Details, "existing SSH entry mine is reused as it is (account carol)") {
		t.Errorf("proposal = %+v, %v", p, err)
	}
	if len(*applied) != 0 {
		t.Error("Propose changed something")
	}
}

func TestHomeMissingInPrompt(t *testing.T) {
	info := SiteInfo{Name: "Home University", Skill: "site-home-example-edu"}
	for _, index := range []bool{false, true} {
		if got := siteSection(info, index); strings.Contains(got, "not set up yet") {
			t.Errorf("brokerIndex=%v: mentions missing clusters without any:\n%s", index, got)
		}
	}
	info.HomeMissing = []HomeCluster{{Name: "extra", Type: "slurm"}, {Name: "gpu2", Type: "slurm"}}
	want := "Also available at your institution, not set up yet: extra (Slurm), gpu2 (Slurm). Add one (add_cluster with your own institution as site) only when the user asks for it by name or clearly needs it; never as a default.\n"
	for _, index := range []bool{false, true} {
		if got := siteSection(info, index); !strings.Contains(got, want) {
			t.Errorf("brokerIndex=%v: section lacks the line:\n%s", index, got)
		}
	}
}
