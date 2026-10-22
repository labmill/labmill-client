package launch

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/skill"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
	"labmill-client/internal/sshx/sshtest"
)

const farDomain = "far.example.org"

// farSite is a second fixture institution: a key-only login host behind a
// jump host, its own 2FA text and key-upload page, a Slurm cluster and a
// Slurm cluster with a preemptible partition. Nothing here depends on private profiles.
func farSite(t *testing.T) site.Site {
	t.Helper()
	s, err := site.Load(fstest.MapFS{
		"far.example.org/Site.yaml": {Data: []byte(`domain: far.example.org
name: Far University
skill: site-far-example-org
two_factor: {help: "Enroll at https://2fa.far.example.org", name: "FarAuth"}
key_upload: {url: "https://keys.far.example.org", instruction: "Paste the key there."}
hosts:
  - {alias: far-gw, hostname: gw.far.example.org, role: jump}
  - {alias: far-login, hostname: login.far.example.org, role: login, proxy_jump: far-gw, auth: key}
  - {alias: far-files, hostname: files.far.example.org, role: transfer}
`)},
		"far.example.org/SKILL.md":                     {Data: []byte("---\nname: site-far-example-org\ndescription: Far University specifics.\n---\nbody\n")},
		"far.example.org/farone/Cluster.yaml":          {Data: []byte("name: farone\ntype: slurm\nssh_host: far-login\nfiles_host: far-files\ntransfer: sftp\n")},
		"far.example.org/farai/Cluster.yaml":           {Data: []byte("name: farai\ntype: slurm\nssh_host: far-login\npreemptible: {partition: farpre}\n")},
		"far.example.org/farone/References/Cluster.md": {Data: []byte("# Far facts\n")},
	}, farDomain)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// homeSetup writes the home fixture's hosts and clusters.yaml like first run.
func homeSetup(t *testing.T) (sshconfig.Paths, string) {
	t.Helper()
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	cfgPath := filepath.Join(t.TempDir(), "labmill", "clusters.yaml")
	if _, err := WriteSetup(p, cfgPath, fixtureSite(t), SetupAnswers{Username: "alice"}, "linux"); err != nil {
		t.Fatal(err)
	}
	return p, cfgPath
}

func TestPlanAddCluster(t *testing.T) {
	far := farSite(t)
	p, cfgPath := homeSetup(t)
	plan, err := planAddCluster(p, cfgPath, []site.Site{far}, farDomain, "farone")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Site.Name != "Far University" || plan.Cluster.Name != "farone" || len(plan.Files) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	// Jump host first, then the login host (key only), then the transfer host.
	var got []string
	for _, h := range plan.Hosts {
		got = append(got, h.HostName)
		if h.KeyOnly != (h.HostName == "login.far.example.org") {
			t.Errorf("%s KeyOnly = %v", h.HostName, h.KeyOnly)
		}
	}
	if strings.Join(got, " ") != "gw.far.example.org login.far.example.org files.far.example.org" {
		t.Errorf("hosts = %v", got)
	}
	ai, err := planAddCluster(p, cfgPath, []site.Site{far}, farDomain, "farai")
	if err != nil || ai.Hints.Slurm.Preemptible == nil {
		t.Errorf("farai plan = %+v, %v", ai, err)
	}
	if _, err := planAddCluster(p, cfgPath, []site.Site{far}, farDomain, "nope"); err == nil {
		t.Error("unknown cluster planned")
	}
	if _, err := planAddCluster(p, cfgPath, []site.Site{far}, "other.example.org", "farone"); err == nil {
		t.Error("unknown site planned")
	}
}

func TestAddSiteCluster(t *testing.T) {
	far := farSite(t)
	p, cfgPath := homeSetup(t)
	before, _ := os.ReadFile(p.LabmillConfig())
	plan, _ := planAddCluster(p, cfgPath, []site.Site{far}, farDomain, "farone")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	c, err := addSiteCluster(p, cfgPath, []site.Site{far}, plan, "bob", "linux", now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != farDomain || c.SSHHost != "far-login" || c.FilesHost != "far-files" {
		t.Errorf("cluster = %+v", c)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site != fixtureDomain {
		t.Errorf("top-level site changed: %q", cfg.Site)
	}
	if got, ok := cfg.Cluster("farone"); !ok || got.Site != farDomain {
		t.Errorf("saved cluster = %+v %v", got, ok)
	}
	if home, ok := cfg.Cluster("bigiron"); !ok || home.Site != "" {
		t.Errorf("home cluster = %+v %v", home, ok)
	}
	// The home hosts stay, the new ones are added with the new account.
	after, _ := os.ReadFile(p.LabmillConfig())
	if !strings.HasPrefix(string(after), strings.TrimRight(string(before), "\n")) {
		t.Errorf("existing ssh entries changed:\n%s", after)
	}
	h, found, err := sshconfig.Lookup(p, "far-login")
	if err != nil || !found || h.User != "bob" || h.HostName != "login.far.example.org" || h.ProxyJump != "far-gw" {
		t.Errorf("far-login = %+v %v %v", h, found, err)
	}
	if h, _, _ := sshconfig.Lookup(p, "ex-login"); h.User != "alice" {
		t.Errorf("home host user = %q", h.User)
	}
	// A second add of the same name is refused and changes nothing.
	if _, err := addSiteCluster(p, cfgPath, []site.Site{far}, plan, "bob", "linux", now); err == nil {
		t.Error("second add succeeded")
	}
	if _, err := addSiteCluster(p, cfgPath, []site.Site{far}, plan, "Bad Name", "linux", now); err == nil {
		t.Error("invalid user accepted")
	}
}

// A host the user already reaches under their own alias is referenced, not
// rewritten into Labmill's file.
func TestAddSiteClusterUserAlias(t *testing.T) {
	far := farSite(t)
	p, cfgPath := homeSetup(t)
	existing, _ := os.ReadFile(p.UserConfig())
	mine := string(existing) + "\nHost mygw\n    HostName gw.far.example.org\n    User bob\n"
	if err := os.WriteFile(p.UserConfig(), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, _ := planAddCluster(p, cfgPath, []site.Site{far}, farDomain, "farone")
	if _, err := addSiteCluster(p, cfgPath, []site.Site{far}, plan, "bob", "linux", time.Now()); err != nil {
		t.Fatal(err)
	}
	h, _, _ := sshconfig.Lookup(p, "far-login")
	if h.ProxyJump != "mygw" {
		t.Errorf("jump = %q, want the user's own alias", h.ProxyJump)
	}
	yb, _ := os.ReadFile(p.LabmillConfig())
	if strings.Contains(string(yb), "Host mygw") {
		t.Errorf("the user's alias was rewritten:\n%s", yb)
	}
}

func TestLoginOptionsPerHostSite(t *testing.T) {
	home, far := fixtureSite(t), farSite(t)
	cfg := &config.Config{Site: fixtureDomain, Clusters: []config.Cluster{
		{Name: "bigiron", Type: config.TypeSlurm, SSHHost: "ex-login"},
		{Name: "farone", Type: config.TypeSlurm, SSHHost: "far-login", Site: farDomain},
		{Name: "gone", Type: config.TypeSlurm, SSHHost: "x", Site: "unknown.example.net"},
	}}
	sites := sitesOfConfig(home, cfg, []site.Site{far})
	if len(sites) != 2 || sites[0].Domain != fixtureDomain || sites[1].Domain != farDomain {
		t.Fatalf("sites = %v", sites)
	}
	o := loginOptionsFor(sites)
	if o.TwoFactorHelp != home.TwoFactorHelp || o.TwoFactorName != home.TwoFactorName {
		t.Errorf("defaults are not the home institution's: %+v", o)
	}
	if len(o.KeyOnlyHosts) != 1 || o.KeyOnlyHosts[0] != "login.far.example.org" {
		t.Errorf("KeyOnlyHosts = %v", o.KeyOnlyHosts)
	}
	hl, ok := o.Hosts["files.far.example.org"]
	if !ok || hl.TwoFactorName != "FarAuth" || !strings.Contains(hl.TwoFactorHelp, "2fa.far.example.org") {
		t.Errorf("per-host options = %+v", o.Hosts)
	}
	if _, ok := o.Hosts["login.hpc.example.edu"]; ok {
		t.Error("a home host got a per-host override")
	}
	// Only the home institution: the old behavior.
	if o := loginOptionsFor(sitesOfConfig(home, nil, nil)); o.Hosts != nil || len(o.KeyOnlyHosts) != 0 {
		t.Errorf("home only = %+v", o)
	}
}

func TestKeySiteMergedPerHostUpload(t *testing.T) {
	home, far := fixtureSite(t), farSite(t)
	k := keySiteOf(home).merged(far)
	if u := k.uploadFor("LOGIN.far.example.org"); u.URL != "https://keys.far.example.org" {
		t.Errorf("far upload = %+v", u)
	}
	if u := k.uploadFor("login.hpc.example.edu"); u.URL != home.KeyUpload.URL {
		t.Errorf("home upload = %+v", u)
	}
	cc := newConnCache(&fakeUI{})
	cc.addKeySite(far)
	if got := cc.keySnapshot().hostNames; len(got) != 1 || got[0] != "login.far.example.org" {
		t.Errorf("hostNames = %v", got)
	}
}

func TestSiteInfoOtherInstitutions(t *testing.T) {
	home, far := fixtureSite(t), farSite(t)
	info := siteInfo(home, []config.Cluster{
		{Name: "farai", Type: config.TypeSlurm, Site: farDomain},
		{Name: "farone", Type: config.TypeSlurm, Site: farDomain},
		{Name: "second", Type: config.TypeSlurm},
	}, far)
	if info.Domain != fixtureDomain || len(info.Others) != 1 {
		t.Fatalf("info = %+v", info)
	}
	o := info.Others[0]
	if o.Name != "Far University" || o.Domain != farDomain || o.Skill != "site-far-example-org" || strings.Join(o.Clusters, ",") != "farone,farai" && strings.Join(o.Clusters, ",") != "farai,farone" {
		t.Errorf("other = %+v", o)
	}
	if info.SlurmFor("farai").Preemptible == nil {
		t.Errorf("a far cluster must use the far profile's hints: %+v", info.Clusters)
	}
	set, _, lerr := skill.LoadWithOverride(genericWith("g"), filepath.Join(t.TempDir(), "none"))
	if lerr != nil {
		t.Fatal(lerr)
	}
	prompt := app.SystemPrompt(&app.Env{Site: info, Skills: set})
	if !strings.Contains(prompt, "Far University (far.example.org)") {
		t.Errorf("prompt lacks the other institution")
	}
}

func TestSessionSkillsHaveOtherSiteAndBundleOverlay(t *testing.T) {
	home, far := fixtureSite(t), farSite(t)
	all, err := site.SkillsFSAll(genericWith("g"), home, []site.Site{far})
	if err != nil {
		t.Fatal(err)
	}
	set, _, err := skill.LoadWithOverride(all, filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"alpha", "site-example-edu", "site-far-example-org"} {
		if _, ok := set.Get(n); !ok {
			t.Errorf("skill %s missing", n)
		}
	}
	// A cached bundle has the home skill only: the other skill is overlaid
	// from the embedded copy, and the bundle wins for names it has.
	emb, _ := buildBundle(t, "old body")
	newer, raw := buildBundle(t, "new body")
	f := &fakeKnowledge{manifest: newer.Manifest(), raw: raw}
	dir := filepath.Join(t.TempDir(), "knowledge")
	if ok, err := syncKnowledge(context.Background(), f.serve(t), home, emb.Version, dir); err != nil || !ok {
		t.Fatalf("sync = %v %v", ok, err)
	}
	base, logs := chooseSkillBase(dir, home, all, emb.Version, far)
	if !base.Cached || base.Version != newer.Version || len(logs) != 0 {
		t.Fatalf("base = %+v %v", base, logs)
	}
	set, _, err = skill.LoadWithOverride(base.FS, filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := set.Get("alpha")
	if txt, _ := set.LoadSkillResult(a.Name); !ok || !strings.Contains(txt, "new body") {
		t.Errorf("bundle skill not used: %q", txt)
	}
	fs, ok := set.Get("site-far-example-org")
	if !ok {
		t.Fatal("other site skill missing from the cached session")
	}
	if len(fs.References) == 0 {
		t.Error("other site skill lost its references")
	}
}

// TestKeyGuideHomeLookup: a key-only host of another institution gets the
// home-cluster key look-up and its own institution's name; the home
// institution's own hosts do not.
func TestKeyGuideHomeLookup(t *testing.T) {
	home, far := fixtureSite(t), farSite(t)
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{
		{Alias: "far-login", HostName: "login.far.example.org", User: "alice"},
		{Alias: "home-key", HostName: "keyonly.hpc.example.edu", User: "alice"},
	}, "linux"); err != nil {
		t.Fatal(err)
	}
	cc := newConnCache(&fakeUI{})
	cc.sshPaths = func() (sshconfig.Paths, error) { return p, nil }
	cc.keys = keySiteOf(home).merged(far)
	cc.keys.hostNames = append(cc.keys.hostNames, "keyonly.hpc.example.edu")
	g, ok := cc.keyGuideFor("far-login")
	if !ok {
		t.Fatal("no guide for the far host")
	}
	if g.SiteName != far.Name {
		t.Errorf("SiteName = %q, want %q", g.SiteName, far.Name)
	}
	if home.LoginHost() == "" || g.Home == nil || g.Home.Host != home.LoginHost() || g.Home.Alias == "" {
		t.Errorf("Home = %+v, want the home login host %q", g.Home, home.LoginHost())
	}
	if g, ok := cc.keyGuideFor("home-key"); !ok || g.Home != nil {
		t.Errorf("home institution's own key-only host: ok=%v home=%+v", ok, g)
	}
}

// TestHomeClientLockedNeverLogsIn: without a cached or helper-held login the
// look-up gets no client and starts no login.
func TestHomeClientLockedNeverLogsIn(t *testing.T) {
	oldCached, oldConnect := connectCached, sshConnect
	t.Cleanup(func() { connectCached, sshConnect = oldCached, oldConnect })
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		t.Error("the key look-up started a login")
		return nil, errors.New("no")
	}
	connectCached = func(context.Context, string) (*ssh.Client, error) { return nil, sshx.ErrNotConnected }
	cc := newConnCache(&fakeUI{})
	if cl, rel := cc.homeClientLocked(context.Background(), sshconfig.Paths{SSHDir: t.TempDir()}, "home", "login.hpc.example.edu"); cl != nil || rel != nil {
		t.Errorf("got a client without a login: %v", cl)
	}
}

type pwPrompter struct{}

func (pwPrompter) Ask(context.Context, sshx.Question) (string, error) { return "pw", nil }
func (pwPrompter) Notify(string)                                      {}

// TestHomeClientLockedCachedAndOwnAlias: a live cached client is returned,
// also under the user's own alias for the home login host; the release
// function leaves it open.
func TestHomeClientLockedCachedAndOwnAlias(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	host, port, _ := net.SplitHostPort(srv.Addr)
	pn, _ := strconv.Atoi(port)
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "mine", HostName: host, User: "alice", Port: pn}, {Alias: "other", HostName: "elsewhere.example.edu"}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl, err := sshx.Dial(context.Background(), p, "mine", pwPrompter{}, sshx.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	oldCached := connectCached
	t.Cleanup(func() { connectCached = oldCached })
	connectCached = func(context.Context, string) (*ssh.Client, error) { return nil, sshx.ErrNotConnected }
	cc := newConnCache(&fakeUI{})
	cc.clients["mine"] = &cachedClient{Client: cl, dead: make(chan struct{})}
	cc.homeSSHHosts = []string{"other", "mine"}
	got, rel := cc.homeClientLocked(context.Background(), p, "site-login", host)
	if got != cl || rel == nil {
		t.Fatalf("got %v, want the cached client under the user's alias", got)
	}
	rel()
	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()
	select {
	case <-closed:
		t.Error("release closed the cached client")
	case <-time.After(200 * time.Millisecond):
	}
	// An alias for another host is not used.
	cc.homeSSHHosts = []string{"other"}
	if got, _ := cc.homeClientLocked(context.Background(), p, "site-login", host); got != nil {
		t.Error("used an alias of another host")
	}
}
