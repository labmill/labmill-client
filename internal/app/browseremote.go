package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"labmill-client/internal/agent"
)

// browse_remote: the remote folder picker. One read-only round trip on the login node lists the sub-folders of one
// directory. The model offers them with ask_user, so the user picks a work folder or an install location on the
// cluster the same way they pick one on the laptop. It changes nothing.

const (
	maxBrowseEntries = 200
	maxBrowsePath    = 1024
)

// browseScript gets the directory in $1 (empty: the home folder). It prints the real path, the free space in 1 KiB
// blocks, a write flag and the sub-folders. Exit 3 means the folder can't be entered.
const browseScript = `d=$1; [ -n "$d" ] || d=$HOME
cd -- "$d" 2>/dev/null || exit 3
printf '@@PWD\n%s\n' "$(pwd -P)"
printf '@@FREE\n'; df -Pk . 2>/dev/null | awk 'NR==2{print $4}'
printf '@@WRITE\n'; if [ -w . ]; then echo yes; else echo no; fi
printf '@@DIRS\n'; ls -1Ap 2>/dev/null | grep '/$' | head -n 201
exit 0`

// BrowseResult is the parsed probe output: one folder of a cluster.
type BrowseResult struct {
	Path     string
	FreeKiB  string
	Writable bool
	Dirs     []string // names without the trailing slash
}

func parseBrowse(out string) BrowseResult {
	var r BrowseResult
	sec := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@@") {
			sec = line
			continue
		}
		switch sec {
		case "@@PWD":
			if r.Path == "" {
				r.Path = line
			}
		case "@@FREE":
			if r.FreeKiB == "" {
				r.FreeKiB = strings.TrimSpace(line)
			}
		case "@@WRITE":
			r.Writable = strings.TrimSpace(line) == "yes"
		case "@@DIRS":
			if name := strings.TrimSuffix(line, "/"); name != "" && name != line {
				r.Dirs = append(r.Dirs, name)
			}
		}
	}
	return r
}

// renderBrowse is the text the model reads. Names from the cluster are data, so they go through sanitize.
func renderBrowse(cluster string, r BrowseResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Folder %s on %s (login node, read only).", sanitize(r.Path), sanitize(cluster))
	if r.Writable {
		b.WriteString(" The user can write here.")
	} else {
		b.WriteString(" The user can not write here.")
	}
	if r.FreeKiB != "" {
		fmt.Fprintf(&b, " Free space on this file system: %s KiB.", sanitize(r.FreeKiB))
	}
	dirs := r.Dirs
	more := false
	if len(dirs) > maxBrowseEntries {
		dirs, more = dirs[:maxBrowseEntries], true
	}
	if len(dirs) == 0 {
		b.WriteString("\nNo sub-folders.")
		return b.String()
	}
	b.WriteString("\nSub-folders:\n")
	for _, d := range dirs {
		b.WriteString("- " + sanitize(d) + "\n")
	}
	if more {
		fmt.Fprintf(&b, "(more than %d sub-folders: only the first are shown; ask the user for the name or call again with a deeper path.)\n", maxBrowseEntries)
	}
	return b.String()
}

func (t *toolset) browseRemote() agent.Tool {
	return agent.Tool{
		Name: "browse_remote",
		Description: "List the sub-folders of one folder on a cluster (read only, one round trip on the login node), to help the user pick a work folder or an install location there. " +
			"Leave path empty to start in their home folder. Show the sub-folders with ask_user (put '.. (up)' and 'Use this folder' as options), then call again with the chosen path; repeat until the user picks a folder. " +
			"The result says if the user can write in the folder and how much space is free. It never changes anything.",
		Schema: objSchema(map[string]any{
			"cluster": strProp("cluster name from list_clusters"),
			"path":    strProp("absolute folder on the cluster, or empty for the home folder"),
		}, "cluster"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Cluster string `json:"cluster"`
				Path    string `json:"path"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			r, err := t.env.BrowseRemote(ctx, in.Cluster, in.Path)
			if err != nil {
				return "", err
			}
			return renderBrowse(in.Cluster, r), nil
		},
	}
}

// BrowseRemote lists the sub-folders of path (empty: the home folder) on the named cluster over the open SSH
// session. It is the one lazy folder source for every picker: the tool above, the TUI and the desktop app call it
// (a tree opens one level at a time). Read only; a path that can't be entered is an error.
func (e *Env) BrowseRemote(ctx context.Context, cluster, path string) (BrowseResult, error) {
	if path != "" && !strings.HasPrefix(path, "/") {
		return BrowseResult{}, errors.New("path must be an absolute folder (starting with /), or empty for the home folder")
	}
	if len(path) > maxBrowsePath || strings.ContainsAny(path, "\x00\n\r") {
		return BrowseResult{}, errors.New("path is not valid")
	}
	t := &toolset{env: e}
	c, err := t.cluster(cluster)
	if err != nil {
		return BrowseResult{}, err
	}
	if e.LoginRunner == nil {
		return BrowseResult{}, errors.New("Looking at the folders of a cluster isn't available right now.")
	}
	r, err := e.LoginRunner(ctx, c)
	if err != nil {
		return BrowseResult{}, fmt.Errorf("I couldn't connect to %s: %w", c.Name, err)
	}
	res, err := r.Run(ctx, []string{"sh", "-c", browseScript, "sh", path}, nil)
	if err != nil {
		return BrowseResult{}, fmt.Errorf("I couldn't look at the folder on %s: %w", c.Name, err)
	}
	if res.ExitCode == 3 || !strings.Contains(res.Stdout, "@@PWD") {
		return BrowseResult{}, fmt.Errorf("the folder %s can't be opened on %s (it does not exist or the user may not enter it)", sanitize(path), c.Name)
	}
	out := parseBrowse(res.Stdout)
	for i, d := range out.Dirs {
		out.Dirs[i] = sanitize(d)
	}
	out.Path = sanitize(out.Path)
	return out, nil
}
