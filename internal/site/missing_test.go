package site

import (
	"slices"
	"testing"

	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
)

func TestMissingClustersSharedHost(t *testing.T) {
	s := Site{
		Hosts: []sshconfig.Host{{Alias: "a", HostName: "login.example.edu"}, {Alias: "b", HostName: "login.example.edu"}, {Alias: "c", HostName: "other.example.edu"}},
		Clusters: []config.Cluster{
			{Name: "one", Type: config.TypeSlurm, SSHHost: "a"},
			{Name: "two", Type: config.TypeSlurm, SSHHost: "b"},
			{Name: "three", Type: config.TypeSlurm, SSHHost: "c"},
		},
	}
	hostName := func(alias string) string {
		return map[string]string{"mine": "login.example.edu", "mine3": "other.example.edu"}[alias]
	}
	tests := []struct {
		name string
		have []config.Cluster
		want []string
	}{
		{"none", nil, []string{"one", "two", "three"}},
		{"shared host: name matches only", []config.Cluster{{Name: "one", Type: config.TypeSlurm, SSHHost: "mine"}}, []string{"two", "three"}},
		{"shared host, renamed hides neither", []config.Cluster{{Name: "x", Type: config.TypeSlurm, SSHHost: "mine"}}, []string{"one", "two", "three"}},
		{"unique host matches by host", []config.Cluster{{Name: "x", Type: config.TypeSlurm, SSHHost: "mine3"}}, []string{"one", "two"}},
	}
	for _, tc := range tests {
		var got []string
		for _, c := range s.MissingClusters(tc.have, hostName) {
			got = append(got, c.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
