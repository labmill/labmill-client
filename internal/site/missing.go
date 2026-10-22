package site

import (
	"strings"

	"labmill-client/internal/config"
)

// MissingClusters returns the profile's cluster templates that have is
// missing (M57.2: the profile gained a cluster after an install was set up),
// in profile order. A template counts as configured, and is not returned,
// when a configured cluster has its name, or is of the same type and points
// at the same place: an ssh_host that resolves to the template's login
// HostName. So a renamed cluster is not offered
// again. hostName returns the HostName of an ssh alias in the user's SSH
// settings ("" when unknown); it may be nil, then Slurm clusters match by
// name only. A host shared by several Slurm templates of the profile doesn't
// match by host (configuring one would hide the others). A cluster the user
// removed on purpose is offered again.
func (s Site) MissingClusters(have []config.Cluster, hostName func(alias string) string) []config.Cluster {
	hostOf := map[string]string{}
	for _, h := range s.Hosts {
		hostOf[h.Alias] = h.HostName
	}
	var out []config.Cluster
	for _, t := range s.Clusters {
		host := hostOf[t.SSHHost]
		if t.Type == config.TypeSlurm && host != "" {
			// Another Slurm template on the same login host would be hidden by
			// a match on the host alone, so those match by name only.
			for _, o := range s.Clusters {
				if o.Name != t.Name && o.Type == config.TypeSlurm && strings.EqualFold(hostOf[o.SSHHost], host) {
					host = ""
					break
				}
			}
		}
		if !configuredAs(t, host, have, hostName) {
			out = append(out, t)
		}
	}
	return out
}

// configuredAs reports whether have contains the template t, whose login
// host name is tHost.
func configuredAs(t config.Cluster, tHost string, have []config.Cluster, hostName func(string) string) bool {
	for _, c := range have {
		if c.Name == t.Name {
			return true
		}
		if c.Type != t.Type {
			continue
		}
		if tHost != "" && hostName != nil && c.SSHHost != "" && strings.EqualFold(hostName(c.SSHHost), tHost) {
			return true
		}
	}
	return false
}
