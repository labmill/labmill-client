package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// /browse is the Cluster source of the folder picker: a lazy walk through the folders of a cluster over the open
// SSH session, so the user never types a cluster path. /browse [cluster] lists the home folder, /browse <number>
// opens a listed sub-folder, /browse .. goes up, /browse use puts "Use the folder ... on ..." in the input line.

type browseState struct {
	cluster string
	dir     string
	dirs    []string
}

func browseCmd(text string) (arg string, ok bool) {
	if text == "/browse" {
		return "", true
	}
	if rest, found := strings.CutPrefix(text, "/browse "); found {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

func (m *model) browseCommand(arg string) {
	if m.o.Browse == nil || m.o.Clusters == nil {
		m.addEntry(roleError, "Browsing cluster folders isn't available here.")
		return
	}
	b := &m.browse
	switch {
	case arg == "use":
		if b.cluster == "" || b.dir == "" {
			m.addEntry(roleNotice, "Open a folder first with /browse.")
			return
		}
		m.input.SetValue(strings.TrimSpace(m.input.Value() + " Use the folder " + b.dir + " on " + b.cluster + "."))
		m.input.CursorEnd()
		m.addEntry(roleNotice, "Folder put in your message: "+b.dir+" on "+b.cluster+". Press Enter to send it.")
		return
	case arg == "..":
		if b.cluster == "" {
			m.addEntry(roleNotice, "Open a folder first with /browse.")
			return
		}
		m.browseLoad(b.cluster, parentDir(b.dir))
		return
	}
	if n, err := strconv.Atoi(arg); err == nil {
		if b.cluster == "" || n < 1 || n > len(b.dirs) {
			m.addEntry(roleError, "No such entry. Run /browse to list folders.")
			return
		}
		m.browseLoad(b.cluster, strings.TrimRight(b.dir, "/")+"/"+b.dirs[n-1])
		return
	}
	names := m.o.Clusters()
	cluster := arg
	if cluster == "" {
		if len(names) == 1 {
			cluster = names[0]
		} else {
			m.addEntry(roleNotice, "Clusters: "+strings.Join(names, ", ")+". Use /browse <cluster>.")
			return
		}
	}
	m.browseLoad(cluster, "")
}

func (m *model) browseLoad(cluster, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir, dirs, err := m.o.Browse(ctx, cluster, path)
	if err != nil {
		m.addEntry(roleError, err.Error())
		return
	}
	m.browse = browseState{cluster: cluster, dir: dir, dirs: dirs}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s on %s\n", dir, cluster)
	for i, d := range dirs {
		fmt.Fprintf(&sb, "%3d  %s/\n", i+1, d)
	}
	if len(dirs) == 0 {
		sb.WriteString("(no sub-folders)\n")
	}
	sb.WriteString("/browse <number> opens a folder, /browse .. goes up, /browse use picks this one.")
	m.addEntry(roleNotice, sb.String())
}

func parentDir(dir string) string {
	d := strings.TrimRight(dir, "/")
	if i := strings.LastIndex(d, "/"); i > 0 {
		return d[:i]
	}
	return "/"
}
