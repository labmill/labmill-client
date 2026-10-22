package launch

import (
	"testing"
	"testing/fstest"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
)

// A Slurm cluster's preemptible and scratch facts reach the model's site
// info (M61); a cluster without any gets no entry.
func TestSiteInfoSlurmHints(t *testing.T) {
	files := fstest.MapFS{
		"example.edu/SKILL.md":           {Data: []byte("---\nname: site-example-edu\ndescription: Example University specifics.\n---\nbody\n")},
		"example.edu/Site.yaml":          {Data: []byte("domain: example.edu\nname: Example University\nskill: site-example-edu\nhosts:\n  - {alias: ex-login, hostname: login.example.edu, role: login}\n")},
		"example.edu/main/Cluster.yaml":  {Data: []byte("name: main\ntype: slurm\nssh_host: ex-login\ntransfer: sftp\npreemptible: {partition: low, requeue: true}\nlocal_scratch: {path: /tmp}\n")},
		"example.edu/plain/Cluster.yaml": {Data: []byte("name: plain\ntype: slurm\nssh_host: ex-login\ntransfer: sftp\norder: 1\n")},
	}
	s, err := site.Load(files, fixtureDomain)
	if err != nil {
		t.Fatal(err)
	}
	clusters := []config.Cluster{
		{Name: "main", Type: config.TypeSlurm, SSHHost: "ex-login"},
		{Name: "plain", Type: config.TypeSlurm, SSHHost: "ex-login"},
	}
	info := siteInfo(s, clusters)
	h := info.SlurmFor("main")
	if h.Preemptible == nil || h.Preemptible.Partition != "low" || !h.Preemptible.Requeue || h.LocalScratch == nil || h.LocalScratch.Path != "/tmp" || h.GlobalScratch != nil {
		t.Errorf("main hints = %+v", h)
	}
	if _, ok := info.Clusters["plain"]; ok || !info.SlurmFor("plain").Zero() {
		t.Errorf("plain has hints: %+v", info.Clusters)
	}
}
