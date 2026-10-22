package site

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"labmill-client/internal/config"
	"labmill-client/internal/skill"
	"labmill-client/sites"
	"labmill-client/skills"
)

// The generic profile's cluster-folder template is ignored by the loader but
// stays a valid Cluster.yaml.
func TestGenericClusterTemplate(t *testing.T) {
	g := Generic()
	if len(g.Folders) != 0 || len(g.Clusters) != 0 {
		t.Errorf("generic folders %+v clusters %+v", g.Folders, g.Clusters)
	}
	data, err := fs.ReadFile(sites.FS, "generic/_cluster-template/Cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cf, err := ParseCluster("bigcluster", data)
	if err != nil || cf.Template == nil || cf.Template.SSHHost != "ex-login" {
		t.Errorf("template = %+v, %v", cf, err)
	}
	if _, err := fs.Stat(sites.FS, "generic/_cluster-template/References/Cluster.md"); err != nil {
		t.Error(err)
	}
}

func TestCheckEmbedded(t *testing.T) {
	if err := Check(); err != nil {
		t.Fatal(err)
	}
}

// Every embedded site loads, and its skill loads next to the generic skills
// under its own name, with references.
func TestEmbeddedSites(t *testing.T) {
	if _, err := listChecked(sites.FS); err != nil {
		t.Error(err)
	}
	entries, err := fs.ReadDir(sites.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() != GenericDomain {
			n++
		}
		s, err := Get(e.Name())
		if err != nil {
			t.Errorf("site %s: %v", e.Name(), err)
			continue
		}
		fsys, err := s.SkillsFS(skills.FS)
		if err != nil {
			t.Errorf("site %s: SkillsFS: %v", s.Domain, err)
			continue
		}
		set, err := skill.Load(fsys, "embedded")
		if err != nil {
			t.Errorf("site %s: skills: %v", s.Domain, err)
			continue
		}
		k, ok := set.Get(s.Skill)
		if !ok {
			t.Errorf("site %s: skill %s not loaded", s.Domain, s.Skill)
			continue
		}
		if len(k.References) == 0 {
			t.Errorf("site %s: skill has no references", s.Domain)
		}
		for _, g := range []string{"slurm-user", "research-computing"} {
			if _, ok := set.Get(g); !ok {
				t.Errorf("site %s: generic skill %s missing", s.Domain, g)
			}
		}
		if _, err := k.ReadReference("../Site.yaml"); err == nil {
			t.Errorf("site %s: Site.yaml readable as a reference", s.Domain)
		}
	}
	if n != len(List()) {
		t.Errorf("List() has %d sites, the embedded FS %d (one fails to load?)", len(List()), n)
	}
}

func TestChoices(t *testing.T) {
	c := Choices()
	if len(c) < 1 || c[0].Domain != Default().Domain || c[len(c)-1].Domain != GenericDomain {
		var d []string
		for _, s := range c {
			d = append(d, s.Domain)
		}
		t.Errorf("choices = %v", d)
	}
}

func TestGeneric(t *testing.T) {
	g, err := Get(GenericDomain)
	if err != nil {
		t.Fatal(err)
	}
	if g.Skill != "site-generic" || len(g.Hosts) != 0 || len(g.Clusters) != 0 || g.BrokerURL != "" || g.LoginHost() != "" {
		t.Errorf("generic = %+v", g)
	}
	fsys := fstest.MapFS{"a/SKILL.md": {Data: []byte("x")}}
	got, err := g.SkillsFS(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(got, "a/SKILL.md"); err != nil {
		t.Error("generic SkillsFS lost the generic skills")
	}
	if _, err := fs.Stat(got, "site-generic/SKILL.md"); err != nil {
		t.Error("generic SkillsFS has no site-generic skill")
	}
}

func TestForConfig(t *testing.T) {
	t.Setenv("LABMILL_BROKER_URL", "")
	def := Default().Domain
	s, _ := ForConfig(nil)
	if s.Domain != def {
		t.Errorf("nil config: %q", s.Domain)
	}
	s, _ = ForConfig(&config.Config{})
	if s.Domain != def {
		t.Errorf("config without site: %q", s.Domain)
	}
	s, err := ForConfig(&config.Config{Site: def})
	if err != nil || s.Domain != def {
		t.Errorf("default site named: %q %v", s.Domain, err)
	}
	s, err = ForConfig(&config.Config{Site: GenericDomain})
	if err != nil || s.Domain != GenericDomain {
		t.Errorf("generic: %q %v", s.Domain, err)
	}
	s, err = ForConfig(&config.Config{Site: "nowhere.example"})
	if err == nil || s.Domain != GenericDomain {
		t.Errorf("unknown site: %q %v", s.Domain, err)
	}
	t.Setenv("LABMILL_BROKER_URL", "https://broker.example/")
	if s, _ := ForConfig(nil); s.BrokerURL != "https://broker.example/" {
		t.Errorf("env override not applied: %q", s.BrokerURL)
	}
	if d := Default(); d.BrokerURL == "https://broker.example/" {
		t.Error("env override leaked into Default")
	}
}

func TestCurrentDefaultsWithoutConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LABMILL_BROKER_URL", "")
	if s := Current(); s.Domain != Default().Domain {
		t.Errorf("Current() = %q", s.Domain)
	}
}

const minimal = "domain: example.edu\nname: Example University\nskill: site-example-edu\n"

// fixtureYAML is a complete example profile: login and transfer hosts, a
// two Slurm clusters.
const fixtureYAML = minimal + `hosts:
  - {alias: ex-login, hostname: login.hpc.example.edu, role: login}
  - {alias: ex-files, hostname: files.example.edu, role: transfer}
clusters:
  - {name: bigiron, type: slurm, ssh_host: ex-login, files_host: ex-files, transfer: sftp}
  - {name: second, type: slurm, ssh_host: ex-login, files_host: ex-files, transfer: sftp}
`

func TestLoadFixture(t *testing.T) {
	s, err := Load(fstest.MapFS{
		"example.edu/Site.yaml":        {Data: []byte(fixtureYAML)},
		"example.edu/SKILL.md":         {Data: []byte("---\nname: site-example-edu\ndescription: Example University specifics.\n---\nbody\n")},
		"example.edu/References/a.md":  {Data: []byte("facts\n")},
		"example.edu/QUESTIONS.md":     {Data: []byte("private questions\n")},
		"example.edu/Evals/evals.json": {Data: []byte("{}")},
	}, "example.edu")
	if err != nil {
		t.Fatal(err)
	}
	if s.LoginHost() != "login.hpc.example.edu" || len(s.Clusters) != 2 {
		t.Errorf("fixture = %+v", s)
	}
	fsys, err := s.SkillsFS(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"site-example-edu/SKILL.md": true, "site-example-edu/References/a.md": true,
		"site-example-edu/Site.yaml": false, "site-example-edu/QUESTIONS.md": false, "site-example-edu/Evals/evals.json": false,
	} {
		if _, err := fs.Stat(fsys, p); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", p, err == nil, want)
		}
	}
}

func TestParseMinimal(t *testing.T) {
	s, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "Example University" || len(s.Hosts) != 0 || s.LoginHost() != "" || s.BrokerURL != "" {
		t.Errorf("minimal = %+v", s)
	}
}

func TestParseHostsAndJump(t *testing.T) {
	s, err := Parse([]byte(minimal + `hosts:
  - {alias: ex-gw, hostname: gw.example.edu, role: jump}
  - {alias: ex-login, hostname: login.example.edu, role: login, proxy_jump: ex-gw, port: 2222}
clusters:
  - {name: big, type: slurm, ssh_host: ex-login, transfer: sftp}
`))
	if err != nil {
		t.Fatal(err)
	}
	if s.LoginHost() != "login.example.edu" || s.Hosts[1].ProxyJump != "ex-gw" || s.Hosts[1].Port != 2222 {
		t.Errorf("hosts = %+v", s.Hosts)
	}
}

func TestParseKeyAuth(t *testing.T) {
	s, err := Parse([]byte(minimal + `hosts:
  - {alias: ex-login, hostname: login.example.edu, role: login, auth: key}
  - {alias: ex-files, hostname: files.example.edu, role: transfer}
key_upload:
  url: https://reg.example.edu/keys
  instruction: " Add the public key under Profile, SSH keys. "
`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.KeyOnly("ex-login") || s.KeyOnly("ex-files") || s.Auth["ex-files"] != AuthPassword {
		t.Errorf("Auth = %v", s.Auth)
	}
	if got := s.KeyOnlyHostNames(); len(got) != 1 || got[0] != "login.example.edu" {
		t.Errorf("KeyOnlyHostNames = %v", got)
	}
	if s.KeyUpload.URL != "https://reg.example.edu/keys" || s.KeyUpload.Instruction != "Add the public key under Profile, SSH keys." {
		t.Errorf("KeyUpload = %+v", s.KeyUpload)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"bad domain", "domain: Example\nname: x\nskill: site-Example\n", "domain"},
		{"no name", "domain: example.edu\nskill: site-example-edu\n", "name is empty"},
		{"wrong skill", "domain: example.edu\nname: x\nskill: example\n", "skill"},
		{"unknown key", minimal + "colour: blue\n", "colour"},
		{"http broker", minimal + "broker_url: http://b.example\n", "broker_url"},
		{"bad 2fa url", minimal + "two_factor: {url: ftp://x}\n", "two_factor.url"},
		{"host no name", minimal + "hosts: [{alias: a, role: login}]\n", "hostname is empty"},
		{"bad role", minimal + "hosts: [{alias: a, hostname: a.example.edu, role: boss}]\n", "unknown role"},
		{"dup alias", minimal + "hosts: [{alias: a, hostname: a.example.edu, role: login}, {alias: a, hostname: b.example.edu, role: transfer}]\n", "duplicate alias"},
		{"bad alias", minimal + "hosts: [{alias: 'a*', hostname: a.example.edu, role: login}]\n", "invalid character"},
		{"injection", minimal + "hosts: [{alias: a, hostname: 'a.example.edu ProxyCommand x', role: login}]\n", "invalid character"},
		{"jump unknown", minimal + "hosts: [{alias: a, hostname: a.example.edu, role: login, proxy_jump: gw}]\n", "proxy_jump"},
		{"cluster host unknown", minimal + "clusters: [{name: c, type: slurm, ssh_host: nope}]\n", "not one of the hosts"},
		{"cluster invalid", minimal + "clusters: [{name: c, type: kubernetes}]\n", "unknown type"},
		{"short forbidden term", minimal + "forbidden_terms: [ab]\n", "forbidden_terms"},
		{"second document", minimal + "---\nname: other\n", "only one YAML document"},
		{"bad auth", minimal + "hosts: [{alias: a, hostname: a.example.edu, role: login, auth: token}]\n", "unknown auth"},
		{"http key upload", minimal + "key_upload: {url: http://x.example}\n", "key_upload.url"},
		{"multiline key instruction", minimal + "key_upload: {instruction: \"a\\nb\"}\n", "key_upload.instruction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadChecksDirectory(t *testing.T) {
	fsys := fstest.MapFS{
		"other.edu/Site.yaml":   {Data: []byte(minimal)},
		"other.edu/SKILL.md":    {Data: []byte("x")},
		"example.edu/Site.yaml": {Data: []byte(minimal)},
	}
	if _, err := load(fsys, "other.edu"); err == nil || !strings.Contains(err.Error(), "example.edu") {
		t.Errorf("domain/directory mismatch: %v", err)
	}
	if _, err := load(fsys, "example.edu"); err == nil {
		t.Error("site without SKILL.md loaded")
	}
	if got := listFS(fsys); len(got) != 0 {
		t.Errorf("listFS kept broken sites: %d", len(got))
	}
}

func TestSkillsFSClash(t *testing.T) {
	ex, err := load(fstest.MapFS{
		"example.edu/Site.yaml": {Data: []byte(minimal)},
		"example.edu/SKILL.md":  {Data: []byte("x")},
	}, "example.edu")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"site-example-edu/SKILL.md": {Data: []byte("x")}}
	if _, err := ex.SkillsFS(fsys); err == nil {
		t.Error("clash not reported")
	}
	if _, err := ex.SkillsFS(fstest.MapFS{"other/SKILL.md": {Data: []byte("x")}}); err != nil {
		t.Errorf("no clash: %v", err)
	}
}

func TestSkillName(t *testing.T) {
	if got := SkillName("example.org"); got != "site-example-org" {
		t.Errorf("SkillName = %q", got)
	}
	for d, want := range map[string]bool{"example.edu": true, "a.b.c": true, "edu": false, "Ex.edu": false, "-a.edu": false, "generic": false} {
		if ValidDomain(d) != want {
			t.Errorf("ValidDomain(%q) = %v", d, !want)
		}
	}
}

// gpupodYAML is a second Slurm cluster folder's Cluster.yaml for the
// fixture profile (hosts from fixtureYAML).
const gpupodYAML = `name: gpubox
type: slurm
ssh_host: ex-login
files_host: ex-files
transfer: sftp
`

// hostsOnly is a profile whose clusters all live in cluster folders.
const hostsOnly = minimal + `hosts:
  - {alias: ex-login, hostname: login.hpc.example.edu, role: login}
  - {alias: ex-files, hostname: files.example.edu, role: transfer}
`

func folderFS(siteYAML string, extra map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"example.edu/Site.yaml":                   {Data: []byte(siteYAML)},
		"example.edu/SKILL.md":                    {Data: []byte("---\nname: site-example-edu\ndescription: Example University specifics.\n---\nbody\n")},
		"example.edu/References/access.md":        {Data: []byte("access\n")},
		"example.edu/Diagrams/architecture.svg":   {Data: []byte("<svg/>")},
		"example.edu/Evals/e.json":                {Data: []byte("{}")},
		"example.edu/bigiron/Cluster.yaml":        {Data: []byte("# Slurm\nname: bigiron\ntype: slurm\nssh_host: ex-login\nfiles_host: ex-files\ntransfer: sftp\nforbidden_terms: [bigq]\n")},
		"example.edu/bigiron/References/slurm.md": {Data: []byte("partitions\n")},
		"example.edu/bigiron/QUESTIONS.md":        {Data: []byte("private\n")},
		"example.edu/gpupod/Cluster.yaml":         {Data: []byte(gpupodYAML)},
		"example.edu/gpupod/References/pools.md":  {Data: []byte("pools\n")},
		"example.edu/gpupod/Diagrams/pools.svg":   {Data: []byte("<svg/>")},
		"example.edu/gpupod/Evals/e.json":         {Data: []byte("{}")},
		"example.edu/oldbox/Cluster.yaml":         {Data: []byte("# not confirmed yet:\n# name: oldbox\n# type: slurm\n")},
		"example.edu/oldbox/References/notes.md":  {Data: []byte("unverified\n")},
		"example.edu/Tools/helper.sh":             {Data: []byte("#!/bin/sh\n")},
		"example.edu/Tools/Cluster.yaml":          {Data: []byte("not: [valid")},
		"example.edu/_template/Cluster.yaml":      {Data: []byte("not: [valid")},
		"example.edu/_template/References/x.md":   {Data: []byte("template\n")},
	}
	for p, c := range extra {
		if c == "" {
			delete(fsys, p)
			continue
		}
		fsys[p] = &fstest.MapFile{Data: []byte(c)}
	}
	return fsys
}

func TestLoadClusterFolders(t *testing.T) {
	s, err := Load(folderFS(hostsOnly, nil), "example.edu")
	if err != nil {
		t.Fatal(err)
	}
	var names, dirs []string
	for _, c := range s.Clusters {
		names = append(names, c.Name)
	}
	for _, f := range s.Folders {
		dirs = append(dirs, f.Dir)
	}
	if strings.Join(names, ",") != "bigiron,gpubox" || strings.Join(dirs, ",") != "bigiron,gpupod,oldbox" {
		t.Errorf("clusters %v, folders %v", names, dirs)
	}
	if s.Folders[2].Template != nil {
		t.Errorf("comment-only Cluster.yaml has a template: %+v", s.Folders[2].Template)
	}
	if f, ok := s.Cluster("gpubox"); !ok || f.Dir != "gpupod" {
		t.Errorf("Cluster(gpubox) = %+v %v", f, ok)
	}
	if _, ok := s.Cluster("oldbox"); ok {
		t.Error("a folder without a template is found by cluster name")
	}
	if !strings.Contains(strings.Join(s.ForbiddenTerms, ","), "bigq") {
		t.Errorf("cluster forbidden_terms not merged: %v", s.ForbiddenTerms)
	}
	fsys, err := s.SkillsFS(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"site-example-edu/SKILL.md": true, "site-example-edu/References/access.md": true,
		"site-example-edu/bigiron/References/slurm.md": true, "site-example-edu/gpupod/References/pools.md": true,
		"site-example-edu/oldbox/References/notes.md": true,
		"site-example-edu/bigiron/Cluster.yaml":       false, "site-example-edu/bigiron/QUESTIONS.md": false,
		"site-example-edu/gpupod/Diagrams/pools.svg": false, "site-example-edu/gpupod/Evals/e.json": false,
		"site-example-edu/Diagrams/architecture.svg": false, "site-example-edu/Evals/e.json": false,
		"site-example-edu/Site.yaml": false, "site-example-edu/_template/References/x.md": false,
	} {
		if _, err := fs.Stat(fsys, p); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", p, err == nil, want)
		}
	}
	set, err := skill.Load(fsys, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := set.Get("site-example-edu")
	if got := strings.Join(k.References, ","); got != "access.md,bigiron/slurm.md,gpupod/pools.md,oldbox/notes.md" {
		t.Errorf("references = %s", got)
	}
	if got, err := k.ReadReference("gpupod/References/pools.md"); err != nil || got != "pools\n" {
		t.Errorf("ReadReference = %q, %v", got, err)
	}
}

// Cluster.yaml's order: sorts the templates: lower first, negative allowed,
// default 0, ties by folder name.
func TestClusterFolderOrder(t *testing.T) {
	slurm := func(name, extra string) string {
		return "name: " + name + "\ntype: slurm\nssh_host: ex-login\n" + extra
	}
	for _, tc := range []struct {
		name  string
		extra map[string]string
		want  string
	}{
		// bigiron (0) and gpupod (0) come from folderFS.
		{"by folder name without order", map[string]string{
			"example.edu/aaa/Cluster.yaml": slurm("aaa", ""),
		}, "aaa,bigiron,gpubox"},
		{"higher order goes after", map[string]string{
			"example.edu/aaa/Cluster.yaml": slurm("aaa", "order: 10\n"),
		}, "bigiron,gpubox,aaa"},
		{"negative order goes first", map[string]string{
			"example.edu/zzz/Cluster.yaml": slurm("zzz", "order: -1\n"),
		}, "zzz,bigiron,gpubox"},
		{"ties by folder name", map[string]string{
			"example.edu/ccc/Cluster.yaml": slurm("ccc", "order: 5\n"),
			"example.edu/bbb/Cluster.yaml": slurm("bbb", "order: 5\n"),
		}, "bigiron,gpubox,bbb,ccc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Load(folderFS(hostsOnly, tc.extra), "example.edu")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(s.ClusterNames(), ","); got != tc.want {
				t.Errorf("clusters = %s, want %s", got, tc.want)
			}
		})
	}
	if cf, err := ParseCluster("ordered", []byte(slurm("o", "order: 7\n"))); err != nil || cf.Order != 7 {
		t.Errorf("order = %+v, %v", cf.Order, err)
	}
	if _, err := ParseCluster("ordered", []byte(slurm("o", "order: high\n"))); err == nil {
		t.Error("a non-integer order was accepted")
	}
}

func TestClusterFolderErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		site  string
		extra map[string]string
		want  string
	}{
		{"duplicate name", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "name: bigiron\ntype: slurm\nssh_host: ex-login\n"}, "duplicate name"},
		{"duplicate with Site.yaml", hostsOnly + "clusters: [{name: bigiron, type: slurm, ssh_host: ex-login}]\n", nil, "duplicate name"},
		{"unknown host", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "name: other\ntype: slurm\nssh_host: nowhere\n"}, "not one of the hosts in Site.yaml"},
		{"unknown files host", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "name: other\ntype: slurm\nssh_host: ex-login\nfiles_host: nope\n"}, "files_host"},
		{"unknown key", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "name: other\ntype: slurm\nssh_host: ex-login\ncolour: blue\n"}, "colour"},
		{"no type", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "name: other\n"}, "unknown type"},
		{"no name", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "type: slurm\nssh_host: ex-login\n"}, "missing name"},
		{"keys without name and type", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "ssh_host: ex-login\n"}, "missing name"},
		{"short term", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "forbidden_terms: [ab]\n"}, "forbidden_terms"},
		{"bad folder name", hostsOnly, map[string]string{"example.edu/Big_Iron/References/x.md": "x"}, "Big_Iron"},
		{"invalid cluster", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "name: other\ntype: kubernetes\n"}, "unknown type"},
		{"second document", hostsOnly, map[string]string{"example.edu/other/Cluster.yaml": "name: other\ntype: slurm\nssh_host: ex-login\n---\nname: sneaky\ntype: slurm\n"}, "only one YAML document"},
		{"second document in Site.yaml", hostsOnly + "---\nbroker_url: https://evil.example\n", nil, "only one YAML document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(folderFS(tc.site, tc.extra), "example.edu")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if _, err := ParseCluster("tools", []byte("")); err == nil {
		t.Error("ParseCluster accepted the reserved folder name tools")
	}
	if _, err := ParseCluster("References", []byte("")); err == nil {
		t.Error("ParseCluster accepted a reserved folder name")
	}
	if cf, err := ParseCluster("empty", nil); err != nil || cf.Template != nil || cf.Dir != "empty" {
		t.Errorf("empty Cluster.yaml = %+v, %v", cf, err)
	}
	// A single document may start with "---"; a comment-only file declares
	// no template.
	if cf, err := ParseCluster("marked", []byte("---\nname: m\ntype: slurm\nssh_host: ex-login\n")); err != nil || cf.Template == nil || cf.Template.Name != "m" {
		t.Errorf("--- document = %+v, %v", cf, err)
	}
	if cf, err := ParseCluster("comments", []byte("# name: x\n# type: slurm\n")); err != nil || cf.Template != nil {
		t.Errorf("comment-only Cluster.yaml = %+v, %v", cf, err)
	}
	// Any cluster key at all makes a template (and so must validate).
	if _, err := ParseCluster("tz", []byte("timezone: UTC\n")); err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Errorf("a lone timezone key = %v, want a missing-name error", err)
	}
}

// siteFixture is one in-memory profile; def adds `default: true`.
func siteFixture(fsys fstest.MapFS, domain string, def bool) {
	y := "domain: " + domain + "\nname: " + domain + " U\nskill: " + SkillName(domain) + "\n"
	if def {
		y += "default: true\n"
	}
	fsys[domain+"/Site.yaml"] = &fstest.MapFile{Data: []byte(y)}
	fsys[domain+"/SKILL.md"] = &fstest.MapFile{Data: []byte("x")}
}

func TestDefaultSite(t *testing.T) {
	tests := []struct {
		name    string
		sites   map[string]bool // domain -> default
		want    string          // Default().Domain
		first   string          // first listed ("" for none)
		wantErr bool
	}{
		{"one default sorts first", map[string]bool{"a.edu": false, "z.edu": true, "m.edu": false}, "z.edu", "z.edu", false},
		{"two defaults", map[string]bool{"a.edu": true, "z.edu": true}, GenericDomain, "a.edu", true},
		{"none marked", map[string]bool{"a.edu": false, "z.edu": false}, GenericDomain, "a.edu", false},
		{"no sites", map[string]bool{}, GenericDomain, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for d, def := range tc.sites {
				siteFixture(fsys, d, def)
			}
			list, err := listChecked(fsys)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := defaultOf(list).Domain; got != tc.want {
				t.Errorf("default = %q, want %q", got, tc.want)
			}
			if tc.first == "" {
				if len(list) != 0 {
					t.Errorf("list = %d", len(list))
				}
			} else if list[0].Domain != tc.first {
				t.Errorf("first = %q, want %q", list[0].Domain, tc.first)
			}
		})
	}
}

func TestParseDefaultFlag(t *testing.T) {
	s, err := Parse([]byte(minimal + "default: true\n"))
	if err != nil || !s.IsDefault {
		t.Errorf("default: true -> %+v %v", s.IsDefault, err)
	}
	if _, err := Parse([]byte("domain: generic\nname: G\nskill: site-generic\ndefault: true\n")); err == nil {
		t.Error("generic marked default was accepted")
	}
}

func TestParseTwoFactorName(t *testing.T) {
	s, err := Parse([]byte(minimal + "two_factor: {name: Duo}\n"))
	if err != nil || s.TwoFactorName != "Duo" {
		t.Fatalf("name = %q, %v", s.TwoFactorName, err)
	}
	if s, err := Parse([]byte(minimal)); err != nil || s.TwoFactorName != "" {
		t.Fatalf("default = %q, %v", s.TwoFactorName, err)
	}
	for name, y := range map[string]string{
		"too long":   "two_factor: {name: " + strings.Repeat("x", 41) + "}\n",
		"control":    "two_factor: {name: \"Du\\no\"}\n",
		"whitespace": "two_factor: {name: \" Duo\"}\n",
	} {
		if _, err := Parse([]byte(minimal + y)); err == nil || !strings.Contains(err.Error(), "two_factor.name") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestParseBrokerIssuers(t *testing.T) {
	s, err := Parse([]byte(minimal + "broker:\n  issuers: [\"https://idp.example.edu/realms/x/\", https://idp2.example.edu]\n"))
	if err != nil || len(s.BrokerIssuers) != 2 || s.BrokerIssuers[0] != "https://idp.example.edu/realms/x" {
		t.Fatalf("issuers = %v %v", s.BrokerIssuers, err)
	}
	for _, bad := range []string{
		"http://idp.example.edu/x", "/realms/x", "idp.example.edu", "https://", "https://idp.example.edu/x?a=1",
		"https://idp.example.edu/x#f", "[https://a.example.edu, https://a.example.edu/]",
	} {
		y := bad
		if !strings.HasPrefix(bad, "[") {
			y = "[\"" + bad + "\"]"
		}
		if _, err := Parse([]byte(minimal + "broker: {issuers: " + y + "}\n")); err == nil {
			t.Errorf("issuers %s accepted", bad)
		}
	}
}

func issuerFixture(fsys fstest.MapFS, domain, issuer string) {
	siteFixture(fsys, domain, false)
	fsys[domain+"/Site.yaml"].Data = append(fsys[domain+"/Site.yaml"].Data, []byte("broker: {issuers: [\""+issuer+"\"]}\n")...)
}

func TestIssuersAcrossSites(t *testing.T) {
	fsys := fstest.MapFS{}
	issuerFixture(fsys, "a.edu", "https://idp.example.org/realms/a")
	issuerFixture(fsys, "b.edu", "https://idp.example.org/realms/a/")
	if _, err := listChecked(fsys); err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Errorf("duplicate issuer err = %v", err)
	}
	list, _ := listChecked(fsys)
	if _, ok := forIssuer(list, "https://idp.example.org/realms/a"); ok {
		t.Error("ambiguous issuer resolved to a site")
	}
}

// A profile that fails to load is skipped by List but reported by the check.
func TestListCheckedBrokenProfile(t *testing.T) {
	fsys := fstest.MapFS{"broken.edu/Site.yaml": {Data: []byte("not: [valid")}}
	issuerFixture(fsys, "a.edu", "https://idp.example.org/realms/a")
	list, err := listChecked(fsys)
	if err == nil || !strings.Contains(err.Error(), "broken.edu") {
		t.Fatalf("err = %v", err)
	}
	if len(list) != 1 || list[0].Domain != "a.edu" {
		t.Errorf("list = %+v", list)
	}
	if got := listFS(fsys); len(got) != 1 {
		t.Errorf("listFS = %d profiles", len(got))
	}
}

func TestForIssuer(t *testing.T) {
	fsys := fstest.MapFS{}
	issuerFixture(fsys, "a.edu", "https://idp.example.org/realms/a")
	issuerFixture(fsys, "b.edu", "https://idp.example.org/realms/b")
	list, err := listChecked(fsys)
	if err != nil {
		t.Fatal(err)
	}
	for iss, want := range map[string]string{
		"https://idp.example.org/realms/a":   "a.edu",
		"https://idp.example.org/realms/b/":  "b.edu",
		" https://idp.example.org/realms/b ": "b.edu",
		"https://idp.example.org/realms/c":   GenericDomain,
		"":                                   GenericDomain,
	} {
		s, ok := forIssuer(list, iss)
		if s.Domain != want || ok != (want != GenericDomain) {
			t.Errorf("forIssuer(%q) = %q, %v", iss, s.Domain, ok)
		}
	}
}
