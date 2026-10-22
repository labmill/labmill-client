package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
)

// homeSiteFixture is a home profile with the cluster "main" and, when
// withExtra, a later addition "extra" with a host of its own (M57.2).
func homeSiteFixture(t *testing.T, withExtra bool) site.Site {
	t.Helper()
	hosts := "  - {alias: ex-login, hostname: login.example.edu, role: login}\n"
	files := fstest.MapFS{
		"example.edu/SKILL.md":          {Data: []byte("---\nname: site-example-edu\ndescription: Example University specifics.\n---\nbody\n")},
		"example.edu/main/Cluster.yaml": {Data: []byte("name: main\ntype: slurm\nssh_host: ex-login\ntransfer: sftp\n")},
	}
	if withExtra {
		hosts += "  - {alias: ex-extra, hostname: extra.example.edu, role: login}\n"
		files["example.edu/extra/Cluster.yaml"] = &fstest.MapFile{Data: []byte("name: extra\ntype: slurm\nssh_host: ex-extra\ntransfer: sftp\norder: 10\n")}
	}
	files["example.edu/Site.yaml"] = &fstest.MapFile{Data: []byte("domain: example.edu\nname: Example University\nskill: site-example-edu\nhosts:\n" + hosts)}
	s, err := site.Load(files, fixtureDomain)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// oldInstall sets up an install from the profile without "extra".
func oldInstall(t *testing.T) (sshconfig.Paths, string, *config.Config) {
	t.Helper()
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	cfgPath := filepath.Join(t.TempDir(), "labmill", "clusters.yaml")
	cfg, err := WriteSetup(p, cfgPath, homeSiteFixture(t, false), SetupAnswers{Username: "alice"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	return p, cfgPath, cfg
}

func TestHomeMissing(t *testing.T) {
	_, _, cfg := oldInstall(t)
	full := homeSiteFixture(t, true)
	got := homeMissing(full, cfg.Clusters)
	if len(got) != 1 || got[0] != (app.HomeCluster{Name: "extra", Type: "slurm"}) {
		t.Errorf("missing = %+v", got)
	}
	if got := homeMissing(homeSiteFixture(t, false), cfg.Clusters); len(got) != 0 {
		t.Errorf("configured cluster offered: %+v", got)
	}
	if got := homeMissing(site.Generic(), nil); len(got) != 0 {
		t.Errorf("generic offers: %+v", got)
	}
	if info := siteInfo(full, cfg.Clusters); len(info.HomeMissing) != 1 || info.HomeMissing[0].Name != "extra" {
		t.Errorf("siteInfo.HomeMissing = %+v", info.HomeMissing)
	}
	if info := siteInfo(homeSiteFixture(t, false), cfg.Clusters); len(info.HomeMissing) != 0 {
		t.Errorf("siteInfo offers a configured cluster: %+v", info.HomeMissing)
	}
}

func TestAddHomeSiteCluster(t *testing.T) {
	p, cfgPath, cfg := oldInstall(t)
	full := homeSiteFixture(t, true)
	all := []site.Site{full}
	if u, from := knownUsername(p, cfg.Clusters, fixtureDomain, fixtureDomain); u != "alice" || from != "main" {
		t.Errorf("home username = %q", u)
	}
	before, _ := os.ReadFile(p.LabmillConfig())
	plan, err := planAddCluster(p, cfgPath, all, fixtureDomain, "extra")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Hosts) != 1 || plan.Hosts[0].HostName != "extra.example.edu" || len(plan.Files) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	c, err := addSiteCluster(p, cfgPath, all, plan, "alice", "linux", time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != "" || c.SSHHost != "ex-extra" {
		t.Errorf("cluster = %+v (a home cluster carries no site:)", c)
	}
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Site != fixtureDomain || len(got.Clusters) != 2 || got.Clusters[0].Name != "main" || got.Clusters[1].Name != "extra" {
		t.Errorf("clusters.yaml = site %q, %+v (want main, then extra appended)", got.Site, got.Clusters)
	}
	if x, _ := got.Cluster("extra"); x.Site != "" {
		t.Errorf("saved extra has site %q", x.Site)
	}
	// The alias is written with the account, the existing entries stay.
	after, _ := os.ReadFile(p.LabmillConfig())
	if !strings.HasPrefix(string(after), strings.TrimRight(string(before), "\n")) {
		t.Errorf("existing ssh entries changed:\n%s", after)
	}
	h, found, err := sshconfig.Lookup(p, "ex-extra")
	if err != nil || !found || h.User != "alice" || h.HostName != "extra.example.edu" {
		t.Errorf("ex-extra = %+v %v %v", h, found, err)
	}
	if len(homeMissing(full, got.Clusters)) != 0 {
		t.Error("still offered after the add")
	}
	// A second add changes nothing.
	if _, err := addSiteCluster(p, cfgPath, all, plan, "alice", "linux", time.Now()); err == nil {
		t.Error("second add succeeded")
	}
}

// A host the user already reaches under their own alias is referenced.
func TestAddHomeSiteClusterUserAlias(t *testing.T) {
	p, cfgPath, _ := oldInstall(t)
	existing, _ := os.ReadFile(p.UserConfig())
	if err := os.WriteFile(p.UserConfig(), []byte(string(existing)+"\nHost mine\n    HostName extra.example.edu\n    User alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	all := []site.Site{homeSiteFixture(t, true)}
	plan, _ := planAddCluster(p, cfgPath, all, fixtureDomain, "extra")
	c, err := addSiteCluster(p, cfgPath, all, plan, "alice", "linux", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.SSHHost != "mine" {
		t.Errorf("ssh_host = %q, want the user's own alias", c.SSHHost)
	}
	if yb, _ := os.ReadFile(p.LabmillConfig()); strings.Contains(string(yb), "ex-extra") {
		t.Errorf("a duplicate entry was written:\n%s", yb)
	}
}

// The settings page's path: preview, then add, through the Env functions.
func TestRuntimeAddHomeCluster(t *testing.T) {
	p, cfgPath, cfg := oldInstall(t)
	full := homeSiteFixture(t, true)
	all := []site.Site{full}
	env := &app.Env{Config: cfg, Site: siteInfo(full, cfg.Clusters)}
	env.PlanAddCluster = func(domain, cluster string) (app.AddClusterPlan, error) {
		return planAddCluster(p, cfgPath, all, domain, cluster)
	}
	env.KnownUsername = func(d string) (string, string) { return knownUsername(p, cfg.List(), fixtureDomain, d) }
	env.AddCluster = func(_ context.Context, plan app.AddClusterPlan, username string) (config.Cluster, error) {
		return addSiteCluster(p, cfgPath, all, plan, username, "linux", time.Now())
	}
	rt := &Runtime{Env: env, Site: full}
	plan, err := rt.PlanHomeCluster("extra")
	if err != nil || plan.Username != "alice" || len(plan.Hosts) != 1 || !strings.Contains(plan.Hosts[0], "extra.example.edu") {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if _, err := rt.PlanHomeCluster("main"); err == nil {
		t.Error("a configured cluster was planned")
	}
	if _, err := rt.AddHomeCluster(context.Background(), "extra", "Bad Name"); err == nil {
		t.Error("invalid account accepted")
	}
	if _, err := rt.AddHomeCluster(context.Background(), "extra", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Cluster("extra"); !ok {
		t.Error("the running session's config lacks the cluster")
	}
	if len(env.Site.HomeMissing) != 0 {
		t.Errorf("still offered: %+v", env.Site.HomeMissing)
	}
	if _, err := rt.PlanHomeCluster("extra"); err == nil {
		t.Error("planned again after the add")
	}
}
