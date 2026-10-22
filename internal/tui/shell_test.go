package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/sshx"
)

func TestShellCmdParse(t *testing.T) {
	for _, c := range []struct {
		in           string
		cluster, dir string
		ok, bad      bool
	}{
		{"/shell hpc", "hpc", "", true, false},
		{"/shell hpc --dir /data/x", "hpc", "/data/x", true, false},
		{"/shell --dir=/data/x hpc", "hpc", "/data/x", true, false},
		{`/shell hpc --dir "/data/my group"`, "hpc", "/data/my group", true, false},
		{"/shell", "", "", true, true},
		{"/shell hpc --dir \"/x\n/y\"", "", "", true, true},
		{"/shell hpc --dir", "", "", true, true},
		{"/shell a b", "", "", true, true},
		{"/shell -x hpc", "", "", true, true},
		{"/shelly hpc", "", "", false, false},
		{"run /shell hpc", "", "", false, false},
	} {
		cl, dir, ok, err := shellCmd(c.in)
		if ok != c.ok || (err != nil) != c.bad || cl != c.cluster || dir != c.dir {
			t.Errorf("shellCmd(%q) = %q %q %v %v", c.in, cl, dir, ok, err)
		}
	}
}

type fakeExec struct{}

func (fakeExec) Run() error          { return nil }
func (fakeExec) SetStdin(io.Reader)  {}
func (fakeExec) SetStdout(io.Writer) {}
func (fakeExec) SetStderr(io.Writer) {}

// /shell connects through Options.Shell, never starts a turn, then hands the
// command to tea.Exec; the end of the shell is a note in the chat.
func TestShellCommandFlow(t *testing.T) {
	var gotCluster, gotDir string
	sess := &countSession{}
	m := newModel(Options{Session: sess, Shell: func(_ context.Context, c, d string) (tea.ExecCommand, error) {
		gotCluster, gotDir = c, d
		return fakeExec{}, nil
	}})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	enter(t, m, "/shell hpc --dir /data/x")
	if gotCluster != "hpc" || gotDir != "/data/x" || m.running || m.shellBusy {
		t.Fatalf("cluster=%q dir=%q running=%v busy=%v", gotCluster, gotDir, m.running, m.shellBusy)
	}
	if len(sess.texts) != 0 {
		t.Errorf("a chat turn started: %q", sess.texts)
	}
	found := false
	for _, e := range m.entries {
		if strings.Contains(e.text, "Type exit to come back to Labmill") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note before the shell: %+v", m.entries)
	}
	_, cmd := m.Update(shellReadyMsg{cluster: "hpc", cmd: fakeExec{}})
	if cmd == nil {
		t.Fatal("no tea.Exec command")
	}
	step(t, m, shellDoneMsg{cluster: "hpc"})
	if e := lastEntry(m); e.role != roleNotice || !strings.Contains(e.text, "Back from the shell on hpc") {
		t.Errorf("after: %+v", e)
	}
	step(t, m, shellDoneMsg{cluster: "hpc", err: errors.New("boom")})
	if e := lastEntry(m); e.role != roleError || !strings.Contains(e.text, "boom") {
		t.Errorf("after error: %+v", e)
	}
}

func TestShellCommandErrors(t *testing.T) {
	m := newModel(Options{Session: &countSession{}})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	enter(t, m, "/shell hpc")
	if m.running || lastEntry(m).role != roleError || !strings.Contains(lastEntry(m).text, "can't be opened") {
		t.Errorf("no Shell: %+v", lastEntry(m))
	}

	m = newModel(Options{Session: &countSession{}, Shell: func(context.Context, string, string) (tea.ExecCommand, error) {
		return nil, errors.New("no such cluster")
	}})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	enter(t, m, "/shell")
	if lastEntry(m).role != roleError || !strings.Contains(lastEntry(m).text, "Which cluster") {
		t.Errorf("usage: %+v", lastEntry(m))
	}
	enter(t, m, "/shell nope")
	if e := lastEntry(m); e.role != roleError || !strings.Contains(e.text, "no such cluster") || m.shellBusy {
		t.Errorf("failed connect: %+v busy=%v", e, m.shellBusy)
	}
	m.running = true
	enter(t, m, "/shell hpc")
	if e := lastEntry(m); e.role != roleError || !strings.Contains(e.text, "current step") {
		t.Errorf("while a turn runs: %+v", e)
	}
}

// A SIGTERM/SIGHUP that ended the shell quits Labmill too.
func TestShellSignaledQuits(t *testing.T) {
	m := newModel(Options{Session: &countSession{}})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	_, cmd := m.Update(shellDoneMsg{cluster: "hpc", err: &sshx.ErrShellSignaled{Sig: syscall.SIGTERM}})
	if cmd == nil {
		t.Fatal("no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("did not quit")
	}
}
