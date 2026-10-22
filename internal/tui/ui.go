package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
)

const (
	minWidth, minHeight = 60, 15
	jobsPanelWidth      = 44
	jobsRefresh         = 5 * time.Second
	inputHeight         = 3
	flushInterval       = 100 * time.Millisecond // streaming re-render throttle (10 Hz)
)

var errCanceled = errors.New("canceled")

type role int

const (
	roleUser role = iota
	roleAssistant
	roleTool
	roleError
	roleNotice // a background job notice
)

// entry is one chat item. text is raw (unsanitized); rendered caches the
// display form for the width and style version it was rendered at.
type entry struct {
	role role
	text string

	rendered string
	rw       int
	rver     int
	valid    bool
}

type jobsMsg struct {
	jobs []store.Job
	err  error
}
type jobsTickMsg struct{}
type flushMsg struct{}

var (
	dimStyle    = lipgloss.NewStyle().Faint(true)
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	noticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	statusStyle = lipgloss.NewStyle().Reverse(true).Padding(0, 1)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

// modal is the single open dialog (approval, choice or login).
type modal struct {
	src      tea.Msg // the request being shown
	approval *ApprovalRequestMsg
	ap       *approvalState

	choice *ChoiceRequestMsg
	cs     *choiceState

	prompt  *PromptRequestMsg
	form    *huh.Form
	ok      *bool
	text    *string
	hostKey bool
	// pubKey is the public key line of an SSH key upload question
	// (sshx.AskKeyUpload); keyCopied says Ctrl+Y put it on the clipboard.
	pubKey    string
	keyCopied bool
	// copyKey marks the question to copy a private key to this computer
	// (sshx.AskKeyCopy): its prompt is drawn above the confirm (keyCopyView).
	copyKey bool

	answered bool // exactly-one-reply guard

	openedAt time.Time // when the modal appeared (typed-ahead keys are dropped for a while)
}

// keyGrace is how long a newly opened modal ignores keys other than Esc,
// Ctrl+C and Ctrl+D, so text typed ahead cannot answer a dialog nobody saw.
const keyGrace = 500 * time.Millisecond

// reply answers an approval request (once).
func (md *modal) reply(d agent.Decision) {
	if md.answered || md.approval == nil {
		return
	}
	md.answered = true
	select {
	case md.approval.Reply <- d:
	default:
	}
}

// replyChoice answers a choice request (once).
func (md *modal) replyChoice(a agent.ChoiceAnswer) {
	if md.answered || md.choice == nil {
		return
	}
	md.answered = true
	select {
	case md.choice.Reply <- a:
	default:
	}
}

// replyPrompt answers a login prompt (once).
func (md *modal) replyPrompt(a PromptAnswer) {
	if md.answered || md.prompt == nil {
		return
	}
	md.answered = true
	select {
	case md.prompt.Reply <- a:
	default:
	}
}

type model struct {
	o      Options
	bridge *Bridge

	width, height int

	entries  []entry
	vp       viewport.Model
	input    textarea.Model
	spin     spinner.Model
	renderer *glamour.TermRenderer
	rwidth   int
	rdark    bool
	dark     bool
	styleVer int
	bg       *tea.BackgroundColorMsg

	flushPending bool
	streaming    bool
	running      bool
	shellBusy    bool // a /shell is connecting
	canceling    bool
	cancel       context.CancelFunc
	notice       string
	step         string     // the running turn's current step (agent.EventStep, "Running <tool>…"); "" while the reply streams
	turnStart    time.Time  // when the running turn started (the elapsed time after stepElapsedAfter)
	xfer         string     // live transfer progress line (status bar); "" when none
	queued       string     // the message waiting for the running turn ("" = none)
	todos        []app.Todo // the visible to-do list (update_todos); per session, in memory
	todoStale    int        // turns that ended without an update_todos call
	todoUpdated  bool       // update_todos was called during this turn

	dirty bool // chat content changed; refresh once per batch

	now func() time.Time // clock; tests replace it
	// autoOn is the cached "Approve everything automatically" setting, read
	// at autoAt (see autoApproveOn).
	autoOn bool
	autoAt time.Time

	modal *modal
	queue []tea.Msg // requests waiting for the open modal

	showJobs bool
	jobs     []store.Job
	jobsErr  error
}

func newModel(o Options) *model {
	m := &model{o: o, bridge: o.Bridge, dark: true, now: time.Now}
	m.vp = viewport.New()
	m.vp.SoftWrap = true
	m.input = textarea.New()
	m.input.ShowLineNumbers = false
	m.input.Prompt = "> "
	m.input.Placeholder = "Describe what you want to run…"
	m.input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	m.input.SetHeight(inputHeight)
	m.input.Focus()
	m.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
	if strings.TrimSpace(o.Welcome) != "" {
		m.entries = append(m.entries, entry{role: roleAssistant, text: o.Welcome})
	}
	return m
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, tea.RequestBackgroundColor}
	if m.bridge != nil {
		cmds = append(cmds, m.bridge.Next())
	}
	if m.o.Jobs != nil {
		cmds = append(cmds, m.fetchJobs(), tickJobs())
	}
	return tea.Batch(cmds...)
}

func tickJobs() tea.Cmd {
	return tea.Tick(jobsRefresh, func(time.Time) tea.Msg { return jobsTickMsg{} })
}

func tickFlush() tea.Cmd {
	return tea.Tick(flushInterval, func(time.Time) tea.Msg { return flushMsg{} })
}

func (m *model) fetchJobs() tea.Cmd {
	lister := m.o.Jobs
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		jobs, err := lister(ctx)
		return jobsMsg{jobs: jobs, err: err}
	}
}

func (m *model) listen() tea.Cmd {
	if m.bridge == nil {
		return nil
	}
	return m.bridge.nextBatch()
}

// ---- layout ----

func (m *model) tooSmall() bool { return m.width < minWidth || m.height < minHeight }

// innerWidth is the usable width inside the modal box (border + padding).
func (m *model) innerWidth() int { return max(m.width-4, 10) }

func (m *model) chatWidth() int {
	w := m.width
	if m.showJobs && m.o.Jobs != nil {
		w -= jobsPanelWidth + 1
	}
	return max(w, 20)
}

func (m *model) layout() {
	cw := m.chatWidth()
	m.vp.SetWidth(cw)
	m.vp.SetHeight(max(m.height-inputHeight-1-m.queuedRows()-m.todoRows(), 1))
	m.input.SetWidth(cw)
	if m.renderer == nil || m.rwidth != cw || m.rdark != m.dark {
		w := max(cw-4, 20)
		style := "dark"
		if !m.dark {
			style = "light"
		}
		if r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(w)); err == nil {
			m.renderer = r
			m.rwidth = cw
			m.rdark = m.dark
		}
	}
	m.layoutApproval()
	m.layoutChoice()
	m.refresh()
}

// refresh re-renders stale chat entries (cached ones are reused) into the
// viewport.
func (m *model) refresh() {
	atBottom := m.vp.AtBottom()
	parts := make([]string, len(m.entries))
	for i := range m.entries {
		parts[i] = m.renderEntry(i)
	}
	m.vp.SetContent(strings.Join(parts, "\n"))
	if atBottom {
		m.vp.GotoBottom()
	}
}

func (m *model) renderEntry(i int) string {
	e := &m.entries[i]
	w := m.chatWidth()
	if e.valid && e.rw == w && e.rver == m.styleVer {
		return e.rendered
	}
	text := sanitize(e.text)
	var out string
	switch e.role {
	case roleUser:
		out = userStyle.Render("you") + "\n" + lipgloss.NewStyle().Width(w).Render(text)
	case roleAssistant:
		out = m.renderMarkdown(text)
	case roleTool:
		out = dimStyle.Width(w).Render("  " + text)
	case roleError:
		out = errStyle.Width(w).Render("  " + text)
	case roleNotice:
		out = noticeStyle.Width(w).Render("• " + text)
	}
	e.rendered, e.rw, e.rver, e.valid = out, w, m.styleVer, true
	return out
}

func (m *model) renderMarkdown(s string) string {
	if m.renderer == nil {
		return s
	}
	out, err := m.renderer.Render(s)
	if err != nil {
		return s
	}
	return strings.Trim(out, "\n")
}

func (m *model) addEntry(r role, text string) {
	m.entries = append(m.entries, entry{role: r, text: text})
}

// ---- update ----

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		if m.modal != nil && m.modal.form != nil {
			m.modal.form.WithWidth(m.formWidth())
		}
		return m, nil

	case tea.BackgroundColorMsg:
		m.bg = &msg
		if d := msg.IsDark(); d != m.dark {
			m.dark = d
			m.styleVer++
			m.input.SetStyles(textarea.DefaultStyles(d))
			if m.modal != nil && m.modal.ap != nil && m.modal.ap.editable {
				m.modal.ap.ta.SetStyles(textarea.DefaultStyles(d))
			}
			if m.width > 0 {
				m.layout()
			}
		}
		if m.modal != nil && m.modal.form != nil {
			return m.updateModal(msg)
		}
		return m, nil

	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case shellReadyMsg:
		m.shellBusy = false
		cluster := msg.cluster
		return m, tea.Exec(msg.cmd, func(err error) tea.Msg { return shellDoneMsg{cluster: cluster, err: err} })

	case shellFailedMsg:
		m.shellBusy = false
		m.addEntry(roleError, "Couldn't open a shell on "+msg.cluster+": "+firstLine(oneLine(msg.err.Error())))
		m.refresh()
		return m, nil

	case shellDoneMsg:
		var sig *sshx.ErrShellSignaled
		if errors.As(msg.err, &sig) {
			return m.quit() // Labmill was asked to stop while the shell was open
		}
		if msg.err != nil {
			m.addEntry(roleError, "The shell on "+msg.cluster+" ended with an error: "+firstLine(oneLine(msg.err.Error())))
		} else {
			m.addEntry(roleNotice, "Back from the shell on "+msg.cluster+".")
		}
		m.refresh()
		return m, nil

	case jobsMsg:
		m.jobs, m.jobsErr = msg.jobs, msg.err
		return m, nil

	case jobsTickMsg:
		if m.o.Jobs == nil {
			return m, nil
		}
		return m, tea.Batch(m.fetchJobs(), tickJobs())

	case flushMsg:
		m.flushPending = false
		m.refresh()
		return m, nil

	case bridgeBatch:
		var cmds []tea.Cmd
		for _, bm := range msg {
			cmds = append(cmds, m.handleBridge(bm))
		}
		m.flushDirty()
		cmds = append(cmds, m.listen())
		return m, tea.Batch(cmds...)

	case ApprovalRequestMsg, ChoiceRequestMsg, PromptRequestMsg, NotifyMsg, StatusMsg, TransferMsg, TodosMsg, JobNoticeMsg, AgentEventMsg, TurnDoneMsg:
		cmd := m.handleBridge(msg)
		m.flushDirty()
		return m, tea.Batch(cmd, m.listen())

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	if m.modal != nil {
		return m.updateModal(msg)
	}
	// Anything else (blink, paste, mouse) goes to the input, also while a
	// turn runs (the next message can be typed and queued).
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// flushDirty refreshes the chat once if events changed it.
func (m *model) flushDirty() {
	if m.dirty {
		m.dirty = false
		m.refresh()
	}
}

// handleBridge applies one message that came through the Bridge.
func (m *model) handleBridge(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case ApprovalRequestMsg:
		return m.enqueue(msg)
	case ChoiceRequestMsg:
		return m.enqueue(msg)
	case PromptRequestMsg:
		return m.enqueue(msg)
	case StatusMsg:
		m.o.Status = msg.Text
	case NotifyMsg:
		m.notice = oneLine(msg.Text)
	case TransferMsg:
		if msg.Update.Kind == "end" {
			m.xfer = ""
		} else {
			m.xfer = oneLine(msg.Update.Text)
		}
	case TodosMsg:
		m.setTodos(msg.Todos)
	case JobNoticeMsg:
		return m.jobNotice(msg.Text)
	case AgentEventMsg:
		return m.handleEvent(msg.Event)
	case TurnDoneMsg:
		return m.turnDone(msg)
	}
	return nil
}

// settingsCommand runs /settings: the settings as an assistant entry,
// above a reply that is still streaming.
func (m *model) settingsCommand() {
	if m.o.Settings == nil {
		m.addEntry(roleError, "Settings can't be shown here.")
		return
	}
	e := entry{role: roleAssistant, text: m.o.Settings()}
	if n := len(m.entries); m.streaming && n > 0 && m.entries[n-1].role == roleAssistant {
		m.entries = append(m.entries[:n-1], e, m.entries[n-1])
	} else {
		m.entries = append(m.entries, e)
	}
	m.dirty = true
}

// autoApproveCmd recognizes "/autoapprove" with any arguments: it is always
// the command, never a chat message. arg is "", "on" or "off"; anything else
// comes back as "?" and gets a usage hint.
func autoApproveCmd(text string) (arg string, ok bool) {
	f := strings.Fields(text)
	if len(f) == 0 || f[0] != "/autoapprove" {
		return "", false
	}
	if len(f) == 1 {
		return "", true
	}
	if len(f) == 2 {
		switch strings.ToLower(f[1]) {
		case "on", "off":
			return strings.ToLower(f[1]), true
		}
	}
	return "?", true
}

// autoApproveRefresh is how long the saved setting is trusted before gui.json
// is read again (the desktop app may change it); /autoapprove and the start
// of a turn read it at once.
const autoApproveRefresh = 3 * time.Second

// autoApproveOn reports the setting from a cache, so rendering never reads
// the file for every frame.
func (m *model) autoApproveOn() bool {
	if m.o.AutoApprove == nil {
		return false
	}
	if now := m.now(); m.autoAt.IsZero() || now.Sub(m.autoAt) >= autoApproveRefresh {
		m.autoOn, m.autoAt = m.o.AutoApprove(), now
	}
	return m.autoOn
}

// refreshAutoApprove forgets the cached setting.
func (m *model) refreshAutoApprove() { m.autoAt = time.Time{} }

// AutoApproveBanner is the line shown while "Approve everything
// automatically" is on.
const AutoApproveBanner = "Auto-approve is on: Labmill approves every step for you"

// autoApproveCommand runs /autoapprove: "on" or "off" saves the setting
// (it applies to the next approval), no argument reports it.
func (m *model) autoApproveCommand(arg string) {
	if m.o.AutoApprove == nil || m.o.SetAutoApprove == nil {
		m.addEntry(roleError, "Auto-approve can't be changed here.")
		return
	}
	if arg == "?" {
		m.addEntry(roleError, "Use /autoapprove on or /autoapprove off.")
		return
	}
	switch arg {
	case "on", "off":
		if err := m.o.SetAutoApprove(arg == "on"); err != nil {
			m.addEntry(roleError, "Couldn't save the setting: "+firstLine(oneLine(err.Error())))
			return
		}
	}
	m.refreshAutoApprove()
	if m.autoApproveOn() {
		m.addEntry(roleNotice, AutoApproveBanner+". Turn it off with /autoapprove off. Questions, key or token file reads and logins still ask.")
	} else {
		m.addEntry(roleNotice, "Auto-approve is off: Labmill asks before each step. Turn it on with /autoapprove on.")
	}
}

// autoDownloadCmd recognizes "/autodownload" with any arguments, like
// autoApproveCmd does for /autoapprove (M60.1).
func autoDownloadCmd(text string) (arg string, ok bool) {
	f := strings.Fields(text)
	if len(f) == 0 || f[0] != "/autodownload" {
		return "", false
	}
	if len(f) == 1 {
		return "", true
	}
	if len(f) == 2 {
		switch strings.ToLower(f[1]) {
		case "on", "off":
			return strings.ToLower(f[1]), true
		}
	}
	return "?", true
}

// autoDownloadCommand runs /autodownload: "on" or "off" saves the setting
// (the job monitor reads it when a job ends), no argument reports it.
func (m *model) autoDownloadCommand(arg string) {
	if m.o.AutoDownload == nil || m.o.SetAutoDownload == nil {
		m.addEntry(roleError, "Automatic downloads can't be changed here.")
		return
	}
	if arg == "?" {
		m.addEntry(roleError, "Use /autodownload on or /autodownload off.")
		return
	}
	switch arg {
	case "on", "off":
		if err := m.o.SetAutoDownload(arg == "on"); err != nil {
			m.addEntry(roleError, "Couldn't save the setting: "+firstLine(oneLine(err.Error())))
			return
		}
	}
	if m.o.AutoDownload() {
		m.addEntry(roleNotice, "Download results automatically is on: when a job you submitted finishes, Labmill brings its results (up to 2 GB) to your results folder by itself. Turn it off with /autodownload off.")
	} else {
		m.addEntry(roleNotice, "Download results automatically is off: ask Labmill to fetch the results of a finished job. Turn it on with /autodownload on.")
	}
}

// jobNotice adds a job monitor notice to the chat and refreshes the jobs
// panel. While a reply streams, the notice goes above it so the reply stays
// one piece.
func (m *model) jobNotice(text string) tea.Cmd {
	e := entry{role: roleNotice, text: oneLine(text)}
	if n := len(m.entries); m.streaming && n > 0 && m.entries[n-1].role == roleAssistant {
		m.entries = append(m.entries[:n-1], e, m.entries[n-1])
	} else {
		m.entries = append(m.entries, e)
	}
	m.dirty = true
	if m.o.Jobs != nil {
		return m.fetchJobs()
	}
	return nil
}

// queuedRows is the height of the "queued" line above the input.
func (m *model) queuedRows() int {
	if m.queued != "" {
		return 1
	}
	return 0
}

// setQueued changes the queued message and relayouts (the line above the
// input appears or goes).
func (m *model) setQueued(text string) {
	had := m.queued != ""
	m.queued = text
	if had != (text != "") && m.width > 0 {
		m.layout()
	}
}

// turnDone ends the turn and starts the queued message, if any (also after
// a cancel).
func (m *model) turnDone(msg TurnDoneMsg) tea.Cmd {
	m.todoTurnDone(msg.Err)
	m.running = false
	m.canceling = false
	m.cancel = nil
	if m.modal == nil {
		m.input.Focus()
	}
	switch {
	case msg.Err == nil:
	case errors.Is(msg.Err, context.Canceled):
		m.addEntry(roleTool, "canceled")
	default:
		m.addEntry(roleError, "✗ "+msg.Err.Error())
	}
	// Whatever the turn still had open or queued can no longer be answered
	// usefully; reply to each exactly once.
	m.answerAll("the turn ended")
	m.streaming = false
	m.step = ""
	if next := m.queued; next != "" {
		m.setQueued("")
		return m.start(next)
	}
	m.refresh()
	return nil
}

// handleEvent applies an agent event. Text deltas only append to the raw text
// of the streaming entry; the re-render is deferred to a flush tick, at most
// flushInterval later.
func (m *model) handleEvent(e agent.Event) tea.Cmd {
	switch e.Kind {
	case agent.EventText:
		m.step = ""
		if n := len(m.entries); n > 0 && m.entries[n-1].role == roleAssistant && m.streaming {
			m.entries[n-1].text += e.Text
			m.entries[n-1].valid = false
		} else {
			m.addEntry(roleAssistant, e.Text)
			m.streaming = true
		}
		if m.flushPending {
			return nil
		}
		m.flushPending = true
		return tickFlush()
	case agent.EventStep:
		m.step = oneLine(e.Text)
		return nil // only the status bar changes
	case agent.EventToolCall:
		m.streaming = false
		m.step = agent.StepRunning(oneLine(e.Tool))
		m.addEntry(roleTool, toolNote(e))
	case agent.EventToolResult:
		m.streaming = false
		if e.IsError {
			m.addEntry(roleError, "✗ "+firstLine(sanitize(e.Text)))
		} else {
			m.addEntry(roleTool, "✓ done")
		}
	case agent.EventDeclined:
		m.streaming = false
		m.addEntry(roleTool, "declined")
	case agent.EventNote:
		m.streaming = false
		m.addEntry(roleNotice, oneLine(e.Text))
	case agent.EventAutoApproved:
		m.streaming = false
		m.addEntry(roleNotice, "Auto-approved: "+oneLine(sanitize(e.Text)))
	}
	m.dirty = true
	return nil
}

func toolNote(e agent.Event) string {
	name := strings.ReplaceAll(oneLine(e.Tool), "_", " ")
	if name == "" {
		name = "tool"
	}
	return name + "…"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		s = "failed"
	}
	return s
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	if m.modal != nil {
		switch s {
		case "esc":
			return m, m.dismiss("declined in the dialog")
		case "ctrl+c":
			if m.canceling {
				return m.quit()
			}
			if m.running {
				m.answerAll("the turn was canceled")
				m.interruptTurn()
				return m, nil
			}
			return m, m.dismiss("declined in the dialog")
		case "ctrl+d":
			return m.quit()
		case "ctrl+y":
			if m.modal.pubKey != "" {
				m.modal.keyCopied = true
				return m, tea.SetClipboard(m.modal.pubKey)
			}
		}
		if m.now().Sub(m.modal.openedAt) < keyGrace {
			return m, nil
		}
		if m.approvalBlocked() {
			return m, nil
		}
		if m.modal.ap != nil {
			return m, m.approvalKey(msg)
		}
		if m.modal.cs != nil {
			return m, m.choiceKey(msg)
		}
		return m.updateModal(msg)
	}
	switch s {
	case "ctrl+c":
		if m.running && m.cancel != nil && !m.canceling {
			m.interruptTurn()
			return m, nil
		}
		return m.quit()
	case "ctrl+d":
		return m.quit()
	case "esc":
		// Esc removes the queued message (it is dropped, not moved back
		// into the input).
		if m.queued != "" {
			m.setQueued("")
			m.notice = "queued message removed"
			return m, nil
		}
	case "tab":
		if m.o.Folders != nil {
			if out, matches, ok := completeFolder(m.input.Value(), m.o.Home); ok {
				m.input.SetValue(out)
				m.input.CursorEnd()
				m.notice = ""
				if len(matches) > 0 {
					m.notice = oneLine(strings.Join(matches, "  "))
				}
				return m, nil
			}
		}
		if m.o.Jobs != nil {
			m.showJobs = !m.showJobs
			m.layout()
			if m.showJobs {
				return m, m.fetchJobs()
			}
			return m, nil
		}
	case "pgup", "pgdown":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		if arg, ok := autoApproveCmd(text); ok {
			m.input.Reset()
			m.notice = ""
			m.autoApproveCommand(arg)
			m.refresh()
			m.vp.GotoBottom()
			return m, nil
		}
		if arg, ok := autoDownloadCmd(text); ok {
			m.input.Reset()
			m.notice = ""
			m.autoDownloadCommand(arg)
			m.refresh()
			m.vp.GotoBottom()
			return m, nil
		}
		if cluster, dir, ok, perr := shellCmd(text); ok {
			m.input.Reset()
			m.notice = ""
			cmd := m.shellCommand(cluster, dir, perr)
			m.refresh()
			m.vp.GotoBottom()
			return m, cmd
		}
		if text == "/settings" {
			m.input.Reset()
			m.notice = ""
			m.settingsCommand()
			m.refresh()
			m.vp.GotoBottom()
			return m, nil
		}
		if kind, arg, ok := folderCmd(text); ok {
			m.input.Reset()
			m.notice = ""
			m.folderCommand(kind, arg)
			m.refresh()
			m.vp.GotoBottom()
			return m, nil
		}
		return m, m.send(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// interruptTurn cancels the running turn; a second Ctrl+C then quits.
func (m *model) interruptTurn() {
	m.canceling = true
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *model) quit() (tea.Model, tea.Cmd) {
	m.answerAll("Labmill is quitting")
	if m.cancel != nil {
		m.cancel()
	}
	return m, tea.Quit
}

// send sends the typed text: it starts a turn, or while one runs it queues
// the text (one message; a newer one replaces it), shown above the input.
func (m *model) send(text string) tea.Cmd {
	m.input.Reset()
	if m.running {
		if m.queued != "" && m.queued != text {
			m.notice = "replaced the queued message"
		}
		m.setQueued(text)
		return nil
	}
	return m.start(text)
}

// start runs a turn with text.
func (m *model) start(text string) tea.Cmd {
	m.addEntry(roleUser, text)
	m.streaming = false
	m.notice = ""
	m.step = stepWorking
	m.turnStart = m.now()
	m.refreshAutoApprove()
	m.running = true
	m.canceling = false
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.refresh()
	m.vp.GotoBottom()
	sess := m.o.Session
	run := func() tea.Msg {
		go sess.Send(ctx, text)
		return nil
	}
	return tea.Batch(run, m.spin.Tick)
}

// ---- modals ----

func (m *model) formWidth() int { return max(m.width-6, 20) }

func (m *model) newForm(fields ...huh.Field) *huh.Form {
	km := huh.NewDefaultKeyMap()
	// No single-key answers (y/n) and no h/l toggles: only the arrow keys
	// and Enter answer a Confirm, so typed-ahead text cannot accept.
	km.Confirm.Accept.SetEnabled(false)
	km.Confirm.Reject.SetEnabled(false)
	km.Confirm.Toggle = key.NewBinding(key.WithKeys("right", "left"), key.WithHelp("←/→", "toggle"))
	return huh.NewForm(huh.NewGroup(fields...)).WithKeyMap(km).WithShowHelp(false).WithWidth(m.formWidth())
}

// enqueue shows a request now, or queues it behind the open modal.
func (m *model) enqueue(req tea.Msg) tea.Cmd {
	if m.modal != nil {
		m.queue = append(m.queue, req)
		return nil
	}
	return m.open(req)
}

func (m *model) open(req tea.Msg) tea.Cmd {
	switch r := req.(type) {
	case ApprovalRequestMsg:
		return m.openApproval(r)
	case ChoiceRequestMsg:
		return m.openChoice(r)
	case PromptRequestMsg:
		return m.openPrompt(r)
	}
	return nil
}

// keyView is the SSH key upload's text above its confirm: what to do (from
// the site profile), the public key and the Ctrl+Y hint.
func (m *model) keyView() string {
	w := max(m.width-6, 20)
	lines := []string{
		lipgloss.NewStyle().Bold(true).Render("Register your SSH key with " + sanitize(m.modal.prompt.Question.Host)),
		lipgloss.NewStyle().Width(w).Render(sanitize(m.modal.prompt.Question.Prompt)),
		"",
		lipgloss.NewStyle().Width(w).Render(m.modal.pubKey),
		"",
	}
	if m.modal.keyCopied {
		lines = append(lines, noticeStyle.Render("Key copied (if your terminal allows it). Paste it where the site asks for it."))
	} else {
		lines = append(lines, dimStyle.Render("Ctrl+Y copies the public key."))
	}
	return strings.Join(lines, "\n")
}

// keyCopyView is the text above the confirm that asks to copy a private key
// to this computer: the whole prompt, wrapped.
func (m *model) keyCopyView() string {
	w := max(m.width-6, 20)
	return strings.Join([]string{
		lipgloss.NewStyle().Bold(true).Render("Copy a private key to this computer?"),
		lipgloss.NewStyle().Width(w).Render(sanitize(m.modal.prompt.Question.Prompt)),
	}, "\n")
}

func (m *model) openApproval(req ApprovalRequestMsg) tea.Cmd {
	ap := newApprovalState(req.Proposal, m.dark)
	m.modal = &modal{src: req, approval: &req, ap: ap, openedAt: m.now()}
	m.input.Blur()
	m.layoutApproval()
	if ap.editable {
		return ap.ta.Focus()
	}
	return nil
}

func (m *model) openPrompt(req PromptRequestMsg) tea.Cmd {
	q := req.Question
	md := &modal{src: req, prompt: &req, openedAt: m.now()}
	title := sanitize(q.Host) + ": " + strings.TrimSpace(oneLine(q.Prompt))
	switch q.Kind {
	case sshx.AskHostKey, sshx.AskKeyCreate:
		md.hostKey = true
		ok := false // the default answer is No
		md.ok = &ok
		md.form = m.newForm(huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(&ok))
	case sshx.AskKeyCopy:
		// The multi-line prompt (file, source, destination, risk) is drawn
		// above the confirm (keyCopyView); the default answer is No.
		md.hostKey = true
		md.copyKey = true
		ok := false
		md.ok = &ok
		md.form = m.newForm(huh.NewConfirm().Title(sanitize(q.Host) + ": copy the key to this computer?").Affirmative("Copy key").Negative("No, create a new key").Value(&ok))
	case sshx.AskKeyUpload:
		// The prompt and the public key are drawn above the form (keyView).
		md.hostKey = true
		md.pubKey = sanitize(q.Data)
		ok := false
		md.ok = &ok
		md.form = m.newForm(huh.NewConfirm().Title(sanitize(q.Host) + ": have you registered the key?").Affirmative("I've added it, try again").Negative("Cancel").Value(&ok))
	default:
		txt := ""
		md.text = &txt
		in := huh.NewInput().Title(title).Value(&txt)
		if !q.Echo {
			in = in.EchoMode(huh.EchoModePassword)
		}
		md.form = m.newForm(in)
	}
	m.modal = md
	m.input.Blur()
	cmds := []tea.Cmd{md.form.Init()}
	if m.bg != nil {
		_, c := md.form.Update(*m.bg)
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

func (m *model) updateModal(msg tea.Msg) (tea.Model, tea.Cmd) {
	md := m.modal
	if md == nil {
		return m, nil
	}
	if md.form == nil && md.cs != nil { // choice: only the Other input takes non-key messages (paste)
		if md.cs.onOther() {
			var cmd tea.Cmd
			md.cs.other, cmd = md.cs.other.Update(msg)
			return m, cmd
		}
		return m, nil
	}
	if md.form == nil { // approval: only the editor or reason input takes non-key messages
		if md.ap != nil && md.ap.asking {
			var cmd tea.Cmd
			md.ap.ri, cmd = md.ap.ri.Update(msg)
			return m, cmd
		}
		if md.ap != nil && md.ap.editable && md.ap.inText {
			var cmd tea.Cmd
			md.ap.ta, cmd = md.ap.ta.Update(msg)
			return m, cmd
		}
		return m, nil
	}
	fm, cmd := md.form.Update(msg)
	if f, ok := fm.(*huh.Form); ok {
		md.form = f
	}
	switch md.form.State {
	case huh.StateCompleted:
		return m, tea.Batch(cmd, m.finishPrompt())
	case huh.StateAborted:
		return m, tea.Batch(cmd, m.dismiss("declined in the dialog"))
	}
	return m, cmd
}

// finishPrompt sends the user's answer for a completed login form.
func (m *model) finishPrompt() tea.Cmd {
	md := m.modal
	switch {
	case md.hostKey:
		a := PromptAnswer{Text: "no"}
		if *md.ok {
			a.Text = "yes"
		}
		md.replyPrompt(a)
	default:
		md.replyPrompt(PromptAnswer{Text: *md.text})
	}
	return m.closeModal()
}

// dismiss answers the open modal as declined/canceled and shows the next
// queued request, if any.
func (m *model) dismiss(reason string) tea.Cmd {
	if m.modal == nil {
		return nil
	}
	m.answer(m.modal, reason)
	return m.closeModal()
}

// answer sends the declined/canceled reply for md (a no-op if already
// answered).
func (m *model) answer(md *modal, reason string) {
	if md.approval != nil {
		// Esc or the turn ending during the "Why?" prompt sends the same
		// reason as from the buttons ("declined in the dialog", "the turn was
		// canceled", ...), matching the GUI; a typed-but-unsent reason is
		// dropped.
		md.reply(agent.Decision{Approved: false, Reason: reason})
	} else if md.choice != nil {
		md.replyChoice(agent.ChoiceAnswer{Index: -1, Skipped: true})
	} else {
		md.replyPrompt(PromptAnswer{Err: errCanceled})
	}
}

// answerAll answers the open modal and every queued request as
// declined/canceled, leaving nothing open.
func (m *model) answerAll(reason string) {
	for _, req := range m.queue {
		switch r := req.(type) {
		case ApprovalRequestMsg:
			select {
			case r.Reply <- agent.Decision{Approved: false, Reason: reason}:
			default:
			}
		case ChoiceRequestMsg:
			select {
			case r.Reply <- agent.ChoiceAnswer{Index: -1, Skipped: true}:
			default:
			}
		case PromptRequestMsg:
			select {
			case r.Reply <- PromptAnswer{Err: errCanceled}:
			default:
			}
		}
	}
	m.queue = nil
	if m.modal != nil {
		m.answer(m.modal, reason)
		m.clearModal()
	}
}

func (m *model) clearModal() {
	if m.modal != nil && m.modal.text != nil {
		*m.modal.text = ""
	}
	m.modal = nil
	m.input.Focus() // the input stays usable while a turn runs
}

// closeModal removes the open modal and opens the next queued request.
func (m *model) closeModal() tea.Cmd {
	m.clearModal()
	if len(m.queue) == 0 {
		return nil
	}
	next := m.queue[0]
	m.queue = m.queue[1:]
	return m.open(next)
}

// ---- view ----

func (m *model) View() tea.View {
	var v tea.View
	if m.tooSmall() {
		note := fmt.Sprintf("window too small (%dx%d); need at least %dx%d", m.width, m.height, minWidth, minHeight)
		if m.modal != nil {
			note += "\n" + enlargeNote
		}
		v = tea.NewView(note)
		v.AltScreen = true
		return v
	}
	var main string
	if m.modal != nil {
		var body string
		switch {
		case m.modal.ap != nil:
			body = m.approvalView()
		case m.modal.cs != nil:
			body = m.choiceView()
		case m.modal.copyKey:
			body = m.keyCopyView() + "\n\n" + m.modal.form.View() + "\n\n" + dimStyle.Render(m.promptHint())
		case m.modal.pubKey != "":
			body = m.keyView() + "\n\n" + m.modal.form.View() + "\n\n" + dimStyle.Render(m.promptHint())
		default:
			body = m.modal.form.View() + "\n\n" + dimStyle.Render(m.promptHint())
		}
		box := boxStyle.Width(m.width).Render(body)
		main = lipgloss.NewStyle().MaxHeight(m.height - 1).Render(box)
		main = lipgloss.PlaceVertical(m.height-1, lipgloss.Top, main)
	} else {
		parts := []string{m.vp.View()}
		if tv := m.todoView(); tv != "" {
			parts = append(parts, tv)
		}
		if m.queued != "" {
			parts = append(parts, dimStyle.MaxWidth(m.chatWidth()).Render("queued: "+oneLine(m.queued)+" (sent when this reply ends; Esc removes it)"))
		}
		chat := lipgloss.JoinVertical(lipgloss.Left, append(parts, m.input.View())...)
		main = chat
		if m.showJobs && m.o.Jobs != nil {
			rows := max(m.height-1-2, 1)
			panel := boxStyle.Width(jobsPanelWidth).Height(m.height - 1).MaxHeight(m.height - 1).Render(m.jobsView(rows, jobsPanelWidth-4))
			main = lipgloss.JoinHorizontal(lipgloss.Top, chat, " ", panel)
		}
	}
	v = tea.NewView(lipgloss.JoinVertical(lipgloss.Left, main, m.statusBar()))
	v.AltScreen = true
	return v
}

func (m *model) promptHint() string {
	if m.modal != nil && m.modal.pubKey != "" {
		return "Ctrl+Y copy key · ←/→ choose · Enter confirm · Esc cancel · Ctrl+C cancel turn · Ctrl+D quit"
	}
	if m.modal != nil && m.modal.hostKey {
		return "←/→ choose · Enter confirm · Esc cancel · Ctrl+C cancel turn · Ctrl+D quit"
	}
	return "Enter confirm · Esc cancel · Ctrl+C cancel turn · Ctrl+D quit"
}

// hintCandidates lists key hints from most to least complete; the status bar
// picks the first that fits so the hints are never cut off.
func (m *model) hintCandidates() []string {
	jobs := ""
	if m.o.Jobs != nil {
		jobs = " · Tab jobs"
	}
	switch {
	case m.modal != nil && m.modal.cs != nil:
		return []string{
			fmt.Sprintf("↑/↓ choose · Enter pick · 1-%d pick · Esc skip · Ctrl+C cancel turn · Ctrl+D quit", len(m.modal.cs.q.Options)),
			"Esc skip · Ctrl+C cancel turn · Ctrl+D quit",
			"Esc skip · Ctrl+D quit",
		}
	case m.modal != nil && m.modal.ap != nil:
		return []string{
			"←/→/Tab choose · Enter confirm · PgUp/PgDn scroll · Esc decline · Ctrl+C cancel turn · Ctrl+D quit",
			"Esc decline · Ctrl+C cancel turn · Ctrl+D quit",
			"Esc decline · Ctrl+D quit",
		}
	case m.modal != nil:
		return []string{
			m.promptHint(),
			"Esc cancel · Ctrl+C cancel turn · Ctrl+D quit",
			"Esc cancel · Ctrl+D quit",
		}
	case m.canceling:
		return []string{"Ctrl+C again quit · Ctrl+D quit", "Ctrl+C quit"}
	case m.running:
		return []string{"Enter queue · Ctrl+C cancel · PgUp/PgDn scroll" + jobs + " · Ctrl+D quit", "Ctrl+C cancel · Ctrl+D quit"}
	}
	return []string{
		"Enter send · Alt+Enter newline · PgUp/PgDn scroll" + jobs + " · Ctrl+C quit · Ctrl+D quit",
		"Enter send" + jobs + " · Ctrl+C quit · Ctrl+D quit",
		"Ctrl+C quit · Ctrl+D quit",
	}
}

// stepWorking is the busy text before the first step arrives and while
// the reply streams.
const stepWorking = "Working…"

// stepElapsedAfter is when the status bar starts showing how long the turn
// has run, so a long step never looks frozen.
const stepElapsedAfter = 5 * time.Second

// stepText is the running turn's status: the current step (or "Working…"),
// plus the elapsed seconds once the turn ran stepElapsedAfter.
func (m *model) stepText() string {
	s := m.step
	if s == "" {
		s = stepWorking
	}
	if d := m.now().Sub(m.turnStart); !m.turnStart.IsZero() && d >= stepElapsedAfter {
		s += fmt.Sprintf(" %d s", int(d.Seconds()))
	}
	return s
}

func (m *model) statusBar() string {
	avail := max(m.width-2, 1)
	cands := m.hintCandidates()
	hint := cands[len(cands)-1]
	for _, c := range cands {
		if lipgloss.Width(c) <= avail {
			hint = c
			break
		}
	}
	left := oneLine(m.o.Status + m.foldersStatus())
	if m.xfer != "" {
		left = m.xfer
		if m.autoApproveOn() {
			left += " · AUTO-APPROVE ON" // still visible during a transfer
		}
	} else if m.autoApproveOn() {
		left += " · AUTO-APPROVE ON (/autoapprove off)"
	}
	switch {
	case m.xfer != "":
	case m.canceling:
		left += " · canceling…"
	case m.running:
		left += " · " + m.spin.View() + " " + m.stepText()
		if m.notice != "" {
			left += " · " + m.notice
		}
	case m.notice != "":
		left += " · " + m.notice
	}
	const sep = "  |  "
	// While a turn runs, a shorter key hint (at worst the shortest) makes
	// room for the step text: it says what Labmill is waiting for.
	if m.running && m.xfer == "" {
		hint = cands[len(cands)-1]
		for _, c := range cands {
			if lipgloss.Width(left)+len(sep)+lipgloss.Width(c) <= avail {
				hint = c
				break
			}
		}
	}
	room := avail - lipgloss.Width(hint) - len(sep)
	line := hint
	if m.xfer != "" {
		line = lipgloss.NewStyle().MaxWidth(avail).Render(left) // the whole width for the progress; the key hint comes back after
	} else if room >= 4 {
		line = lipgloss.NewStyle().MaxWidth(room).Render(left) + sep + hint
	}
	return statusStyle.Width(m.width).MaxWidth(m.width).Render(line)
}

// jobsView renders the jobs list into at most rows lines of width w.
func (m *model) jobsView(rows, w int) string {
	clip := lipgloss.NewStyle().MaxWidth(w)
	lines := []string{lipgloss.NewStyle().Bold(true).Render("Jobs")}
	if m.jobsErr != nil {
		lines = append(lines, errStyle.Render(firstLine(oneLine(m.jobsErr.Error()))))
	}
	if len(m.jobs) == 0 {
		lines = append(lines, dimStyle.Render("no jobs yet"))
	}
	for _, j := range m.jobs {
		lines = append(lines,
			lipgloss.NewStyle().Bold(true).Render(oneLine(j.Name)),
			dimStyle.Render(fmt.Sprintf("  %s · %s · %s", oneLine(j.Cluster), oneLine(string(j.State)), ago(j.UpdatedAt))))
	}
	if len(lines) > rows {
		hidden := len(lines) - (rows - 1)
		lines = append(lines[:rows-1], dimStyle.Render(fmt.Sprintf("… %d more lines", hidden)))
	}
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
