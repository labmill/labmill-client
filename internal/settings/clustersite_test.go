package settings

import (
	"os"
	"strings"
	"testing"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
)

// M57.2: the clusters of the profile that clusters.yaml lacks are offered;
// configured ones, and the generic profile, offer nothing.
func TestMoreClusters(t *testing.T) {
	prof := site.Site{Domain: "home.example.edu",
		Hosts: []sshconfig.Host{{Alias: "p-main", HostName: "login.example.edu"}, {Alias: "p-extra", HostName: "extra.example.edu"}, {Alias: "p-gpu", HostName: "gpu.example.edu"}, {Alias: "p-gpu2", HostName: "spare.example.edu"}},
		Clusters: []config.Cluster{
			{Name: "main", Type: config.TypeSlurm, SSHHost: "p-main"}, {Name: "extra", Type: config.TypeSlurm, SSHHost: "p-extra"},
			{Name: "gpu", Type: config.TypeSlurm, SSHHost: "p-gpu"}, {Name: "gpu2", Type: config.TypeSlurm, SSHHost: "p-gpu2"}}}
	s := newService(t) // clusters hpc and gpu (both ssh_host login = login.example.edu)
	s.Profile = func(*config.Config) site.Site { return prof }
	// main is hpc under another name (same host), gpu is configured; extra and gpu2 are new.
	if v := s.Get(Info{}); len(v.MoreClusters) != 2 || v.MoreClusters[0] != (MoreCluster{"extra", "slurm"}) || v.MoreClusters[1] != (MoreCluster{"gpu2", "slurm"}) {
		t.Errorf("view more = %+v", v.MoreClusters)
	}
	// A cluster the user renamed, same login host: not offered again.
	cfg := "site: generic\nclusters:\n  - name: mygpu\n    type: slurm\n    ssh_host: spare\n"
	if err := os.WriteFile(s.Paths.Config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Get(Info{}).MoreClusters {
		if m.Name == "gpu2" {
			t.Errorf("a renamed cluster (same host) is offered: %+v", m)
		}
	}
	if got := moreClusters(site.Site{Domain: site.GenericDomain, Clusters: prof.Clusters}, nil, nil); got == nil || len(got) != 0 {
		t.Errorf("generic: %#v", got)
	}
	v := View{MoreClusters: []MoreCluster{{"extra", "slurm"}}}
	if txt := v.Text(); !strings.Contains(txt, "More clusters at your institution") || !strings.Contains(txt, "- extra (Slurm)") {
		t.Errorf("text lacks the list:\n%s", txt)
	}
	if txt := (View{}).Text(); strings.Contains(txt, "More clusters") {
		t.Error("empty list shown")
	}
	// The view carries an empty list, never null.
	if s := newService(t); s.Get(Info{}).MoreClusters == nil {
		t.Error("MoreClusters is nil in the view")
	}
}

// The page doesn't edit a cluster's institution; saving a cluster keeps it,
// whether the page sends it back or not.
func TestSaveClusterKeepsSite(t *testing.T) {
	s := newService(t)
	yaml := "site: home.example.edu\nclusters:\n  - name: far\n    type: slurm\n    ssh_host: far-login\n    site: far.example.org\n"
	if err := os.WriteFile(s.Paths.Config, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c := s.Get(Info{}).Clusters[0]
	if c.Site != "far.example.org" {
		t.Fatalf("view cluster site = %q", c.Site)
	}
	for _, sent := range []string{"far.example.org", ""} {
		c.Site = sent
		c.Timezone = "America/Los_Angeles"
		if r := s.SaveCluster("far", c); !r.OK {
			t.Fatalf("save (site %q): %+v", sent, r)
		}
		data, _ := os.ReadFile(s.Paths.Config)
		if !strings.Contains(string(data), "site: far.example.org") {
			t.Errorf("site lost when the page sent %q:\n%s", sent, data)
		}
		c.Timezone = ""
	}
}
