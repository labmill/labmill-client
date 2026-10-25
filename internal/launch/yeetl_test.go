package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/sshconfig"
)

func TestImportYeetl(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, ".config", "labmill", "clusters.yaml")
	p := sshconfig.Paths{SSHDir: filepath.Join(home, ".ssh")}
	put := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	yeetlCfg := filepath.Join(home, ".config", "yeetl", "clusters.yaml")
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

	if ok, err := importYeetl(cfgPath, p, now); ok || err != nil {
		t.Fatalf("no yeetl setup: got %v %v", ok, err)
	}
	put(yeetlCfg, "clusters:\n  - {name: a, type: runai}\n")
	if ok, err := importYeetl(cfgPath, p, now); ok || err != nil {
		t.Fatalf("a config Labmill can't parse must be skipped: got %v %v", ok, err)
	}

	cfg := "clusters:\n  - {name: a, type: slurm, ssh_host: a-login}\n"
	put(yeetlCfg, cfg)
	put(filepath.Join(p.SSHDir, "config.d", "yeetl"), "Host a-login # yeetl-match\n  User me\n")
	ok, err := importYeetl(cfgPath, p, now)
	if !ok || err != nil {
		t.Fatalf("import: got %v %v", ok, err)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != cfg {
		t.Errorf("clusters.yaml = %q", b)
	}
	if b, _ := os.ReadFile(p.LabmillConfig()); string(b) != "Host a-login # labmill-match\n  User me\n" {
		t.Errorf("ssh file = %q", b)
	}
	if b, _ := os.ReadFile(p.UserConfig()); !strings.HasPrefix(string(b), sshconfig.IncludeLine) {
		t.Errorf("~/.ssh/config = %q", b)
	}
	if b, _ := os.ReadFile(yeetlCfg); string(b) != cfg {
		t.Error("yeetl's file must stay as it was")
	}

	put(yeetlCfg, "clusters:\n  - {name: b, type: slurm, ssh_host: b}\n")
	if ok, _ := importYeetl(cfgPath, p, now); ok {
		t.Error("an existing Labmill config must never be replaced")
	}
}
