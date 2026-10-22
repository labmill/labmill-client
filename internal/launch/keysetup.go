package launch

import (
	"context"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
)

// keySite is what the connection cache knows about the site's key-only hosts
// (Site.yaml hosts with auth: key): their host names and the key-upload text.
type keySite struct {
	hostNames []string
	upload    sshx.KeyUploadInfo
	// byHost holds the key-upload text of each key-only host (lowercase
	// host name) of the institutions added to the session later (M45.1):
	// their own text, not the home institution's. A host missing here uses
	// upload.
	byHost map[string]sshx.KeyUploadInfo
	// name is the home institution's name; nameByHost the names of the
	// institutions added later, by lowercase host name (texts only).
	name       string
	nameByHost map[string]string
	// homeAlias and homeHost are the home institution's login host (its
	// ssh alias and host name): the place M45.3 looks for keys that a host
	// of another institution (a byHost entry) may accept.
	homeAlias, homeHost string
}

// merged returns k plus the key-only hosts of s, each with s's own
// key-upload text.
func (k keySite) merged(s site.Site) keySite {
	out := keySite{hostNames: append([]string(nil), k.hostNames...), upload: k.upload, byHost: map[string]sshx.KeyUploadInfo{},
		name: k.name, nameByHost: map[string]string{}, homeAlias: k.homeAlias, homeHost: k.homeHost}
	for h, u := range k.byHost {
		out.byHost[h] = u
	}
	for h, n := range k.nameByHost {
		out.nameByHost[h] = n
	}
	up := sshx.KeyUploadInfo{URL: s.KeyUpload.URL, Instruction: s.KeyUpload.Instruction}
	for _, n := range s.KeyOnlyHostNames() {
		out.hostNames = append(out.hostNames, n)
		out.byHost[strings.ToLower(n)] = up
		out.nameByHost[strings.ToLower(n)] = s.Name
	}
	return out
}

// uploadFor is the key-upload text for the host named hostName.
func (k keySite) uploadFor(hostName string) sshx.KeyUploadInfo {
	if u, ok := k.byHost[strings.ToLower(hostName)]; ok {
		return u
	}
	return k.upload
}

// siteNameFor is the name of the institution that runs hostName.
func (k keySite) siteNameFor(hostName string) string {
	if n, ok := k.nameByHost[strings.ToLower(hostName)]; ok {
		return n
	}
	return k.name
}

// foreign reports that hostName belongs to an institution added to the
// session, not to the home one.
func (k keySite) foreign(hostName string) bool {
	_, ok := k.byHost[strings.ToLower(hostName)]
	return ok
}

// addKeySite adds the key-only hosts of s to the connection cache (a
// cluster of another institution was added to the session).
func (cc *connCache) addKeySite(s site.Site) {
	cc.keysMu.Lock()
	defer cc.keysMu.Unlock()
	cc.keys = cc.keys.merged(s)
}

// addHomeKeyHosts adds the key-only hosts of the home profile s to the
// connection cache (a home cluster was added to the session, M57.2). Unlike
// addKeySite they are not "foreign": the key setup stays the home flow and
// the other-institution key copy (M45.3) stays off for them.
func (cc *connCache) addHomeKeyHosts(s site.Site) {
	cc.keysMu.Lock()
	defer cc.keysMu.Unlock()
	k := cc.keys
	k.hostNames = append([]string(nil), k.hostNames...)
	for _, n := range s.KeyOnlyHostNames() {
		if !slices.ContainsFunc(k.hostNames, func(x string) bool { return strings.EqualFold(x, n) }) {
			k.hostNames = append(k.hostNames, n)
		}
	}
	cc.keys = k
}

// noteAddedCluster makes the key-only hosts of a cluster that was just added
// to the session known to the connection cache.
// o is the profile the cluster comes from; home is the home domain.
func (cc *connCache) noteAddedCluster(home string, c config.Cluster, o site.Site) {
	if o.Domain == home {
		cc.addHomeKeyHosts(o)
	} else {
		cc.addKeySite(o)
	}
}

// keySnapshot returns the current key-only host data.
func (cc *connCache) keySnapshot() keySite {
	cc.keysMu.RLock()
	defer cc.keysMu.RUnlock()
	return cc.keys
}

// keySiteOf extracts the key-only hosts and the key-upload text of s.
func keySiteOf(s site.Site) keySite {
	k := keySite{
		hostNames: s.KeyOnlyHostNames(),
		upload:    sshx.KeyUploadInfo{URL: s.KeyUpload.URL, Instruction: s.KeyUpload.Instruction},
		name:      s.Name,
		homeHost:  s.LoginHost(),
	}
	for _, h := range s.Hosts {
		if s.Roles[h.Alias] == site.RoleLogin {
			k.homeAlias = h.Alias
			break
		}
	}
	return k
}

// keyGuideFor returns the guided SSH key setup for alias when the host it
// names (its HostName in the ssh config) is one of the site's key-only hosts.
// The setup asks through the front end (cc.ui).
func (cc *connCache) keyGuideFor(alias string) (*sshx.KeyGuide, bool) {
	keys := cc.keySnapshot()
	if len(keys.hostNames) == 0 {
		return nil, false
	}
	paths := cc.sshPaths
	if paths == nil {
		paths = sshconfig.DefaultPaths
	}
	p, err := paths()
	if err != nil {
		return nil, false
	}
	cfg, _, err := sshconfig.Lookup(p, alias)
	if err != nil || cfg.HostName == "" {
		return nil, false
	}
	listed := false
	for _, n := range keys.hostNames {
		if strings.EqualFold(n, cfg.HostName) {
			listed = true
			break
		}
	}
	if !listed {
		return nil, false
	}
	g := &sshx.KeyGuide{Paths: p, Alias: alias, Host: cfg.HostName, Upload: keys.uploadFor(cfg.HostName), Prompter: cc.ui,
		SiteName: keys.siteNameFor(cfg.HostName), QueryOpts: sshx.LoginOptions{KeyOnlyHosts: keys.hostNames}}
	// A host of another institution may accept a key the user has on the
	// home login node (M45.3). Not for the home institution's own hosts.
	if keys.foreign(cfg.HostName) && keys.homeAlias != "" && !strings.EqualFold(keys.homeAlias, alias) {
		g.Home = &sshx.HomeKeys{Alias: keys.homeAlias, Host: keys.homeHost, Client: func(ctx context.Context) (*ssh.Client, func()) {
			return cc.homeClientLocked(ctx, p, keys.homeAlias, keys.homeHost)
		}}
	}
	return g, true
}

// homeClientLocked returns a live connection to the home login host for the
// key look-up without ever logging in: a cached client, else one on a login
// the helper already holds. The aliases tried are the site's own login alias
// and the ssh alias of any configured home cluster whose HostName is the home
// login host (the user's own alias for it). connect runs under cc.mu, so the
// cache is read without locking again. The release function closes a client
// taken from the helper.
func (cc *connCache) homeClientLocked(ctx context.Context, p sshconfig.Paths, alias, homeHost string) (*ssh.Client, func()) {
	aliases := []string{alias}
	for _, a := range cc.homeSSHHosts {
		if a == "" || a == alias {
			continue
		}
		if cfg, _, err := sshconfig.Lookup(p, a); err == nil && homeHost != "" && strings.EqualFold(cfg.HostName, homeHost) {
			aliases = append(aliases, a)
		}
	}
	for _, a := range aliases {
		if c := cc.clients[a]; c != nil {
			select {
			case <-c.dead:
			default:
				return c.Client, func() {}
			}
		}
	}
	for _, a := range aliases {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cl, err := connectCached(cctx, a)
		cancel()
		if err == nil && cl != nil {
			return cl, func() { cl.Close() }
		}
	}
	return nil, nil
}

// connect logs in to alias through the helper; on a key-only host the guided
// key setup tries the login first and runs the setup when there is no usable
// key or the server rejects the keys.
func (cc *connCache) connect(ctx context.Context, alias string) (*ssh.Client, error) {
	do := func(ctx context.Context) (*ssh.Client, error) {
		return sshConnect(ctx, alias, cc.ui, sshx.ConnectOptions{})
	}
	if g, ok := cc.keyGuideFor(alias); ok {
		return g.Run(ctx, do)
	}
	return do(ctx)
}
