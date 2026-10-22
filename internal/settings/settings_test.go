package settings

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
)

const fixtureYAML = `# my clusters (keep this comment)
site: generic
clusters:
  # the HPC cluster
  - name: hpc
    type: slurm
    ssh_host: login # the login host
    transfer: sftp
  - name: gpu
    type: slurm
    ssh_host: login
    s3:
      bucket: lab-bucket
      profile: lab
`

const fixtureLabmillSSH = `# Managed by Labmill. Edit with care: Labmill may rewrite this file.

Host login
    HostName login.example.edu
    User alice
    ServerAliveInterval 30

Host spare
    HostName spare.example.edu
    User alice
`

const fixtureUserSSH = `Include config.d/labmill

Host mine
    HostName mine.example.org
    User me
`

func newService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	p := Paths{
		Config: filepath.Join(dir, "config", "clusters.yaml"),
		SSH:    sshconfig.Paths{SSHDir: filepath.Join(dir, "ssh")},
		Prefs:  filepath.Join(dir, "config", "gui.json"),
		State:  filepath.Join(dir, "state", "labmill.db"),
		Log:    filepath.Join(dir, "state", "labmill.log"),
	}
	for _, f := range []struct{ path, data string }{
		{p.Config, fixtureYAML}, {p.SSH.LabmillConfig(), fixtureLabmillSSH}, {p.SSH.UserConfig(), fixtureUserSSH},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, []byte(f.data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{Paths: p, GOOS: "linux", Now: func() time.Time { return time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC) }}
}

func TestGet(t *testing.T) {
	s := newService(t)
	v := s.Get(Info{Version: "1.2.3", BrokerURL: "https://user:pw@broker.example/x?sig=abc",
		LoggedIn: []string{"login"}, LoginKnown: true})
	if v.ConfigError != "" || v.SSHError != "" {
		t.Fatalf("errors: %q %q", v.ConfigError, v.SSHError)
	}
	if v.Site.Domain != "generic" || len(v.Sites) == 0 {
		t.Errorf("site = %+v, sites = %d", v.Site, len(v.Sites))
	}
	if len(v.Clusters) != 2 || v.Clusters[1].S3 == nil || v.Clusters[1].S3.Bucket != "lab-bucket" {
		t.Errorf("clusters = %+v", v.Clusters)
	}
	if len(v.Hosts) != 3 || !v.Hosts[0].LoggedIn || !v.Hosts[0].InLabmill || v.Hosts[2].InLabmill || v.Hosts[2].Alias != "mine" {
		t.Errorf("hosts = %+v", v.Hosts)
	}
	if strings.Join(v.Hosts[0].UsedBy, ",") != "hpc,gpu" || len(v.Hosts[1].UsedBy) != 0 {
		t.Errorf("usedBy = %v / %v", v.Hosts[0].UsedBy, v.Hosts[1].UsedBy)
	}
	if v.About.BrokerURL != "https://broker.example/x" {
		t.Errorf("broker = %q", v.About.BrokerURL)
	}
	if v.Restart {
		t.Error("restart before any save")
	}
}

// secretLike matches strings that look like credentials.
var secretLike = regexp.MustCompile(`(?i)(password|passwd|secret|token|BEGIN [A-Z ]*PRIVATE KEY|AKIA[0-9A-Z]{16}|aws_secret|:pw@|sig=)`)

func TestDiagnosticsAndTextAreClean(t *testing.T) {
	s := newService(t)
	if r := s.SaveHost("", HostView{Alias: "inner", HostName: "inner.example.edu", ProxyJump: "bob@login:2222,alice@spare"}); !r.OK {
		t.Fatal(r)
	}
	v := s.Get(Info{Version: "1.2.3", BrokerURL: "https://user:pw@broker.example/x?sig=abc"})
	d := v.Diagnostics()
	if m := secretLike.FindString(d); m != "" {
		t.Errorf("diagnostics contain %q:\n%s", m, d)
	}
	for _, want := range []string{"version: 1.2.3", "hpc (Slurm)", "login → login.example.edu", "clusters.yaml", "via login:2222,spare"} {
		if !strings.Contains(d, want) {
			t.Errorf("diagnostics lack %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "alice") || strings.Contains(d, "AWS profile lab") || strings.Contains(d, "bob") {
		t.Errorf("diagnostics name the user or the AWS profile:\n%s", d)
	}
	txt := v.Text()
	for _, want := range []string{"## Settings", "gpu (Slurm)", "alice@login.example.edu", "(yours, read-only)", s.Paths.Config} {
		if !strings.Contains(txt, want) {
			t.Errorf("text lacks %q:\n%s", want, txt)
		}
	}
	if m := secretLike.FindString(txt); m != "" {
		t.Errorf("text contains %q", m)
	}
}

func TestSaveClusterInvalidThenValid(t *testing.T) {
	s := newService(t)
	c := s.Get(Info{}).Clusters[0]
	c.SSHHost = "bad host"
	r := s.SaveCluster("hpc", c)
	if r.OK || len(r.Errors) != 1 || r.Errors[0].Field != "ssh_host" {
		t.Fatalf("invalid save = %+v", r)
	}
	if s.BackupPath() != "" {
		t.Error("a failed save made a backup")
	}
	data, _ := os.ReadFile(s.Paths.Config)
	if string(data) != fixtureYAML {
		t.Error("a failed save changed the file")
	}
	c.SSHHost = "spare"
	c.Timezone = "America/Los_Angeles"
	r = s.SaveCluster("hpc", c)
	if !r.OK || !r.Restart {
		t.Fatalf("valid save = %+v", r)
	}
	data, _ = os.ReadFile(s.Paths.Config)
	for _, want := range []string{"# my clusters (keep this comment)", "# the HPC cluster", "ssh_host: spare # the login host", "timezone: America/Los_Angeles"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file lacks %q:\n%s", want, data)
		}
	}
	b := s.BackupPath()
	old, err := os.ReadFile(b)
	if err != nil || string(old) != fixtureYAML {
		t.Fatalf("backup %q: %v", b, err)
	}
	if fi, err := os.Stat(s.Paths.Config); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
	// A second save makes no second backup.
	c.Timezone = ""
	if r := s.SaveCluster("hpc", c); !r.OK {
		t.Fatal(r)
	}
	if s.BackupPath() != b {
		t.Error("the backup changed")
	}
	matches, _ := filepath.Glob(s.Paths.Config + ".labmill-backup-*")
	if len(matches) != 1 {
		t.Errorf("backups = %v", matches)
	}
	if !s.Get(Info{}).Restart {
		t.Error("View.Restart is false after a save")
	}
}

func TestSaveClusterAddDuplicateRemove(t *testing.T) {
	s := newService(t)
	r := s.SaveCluster("", Cluster{Name: "hpc", Type: "slurm", SSHHost: "login"})
	if r.OK || len(r.Errors) != 1 || r.Errors[0].Field != "name" {
		t.Fatalf("duplicate = %+v", r)
	}
	r = s.SaveCluster("", Cluster{Name: "new", Type: "slurm", S3: &S3{Bucket: "Bad_Bucket"}})
	fields := map[string]bool{}
	for _, e := range r.Errors {
		fields[e.Field] = true
	}
	if r.OK || !fields["ssh_host"] || !fields["s3.bucket"] {
		t.Fatalf("invalid new = %+v", r)
	}
	if r := s.SaveCluster("", Cluster{Name: "new", Type: "slurm", SSHHost: "spare", S3: &S3{}}); !r.OK {
		t.Fatalf("add = %+v", r)
	}
	cfg, err := config.Load(s.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := cfg.Cluster("new"); !ok || c.S3 != nil {
		t.Errorf("new = %+v (an empty s3 block must be dropped)", c)
	}
	if r := s.RemoveCluster("gpu"); !r.OK {
		t.Fatal(r)
	}
	if r := s.RemoveCluster("gpu"); r.OK || r.Error == "" {
		t.Errorf("second remove = %+v", r)
	}
	if r := s.SaveCluster("gone", Cluster{Name: "gone", Type: "slurm", SSHHost: "x"}); r.OK || r.Error == "" {
		t.Errorf("save of a missing cluster = %+v", r)
	}
}

func TestSaveSite(t *testing.T) {
	s := newService(t)
	if r := s.SaveSite("not a site"); r.OK || len(r.Errors) != 1 || r.Errors[0].Field != "site" {
		t.Errorf("bad site = %+v", r)
	}
	if r := s.SaveSite("generic"); !r.OK || r.Restart {
		t.Errorf("same site = %+v", r)
	}
}

func TestSaveHost(t *testing.T) {
	s := newService(t)
	userBefore, _ := os.ReadFile(s.Paths.SSH.UserConfig())
	// Field errors.
	r := s.SaveHost("", HostView{Alias: "bad alias", HostName: "", Port: 70000, ProxyJump: "-oProxyCommand"})
	fields := map[string]bool{}
	for _, e := range r.Errors {
		fields[e.Field] = true
	}
	for _, f := range []string{"alias", "hostName", "port", "proxyJump"} {
		if !fields[f] {
			t.Errorf("no %s error in %+v", f, r)
		}
	}
	// The user's own alias can't be added or edited.
	if r := s.SaveHost("", HostView{Alias: "mine", HostName: "x.example"}); r.OK || r.Errors[0].Field != "alias" {
		t.Errorf("shadowing = %+v", r)
	}
	if r := s.SaveHost("mine", HostView{Alias: "mine", HostName: "x.example"}); r.OK || r.Error == "" {
		t.Errorf("edit of a user entry = %+v", r)
	}
	// Rename refused; edit and add work.
	if r := s.SaveHost("login", HostView{Alias: "login2", HostName: "login.example.edu"}); r.OK || r.Errors[0].Field != "alias" {
		t.Errorf("rename = %+v", r)
	}
	if r := s.SaveHost("login", HostView{Alias: "login", HostName: "login.example.edu", User: "bob", Port: 2222}); !r.OK {
		t.Fatalf("edit = %+v", r)
	}
	h, _, err := sshconfig.Lookup(s.Paths.SSH, "login")
	if err != nil || h.User != "bob" || h.Port != 2222 {
		t.Errorf("after edit: %+v, %v", h, err)
	}
	if r := s.SaveHost("", HostView{Alias: "extra", HostName: "extra.example.edu", ProxyJump: "login"}); !r.OK {
		t.Fatalf("add = %+v", r)
	}
	userAfter, _ := os.ReadFile(s.Paths.SSH.UserConfig())
	if string(userAfter) != string(userBefore) {
		t.Errorf("the user's config changed:\n%s", userAfter)
	}
	// Remove: used ones are kept.
	if r := s.RemoveHost("login"); r.OK || !strings.Contains(r.Error, "hpc") || !strings.Contains(r.Error, "extra (as its jump host)") {
		t.Errorf("remove used = %+v", r)
	}
	if r := s.RemoveHost("mine"); r.OK {
		t.Errorf("remove of a user entry = %+v", r)
	}
	if r := s.RemoveHost("spare"); !r.OK {
		t.Errorf("remove spare = %+v", r)
	}
	if _, found, _ := sshconfig.Lookup(s.Paths.SSH, "spare"); found {
		t.Error("spare is still defined")
	}
}

func TestConcurrentSaves(t *testing.T) {
	s := newService(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := Cluster{Name: "hpc", Type: "slurm", SSHHost: "login"}
			if i%2 == 0 {
				c.SSHHost = "spare"
			}
			s.SaveCluster("hpc", c)
			s.Get(Info{})
		}(i)
	}
	wg.Wait()
	if _, err := config.Load(s.Paths.Config); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(s.Paths.Config + ".labmill-backup-*")
	if len(matches) != 1 {
		t.Errorf("backups = %v", matches)
	}
}

func TestMissingConfig(t *testing.T) {
	s := newService(t)
	s.Paths.Config = filepath.Join(t.TempDir(), "none", "clusters.yaml")
	v := s.Get(Info{})
	if v.ConfigError != "" || len(v.Clusters) != 0 {
		t.Errorf("view = %+v", v)
	}
	if r := s.SaveCluster("", Cluster{Name: "a", Type: "slurm", SSHHost: "login"}); !r.OK {
		t.Fatal(r)
	}
	if s.BackupPath() != "" {
		t.Errorf("backup of a missing file: %q", s.BackupPath())
	}
}

func TestSaveHostRefusesUserAliasesLikeOpenSSH(t *testing.T) {
	s := newService(t)
	user := "Include config.d/labmill\n\nHost hpc hpc.example.edu\n    User me\n\nHost Cluster\n    HostName c.example.org\n"
	if err := os.WriteFile(s.Paths.SSH.UserConfig(), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"hpc", "hpc.example.edu", "HPC", "cluster", "CLUSTER", "Login", "SPARE"} {
		r := s.SaveHost("", HostView{Alias: alias, HostName: "x.example.edu"})
		if r.OK || len(r.Errors) == 0 || r.Errors[0].Field != "alias" {
			t.Errorf("%s: %+v", alias, r)
		}
	}
	if r := s.SaveHost("", HostView{Alias: "Login"}); !strings.Contains(r.Errors[0].Message, "Labmill already has") {
		t.Errorf("a case-only clash with Labmill's own entry: %+v", r)
	}
	after, _ := os.ReadFile(s.Paths.SSH.UserConfig())
	if string(after) != user {
		t.Error("the user's config changed")
	}
	if r := s.SaveHost("", HostView{Alias: "hpc2", HostName: "x.example.edu"}); !r.OK {
		t.Errorf("a free alias: %+v", r)
	}
}

func TestSaveClusterRenameToExistingName(t *testing.T) {
	s := newService(t)
	// hpc comes first, so Validate would put "duplicate name" on gpu.
	r := s.SaveCluster("hpc", Cluster{Name: "gpu", Type: "slurm", SSHHost: "login"})
	if r.OK || len(r.Errors) != 1 || r.Errors[0].Field != "name" || !strings.Contains(r.Errors[0].Message, "already named gpu") {
		t.Errorf("rename to an existing name = %+v", r)
	}
	data, _ := os.ReadFile(s.Paths.Config)
	if string(data) != fixtureYAML {
		t.Error("the file changed")
	}
}

// "Run commands on the cluster without asking" is off by default, survives a
// save and reload of gui.json (its key must match gui.Prefs), and any
// unreadable file means off.
func TestAutoRunRemote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if AutoRunRemote(path) {
		t.Error("a missing file turned the setting on")
	}
	if err := os.WriteFile(path, []byte(`{"startInTray": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if AutoRunRemote(path) {
		t.Error("the default is on")
	}
	if err := os.WriteFile(path, []byte(`{"runWithoutAsking": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !AutoRunRemote(path) {
		t.Error("a saved on was read as off")
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if AutoRunRemote(path) {
		t.Error("a broken file turned the setting on")
	}
	v := (&Service{Paths: Paths{Prefs: path}}).Get(Info{RunWithoutAsking: true})
	if !v.App.RunWithoutAsking || !strings.Contains(v.Text(), "run without asking: on") {
		t.Errorf("the view lacks the setting:\n%s", v.Text())
	}
}
