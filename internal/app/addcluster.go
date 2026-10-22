package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"labmill-client/internal/agent"
	"labmill-client/internal/config"
)

// add_cluster (M45.1): the user explicitly asked for a cluster at another
// institution that this build has a profile for. The tool adds that cluster
// from the profile's Cluster.yaml template to clusters.yaml (with the
// institution recorded as the cluster's `site:`) and writes the SSH settings
// for its hosts. It only writes local configuration, so it is an ordinary
// approval; nothing is installed or run on any cluster.

// AddClusterHost is one SSH host that add_cluster will write to
// ~/.ssh/config.d/labmill (or reuse, when an entry for the same host name
// already exists).
type AddClusterHost struct {
	Alias    string // the profile's alias; the final alias may differ
	HostName string // DNS name
	Role     string // "login", "transfer" or "jump"
	// KeyOnly: the host accepts SSH keys only (Labmill guides the key setup);
	// otherwise it signs in with password and two-factor.
	KeyOnly bool
	// ReusedAlias is the alias of an existing SSH entry for the same host
	// that add_cluster reuses instead of writing a new one ("" when the
	// host is new). ReusedUser is that entry's account ("" when it has no
	// User line). The entry's own User wins, so a different account is
	// refused (AccountConflict).
	ReusedAlias string
	ReusedUser  string
}

// AccountConflict returns a plain-language error when username differs from
// the account of an SSH entry the plan reuses: Labmill would sign in with the
// entry's account, not the typed one, and a wrong account can lock the user
// out. nil when there is no conflict.
func (p AddClusterPlan) AccountConflict(username string) error {
	for _, h := range p.Hosts {
		if h.ReusedAlias != "" && h.ReusedUser != "" && h.ReusedUser != username {
			return fmt.Errorf("Your SSH settings already have an entry %q for %s with the account %s, not %s. Labmill would sign in with %s. Use that account, or change the existing SSH entry first.",
				sanitize(h.ReusedAlias), sanitize(h.HostName), sanitize(h.ReusedUser), sanitize(username), sanitize(h.ReusedUser))
		}
	}
	return nil
}

// AddClusterPlan is what adding one cluster of another institution means,
// built by Env.PlanAddCluster from that institution's profile.
type AddClusterPlan struct {
	// Site is the institution (name, domain, site skill).
	Site OtherSite
	// Cluster is the profile's clusters.yaml template, before aliases are
	// resolved and Site is set.
	Cluster config.Cluster
	// Hosts are the SSH hosts the cluster needs: its ssh_host, files_host
	// and the jump hosts they go through.
	Hosts []AddClusterHost
	// Files are the local files that change, as shown to the user.
	Files []string
	// Hints are the cluster's facts from the site profile, for
	// SiteInfo.Clusters.
	Hints ClusterHints
}

// addClusterUserRE is the account-name rule of the first-run form.
var addClusterUserRE = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)

type addClusterInput struct {
	Site     string `json:"site"`
	Cluster  string `json:"cluster"`
	Username string `json:"username"`
	// fromCluster is the configured cluster whose account name was taken
	// when the request named none ("" when the user's name was given).
	fromCluster string
	// fromEntry is the existing SSH entry whose account was taken instead
	// (the entry's own User wins); "" otherwise.
	fromEntry string
}

// addClusterPlan checks the request against the configuration and returns
// the plan, or an error that says what to do instead.
func (t *toolset) addClusterPlan(input json.RawMessage) (addClusterInput, AddClusterPlan, error) {
	var in addClusterInput
	if err := decode(input, &in); err != nil {
		return in, AddClusterPlan{}, err
	}
	in.Site, in.Cluster, in.Username = strings.TrimSpace(in.Site), strings.TrimSpace(in.Cluster), strings.TrimSpace(in.Username)
	if t.env.PlanAddCluster == nil || t.env.AddCluster == nil {
		return in, AddClusterPlan{}, errors.New("Adding a cluster isn't available in this front end. The user can add it in Settings, or edit clusters.yaml.")
	}
	if in.Site == "" || in.Cluster == "" {
		return in, AddClusterPlan{}, errors.New("add_cluster needs the institution's domain (site) and the cluster's name.")
	}
	home := in.Site == t.env.Site.Domain
	var known *OtherSite
	for i, o := range t.env.Site.Others {
		if o.Domain == in.Site {
			known = &t.env.Site.Others[i]
		}
	}
	if home {
		// M57.2: a cluster the home profile gained after this install was
		// set up: only those in SiteInfo.HomeMissing; the rest is configured.
		missing := false
		var names []string
		for _, c := range t.env.Site.HomeMissing {
			missing = missing || c.Name == in.Cluster
			names = append(names, c.Name)
		}
		if !missing {
			return in, AddClusterPlan{}, fmt.Errorf("%s is the user's own institution: %q isn't a cluster missing from the configuration (not set up yet: %s). The configured clusters are in list_clusters.", in.Site, sanitize(in.Cluster), noneIfEmpty(names))
		}
	} else {
		if known == nil {
			return in, AddClusterPlan{}, fmt.Errorf("This Labmill has no profile for %q. Known institutions: %s.", sanitize(in.Site), otherDomains(t.env.Site.Others))
		}
		found := false
		for _, c := range known.Clusters {
			found = found || c == in.Cluster
		}
		if !found {
			return in, AddClusterPlan{}, fmt.Errorf("%s has no cluster %q in this Labmill. Its clusters: %s.", known.Name, sanitize(in.Cluster), strings.Join(known.Clusters, ", "))
		}
	}
	if t.env.Config != nil {
		if c, ok := t.env.Config.Cluster(in.Cluster); ok {
			where := "the user's own institution"
			if c.Site != "" {
				where = c.Site
			}
			if c.Site == in.Site {
				return in, AddClusterPlan{}, fmt.Errorf("The cluster %s is already set up (list_clusters); use it directly.", in.Cluster)
			}
			return in, AddClusterPlan{}, fmt.Errorf("A cluster named %s already exists (%s), and cluster names must be unique. Tell the user; they can rename it in Settings.", in.Cluster, where)
		}
	}
	if in.Username == "" && t.env.KnownUsername != nil {
		// The account the user already has at that institution (the home
		// one, or another cluster there) is tried first.
		in.Username, in.fromCluster = t.env.KnownUsername(in.Site)
	}
	if in.Username == "" {
		return in, AddClusterPlan{}, errors.New("Labmill knows no account name for that institution yet. Ask the user for their account name there (lowercase letters, digits, '.', '_' and '-'), then call add_cluster again with it.")
	}
	if !addClusterUserRE.MatchString(in.Username) {
		in.fromCluster = ""
		return in, AddClusterPlan{}, errors.New("That account name is not valid. Ask the user for their account name at that institution (lowercase letters, digits, '.', '_' and '-'), then call add_cluster again with it.")
	}
	plan, err := t.env.PlanAddCluster(in.Site, in.Cluster)
	if err != nil {
		return in, AddClusterPlan{}, err
	}
	if in.fromCluster != "" {
		// A reused name the user never typed: an existing SSH entry for the
		// same host has its own User, and ssh signs in with that one.
		for _, h := range plan.Hosts {
			if h.ReusedAlias != "" && h.ReusedUser != "" && h.ReusedUser != in.Username {
				if !addClusterUserRE.MatchString(h.ReusedUser) {
					break
				}
				in.Username, in.fromCluster, in.fromEntry = h.ReusedUser, "", h.ReusedAlias
				break
			}
		}
	}
	if err := plan.AccountConflict(in.Username); err != nil {
		return in, AddClusterPlan{}, err
	}
	return in, plan, nil
}

// RegisterAddedCluster makes a cluster that Env.AddCluster just wrote
// visible in the running session (the tools read the configuration through
// Env.Config and the hints through Env.Site). add_cluster calls it, and so
// does the GUI's "More clusters at your institution" Add button (M57.2). Call
// it only while no turn runs: the front end runs the Add as a turn of its own.
func (e *Env) RegisterAddedCluster(plan AddClusterPlan, added config.Cluster) {
	if e.Config != nil {
		e.Config.AddCluster(added)
	}
	// Copy on write: tools read Env.Site only during a turn, and the front
	// end calls this as a turn of its own (never from the settings page's
	// own thread); a map that is never modified in place can't race.
	hints := make(map[string]ClusterHints, len(e.Site.Clusters)+1)
	for k, v := range e.Site.Clusters {
		hints[k] = v
	}
	hints[added.Name] = plan.Hints
	e.Site.Clusters = hints
	e.Site.HomeMissing = withoutHomeMissing(e.Site.HomeMissing, added.Name)
}

// withoutHomeMissing returns a copy of l without the cluster name.
func withoutHomeMissing(l []HomeCluster, name string) []HomeCluster {
	var out []HomeCluster
	for _, c := range l {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}

func noneIfEmpty(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func otherDomains(others []OtherSite) string {
	var d []string
	for _, o := range others {
		d = append(d, o.Domain)
	}
	if len(d) == 0 {
		return "none"
	}
	return strings.Join(d, ", ")
}

// accountSourceNote says plainly where a reused account name came from and
// what to do if the sign-in rejects it ("" when the user gave the name).
func accountSourceNote(in addClusterInput) string {
	if in.fromEntry != "" {
		return fmt.Sprintf("Account: %s (the account of your existing SSH entry %s for that host). If the sign-in rejects it, ask the user for their account name at that institution and change it with set_cluster_account.",
			sanitize(in.Username), sanitize(in.fromEntry))
	}
	if in.fromCluster == "" {
		return ""
	}
	return fmt.Sprintf("Account: %s (the same as on %s). If the sign-in rejects it, ask the user for their account name at that institution and change it with set_cluster_account.",
		sanitize(in.Username), sanitize(in.fromCluster))
}

func accountResultNote(in addClusterInput) string {
	if in.fromEntry != "" {
		return fmt.Sprintf("The account name is the one of your existing SSH entry %s; if the sign-in rejects it, ask the user for their account name there and change it with set_cluster_account (do not retry the password). ", sanitize(in.fromEntry))
	}
	if in.fromCluster == "" {
		return ""
	}
	return fmt.Sprintf("The account name was reused from %s; if the sign-in rejects it, ask the user for their account name there and change it with set_cluster_account (do not retry the password). ", sanitize(in.fromCluster))
}

func addClusterProposal(in addClusterInput, p AddClusterPlan) agent.Proposal {
	var b strings.Builder
	fmt.Fprintf(&b, "Institution: %s (%s)\n", sanitize(p.Site.Name), sanitize(p.Site.Domain))
	fmt.Fprintf(&b, "Cluster: %s (%s)\n", sanitize(p.Cluster.Name), p.Cluster.Type)
	fmt.Fprintf(&b, "Your account name there: %s\n", sanitize(in.Username))
	if note := accountSourceNote(in); note != "" {
		b.WriteString(note + "\n")
	}
	b.WriteString("SSH hosts that will be added (existing entries for the same host stay as they are):\n")
	for _, h := range p.Hosts {
		auth := "password and two-factor sign-in"
		if h.KeyOnly {
			auth = "SSH key only (Labmill guides the key setup at the first sign-in)"
		}
		fmt.Fprintf(&b, "  %s (%s): %s\n", sanitize(h.HostName), sanitize(h.Role), auth)
		if h.ReusedAlias != "" {
			who := "no account named in it"
			if h.ReusedUser != "" {
				who = "account " + sanitize(h.ReusedUser)
			}
			fmt.Fprintf(&b, "    your existing SSH entry %s is reused as it is (%s)\n", sanitize(h.ReusedAlias), who)
		}
	}
	b.WriteString("Files that change:\n")
	for _, f := range p.Files {
		fmt.Fprintf(&b, "  %s\n", sanitize(f))
	}
	b.WriteString("Nothing is sent to or run on that institution now; you sign in the first time a job or command needs it.")
	return agent.Proposal{
		Tool:    "add_cluster",
		Summary: fmt.Sprintf("Add the cluster %s at %s (account %s)", sanitize(p.Cluster.Name), sanitize(p.Site.Name), sanitize(in.Username)),
		Details: b.String(),
		// Moving work to another institution is the user's call: the
		// setting "Approve everything automatically" never approves it
		// (the same rule as reading a sign-in secret). A new cluster of the
		// user's own institution (M57.2) always asks too.
		Secret: true,
	}
}

func (t *toolset) addCluster() agent.Tool {
	return agent.Tool{
		Name: "add_cluster",
		Description: "Set up a cluster of ANOTHER institution that this Labmill knows (listed in your instructions), only when the user explicitly asks for that institution or one of its clusters. " +
			"It adds the cluster to the user's configuration and writes the SSH settings for its hosts; the user must approve. " +
			"It also adds a cluster of the user's OWN institution that isn't set up yet (listed in your instructions as 'Also available at your institution'): pass the user's own institution's domain as site. " +
			"Leave username empty: Labmill reuses the account the user already has at that institution (the one used by another configured cluster there) and says so in the proposal. " +
			"Ask the user for an account name only when add_cluster says none is known, when the site skill says this cluster uses a different account, or after a sign-in rejected the reused name (then change it with set_cluster_account). After it succeeds the cluster works like the others (list_clusters, cluster_info, draft_job); " +
			"the first sign-in happens when a command needs it. Never use it on your own initiative: work goes to the user's own institution by default.",
		Schema: objSchema(map[string]any{
			"site":     strProp("the institution's domain, e.g. as listed under 'Other institutions known to this labmill'"),
			"cluster":  strProp("the cluster's name at that institution"),
			"username": strProp("the user's account name at that institution; leave empty to reuse the account already used there (ask the user only when add_cluster says none is known)"),
		}, "site", "cluster"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			in, plan, err := t.addClusterPlan(input)
			if err != nil {
				return agent.Proposal{}, err
			}
			return addClusterProposal(in, plan), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			in, plan, err := t.addClusterPlan(input)
			if err != nil {
				return "", err
			}
			if err := sameAsApproved(addClusterProposal(in, plan), d.Proposal); err != nil {
				return "", err
			}
			added, err := t.env.AddCluster(ctx, plan, in.Username)
			if err != nil {
				return "", fmt.Errorf("I couldn't add the cluster %s: %w", plan.Cluster.Name, err)
			}
			t.env.RegisterAddedCluster(plan, added)
			return fmt.Sprintf("Added the cluster %s of %s (account %s). %sIt is ready to use in this conversation (list_clusters, cluster_info); the sign-in happens the first time a command needs it. Load the skill %s for its specifics.",
				added.Name, plan.Site.Name, in.Username, accountResultNote(in), plan.Site.Skill), nil
		},
	}
}
