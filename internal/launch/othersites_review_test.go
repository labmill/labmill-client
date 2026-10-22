package launch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/store"
)

// useFarSite makes the session know the far fixture institution as "other".
func useFarSite(t *testing.T) site.Site {
	t.Helper()
	far := farSite(t)
	orig := othersForSite
	othersForSite = func(string) []site.Site { return []site.Site{far} }
	t.Cleanup(func() { othersForSite = orig })
	return far
}

func tool(t *testing.T, env *app.Env, name string) agent.Tool {
	t.Helper()
	for _, tl := range app.Tools(env) {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return agent.Tool{}
}

// Build knows the other institutions, wires add_cluster, and a cluster added
// in the session is live: config, key-only host, host_path.
func TestBuildOtherInstitutionAddCluster(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("LABMILL_BROKER_URL", "")
	useFixtureSite(t)
	useFarSite(t)
	cfgPath, _ := config.DefaultPath()
	if err := config.Save(cfgPath, &config.Config{Site: fixtureDomain, Clusters: fixtureSite(t).Clusters}); err != nil {
		t.Fatal(err)
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if len(rt.Env.Site.Others) != 1 || rt.Env.Site.Others[0].Domain != farDomain || rt.Env.PlanAddCluster == nil || rt.Env.AddCluster == nil {
		t.Fatalf("Site.Others %+v, hooks %v %v", rt.Env.Site.Others, rt.Env.PlanAddCluster != nil, rt.Env.AddCluster != nil)
	}
	if _, ok := rt.Env.Skills.Get("site-far-example-org"); !ok {
		t.Error("the other institution's skill isn't loadable")
	}
	if !strings.Contains(rt.Agent.System, "Far University (far.example.org)") {
		t.Error("the prompt doesn't list the other institution")
	}
	add := tool(t, rt.Env, "add_cluster")
	in, _ := json.Marshal(map[string]string{"site": farDomain, "cluster": "farai", "username": "bob"})
	p, err := add.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Secret {
		t.Error("add_cluster must always ask, also under Approve everything automatically")
	}
	if _, err := add.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p}); err != nil {
		t.Fatal(err)
	}
	if c, ok := rt.Env.Config.Cluster("farai"); !ok || c.Site != farDomain {
		t.Fatalf("session config: %+v %v", c, ok)
	}
	if saved, err := config.Load(cfgPath); err != nil || func() bool { c, ok := saved.Cluster("farai"); return !ok || c.Site != farDomain }() {
		t.Errorf("saved config: %v %v", saved, err)
	}
	if got := rt.conns.keySnapshot().hostNames; len(got) != 1 || got[0] != "login.far.example.org" {
		t.Errorf("key-only hosts = %v", got)
	}
	if rt.Env.Site.SlurmFor("farai").Preemptible == nil {
		t.Errorf("hints = %+v", rt.Env.Site.Clusters)
	}
}

// The job monitor reads the configuration while add_cluster adds to it.
func TestAddClusterRaceWithMonitor(t *testing.T) {
	f := newMonitorFixture(t)
	f.job(t, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "100", State: store.StateRunning})
	f.be.set("100", backend.Status{State: store.StateRunning})
	far := farSite(t)
	home := fixtureSite(t)
	sp := homeSetupPaths(t)
	env := &app.Env{Config: f.m.cfg, Site: siteInfo(home, nil, far)}
	env.PlanAddCluster = func(d, c string) (app.AddClusterPlan, error) {
		return planAddCluster(sp, "clusters.yaml", []site.Site{far}, d, c)
	}
	n := 0
	env.AddCluster = func(_ context.Context, p app.AddClusterPlan, _ string) (config.Cluster, error) {
		n++
		c := p.Cluster
		c.Site = p.Site.Domain
		return c, nil
	}
	add := tool(t, env, "add_cluster")
	in, _ := json.Marshal(map[string]string{"site": farDomain, "cluster": "farone", "username": "bob"})
	p, err := add.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				f.m.check(context.Background())
				env.Config.List()
				env.Config.Cluster("farone")
			}
		}
	}()
	_, err = add.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
	close(stop)
	wg.Wait()
	if err != nil || n != 1 {
		t.Fatalf("run: %v (applied %d)", err, n)
	}
	if _, ok := f.m.cfg.Cluster("farone"); !ok {
		t.Error("cluster not added")
	}
}

// homeSetupPaths is a throwaway ssh settings location.
func homeSetupPaths(t *testing.T) (p sshconfig.Paths) {
	t.Helper()
	return sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
}
