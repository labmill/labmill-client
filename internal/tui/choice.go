package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/agent"
)

// maxOtherRunes caps a typed ("Other") answer in the choice modal.
const maxOtherRunes = 500

// choiceState is the selection modal for ask_user. The rows are the options
// and, when the question allows it, a last "Other" row with a one-line
// input. Unlike an approval, the first option is selected at first: picking
// an answer changes nothing by itself. The typed-ahead grace of every modal
// still applies (handleKey), so a key meant for the chat can't pick.
type choiceState struct {
	q      agent.Choice // the question with all text sanitized
	cursor int          // selected row: 0..len(Options)-1, or len(Options) for Other
	other  textinput.Model
}

func newChoiceState(q agent.Choice) *choiceState {
	c := agent.Choice{Question: strings.TrimSpace(sanitize(q.Question)), AllowOther: q.AllowOther}
	for _, o := range q.Options {
		c.Options = append(c.Options, agent.ChoiceOption{Label: oneLine(o.Label), Description: oneLine(o.Description)})
	}
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "type your own answer, then Enter"
	ti.CharLimit = maxOtherRunes
	return &choiceState{q: c, other: ti}
}

// rows is the number of selectable rows.
func (cs *choiceState) rows() int {
	n := len(cs.q.Options)
	if cs.q.AllowOther {
		n++
	}
	return n
}

// onOther reports whether the Other row is selected.
func (cs *choiceState) onOther() bool { return cs.q.AllowOther && cs.cursor == len(cs.q.Options) }

// move selects the row d steps away (wrapping) and focuses the Other input
// when it is reached.
func (cs *choiceState) move(d int) tea.Cmd {
	n := cs.rows()
	if n == 0 {
		return nil
	}
	cs.cursor = ((cs.cursor+d)%n + n) % n
	if cs.onOther() {
		return cs.other.Focus()
	}
	cs.other.Blur()
	return nil
}

func (m *model) openChoice(req ChoiceRequestMsg) tea.Cmd {
	cs := newChoiceState(req.Choice)
	m.modal = &modal{src: req, choice: &req, cs: cs, openedAt: m.now()}
	m.input.Blur()
	m.layoutChoice()
	return nil
}

// layoutChoice sizes the Other input for the current window.
func (m *model) layoutChoice() {
	if m.modal == nil || m.modal.cs == nil {
		return
	}
	m.modal.cs.other.SetWidth(max(m.innerWidth()-12, 1))
}

func (m *model) choiceHintText() string {
	const tail = " · Ctrl+C cancel turn · Ctrl+D quit"
	cs := m.modal.cs
	if cs.onOther() {
		return "type an answer · Enter send · ↑/↓ choose · Esc skip" + tail
	}
	return fmt.Sprintf("↑/↓ choose · Enter pick · 1-%d pick · Esc skip", len(cs.q.Options)) + tail
}

// choiceLines renders the modal body. allDesc shows every description (else
// only the selected option's); qMax and dMax cap the question's and the
// selected description's lines (0 means no cap).
func (m *model) choiceLines(allDesc bool, qMax, dMax int) []string {
	cs := m.modal.cs
	iw := m.innerWidth()
	q := wrapLines(cs.q.Question, iw)
	if qMax > 0 && len(q) > qMax {
		q = append(q[:qMax-1:qMax-1], clipLine(q[qMax-1], iw))
	}
	lines := []string{summaryBold.Render(strings.Join(q, "\n")), ""}
	const indent = "     "
	for i, o := range cs.q.Options {
		sel := i == cs.cursor
		mark := "  "
		if sel {
			mark = "› "
		}
		label := wrapLines(o.Label, max(iw-len(indent), 1))
		num := fmt.Sprintf("%d. ", i+1)
		for j, l := range label {
			pre := mark + num
			if j > 0 {
				pre = indent
			}
			if sel {
				l = btnOn.Render(l)
			}
			lines = append(lines, pre+l)
		}
		if o.Description == "" || (!allDesc && !sel) {
			continue
		}
		d := wrapLines(o.Description, max(iw-len(indent), 1))
		switch {
		case sel && dMax < 0:
			d = nil
		case sel && dMax > 0 && len(d) > dMax:
			d = append(d[:dMax-1:dMax-1], clipLine(d[dMax-1], iw-len(indent)))
		}
		for _, l := range d {
			lines = append(lines, indent+dimStyle.Render(l))
		}
	}
	if cs.q.AllowOther {
		if cs.onOther() {
			lines = append(lines, "› Other: "+cs.other.View())
		} else {
			lines = append(lines, "  "+dimStyle.Render("Other… (type your own answer)"))
		}
	}
	hint := wrapLines(m.choiceHintText(), iw)
	lines = append(lines, "", dimStyle.Render(strings.Join(hint, "\n")))
	return strings.Split(strings.Join(lines, "\n"), "\n")
}

// clipLine marks a cut line with an ellipsis.
func clipLine(s string, w int) string {
	r := []rune(s)
	if len(r) > max(w-2, 1) {
		r = r[:max(w-2, 1)]
	}
	return strings.TrimRight(string(r), " ") + " …"
}

// choiceView renders the modal so that every option and the key hint stay
// on screen: when everything doesn't fit, only the selected option keeps its
// description, then the question is cut to three lines, then the selected
// description is cut to the room left.
func (m *model) choiceView() string {
	rows := max(m.height-1-2, 0) // minus status bar and box border
	for _, try := range []struct {
		all        bool
		qMax, dMax int
	}{{true, 0, 0}, {false, 0, 0}, {false, maxSummaryLines, 0}} {
		if l := m.choiceLines(try.all, try.qMax, try.dMax); len(l) <= rows {
			return strings.Join(l, "\n")
		}
	}
	base := len(m.choiceLines(false, maxSummaryLines, -1))
	room := rows - base
	if room <= 0 {
		return strings.Join(m.choiceLines(false, maxSummaryLines, -1), "\n")
	}
	return strings.Join(m.choiceLines(false, maxSummaryLines, room), "\n")
}

// choiceKey handles a key press in the choice modal (after the grace
// period; Esc, Ctrl+C and Ctrl+D are handled by handleKey).
func (m *model) choiceKey(msg tea.KeyPressMsg) tea.Cmd {
	cs := m.modal.cs
	s := msg.String()
	switch s {
	case "up", "shift+tab":
		return cs.move(-1)
	case "down", "tab":
		return cs.move(1)
	case "enter":
		if cs.onOther() {
			text := cleanReason(cs.other.Value())
			if text == "" {
				return nil
			}
			if r := []rune(text); len(r) > maxOtherRunes {
				text = string(r[:maxOtherRunes])
			}
			return m.finishChoice(agent.ChoiceAnswer{Index: -1, Other: text})
		}
		return m.finishChoice(agent.ChoiceAnswer{Index: cs.cursor})
	}
	if cs.onOther() {
		var cmd tea.Cmd
		cs.other, cmd = cs.other.Update(msg)
		return cmd
	}
	if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		if i := int(s[0] - '1'); i < len(cs.q.Options) {
			return m.finishChoice(agent.ChoiceAnswer{Index: i})
		}
	}
	return nil
}

// finishChoice sends the answer for the open choice modal.
func (m *model) finishChoice(a agent.ChoiceAnswer) tea.Cmd {
	m.modal.replyChoice(a)
	return m.closeModal()
}
