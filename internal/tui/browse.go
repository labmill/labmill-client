package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// /browse is the Cluster source of the folder picker: a lazy walk through the folders of a cluster over the open
// SSH session, so the user never types a cluster path. /browse [cluster] lists the home folder, /browse <number>
// opens a listed sub-folder, /browse .. goes up, /browse use puts "Use the folder `...` on ..." in the input line.
// The SSH call runs in a command, not in Update, so a sign-in prompt can still show while it waits.

type browseState struct {
	cluster string
	dir     string
	dirs    []string
	seq     int // the request that may still answer; older replies are dropped
}

// browseResultMsg is the answer of one Options.Browse call.
type browseResultMsg struct {
	seq     int
	cluster string
	dir     string
	dirs    []string
	err     error
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

func (m *model) browseCommand(arg string) tea.Cmd {
	if m.o.Browse == nil || m.o.Clusters == nil {
		m.addEntry(roleError, "Browsing cluster folders isn't available here.")
		return nil
	}
	b := &m.browse
	switch {
	case arg == "use":
		if b.cluster == "" || b.dir == "" {
			m.addEntry(roleNotice, "Open a folder first with /browse.")
			return nil
		}
		m.input.SetValue(strings.TrimSpace(m.input.Value() + " Use the folder `" + b.dir + "` on " + b.cluster + "."))
		m.input.CursorEnd()
		m.addEntry(roleNotice, "Folder put in your message: "+b.dir+" on "+b.cluster+". Press Enter to send it.")
		return nil
	case arg == "..":
		if b.cluster == "" {
			m.addEntry(roleNotice, "Open a folder first with /browse.")
			return nil
		}
		return m.browseLoad(b.cluster, parentDir(b.dir))
	}
	if n, err := strconv.Atoi(arg); err == nil {
		if b.cluster == "" || n < 1 || n > len(b.dirs) {
			m.addEntry(roleError, "No such entry. Run /browse to list folders.")
			return nil
		}
		return m.browseLoad(b.cluster, strings.TrimRight(b.dir, "/")+"/"+b.dirs[n-1])
	}
	names := m.o.Clusters()
	cluster := arg
	if cluster == "" {
		switch len(names) {
		case 0:
			m.addEntry(roleNotice, "No cluster is set up yet.")
			return nil
		case 1:
			cluster = names[0]
		default:
			m.addEntry(roleNotice, "Clusters: "+strings.Join(names, ", ")+". Use /browse <cluster>.")
			return nil
		}
	}
	return m.browseLoad(cluster, "")
}

// browseLoad starts one lookup. The folder listed before stays valid until the answer arrives.
func (m *model) browseLoad(cluster, path string) tea.Cmd {
	m.browse.seq++
	seq, browse := m.browse.seq, m.o.Browse
	m.addEntry(roleNotice, "Looking at the folders on "+cluster+"…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		dir, dirs, err := browse(ctx, cluster, path)
		return browseResultMsg{seq: seq, cluster: cluster, dir: dir, dirs: dirs, err: err}
	}
}

// browseResult shows the answer; an older request's answer is dropped.
func (m *model) browseResult(r browseResultMsg) {
	if r.seq != m.browse.seq {
		return
	}
	if r.err != nil {
		m.addEntry(roleError, r.err.Error())
		return
	}
	m.browse.cluster, m.browse.dir, m.browse.dirs = r.cluster, r.dir, r.dirs
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s on %s\n", r.dir, r.cluster)
	for i, d := range r.dirs {
		fmt.Fprintf(&sb, "%3d  %s/\n", i+1, d)
	}
	if len(r.dirs) == 0 {
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
