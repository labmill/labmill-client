package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/rules"

	"labmill-client/internal/config"
)

// addClusterHarness is the standard harness with a home institution and one
// other institution (fixtures: nothing here depends on private profiles).
func addClusterHarness(t *testing.T) (*harness, *[]string) {
	t.Helper()
	h := newHarness(t)
	h.env.Site = SiteInfo{Name: "Home University", Domain: "home.example.edu", Skill: "site-home-example-edu",
		Others: []OtherSite{{Name: "Far University", Domain: "far.example.org", Skill: "site-far-example-org", Clusters: []string{"farone", "hpcone"}}}}
	var applied []string
	h.env.PlanAddCluster = func(site, cluster string) (AddClusterPlan, error) {
		return AddClusterPlan{
			Site:    h.env.Site.Others[0],
			Cluster: config.Cluster{Name: cluster, Type: config.TypeSlurm, SSHHost: "far-login"},
			Hosts: []AddClusterHost{
				{Alias: "far-login", HostName: "login.far.example.org", Role: "login", KeyOnly: true},
				{Alias: "far-files", HostName: "files.far.example.org", Role: "transfer"},
			},
			Files: []string{"/home/u/.config/labmill/clusters.yaml", "/home/u/.ssh/config.d/labmill"},
			Hints: ClusterHints{Slurm: rules.SlurmHints{Preemptible: &rules.Preemptible{Partition: "preempt"}}},
		}, nil
	}
	h.env.AddCluster = func(_ context.Context, p AddClusterPlan, username string) (config.Cluster, error) {
		applied = append(applied, username)
		c := p.Cluster
		c.Site = p.Site.Domain
		return c, nil
	}
	return h, &applied
}

func TestAddClusterRefusals(t *testing.T) {
	h, applied := addClusterHarness(t)
	tests := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"own institution", map[string]any{"site": "home.example.edu", "cluster": "x", "username": "alice"}, "own institution"},
		{"unknown site", map[string]any{"site": "nowhere.example.com", "cluster": "x", "username": "alice"}, "no profile"},
		{"unknown cluster", map[string]any{"site": "far.example.org", "cluster": "nope", "username": "alice"}, "no cluster"},
		{"name already configured", map[string]any{"site": "far.example.org", "cluster": "hpcone", "username": "alice"}, "already exists"},
		{"bad user", map[string]any{"site": "far.example.org", "cluster": "farone", "username": "Al Ice"}, "account name"},
		{"no user", map[string]any{"site": "far.example.org", "cluster": "farone"}, "account name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.run(t, "add_cluster", tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if len(*applied) != 0 {
		t.Errorf("AddCluster ran for a refused request: %v", *applied)
	}
}

func TestAddClusterNotAvailable(t *testing.T) {
	h := newHarness(t)
	if _, err := h.run(t, "add_cluster", map[string]any{"site": "a.example.org", "cluster": "c", "username": "u"}); err == nil {
		t.Error("add_cluster without Env.PlanAddCluster succeeded")
	}
}

func TestAddClusterProposeAndRun(t *testing.T) {
	h, applied := addClusterHarness(t)
	in := map[string]any{"site": "far.example.org", "cluster": "farone", "username": "alice"}
	tool := h.tools["add_cluster"]
	if !tool.NeedsApproval {
		t.Fatal("add_cluster must need approval")
	}
	p, err := tool.Propose(mustJSON(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Far University", "farone", "alice", "login.far.example.org", "SSH key only", "files.far.example.org", "password and two-factor", "clusters.yaml", ".ssh/config.d/labmill"} {
		if !strings.Contains(p.Summary+"\n"+p.Details, want) {
			t.Errorf("proposal lacks %q:\n%s\n%s", want, p.Summary, p.Details)
		}
	}
	if len(*applied) != 0 {
		t.Fatal("Propose changed something")
	}
	out, err := h.run(t, "add_cluster", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(*applied) != 1 || (*applied)[0] != "alice" || !strings.Contains(out, "site-far-example-org") {
		t.Errorf("applied = %v, out = %q", *applied, out)
	}
	// Live: the other tools see the cluster at once.
	c, ok := h.env.Config.Cluster("farone")
	if !ok || c.Site != "far.example.org" {
		t.Fatalf("cluster not in the session config: %+v %v", c, ok)
	}
	if h.env.Site.SlurmFor("farone").Preemptible == nil {
		t.Errorf("hints not recorded: %+v", h.env.Site.Clusters)
	}
	lc, err := h.run(t, "list_clusters", map[string]any{})
	if err != nil || !strings.Contains(lc, "farone") {
		t.Errorf("list_clusters after add_cluster: %v\n%s", err, lc)
	}
	// A second add refuses: it exists now.
	if _, err := h.run(t, "add_cluster", in); err == nil || !strings.Contains(err.Error(), "already set up") {
		t.Errorf("second add: %v", err)
	}
}

func TestAddClusterRunChangedPlan(t *testing.T) {
	h, applied := addClusterHarness(t)
	tool := h.tools["add_cluster"]
	in := mustJSON(map[string]any{"site": "far.example.org", "cluster": "farone", "username": "alice"})
	p, err := tool.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	// The plan changes between Propose and Run (different hosts): refused.
	old := h.env.PlanAddCluster
	h.env.PlanAddCluster = func(s, c string) (AddClusterPlan, error) {
		pl, err := old(s, c)
		pl.Hosts = pl.Hosts[:1]
		return pl, err
	}
	if _, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p}); err == nil {
		t.Error("Run accepted a plan that differs from the approved one")
	}
	if len(*applied) != 0 {
		t.Error("AddCluster ran")
	}
}

func TestAddClusterApplyError(t *testing.T) {
	h, _ := addClusterHarness(t)
	h.env.AddCluster = func(context.Context, AddClusterPlan, string) (config.Cluster, error) {
		return config.Cluster{}, errors.New("disk full")
	}
	_, err := h.run(t, "add_cluster", map[string]any{"site": "far.example.org", "cluster": "farone", "username": "alice"})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v", err)
	}
	if _, ok := h.env.Config.Cluster("farone"); ok {
		t.Error("cluster added although AddCluster failed")
	}
}

func TestOtherSitesInPrompt(t *testing.T) {
	info := SiteInfo{Name: "Home University", Skill: "site-home-example-edu",
		Others: []OtherSite{
			{Name: "Far University", Domain: "far.example.org", Skill: "site-far-example-org", Clusters: []string{"farone", "fartwo"}},
			{Name: "Else College", Domain: "else.example.net"},
		}}
	for _, index := range []bool{false, true} {
		got := siteSection(info, index)
		for _, want := range []string{"Far University (far.example.org): clusters farone, fartwo; site skill \"site-far-example-org\"", "Else College (else.example.net)", "add_cluster", "explicitly", "own institution is the default"} {
			if !strings.Contains(got, want) {
				t.Errorf("brokerIndex=%v: section lacks %q:\n%s", index, want, got)
			}
		}
	}
	info.Others = nil
	if got := siteSection(info, false); strings.Contains(got, "Other institutions") || strings.Contains(got, "add_cluster") {
		t.Errorf("section mentions other institutions without any:\n%s", got)
	}
}

func TestAddClusterAlwaysAsks(t *testing.T) {
	h, _ := addClusterHarness(t)
	p, err := h.tools["add_cluster"].Propose(mustJSON(map[string]any{"site": "far.example.org", "cluster": "farone", "username": "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Secret {
		t.Error("add_cluster must be marked so that automatic approval never covers it")
	}
	ap := &agent.AutoApprover{Next: denyAll{}, On: func() bool { return true }}
	if d, err := ap.Approve(context.Background(), p); err != nil || d.Approved || d.Auto {
		t.Errorf("auto approver approved add_cluster: %+v %v", d, err)
	}
}

type denyAll struct{}

func (denyAll) Approve(context.Context, agent.Proposal) (agent.Decision, error) {
	return agent.Decision{}, nil
}
