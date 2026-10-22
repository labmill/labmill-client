package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"labmill-client/internal/agent"
)

const (
	maxSummaryLines = 3
	minDetailLines  = 2
)

var (
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	btnOn       = lipgloss.NewStyle().Reverse(true).Bold(true)
	btnOff      = lipgloss.NewStyle().Faint(true)
	summaryBold = lipgloss.NewStyle().Bold(true)
)

// approvalState is the approval modal. It is not a Huh form: the layout has
// to guarantee that Summary, buttons and key hint stay on screen while
// Details scrolls, which needs full control of the rows.
type approvalState struct {
	editable bool
	original string // Details exactly as the proposal carried it
	initial  string // the text the editor showed at the start
	warn     string // set when initial differs from original

	ta textarea.Model // editable Details
	vp viewport.Model // read-only Details

	hasDetails bool // read-only mode: there is something to scroll
	showVP     bool // read-only mode: the viewport is on screen
	approve    bool // selected button; false (Decline) is the default
	inText     bool // editable: focus is in the text area, not the buttons

	// The optional reason prompt shown after the user chose Decline.
	asking bool
	ri     textinput.Model

	// layout results
	head string // rendered Summary block
	hint string // rendered key hint
	fits bool   // false: the window is too small to show everything
}

func newApprovalState(p agent.Proposal, dark bool) *approvalState {
	ap := &approvalState{editable: p.Editable, original: p.Details}
	details := sanitize(p.Details)
	if p.Editable {
		ta := textarea.New()
		ta.SetStyles(textarea.DefaultStyles(dark))
		ta.ShowLineNumbers = false
		ta.Prompt = ""
		ta.CharLimit = 0
		ta.MaxHeight = 0 // unlimited
		ta.MaxWidth = 0
		ta.MaxContentHeight = 0
		ta.SetValue(details)
		ta.MoveToBegin()
		ap.initial = ta.Value()
		if ap.initial != p.Details {
			ap.warn = "warning: the editor shows tabs as spaces and hides control or invisible formatting characters. " +
				"If you change the text, the text as shown (not the original) is submitted."
		}
		ap.ta = ta
		ap.inText = true
	} else {
		ap.vp = viewport.New()
		ap.vp.SoftWrap = false
		ap.hasDetails = strings.TrimSpace(details) != ""
	}
	return ap
}

// wrapLines wraps s to width w and returns its lines.
func wrapLines(s string, w int) []string {
	return strings.Split(strings.TrimRight(lipgloss.Wrap(s, max(w, 1), ""), "\n"), "\n")
}

// layoutApproval sizes the open approval modal for the current window.
func (m *model) layoutApproval() {
	md := m.modal
	if md == nil || md.ap == nil {
		return
	}
	ap := md.ap
	p := md.approval.Proposal
	iw := m.innerWidth()
	rows := max(m.height-1-2, 0) // minus status bar and box border

	sum := wrapLines(sanitize(p.Summary), iw)
	truncated := false
	if len(sum) > maxSummaryLines {
		sum = sum[:maxSummaryLines]
		sum[maxSummaryLines-1] = lipgloss.NewStyle().MaxWidth(max(iw-2, 1)).Render(sum[maxSummaryLines-1]) + " …"
		truncated = true
	}
	ap.head = summaryBold.Render(strings.Join(sum, "\n"))

	fixed := len(sum) + 1 // summary + buttons
	if ap.asking {
		fixed++ // the "Why?" label above the input (replaces the buttons row)
	}
	if ap.warn != "" {
		fixed += len(wrapLines(ap.warn, iw))
	}
	showDetails := ap.editable || ap.hasDetails || truncated
	ap.showVP = showDetails && !ap.editable
	if showDetails {
		fixed++ // indicator line
	}
	var content string
	if ap.showVP {
		content = sanitize(p.Details)
		if truncated {
			content = "Full summary:\n" + sanitize(p.Summary) + "\n\n" + content
		}
	}
	// The hint mentions PgUp/PgDn only if the details really scroll; judge
	// that with the longer hint, which leaves the least room.
	longHint := dimStyle.Render(strings.Join(wrapLines(m.approvalHintText(true), iw), "\n"))
	scrolls := ap.showVP && len(wrapLines(content, iw)) > rows-fixed-lipgloss.Height(longHint)
	ap.hint = dimStyle.Render(strings.Join(wrapLines(m.approvalHintText(scrolls), iw), "\n"))
	fixed += lipgloss.Height(ap.hint)
	dh := rows - fixed
	ap.fits = dh >= minDetailLines || (!showDetails && dh >= 0)
	if !ap.fits {
		return
	}
	if ap.editable {
		ap.ta.SetWidth(iw)
		ap.ta.SetHeight(dh)
		return
	}
	if showDetails {
		ap.vp.SetWidth(iw)
		ap.vp.SetHeight(dh)
		ap.vp.SetContent(strings.Join(wrapLines(content, iw), "\n"))
	}
}

// approvalHintText is the key hint for the current focus; scrolls says the
// read-only details do not fit and can be scrolled.
func (m *model) approvalHintText(scrolls bool) string {
	ap := m.modal.ap
	const tail = " · Ctrl+C cancel turn · Ctrl+D quit"
	switch {
	case ap.asking:
		return "Enter send · Esc skip" + tail
	case ap.editable && ap.inText:
		return "Tab to buttons · Esc decline" + tail
	case ap.editable:
		return "←/→ choose · Enter confirm · Tab back to editing · Esc decline" + tail
	case scrolls:
		return "←/→ choose · Enter confirm · PgUp/PgDn scroll · Esc decline" + tail
	}
	return "←/→ choose · Enter confirm · Esc decline" + tail
}

// approvalBlocked reports whether the modal cannot be shown completely; then
// only Esc (and the quit keys) work.
func (m *model) approvalBlocked() bool {
	md := m.modal
	return md != nil && md.ap != nil && (m.tooSmall() || !md.ap.fits)
}

const enlargeNote = "Enlarge the window to review this request. Esc declines."

func (m *model) approvalView() string {
	ap := m.modal.ap
	if m.approvalBlocked() {
		return warnStyle.Render(enlargeNote)
	}
	iw := m.innerWidth()
	parts := []string{ap.head}
	if ap.warn != "" {
		parts = append(parts, warnStyle.Render(strings.Join(wrapLines(ap.warn, iw), "\n")))
	}
	switch {
	case ap.editable:
		parts = append(parts, ap.ta.View(),
			dimStyle.Render(fmt.Sprintf("line %d of %d", ap.ta.Line()+1, ap.ta.LineCount())))
	case ap.showVP:
		first := ap.vp.YOffset() + 1
		last := first + ap.vp.VisibleLineCount() - 1
		parts = append(parts, ap.vp.View(),
			dimStyle.Render(fmt.Sprintf("lines %d-%d of %d", first, last, ap.vp.TotalLineCount())))
	}
	if ap.asking {
		parts = append(parts, summaryBold.Render(reasonLabel), ap.ri.View(), ap.hint)
		return strings.Join(parts, "\n")
	}
	approve, decline := " Approve ", " Decline "
	if ap.approve {
		approve = btnOn.Render(approve)
		decline = btnOff.Render(decline)
	} else {
		approve = btnOff.Render(approve)
		decline = btnOn.Render(decline)
	}
	if ap.editable && ap.inText {
		approve, decline = btnOff.Render(" Approve "), btnOff.Render(" Decline ")
	}
	parts = append(parts, "["+approve+"]  ["+decline+"]", ap.hint)
	return strings.Join(parts, "\n")
}

// approvalKey handles a key press in the approval modal.
func (m *model) approvalKey(msg tea.KeyPressMsg) tea.Cmd {
	ap := m.modal.ap
	s := msg.String()
	if ap.asking {
		if s == "enter" {
			return m.sendDecline(ap.ri.Value())
		}
		var cmd tea.Cmd
		ap.ri, cmd = ap.ri.Update(msg)
		return cmd
	}
	if ap.editable && ap.inText {
		if s == "tab" {
			ap.inText = false
			ap.ta.Blur()
			m.layoutApproval() // the hint changes
			return nil
		}
		var cmd tea.Cmd
		ap.ta, cmd = ap.ta.Update(msg)
		return cmd
	}
	switch s {
	case "left", "right":
		ap.approve = !ap.approve
	case "tab", "shift+tab":
		if ap.editable {
			ap.inText = true
			m.layoutApproval()
			return ap.ta.Focus()
		}
		ap.approve = !ap.approve
	case "enter":
		if !ap.approve {
			return m.askReason()
		}
		return m.finishApproval(true)
	case "up":
		ap.vp.ScrollUp(1)
	case "down":
		ap.vp.ScrollDown(1)
	case "pgup":
		ap.vp.PageUp()
	case "pgdown":
		ap.vp.PageDown()
	case "home":
		ap.vp.GotoTop()
	case "end":
		ap.vp.GotoBottom()
	}
	return nil
}

const (
	reasonLabel    = "Why? (optional — Enter to send, Esc to skip)"
	maxReasonRunes = 500
)

// askReason opens the optional one-line reason prompt after Decline was
// chosen. Esc (or the turn ending) answers the decline with the usual reason
// ("declined in the dialog", "the turn was canceled"); a typed but unsent
// reason is dropped.
func (m *model) askReason() tea.Cmd {
	ap := m.modal.ap
	ri := textinput.New()
	ri.Prompt = "> "
	ri.CharLimit = maxReasonRunes
	ri.SetWidth(max(m.innerWidth()-2, 1))
	ap.ri = ri
	ap.asking = true
	ap.ta.Blur()
	m.layoutApproval()
	return ap.ri.Focus()
}

// cleanReason makes a typed reason a safe single line of at most
// maxReasonRunes runes.
func cleanReason(s string) string {
	s = strings.Join(strings.Fields(sanitize(strings.TrimSpace(s))), " ")
	if r := []rune(s); len(r) > maxReasonRunes {
		s = strings.TrimSpace(string(r[:maxReasonRunes]))
	}
	return s
}

// sendDecline answers the open approval as declined with the typed reason.
func (m *model) sendDecline(reason string) tea.Cmd {
	r := cleanReason(reason)
	if r == "" {
		r = "declined in the dialog"
	}
	m.modal.reply(agent.Decision{Approved: false, Reason: r})
	return m.closeModal()
}

// finishApproval sends the decision for the open approval modal.
func (m *model) finishApproval(approved bool) tea.Cmd {
	md := m.modal
	ap := md.ap
	d := agent.Decision{Approved: approved}
	if approved {
		if ap.editable && ap.ta.Value() != ap.initial {
			d.Edited = ap.ta.Value()
		}
	} else {
		d.Reason = "declined in the dialog"
	}
	md.reply(d)
	return m.closeModal()
}
