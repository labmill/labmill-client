package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
)

// Clusters of the home institution that this install doesn't have (M57.2).
// An install set up before the home profile gained a cluster keeps its old
// clusters.yaml. add_cluster (and the settings page's "More clusters at your
// institution") adds such a cluster from the profile's Cluster.yaml template
// through the same code as the clusters of other institutions
// (planAddCluster, addSiteCluster): the new cluster is appended to
// clusters.yaml (Cluster.yaml's `order` only orders the first-run list, so a
// later addition simply goes last), without a `site:` key. Which clusters
// are "missing" is site.Site.MissingClusters, shared with the settings page.

// configHome is the home institution's domain of cfg: its top-level site,
// or the default profile's when clusters.yaml names none.
func configHome(cfg *config.Config) string {
	if cfg != nil && cfg.Site != "" {
		return cfg.Site
	}
	return site.Default().Domain
}

// sshHostNameFunc returns the HostName of an ssh alias in the user's SSH
// settings ("" when unknown); tests replace it.
var sshHostNameFunc = func() func(alias string) string {
	p, err := sshconfig.DefaultPaths()
	if err != nil {
		return nil
	}
	return hostNameIn(p)
}

func hostNameIn(p sshconfig.Paths) func(alias string) string {
	return func(alias string) string {
		if h, found, err := sshconfig.Lookup(p, alias); err == nil && found {
			return h.HostName
		}
		return ""
	}
}

// homeMissing returns the clusters of the home profile s that clusters does
// not have (site.Site.MissingClusters: by name, or the same url or host
// under another name), in profile order.
func homeMissing(s site.Site, clusters []config.Cluster) []app.HomeCluster {
	if s.Domain == site.GenericDomain {
		return nil
	}
	var out []app.HomeCluster
	for _, t := range s.MissingClusters(clusters, sshHostNameFunc()) {
		out = append(out, app.HomeCluster{Name: t.Name, Type: string(t.Type)})
	}
	return out
}

// HomeClusterPlan is what adding one home-institution cluster from the
// settings page writes, shown before the user clicks Add.
type HomeClusterPlan struct {
	Cluster  string   `json:"cluster"`  // the cluster's name
	Type     string   `json:"type"`     // "slurm"
	Username string   `json:"username"` // the account name that will be used; "" when the page must ask
	Hosts    []string `json:"hosts"`    // one plain line per SSH host: new, or an existing entry reused (with its account)
	Files    []string `json:"files"`    // the local files that change
}

// PlanHomeCluster describes adding the home-institution cluster name that
// this install doesn't have. It reads the local files only.
func (r *Runtime) PlanHomeCluster(name string) (HomeClusterPlan, error) {
	plan, err := r.homePlan(name)
	if err != nil {
		return HomeClusterPlan{}, err
	}
	out := HomeClusterPlan{Cluster: plan.Cluster.Name, Type: string(plan.Cluster.Type), Files: plan.Files}
	if r.Env.KnownUsername != nil {
		out.Username, _ = r.Env.KnownUsername(r.Site.Domain)
	}
	for _, h := range plan.Hosts {
		auth := "password and two-factor sign-in"
		if h.KeyOnly {
			auth = "SSH key only"
		}
		line := fmt.Sprintf("new SSH connection to %s (%s): %s", h.HostName, h.Role, auth)
		if h.ReusedAlias != "" {
			who := "no account named in it"
			if h.ReusedUser != "" {
				who = "account " + h.ReusedUser
			}
			line = fmt.Sprintf("your existing SSH entry %s for %s (%s) is reused as it is (%s; %s)", h.ReusedAlias, h.HostName, h.Role, who, auth)
		}
		out.Hosts = append(out.Hosts, line)
	}
	return out, nil
}

// homePlan builds the plan for the home cluster name. It reads no session
// state that a running turn changes (only the locked Config and the files):
// the settings page calls it from its own thread. The profile template is
// validated by PlanAddCluster.
func (r *Runtime) homePlan(name string) (app.AddClusterPlan, error) {
	name = strings.TrimSpace(name)
	if r.Env.PlanAddCluster == nil || r.Env.AddCluster == nil {
		return app.AddClusterPlan{}, errors.New("adding a cluster isn't available")
	}
	if r.Env.Config != nil {
		if _, ok := r.Env.Config.Cluster(name); ok {
			return app.AddClusterPlan{}, fmt.Errorf("the cluster %s is set up already", name)
		}
	}
	return r.Env.PlanAddCluster(r.Site.Domain, name)
}

// CheckHomeCluster checks, before anything runs, that the home cluster name
// can be added for the account username: a valid account name, and an
// existing SSH entry for one of its hosts must have the same account. The
// settings page calls it before starting the add, so its form stays open
// with the message.
func (r *Runtime) CheckHomeCluster(name, username string) error {
	username = strings.TrimSpace(username)
	if !ValidUsername(username) {
		return errors.New("enter your account name there (lowercase letters, digits, '.', '_' and '-')")
	}
	plan, err := r.homePlan(name)
	if err != nil {
		return err
	}
	return plan.AccountConflict(username)
}

// AddHomeCluster adds the home-institution cluster name for the account
// username (the same step as add_cluster, approved by the user's click) and
// makes it visible in the running session. Call it as a turn of its own, so
// no tool reads the configuration meanwhile. It returns a one-line result.
func (r *Runtime) AddHomeCluster(ctx context.Context, name, username string) (string, error) {
	username = strings.TrimSpace(username)
	if err := r.CheckHomeCluster(name, username); err != nil {
		return "", err
	}
	plan, err := r.homePlan(name)
	if err != nil {
		return "", err
	}
	added, err := r.Env.AddCluster(ctx, plan, username)
	if err != nil {
		return "", fmt.Errorf("couldn't add the cluster %s: %w", name, err)
	}
	r.Env.RegisterAddedCluster(plan, added)
	return fmt.Sprintf("Added the cluster %s (account %s).", added.Name, username), nil
}
