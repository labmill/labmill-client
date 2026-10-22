package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
)

// buildEnv points Build at a throwaway home and returns the clusters.yaml
// path.
func buildEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("LABMILL_BROKER_URL", "")
	p, err := config.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A clusters.yaml without `site:`, or with one this build doesn't know,
// leads back to the institution choice with a plain note; only `site:` is
// added, the clusters and comments stay (M55).
func TestBuildConfigWithoutHomeInstitutionAsksAgain(t *testing.T) {
	for _, tc := range []struct{ name, siteLine string }{
		{"no site", ""},
		{"unknown site", "site: nowhere.example\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := buildEnv(t)
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
				t.Fatal(err)
			}
			body := tc.siteLine + "# my clusters\nclusters:\n  - name: mine\n    type: slurm\n    ssh_host: mine-login\n"
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if !NeedsHomeChoice(cfgPath) {
				t.Fatal("NeedsHomeChoice = false")
			}
			// No setup step: a plain error, the file untouched.
			if _, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, nil); err == nil || !strings.Contains(err.Error(), "home institution") {
				t.Fatalf("Build without setup = %v", err)
			}
			if got, _ := os.ReadFile(cfgPath); string(got) != body {
				t.Errorf("clusters.yaml changed:\n%s", got)
			}
			var note string
			setup := func(ctx context.Context, _ site.Site) (SetupAnswers, error) {
				note = SetupNote(ctx)
				return SetupAnswers{Site: site.GenericDomain, Username: "dana", LoginHost: "hpc.example.edu"}, nil
			}
			rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, setup)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			if note != HomeNote(true) {
				t.Errorf("note = %q", note)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil || cfg.Site != site.GenericDomain {
				t.Fatalf("saved config: %+v %v", cfg, err)
			}
			if _, ok := cfg.Cluster("mine"); !ok || len(cfg.Clusters) != 1 {
				t.Errorf("clusters changed: %+v", cfg.Clusters)
			}
			if got, _ := os.ReadFile(cfgPath); !strings.Contains(string(got), "# my clusters") {
				t.Errorf("comment lost:\n%s", got)
			}
			if NeedsHomeChoice(cfgPath) {
				t.Error("still needs a home institution")
			}
		})
	}
}

// There is no setup without a home institution: an empty choice, an unknown
// domain and the generic profile with no hosts are all refused, and nothing
// is written.
func TestBuildFirstRunRefusesNoHomeInstitution(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    SetupAnswers
		want string
	}{
		{"empty choice", SetupAnswers{Username: "dana"}, "home institution"},
		{"unknown domain", SetupAnswers{Site: "nowhere.example", Username: "dana"}, "home institution"},
		{"generic without hosts", SetupAnswers{Site: site.GenericDomain, Username: "dana"}, "login host of your institution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := buildEnv(t)
			setup := func(context.Context, site.Site) (SetupAnswers, error) { return tc.a, nil }
			_, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, setup)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build = %v, want %q", err, tc.want)
			}
			if _, serr := os.Stat(cfgPath); !os.IsNotExist(serr) {
				t.Errorf("clusters.yaml written: %v", serr)
			}
			if !NeedsHomeChoice(cfgPath) {
				t.Error("NeedsHomeChoice = false after a refused setup")
			}
		})
	}
}

// Adding a cluster at another institution (M45) keeps the home `site:` and
// every home cluster.
func TestAddSiteClusterKeepsHomeInstitution(t *testing.T) {
	useFixtureSite(t)
	far := farSite(t)
	p, cfgPath := homeSetup(t)
	before, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"farone", "farai"} {
		plan, err := planAddCluster(p, cfgPath, []site.Site{far}, farDomain, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := addSiteCluster(p, cfgPath, []site.Site{far}, plan, "bob", "linux", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Site != before.Site || after.Site != fixtureDomain {
		t.Errorf("home site = %q, was %q", after.Site, before.Site)
	}
	for _, c := range before.Clusters {
		got, ok := after.Cluster(c.Name)
		if !ok || got.Site != c.Site || got.SSHHost != c.SSHHost {
			t.Errorf("home cluster %s = %+v %v", c.Name, got, ok)
		}
	}
	if len(after.Clusters) != len(before.Clusters)+2 {
		t.Errorf("clusters = %d, want %d", len(after.Clusters), len(before.Clusters)+2)
	}
	// Adding the home institution's own clusters again is refused too.
	plan, _ := planAddCluster(p, cfgPath, []site.Site{far}, farDomain, "farone")
	plan.Site.Domain = fixtureDomain
	if _, err := addSiteCluster(p, cfgPath, []site.Site{far}, plan, "bob", "linux", time.Now()); err == nil {
		t.Error("a plan naming the home domain was applied from the others list")
	}
	if NeedsHomeChoice(cfgPath) {
		t.Error("home institution lost")
	}
}

// `site: generic` with no clusters names no institution at all, so it goes
// back to the form too; with clusters it is a home institution the user
// entered. Only a file that has clusters and lacks a site is asked for the
// institution alone.
func TestGenericWithoutClustersNeedsHomeChoice(t *testing.T) {
	cfgPath := buildEnv(t)
	save := func(c *config.Config) {
		t.Helper()
		if err := config.Save(cfgPath, c); err != nil {
			t.Fatal(err)
		}
	}
	mine := config.Cluster{Name: "mine", Type: config.TypeSlurm, SSHHost: "mine-login"}
	save(&config.Config{Site: site.GenericDomain})
	if !NeedsHomeChoice(cfgPath) || OnlyInstitution(cfgPath) {
		t.Errorf("generic, no clusters: needs %v, only %v", NeedsHomeChoice(cfgPath), OnlyInstitution(cfgPath))
	}
	save(&config.Config{Site: site.GenericDomain, Clusters: []config.Cluster{mine}})
	if NeedsHomeChoice(cfgPath) {
		t.Error("generic with clusters asks again")
	}
	save(&config.Config{Clusters: []config.Cluster{mine}})
	if !NeedsHomeChoice(cfgPath) || !OnlyInstitution(cfgPath) {
		t.Error("clusters without a site: want only the institution asked")
	}
	// Build asks again for the empty generic file and writes a full setup.
	save(&config.Config{Site: site.GenericDomain})
	var only bool
	setup := func(ctx context.Context, _ site.Site) (SetupAnswers, error) {
		only = SetupOnlyInstitution(ctx)
		return SetupAnswers{Site: site.GenericDomain, Username: "dana", LoginHost: "hpc.example.edu"}, nil
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, setup)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	cfg, _ := config.Load(cfgPath)
	if only || len(cfg.Clusters) == 0 {
		t.Errorf("only = %v, clusters %+v", only, cfg.Clusters)
	}
}
