package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestBrowseCommand(t *testing.T) {
	var asked []string
	tree := map[string][]string{"": {"a", "b"}, "/home/ann": {"a", "b"}, "/home/ann/b": {"deep"}}
	s := &countSession{}
	m := newModel(Options{Session: s, Clusters: func() []string { return []string{"bigcluster"} },
		Browse: func(_ context.Context, c, p string) (string, []string, error) {
			asked = append(asked, c+":"+p)
			if p == "" {
				p = "/home/ann"
			}
			return p, tree[p], nil
		}})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 140, Height: 30})

	enter(t, m, "/browse")
	if e := lastEntry(m); !strings.Contains(e.text, "/home/ann on bigcluster") || !strings.Contains(e.text, "  2  b/") {
		t.Fatalf("list: %+v", e)
	}
	enter(t, m, "/browse 2")
	if e := lastEntry(m); !strings.Contains(e.text, "/home/ann/b on bigcluster") || !strings.Contains(e.text, "deep/") {
		t.Fatalf("open: %+v", e)
	}
	enter(t, m, "/browse ..")
	if e := lastEntry(m); !strings.HasPrefix(e.text, "/home/ann on") {
		t.Fatalf("up: %+v", e)
	}
	enter(t, m, "/browse 1")
	enter(t, m, "/browse use")
	if got := m.input.Value(); got != "Use the folder /home/ann/a on bigcluster." {
		t.Errorf("input = %q", got)
	}
	if len(s.texts) != 0 {
		t.Errorf("nothing is sent: %v", s.texts)
	}
	enter(t, m, "/browse 9")
	if e := lastEntry(m); e.role != roleError {
		t.Errorf("bad number: %+v", e)
	}
}
