package launch

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
)

// A home cluster added to the session must not turn the home institution's
// key-only hosts into hosts of "another institution" (M45.3 key copy).
func TestHomeAddKeepsHomeKeyHostsLocal(t *testing.T) {
	s, err := site.Load(fstest.MapFS{
		"example.edu/Site.yaml": {Data: []byte("domain: example.edu\nname: Example University\nskill: site-example-edu\nhosts:\n" +
			"  - {alias: ex-login, hostname: login.example.edu, role: login, auth: key}\n" +
			"  - {alias: ex-extra, hostname: extra.example.edu, role: login, auth: key}\n")},
		"example.edu/SKILL.md":           {Data: []byte("---\nname: site-example-edu\ndescription: Example University specifics.\n---\nbody\n")},
		"example.edu/extra/Cluster.yaml": {Data: []byte("name: extra\ntype: slurm\nssh_host: ex-extra\n")},
	}, fixtureDomain)
	if err != nil {
		t.Fatal(err)
	}
	cc := &connCache{homeDomain: fixtureDomain}
	cc.keys = keySiteOf(s)
	extra, _ := templateFor(s, "extra")
	cc.noteAddedCluster(fixtureDomain, extra, s)
	k := cc.keySnapshot()
	if k.foreign("login.example.edu") || k.foreign("extra.example.edu") {
		t.Error("a home host became foreign after a home add")
	}
	listed := false
	for _, n := range k.hostNames {
		listed = listed || n == "extra.example.edu"
	}
	if !listed {
		t.Errorf("the new home key-only host isn't known: %v", k.hostNames)
	}
	// Another institution's hosts still become foreign.
	far := farSite(t)
	fc, _ := templateFor(far, "farone")
	cc.noteAddedCluster(fixtureDomain, fc, far)
	if !cc.keySnapshot().foreign("login.far.example.org") {
		t.Error("another institution's key host is not foreign")
	}
}

// A cluster the user renamed (same login host) is not offered
// again; a different one is.
func TestHomeMissingRenamedAndSameHost(t *testing.T) {
	p, _, cfg := oldInstall(t)
	old := sshHostNameFunc
	sshHostNameFunc = func() func(string) string { return hostNameIn(p) }
	t.Cleanup(func() { sshHostNameFunc = old })
	full := homeSiteFixture(t, true)
	// "main" renamed by the user keeps its ssh_host: still configured.
	renamed := []config.Cluster{{Name: "my-own-name", Type: config.TypeSlurm, SSHHost: cfg.Clusters[0].SSHHost}}
	got := homeMissing(full, renamed)
	if len(got) != 1 || got[0].Name != "extra" {
		t.Errorf("missing after a rename = %+v, want only extra", got)
	}
	// "extra" configured under another name through a user alias for its host.
	existing, _ := os.ReadFile(p.UserConfig())
	if err := os.WriteFile(p.UserConfig(), []byte(string(existing)+"\nHost mine\n    HostName extra.example.edu\n    User alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	both := append(renamed, config.Cluster{Name: "mine-cluster", Type: config.TypeSlurm, SSHHost: "mine"})
	if got := homeMissing(full, both); len(got) != 0 {
		t.Errorf("same host under another alias and name is offered: %+v", got)
	}
}

// A reused SSH entry keeps its own account; a different typed account is
// refused with a plain message before anything is written.
func TestHomeAddAccountConflict(t *testing.T) {
	p, cfgPath, cfg := oldInstall(t)
	existing, _ := os.ReadFile(p.UserConfig())
	if err := os.WriteFile(p.UserConfig(), []byte(string(existing)+"\nHost mine\n    HostName extra.example.edu\n    User alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := homeSiteFixture(t, true)
	all := []site.Site{full}
	plan, err := planAddCluster(p, cfgPath, all, fixtureDomain, "extra")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Hosts) != 1 || plan.Hosts[0].ReusedAlias != "mine" || plan.Hosts[0].ReusedUser != "alice" {
		t.Fatalf("plan hosts = %+v", plan.Hosts)
	}
	if err := plan.AccountConflict("alice"); err != nil {
		t.Errorf("same account refused: %v", err)
	}
	err = plan.AccountConflict("bob")
	if err == nil || !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "bob") {
		t.Errorf("conflict message = %v", err)
	}
	before, _ := os.ReadFile(cfgPath)
	if _, err := addSiteCluster(p, cfgPath, all, plan, "bob", "linux", time.Now()); err == nil {
		t.Error("addSiteCluster accepted a different account for a reused entry")
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != string(before) {
		t.Error("clusters.yaml changed by a refused add")
	}
	// Through the Runtime (the settings page): preview shows the reuse, the
	// check refuses before the add starts.
	env := &app.Env{Config: cfg, Site: siteInfo(full, cfg.Clusters)}
	env.PlanAddCluster = func(domain, cluster string) (app.AddClusterPlan, error) {
		return planAddCluster(p, cfgPath, all, domain, cluster)
	}
	env.AddCluster = func(ctx context.Context, plan app.AddClusterPlan, username string) (config.Cluster, error) {
		return addSiteCluster(p, cfgPath, all, plan, username, "linux", time.Now())
	}
	rt := &Runtime{Env: env, Site: full}
	pv, err := rt.PlanHomeCluster("extra")
	if err != nil || len(pv.Hosts) != 1 || !strings.Contains(pv.Hosts[0], "existing SSH entry mine") || !strings.Contains(pv.Hosts[0], "account alice") {
		t.Errorf("preview = %+v, %v", pv, err)
	}
	if err := rt.CheckHomeCluster("extra", "bob"); err == nil || !strings.Contains(err.Error(), "alice") {
		t.Errorf("check with a different account: %v", err)
	}
	if err := rt.CheckHomeCluster("extra", "Bad Name"); err == nil {
		t.Error("invalid account passed the check")
	}
	if err := rt.CheckHomeCluster("extra", "alice"); err != nil {
		t.Errorf("check with the entry's account: %v", err)
	}
}
