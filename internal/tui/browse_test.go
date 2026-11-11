package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// enterWait presses Enter on text and, unlike enter, waits for the command that the line starts.
func enterWait(t *testing.T, m *model, text string) {
	t.Helper()
	m.input.SetValue(text)
	_, cmd := m.Update(kp(tea.KeyEnter))
	if cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
}

func newBrowseModel(t *testing.T, names []string, browse func(context.Context, string, string) (string, []string, error)) (*model, *countSession) {
	t.Helper()
	s := &countSession{}
	m := newModel(Options{Session: s, Clusters: func() []string { return names }, Browse: browse})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 140, Height: 30})
	return m, s
}

func TestBrowseCommand(t *testing.T) {
	tree := map[string][]string{"/home/ann": {"a", "b"}, "/home/ann/b": {"deep"}}
	m, s := newBrowseModel(t, []string{"bigcluster"}, func(_ context.Context, c, p string) (string, []string, error) {
		if p == "" {
			p = "/home/ann"
		}
		return p, tree[p], nil
	})
	enterWait(t, m, "/browse")
	if e := lastEntry(m); !strings.Contains(e.text, "/home/ann on bigcluster") || !strings.Contains(e.text, "  2  b/") {
		t.Fatalf("list: %+v", e)
	}
	enterWait(t, m, "/browse 2")
	if e := lastEntry(m); !strings.Contains(e.text, "/home/ann/b on bigcluster") || !strings.Contains(e.text, "deep/") {
		t.Fatalf("open: %+v", e)
	}
	enterWait(t, m, "/browse ..")
	if e := lastEntry(m); !strings.HasPrefix(e.text, "/home/ann on") {
		t.Fatalf("up: %+v", e)
	}
	enterWait(t, m, "/browse 1")
	enterWait(t, m, "/browse use")
	if got := m.input.Value(); got != "Use the folder `/home/ann/a` on bigcluster." {
		t.Errorf("input = %q", got)
	}
	if len(s.texts) != 0 {
		t.Errorf("nothing is sent: %v", s.texts)
	}
	enterWait(t, m, "/browse 9")
	if e := lastEntry(m); e.role != roleError {
		t.Errorf("bad number: %+v", e)
	}
}

func TestBrowseNoCluster(t *testing.T) {
	m, _ := newBrowseModel(t, nil, func(context.Context, string, string) (string, []string, error) { return "", nil, nil })
	enterWait(t, m, "/browse")
	if e := lastEntry(m); !strings.Contains(e.text, "No cluster is set up yet.") {
		t.Errorf("zero clusters: %+v", e)
	}
}

// The SSH call must not run inside Update: while Browse waits for a sign-in answer, Update returns at once.
func TestBrowseDoesNotBlockUpdate(t *testing.T) {
	release := make(chan struct{})
	m, _ := newBrowseModel(t, []string{"bigcluster"}, func(context.Context, string, string) (string, []string, error) {
		<-release
		return "/home/ann", []string{"a"}, nil
	})
	m.input.SetValue("/browse")
	done := make(chan tea.Cmd, 1)
	go func() { _, cmd := m.Update(kp(tea.KeyEnter)); done <- cmd }()
	var cmd tea.Cmd
	select {
	case cmd = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Update blocked on the SSH call")
	}
	if cmd == nil {
		t.Fatal("no command returned")
	}
	// A prompt can show while the call waits.
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	close(release)
	m.Update(cmd())
	if e := lastEntry(m); !strings.Contains(e.text, "/home/ann on bigcluster") {
		t.Errorf("answer: %+v", e)
	}
}

func TestBrowseStaleReplyDropped(t *testing.T) {
	m, _ := newBrowseModel(t, []string{"c"}, func(context.Context, string, string) (string, []string, error) { return "/x", nil, nil })
	m.browse.seq = 5
	m.Update(browseResultMsg{seq: 4, cluster: "c", dir: "/old"})
	if m.browse.dir == "/old" {
		t.Error("stale reply was used")
	}
}
