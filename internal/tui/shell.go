package tui

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// shellReadyMsg carries the connected shell command; shellFailedMsg the
// reason it couldn't be opened; shellDoneMsg arrives when the shell exits and
// the TUI is back.
type (
	shellReadyMsg struct {
		cluster string
		cmd     tea.ExecCommand
	}
	shellFailedMsg struct {
		cluster string
		err     error
	}
	shellDoneMsg struct {
		cluster string
		err     error
	}
)

// shellCmd recognizes "/shell" with any arguments: it is always the command,
// never a chat message. Arguments are "<cluster>" and "--dir <folder>" in
// either order (quotes group a folder with spaces). err says what is wrong
// with the arguments.
func shellCmd(text string) (cluster, dir string, ok bool, err error) {
	f := splitArgs(text)
	if len(f) == 0 || f[0] != "/shell" {
		return "", "", false, nil
	}
	if strings.ContainsAny(text, "\n\r\x00") {
		return "", "", true, errors.New("No line breaks please.")
	}
	f = f[1:]
	for i := 0; i < len(f); i++ {
		a := f[i]
		switch {
		case a == "--dir" || a == "-d":
			if i+1 >= len(f) {
				return "", "", true, errors.New("--dir needs a folder.")
			}
			i++
			dir = f[i]
		case strings.HasPrefix(a, "--dir="):
			dir = strings.TrimPrefix(a, "--dir=")
		case strings.HasPrefix(a, "-"):
			return "", "", true, errors.New("Unknown option " + a + ".")
		case cluster != "":
			return "", "", true, errors.New("One cluster at a time.")
		default:
			cluster = a
		}
	}
	if cluster == "" {
		return "", "", true, errors.New("Which cluster?")
	}
	return cluster, dir, true, nil
}

// splitArgs splits on white space; a pair of single or double quotes groups
// words (a folder with spaces).
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	started := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, started = r, true
		case r == ' ' || r == '\t':
			if started || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if started || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// shellCommand runs /shell: it connects in the background (login prompts
// appear as usual) and then the TUI hands the terminal to the shell. It
// never starts a turn and nothing of the shell reaches the model.
func (m *model) shellCommand(cluster, dir string, perr error) tea.Cmd {
	switch {
	case perr != nil:
		m.addEntry(roleError, perr.Error()+" Use /shell <cluster> or /shell <cluster> --dir <folder>.")
		return nil
	case m.o.Shell == nil:
		m.addEntry(roleError, "A shell can't be opened here.")
		return nil
	case m.running:
		m.addEntry(roleError, "Wait until the current step is finished (or press Ctrl+C), then open the shell.")
		return nil
	case m.shellBusy:
		return nil
	}
	m.shellBusy = true
	m.addEntry(roleNotice, "Opening a shell on "+cluster+". Type exit to come back to Labmill.")
	open := m.o.Shell
	return func() tea.Msg {
		c, err := open(context.Background(), cluster, dir)
		if err != nil {
			return shellFailedMsg{cluster: cluster, err: err}
		}
		return shellReadyMsg{cluster: cluster, cmd: c}
	}
}
