package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
)

func TestWriteSetupFilesHostAliasReuse(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	os.MkdirAll(p.SSHDir, 0o700)
	os.WriteFile(p.UserConfig(), []byte("Host myfiles\n    HostName files.example.edu\n    User bob\n"), 0o600)
	cfg, err := WriteSetup(p, filepath.Join(t.TempDir(), "c.yaml"), fixtureSite(t), SetupAnswers{Username: "alice"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	bi, _ := cfg.Cluster("bigiron")
	if bi.FilesHost != "myfiles" {
		t.Errorf("files_host = %q, want the user's myfiles alias", bi.FilesHost)
	}
	yb, _ := os.ReadFile(p.LabmillConfig())
	if strings.Contains(string(yb), "myfiles") || strings.Contains(string(yb), "files.example.edu") {
		t.Errorf("the user's files alias was rewritten:\n%s", yb)
	}
}

func TestWriteSetupFilesHostAliasClash(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	os.MkdirAll(p.SSHDir, 0o700)
	// The template alias is taken by an unrelated host.
	os.WriteFile(p.UserConfig(), []byte("Host ex-files\n    HostName other.example.org\n"), 0o600)
	cfg, err := WriteSetup(p, filepath.Join(t.TempDir(), "c.yaml"), fixtureSite(t), SetupAnswers{Username: "alice"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	bi, _ := cfg.Cluster("bigiron")
	if bi.FilesHost == "" || bi.FilesHost == "ex-files" {
		t.Fatalf("files_host = %q, want a non-clashing alias", bi.FilesHost)
	}
	h, found, err := sshconfig.Lookup(p, bi.FilesHost)
	if err != nil || !found || h.HostName != "files.example.edu" {
		t.Errorf("files alias %q = %+v %v %v", bi.FilesHost, h, found, err)
	}
}

func TestWriteSetupRecordsSite(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	cfgPath := filepath.Join(t.TempDir(), "c.yaml")
	cfg, err := WriteSetup(p, cfgPath, fixtureSite(t), SetupAnswers{Username: "alice", LoginHost: "ignored.example.edu"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site != fixtureDomain {
		t.Errorf("site = %q", cfg.Site)
	}
	if _, ok := cfg.Cluster("ignored"); ok {
		t.Error("login host answer used although the site has hosts")
	}
	loaded, err := config.Load(cfgPath)
	if err != nil || loaded.Site != fixtureDomain {
		t.Errorf("saved site = %v %v", loaded, err)
	}
}

func TestWriteSetupGeneric(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	cfgPath := filepath.Join(t.TempDir(), "c.yaml")
	g := site.Generic()
	if !NeedsHosts(g) || NeedsHosts(fixtureSite(t)) {
		t.Fatal("NeedsHosts")
	}
	cfg, err := WriteSetup(p, cfgPath, g, SetupAnswers{Username: "bob", LoginHost: "Login.Example.edu"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site != site.GenericDomain {
		t.Errorf("site = %q", cfg.Site)
	}
	sl, ok := cfg.Cluster("login")
	if !ok || sl.Type != config.TypeSlurm || sl.SSHHost != "login" {
		t.Errorf("slurm cluster = %+v", sl)
	}
	h, found, err := sshconfig.Lookup(p, "login")
	if err != nil || !found || h.HostName != "Login.Example.edu" || h.User != "bob" {
		t.Errorf("host = %+v %v %v", h, found, err)
	}
	if g.LoginHost() != "" {
		t.Error("WriteSetup changed the caller's site")
	}

	// Nothing answered: the home institution is unnamed (M55), so no setup
	// and no SSH files.
	p2 := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	cfg, err = WriteSetup(p2, filepath.Join(t.TempDir(), "c.yaml"), g, SetupAnswers{Username: "bob"}, "linux")
	if err == nil {
		t.Errorf("empty generic setup accepted: %+v", cfg)
	}
	if _, err := os.Stat(p2.UserConfig()); !os.IsNotExist(err) {
		t.Errorf("ssh config touched without hosts: %v", err)
	}

	for _, a := range []SetupAnswers{
		{Username: "bob", LoginHost: "bad host"},
		{Username: "bob", LoginHost: "-oProxyCommand=x"},
	} {
		if _, err := WriteSetup(p2, filepath.Join(t.TempDir(), "c.yaml"), g, a, "linux"); err == nil {
			t.Errorf("%+v accepted", a)
		}
	}
}

func TestWriteSetupProxyJump(t *testing.T) {
	s, err := site.Parse([]byte(`domain: example.edu
name: Example
skill: site-example-edu
hosts:
  - {alias: ex-gw, hostname: gw.example.edu, role: jump}
  - {alias: ex-login, hostname: login.example.edu, role: login, proxy_jump: ex-gw}
clusters:
  - {name: big, type: slurm, ssh_host: ex-login, transfer: sftp}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	os.MkdirAll(p.SSHDir, 0o700)
	// The user already reaches the gateway as "mygw": the jump must use it.
	os.WriteFile(p.UserConfig(), []byte("Host mygw\n    HostName gw.example.edu\n"), 0o600)
	if _, err := WriteSetup(p, filepath.Join(t.TempDir(), "c.yaml"), s, SetupAnswers{Username: "carol"}, "linux"); err != nil {
		t.Fatal(err)
	}
	h, found, err := sshconfig.Lookup(p, "ex-login")
	if err != nil || !found || h.ProxyJump != "mygw" {
		t.Errorf("login host = %+v %v %v", h, found, err)
	}
}

// fixtureDomain is the domain of fixtureSite.
const fixtureDomain = "example.edu"

// fixtureYAML is a complete example profile: login and transfer hosts, a
// two Slurm clusters. The logic tests use it so they run in a build
// without the private site profiles (docs/dev/sites-encryption.md).
const fixtureYAML = `domain: example.edu
name: Example University
skill: site-example-edu
docs_url: https://docs.example.edu
hosts:
  - {alias: ex-login, hostname: login.hpc.example.edu, role: login}
  - {alias: ex-files, hostname: files.example.edu, role: transfer}
clusters:
  - {name: bigiron, type: slurm, ssh_host: ex-login, files_host: ex-files, transfer: sftp}
  - {name: second, type: slurm, ssh_host: ex-login, transfer: sftp}
`

// fixtureSite returns the example.edu profile with its site skill.
func fixtureSite(t *testing.T) site.Site {
	t.Helper()
	s, err := site.Load(fstest.MapFS{
		"example.edu/Site.yaml":          {Data: []byte(fixtureYAML)},
		"example.edu/SKILL.md":           {Data: []byte("---\nname: site-example-edu\ndescription: Example University specifics for HPC/Slurm (bigiron).\n---\n\nSee references.\n")},
		"example.edu/References/info.md": {Data: []byte("# Example facts\n")},
	}, fixtureDomain)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// useFixtureSite makes Build resolve the site fixtureDomain to fixtureSite
// (other sites as usual) for the rest of the test.
func useFixtureSite(t *testing.T) {
	t.Helper()
	ex := fixtureSite(t)
	orig := siteForConfig
	siteForConfig = func(cfg *config.Config) (site.Site, error) {
		if cfg != nil && cfg.Site == fixtureDomain {
			return ex, nil
		}
		return orig(cfg)
	}
	t.Cleanup(func() { siteForConfig = orig })
}
