package gui

import (
	"context"
	"errors"
	"time"

	"labmill-client/internal/textsafe"
)

// The Cluster tab of the folder picker: a lazy tree of the login node's folders over the open SSH session. The page
// asks for one level at a time; Go answers through app.Env.BrowseRemote, so the TUI, the chat tool and this tab
// share one source. Choosing a folder only puts its path into the chat box; nothing is read or sent.

// ClusterFolder is one level of the remote tree for the page.
type ClusterFolder struct {
	Path     string   `json:"path"`
	Writable bool     `json:"writable"`
	FreeKiB  string   `json:"freeKiB"`
	Dirs     []string `json:"dirs"`
}

// ClusterNames returns the configured cluster names for the tab's first choice.
func (a *App) ClusterNames() ([]string, error) {
	env, _, err := a.workspace()
	if env == nil {
		return []string{}, err
	}
	out := []string{}
	if env.Config != nil {
		for _, c := range env.Config.List() {
			out = append(out, textsafe.Clean(c.Name))
		}
	}
	return out, nil
}

// BrowseCluster lists the sub-folders of path (empty: home) on cluster.
func (a *App) BrowseCluster(cluster, path string) (ClusterFolder, error) {
	env, _, err := a.workspace()
	if env == nil {
		return ClusterFolder{}, err
	}
	if env.Config == nil {
		return ClusterFolder{}, errors.New("no clusters are set up yet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := env.BrowseRemote(ctx, cluster, path)
	if err != nil {
		return ClusterFolder{}, err
	}
	out := ClusterFolder{Path: textsafe.Clean(r.Path), Writable: r.Writable, FreeKiB: textsafe.Clean(r.FreeKiB), Dirs: []string{}}
	for _, d := range r.Dirs {
		out.Dirs = append(out.Dirs, textsafe.Clean(d))
	}
	return out, nil
}
