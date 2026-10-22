package launch

import (
	"context"
	"fmt"
	"strings"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
)

// One account usually serves every cluster of an institution (M59.1), so a
// cluster added there starts with the name already in use: knownUsername
// finds it, set_cluster_account changes it when the sign-in rejects it.

// knownUsername is the account name already used at the institution domain:
// the User of the login host of the first configured cluster there (for the
// home institution, the clusters without `site:` too), in clusters.yaml
// order, and that cluster's name. "" when there is none. The host may be
// defined in Labmill's or in the user's own ssh settings.
func knownUsername(p sshconfig.Paths, clusters []config.Cluster, homeDomain, domain string) (user, cluster string) {
	var at []config.Cluster
	if domain == homeDomain {
		at = homeClusters(clusters, homeDomain)
	} else {
		for _, c := range clusters {
			if c.Site == domain {
				at = append(at, c)
			}
		}
	}
	for _, c := range at {
		if h, found, err := sshconfig.Lookup(p, c.SSHHost); err == nil && found && h.User != "" {
			return h.User, c.Name
		}
	}
	return "", ""
}

// accountHosts are the aliases whose User set_cluster_account changes: the
// cluster's login host and its files host when that is another one. Jump
// hosts are left alone.
func accountHosts(c config.Cluster) []string {
	out := []string{c.SSHHost}
	if c.FilesHost != "" && c.FilesHost != c.SSHHost {
		out = append(out, c.FilesHost)
	}
	return out
}

// aliasUsers returns the names of the clusters other than skip that use the
// ssh alias: as their login or files host, or in the ProxyJump chain of one
// of those (add_cluster reuses an existing entry for the same host, so one
// entry can serve several clusters).
func aliasUsers(p sshconfig.Paths, clusters []config.Cluster, skip, alias string) []string {
	var out []string
	for _, o := range clusters {
		if o.Name == skip {
			continue
		}
		seen := map[string]bool{}
		queue := []string{o.SSHHost, o.FilesHost}
		for len(queue) > 0 && len(seen) < 16 {
			a := queue[0]
			queue = queue[1:]
			if a == "" || seen[a] {
				continue
			}
			seen[a] = true
			if a == alias {
				out = append(out, o.Name)
				break
			}
			if h, found, err := sshconfig.Lookup(p, a); err == nil && found && h.ProxyJump != "" {
				for _, j := range strings.Split(h.ProxyJump, ",") {
					j = strings.TrimSpace(j)
					if i := strings.LastIndex(j, "@"); i >= 0 {
						j = j[i+1:]
					}
					if i := strings.LastIndex(j, ":"); i >= 0 {
						j = j[:i]
					}
					queue = append(queue, j)
				}
			}
		}
	}
	return out
}

// planClusterAccount checks that every account host of c has an entry in
// Labmill's own ssh file that no other cluster uses, and describes the change.
// The user's own entries are refused (Labmill never rewrites them), and so is
// an entry another cluster shares: changing it would switch that cluster to
// the new account too. Nothing changes unless all hosts pass.
func planClusterAccount(p sshconfig.Paths, c config.Cluster, clusters []config.Cluster, username string) (app.AccountChange, []sshconfig.Host, error) {
	entries, err := sshconfig.ListHosts(p)
	if err != nil {
		return app.AccountChange{}, nil, err
	}
	ch := app.AccountChange{File: p.LabmillConfig()}
	var edits []sshconfig.Host
	for _, alias := range accountHosts(c) {
		var e *sshconfig.Entry
		for i := range entries {
			if entries[i].Alias == alias {
				e = &entries[i]
				break
			}
		}
		switch {
		case e == nil:
			return app.AccountChange{}, nil, fmt.Errorf("The SSH entry %q for the cluster %s isn't in the SSH settings Labmill manages. Tell the user to change the account name in Settings or in their SSH settings.", alias, c.Name)
		case !e.InLabmill:
			return app.AccountChange{}, nil, fmt.Errorf("The SSH entry %q for the cluster %s is in the user's own SSH settings (~/.ssh/config), which Labmill never rewrites. Tell the user to change it in Settings or in their SSH settings.", alias, c.Name)
		}
		if users := aliasUsers(p, clusters, c.Name, alias); len(users) > 0 {
			return app.AccountChange{}, nil, fmt.Errorf("The SSH entry %q is also used by the cluster %s, so changing its account would change the sign-in there too. Nothing was changed. Tell the user they can give %s its own SSH entry in Settings.", alias, strings.Join(users, ", "), c.Name)
		}
		old := e.User
		if old == "" {
			old = "none"
		}
		ch.Lines = append(ch.Lines, fmt.Sprintf("%s (%s): User %s -> %s", alias, e.HostName, old, username))
		h := e.Host
		h.User = username
		if err := h.Validate(); err != nil {
			return app.AccountChange{}, nil, err
		}
		edits = append(edits, h)
	}
	return ch, edits, nil
}

// setClusterAccount applies planClusterAccount and returns the aliases it
// changed. All hosts are written in one atomic write of Labmill's file.
func setClusterAccount(p sshconfig.Paths, c config.Cluster, clusters []config.Cluster, username, goos string) ([]string, error) {
	_, edits, err := planClusterAccount(p, c, clusters, username)
	if err != nil {
		return nil, err
	}
	if _, err := sshconfig.PutLabmillHosts(p, edits, goos); err != nil {
		return nil, fmt.Errorf("change the account of %s (nothing was changed): %w", c.Name, err)
	}
	changed := make([]string, len(edits))
	for i, h := range edits {
		changed[i] = h.Alias
	}
	return changed, nil
}

// forgetLogins asks the SSH helper to close its logins for aliases (a test
// hook).
var forgetLogins = func(ctx context.Context, aliases []string) error {
	return sshx.ForgetLogins(ctx, sshx.ConnectOptions{}, aliases)
}

// drop closes and forgets the cached connections of the aliases, and the
// session records that depend on the account of cluster, so the next
// command starts fresh. The
// per-turn denied map stays: after a rejection no new sign-in starts in the
// same turn.
func (cc *connCache) drop(c config.Cluster, aliases ...string) {
	cc.mu.Lock()
	for _, a := range aliases {
		if cl := cc.clients[a]; cl != nil {
			cl.Close()
			delete(cc.clients, a)
		}
		if cl := cc.bg[a]; cl != nil {
			cl.Close()
			delete(cc.bg, a)
		}
	}
	cc.mu.Unlock()
}

// wireAccounts sets the Env functions for reusing and changing account
// names. paths returns the ssh settings location.
func wireAccounts(env *app.Env, conns *connCache, cfg *config.Config, homeDomain string, paths func() (sshconfig.Paths, error), goos string) {
	env.KnownUsername = func(domain string) (string, string) {
		p, err := paths()
		if err != nil {
			return "", ""
		}
		return knownUsername(p, cfg.List(), homeDomain, domain)
	}
	env.PlanClusterAccount = func(c config.Cluster, username string) (app.AccountChange, error) {
		p, err := paths()
		if err != nil {
			return app.AccountChange{}, errNoSSHPaths
		}
		ch, _, err := planClusterAccount(p, c, cfg.List(), username)
		return ch, err
	}
	env.SetClusterAccount = func(ctx context.Context, c config.Cluster, username string) ([]string, error) {
		p, err := paths()
		if err != nil {
			return nil, errNoSSHPaths
		}
		username = strings.TrimSpace(username)
		_, edits, err := planClusterAccount(p, c, cfg.List(), username)
		if err != nil {
			return nil, err
		}
		aliases := make([]string, len(edits))
		for i, h := range edits {
			aliases[i] = h.Alias
		}
		// The helper keeps live logins per alias whatever the settings say:
		// have it forget them first, and change nothing if it can't.
		if err := forgetLogins(ctx, aliases); err != nil {
			return nil, fmt.Errorf("the old sign-in can't be closed (%w); nothing was changed", err)
		}
		changed, err := setClusterAccount(p, c, cfg.List(), username, goos)
		conns.drop(c, aliases...)
		return changed, err
	}
}
