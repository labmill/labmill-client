package launch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"labmill-client/internal/config"
)

func TestRememberAccount(t *testing.T) {
	p := filepath.Join(t.TempDir(), "clusters.yaml")
	src := "site: example.edu\nclusters:\n  # main\n  - name: bigcluster\n    type: slurm\n    ssh_host: bigcluster\n"
	os.WriteFile(p, []byte(src), 0o600)
	cfg, _ := config.Parse([]byte(src))
	c, _ := cfg.Cluster("bigcluster")
	if err := rememberAccount(p, cfg, c, "lab2", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c2, _ := got.Cluster("bigcluster"); c2.Account != "lab2" {
		t.Errorf("file account = %q", c2.Account)
	}
	if c2, _ := cfg.Cluster("bigcluster"); c2.Account != "lab2" {
		t.Errorf("memory account = %q", c2.Account)
	}
	if err := rememberAccount(p, cfg, c, "bad acct", time.Now()); err == nil {
		t.Error("bad account accepted")
	}
}
