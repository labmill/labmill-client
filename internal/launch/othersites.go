package launch

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
)

// Clusters at another institution (M45.1). A session's home institution is
// the top-level `site:` of clusters.yaml; the other embedded profiles are
// known (their skills are loaded, the prompt lists them) but nothing is
// configured for them until the user explicitly asks for one of their
// clusters. Then add_cluster (app.Env.PlanAddCluster / AddCluster, below)
// writes the cluster with `site: <domain>` and the SSH settings for its
// hosts. The model calls stay the home institution's.

// othersForSite is site.Others; tests replace it with fixture profiles.
var othersForSite = site.Others

// otherSiteInfos is what the prompt and add_cluster know about others.
func otherSiteInfos(others []site.Site) []app.OtherSite {
	var out []app.OtherSite
	for _, o := range others {
		out = append(out, app.OtherSite{Name: o.Name, Domain: o.Domain, Skill: o.Skill, Clusters: o.ClusterNames()})
	}
	return out
}

// templateFor is the cluster template named name in s.
func templateFor(s site.Site, name string) (config.Cluster, bool) {
	for _, c := range s.Clusters {
		if c.Name == name {
			return c, true
		}
	}
	return config.Cluster{}, false
}

// hostsFor returns the host templates of s that cluster c needs (its
// ssh_host and files_host and the jump hosts they go through), jump hosts
// first so a ProxyJump can be rewritten to an alias that exists already.
func hostsFor(s site.Site, c config.Cluster) []sshconfig.Host {
	byAlias := map[string]sshconfig.Host{}
	for _, h := range s.Hosts {
		byAlias[h.Alias] = h
	}
	var out []sshconfig.Host
	seen := map[string]bool{}
	var add func(alias string)
	add = func(alias string) {
		h, ok := byAlias[alias]
		if !ok || seen[alias] {
			return
		}
		seen[alias] = true
		if h.ProxyJump != "" {
			add(h.ProxyJump)
		}
		out = append(out, h)
	}
	add(c.SSHHost)
	add(c.FilesHost)
	return out
}

// planAddCluster builds the add_cluster plan for the cluster named cluster of
// the institution domain among others.
func planAddCluster(p sshconfig.Paths, cfgPath string, others []site.Site, domain, cluster string) (app.AddClusterPlan, error) {
	s, ok := findSite(others, domain)
	if !ok {
		return app.AddClusterPlan{}, fmt.Errorf("this Labmill has no profile for %q", domain)
	}
	tmpl, ok := templateFor(s, cluster)
	if !ok {
		return app.AddClusterPlan{}, fmt.Errorf("%s has no cluster %q", s.Name, cluster)
	}
	plan := app.AddClusterPlan{
		Site:    app.OtherSite{Name: s.Name, Domain: s.Domain, Skill: s.Skill, Clusters: s.ClusterNames()},
		Cluster: tmpl,
		Files:   []string{cfgPath, p.LabmillConfig()},
	}
	for _, h := range hostsFor(s, tmpl) {
		host := app.AddClusterHost{Alias: h.Alias, HostName: h.HostName, Role: s.Roles[h.Alias], KeyOnly: s.KeyOnly(h.Alias)}
		if alias, _, found, err := sshconfig.AliasSource(p, h.HostName); err == nil && found {
			// An existing entry is reused as it is, its own User included.
			host.ReusedAlias = alias
			if e, _, err := sshconfig.Lookup(p, alias); err == nil {
				host.ReusedUser = e.User
			}
		}
		plan.Hosts = append(plan.Hosts, host)
	}
	if tmpl.Type == config.TypeSlurm {
		plan.Hints = app.ClusterHints{Slurm: s.SlurmHintsFor(tmpl.Name)}
	}
	return plan, nil
}

func findSite(list []site.Site, domain string) (site.Site, bool) {
	for _, s := range list {
		if s.Domain == domain {
			return s, true
		}
	}
	return site.Site{}, false
}

// addSiteCluster applies an approved plan: it adds the SSH hosts of the
// cluster to Labmill's ssh file (an alias that already points at the same host
// is reused; a user's own entry is referenced, never rewritten; other
// entries of the file stay), then appends the cluster, with `site:` set and
// the final aliases, to clusters.yaml (backed up first). It refuses a name
// that is configured already. It does not touch the running session.
func addSiteCluster(p sshconfig.Paths, cfgPath string, others []site.Site, plan app.AddClusterPlan, username, goos string, now time.Time) (config.Cluster, error) {
	if !ValidUsername(username) {
		return config.Cluster{}, fmt.Errorf("account name %q isn't valid", username)
	}
	s, ok := findSite(others, plan.Site.Domain)
	if !ok {
		return config.Cluster{}, fmt.Errorf("this Labmill has no profile for %q", plan.Site.Domain)
	}
	tmpl, ok := templateFor(s, plan.Cluster.Name)
	if !ok {
		return config.Cluster{}, fmt.Errorf("%s has no cluster %q", s.Name, plan.Cluster.Name)
	}
	data, cfg, err := readClusters(cfgPath)
	if err != nil {
		return config.Cluster{}, err
	}
	if _, exists := cfg.Cluster(tmpl.Name); exists {
		return config.Cluster{}, fmt.Errorf("a cluster named %s is already configured", tmpl.Name)
	}
	// A reused entry keeps its own User, which wins at sign-in: refuse a
	// different account before anything is written, rather than ignore it.
	for _, h := range hostsFor(s, tmpl) {
		alias, _, found, err := sshconfig.AliasSource(p, h.HostName)
		if err != nil || !found {
			continue
		}
		if e, _, err := sshconfig.Lookup(p, alias); err == nil && e.User != "" && e.User != username {
			return config.Cluster{}, fmt.Errorf("your SSH entry %q for %s has the account %s, not %s: use that account, or change the entry first", alias, h.HostName, e.User, username)
		}
	}
	rename := map[string]string{} // template alias -> final alias
	wrote := false
	for _, h := range hostsFor(s, tmpl) {
		alias, _, found, err := sshconfig.AliasSource(p, h.HostName)
		if err != nil {
			return config.Cluster{}, fmt.Errorf("read ssh settings: %w", err)
		}
		if found {
			rename[h.Alias] = alias
			continue
		}
		if alias, err = sshconfig.UniqueAlias(p, h.Alias); err != nil {
			return config.Cluster{}, fmt.Errorf("choose a name for %s: %w", h.HostName, err)
		}
		rename[h.Alias] = alias
		h.Alias, h.User = alias, username
		if v, ok := rename[h.ProxyJump]; ok {
			h.ProxyJump = v
		}
		if err := h.Validate(); err != nil {
			return config.Cluster{}, err
		}
		if err := appendHost(p, h, goos); err != nil {
			return config.Cluster{}, fmt.Errorf("write ssh settings: %w", err)
		}
		wrote = true
	}
	if wrote {
		if _, err := sshconfig.EnsureInclude(p, now); err != nil {
			return config.Cluster{}, fmt.Errorf("update ssh settings: %w", err)
		}
	}
	c := tmpl
	if v, ok := rename[c.SSHHost]; ok {
		c.SSHHost = v
	}
	if v, ok := rename[c.FilesHost]; ok {
		c.FilesHost = v
	}
	if s.Domain != configHome(cfg) {
		// A cluster of the user's own institution (M57.2) carries no
		// `site:`: the top-level site is its institution.
		c.Site = s.Domain
	}
	out, err := config.PutClusterYAML(data, "", c)
	if err != nil {
		return config.Cluster{}, fmt.Errorf("update clusters.yaml: %w", err)
	}
	if _, err := config.Backup(cfgPath, now); err != nil {
		return config.Cluster{}, err
	}
	if err := config.WriteFile(cfgPath, out); err != nil {
		return config.Cluster{}, err
	}
	return c, nil
}

// readClusters reads clusters.yaml (following a symlink) and parses it.
func readClusters(cfgPath string) ([]byte, *config.Config, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", cfgPath, err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return data, cfg, nil
}

// homeClusters returns the clusters that belong to the home institution
// (no `site:`, or the home domain).
func homeClusters(clusters []config.Cluster, homeDomain string) []config.Cluster {
	var out []config.Cluster
	for _, c := range clusters {
		if c.Site == "" || c.Site == homeDomain {
			out = append(out, c)
		}
	}
	return out
}

// sitesOfConfig returns the home profile and the profiles of the other
// institutions that cfg's clusters name (c.Site), each once, the unknown
// ones left out (a cluster of an institution this build doesn't know keeps
// working with the home settings). others is the list of known profiles.
func sitesOfConfig(home site.Site, cfg *config.Config, others []site.Site) []site.Site {
	out := []site.Site{home}
	seen := map[string]bool{home.Domain: true}
	if cfg == nil {
		return out
	}
	for _, c := range cfg.Clusters {
		if c.Site == "" || seen[c.Site] {
			continue
		}
		seen[c.Site] = true
		if s, ok := findSite(others, c.Site); ok {
			out = append(out, s)
		}
	}
	return out
}

// loginOptionsFor builds the SSH login options for the institutions in
// sites (home first): the home institution's two-factor text as the default,
// every institution's key-only hosts, and each host's own two-factor text,
// so a host of another institution shows its institution's help.
func loginOptionsFor(sites []site.Site) sshx.LoginOptions {
	if len(sites) == 0 {
		return sshx.LoginOptions{}
	}
	home := sites[0]
	opts := sshx.LoginOptions{TwoFactorHelp: home.TwoFactorHelp, TwoFactorName: home.TwoFactorName}
	for _, s := range sites {
		opts.KeyOnlyHosts = append(opts.KeyOnlyHosts, s.KeyOnlyHostNames()...)
		if s.Domain == home.Domain {
			continue
		}
		for _, h := range s.Hosts {
			if h.HostName == "" {
				continue
			}
			if opts.Hosts == nil {
				opts.Hosts = map[string]sshx.HostLogin{}
			}
			opts.Hosts[strings.ToLower(h.HostName)] = sshx.HostLogin{TwoFactorHelp: s.TwoFactorHelp, TwoFactorName: s.TwoFactorName}
		}
	}
	return opts
}

// errNoSSHPaths is returned when the ssh settings location is unknown.
var errNoSSHPaths = errors.New("can't find your ssh settings folder")

// siblingHosts maps every ssh alias of the clusters to the other aliases of
// the same institution (a cluster without a site belongs to the home one):
// they share the user's account, so a rejected password on one is a warning
// for all (connCache.markDenied).
func siblingHosts(clusters []config.Cluster, homeDomain string) map[string][]string {
	group := map[string][]string{}
	for _, c := range clusters {
		site := c.Site
		if site == "" {
			site = homeDomain
		}
		for _, h := range []string{c.SSHHost, c.FilesHost} {
			if h != "" && !slices.Contains(group[site], h) {
				group[site] = append(group[site], h)
			}
		}
	}
	out := map[string][]string{}
	for _, hosts := range group {
		for _, h := range hosts {
			for _, o := range hosts {
				if o != h {
					out[h] = append(out[h], o)
				}
			}
		}
	}
	return out
}
