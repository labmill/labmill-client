package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterSiteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	in := &Config{Site: "home.example.edu", Clusters: []Cluster{
		{Name: "a", Type: TypeSlurm, SSHHost: "a-login"},
		{Name: "b", Type: TypeSlurm, SSHHost: "b-login", Site: "other.example.org"},
	}}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := out.Cluster("a")
	b, _ := out.Cluster("b")
	if a.Site != "" || b.Site != "other.example.org" || out.Site != "home.example.edu" {
		t.Errorf("sites = %q %q %q", out.Site, a.Site, b.Site)
	}
}

func TestClusterSiteValidation(t *testing.T) {
	_, err := Parse([]byte("clusters:\n  - {name: a, type: slurm, ssh_host: h, site: Not_A_Domain}\n"))
	if err == nil || !strings.Contains(err.Error(), "site") {
		t.Errorf("bad cluster site accepted: %v", err)
	}
}

func TestPutClusterYAMLSite(t *testing.T) {
	data := []byte("site: home.example.edu\nclusters:\n  - {name: a, type: slurm, ssh_host: h}\n")
	out, err := PutClusterYAML(data, "", Cluster{Name: "b", Type: TypeSlurm, SSHHost: "bh", Site: "other.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := cfg.Cluster("b"); b.Site != "other.example.org" {
		t.Errorf("site = %q\n%s", b.Site, out)
	}
	// Editing without a site removes the key, as for the other keys.
	out, err = PutClusterYAML(out, "b", Cluster{Name: "b", Type: TypeSlurm, SSHHost: "bh"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "other.example.org") {
		t.Errorf("site not removed:\n%s", out)
	}
}
