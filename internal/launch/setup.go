package launch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)

// ValidUsername reports whether name is an acceptable cluster account name
// ([a-z0-9._-]{1,32}); setup forms and WriteSetup share this rule.
func ValidUsername(name string) bool { return usernameRE.MatchString(name) }

var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// ValidLoginHost reports whether h is a plain DNS host name (letters, digits,
// dashes and dots, at most 253 characters), as asked for by the setup forms
// when the chosen site has no login host.
func ValidLoginHost(h string) bool { return len(h) <= 253 && hostnameRE.MatchString(h) }

// NeedsHosts reports whether the setup forms should ask for a login host for
// s: true when the site profile has no login host (the
// generic profile, or a site whose hosts aren't documented yet).
func NeedsHosts(s site.Site) bool { return s.LoginHost() == "" }

// HostsRequired is the M55 rule for a site without a login host (NeedsHosts,
// e.g. the generic profile): the home institution is then named only by what
// the user enters, so the login host must be given. It returns nil for any
// other site, or when one is given.
func HostsRequired(s site.Site, loginHost string) error {
	if NeedsHosts(s) && strings.TrimSpace(loginHost) == "" {
		return errors.New("Enter the login host of your institution, so Labmill knows which institution is yours.")
	}
	return nil
}

// withAnswers returns s with host and cluster templates built from the
// answers when the profile has no login host (NeedsHosts): LoginHost becomes
// a login host (alias = its first DNS label) and a Slurm cluster of the same
// name. Profiles with a login host are returned unchanged.
func withAnswers(s site.Site, a SetupAnswers) (site.Site, error) {
	if !NeedsHosts(s) {
		return s, nil
	}
	login := strings.TrimSpace(a.LoginHost)
	if login != "" && !ValidLoginHost(login) {
		return s, fmt.Errorf("login host %q isn't a valid host name", login)
	}
	if err := HostsRequired(s, login); err != nil {
		return s, err
	}
	s.Hosts = append([]sshconfig.Host(nil), s.Hosts...)
	s.Clusters = append([]config.Cluster(nil), s.Clusters...)
	roles := map[string]string{}
	for k, v := range s.Roles {
		roles[k] = v
	}
	s.Roles = roles
	if login != "" {
		alias := strings.ToLower(strings.SplitN(login, ".", 2)[0])
		s.Hosts = append(s.Hosts, sshconfig.Host{Alias: alias, HostName: login})
		s.Roles[alias] = site.RoleLogin
		s.Clusters = append(s.Clusters, config.Cluster{Name: alias, Type: config.TypeSlurm, SSHHost: alias, Transfer: config.TransferSFTP})
	}
	return s, nil
}

// WriteSetup applies the site templates for the answers: for each
// site.Hosts entry, reuse an existing alias for that HostName
// (sshconfig.AliasSource; an alias from the user's own config is referenced
// but never written) or take a UniqueAlias, fill in User, rewrite ProxyJump
// to the final alias of the jump host, and write them (sshconfig.WriteHosts,
// then EnsureInclude). When the site has no login host (NeedsHosts), the
// templates come from a.LoginHost instead (required, as it names the home
// institution, M55). Cluster templates get their
// SSHHost/FilesHost rewritten to the final aliases and are saved to cfgPath
// (config.Save) together with `site: <s.Domain>`. It returns the saved
// config.
func WriteSetup(p sshconfig.Paths, cfgPath string, s site.Site, a SetupAnswers, goos string) (*config.Config, error) {
	if !usernameRE.MatchString(a.Username) {
		return nil, fmt.Errorf("account name %q isn't valid", a.Username)
	}
	s, err := withAnswers(s, a)
	if err != nil {
		return nil, err
	}
	rename, err := writeSiteHosts(p, s.Hosts, a.Username, goos)
	if err != nil {
		return nil, err
	}
	cfg := &config.Config{Site: s.Domain}
	for _, c := range s.Clusters {
		if v, ok := rename[c.SSHHost]; ok {
			c.SSHHost = v
		}
		if v, ok := rename[c.FilesHost]; ok {
			c.FilesHost = v
		}
		cfg.Clusters = append(cfg.Clusters, c)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// writeSiteHosts writes the host templates to Labmill's ssh settings for the
// account username: for each, reuse an existing alias for that HostName
// (sshconfig.AliasSource; an alias from the user's own config is referenced
// but never written) or take a UniqueAlias, fill in User and rewrite
// ProxyJump to the final alias of the jump host; then WriteHosts and
// EnsureInclude. It returns the map from template alias to final alias.
func writeSiteHosts(p sshconfig.Paths, templates []sshconfig.Host, username, goos string) (map[string]string, error) {
	rename := map[string]string{} // template alias -> final alias
	var hosts []sshconfig.Host
	for _, h := range templates {
		alias, inLabmill, found, err := sshconfig.AliasSource(p, h.HostName)
		if err != nil {
			return nil, fmt.Errorf("read ssh settings: %w", err)
		}
		if found && !inLabmill {
			// The user's own alias: reference it, never rewrite it into
			// Labmill's file (which ssh reads first and would override it).
			rename[h.Alias] = alias
			continue
		}
		if !found {
			if alias, err = sshconfig.UniqueAlias(p, h.Alias); err != nil {
				return nil, fmt.Errorf("choose a name for %s: %w", h.HostName, err)
			}
		}
		rename[h.Alias] = alias
		h.Alias, h.User = alias, username
		hosts = append(hosts, h)
	}
	for i, h := range hosts {
		if v, ok := rename[h.ProxyJump]; ok {
			hosts[i].ProxyJump = v
		}
	}
	if len(templates) > 0 {
		if err := sshconfig.WriteHosts(p, hosts, goos); err != nil {
			return nil, fmt.Errorf("write ssh settings: %w", err)
		}
		if _, err := sshconfig.EnsureInclude(p, time.Now()); err != nil {
			return nil, fmt.Errorf("update ssh settings: %w", err)
		}
	}
	return rename, nil
}
