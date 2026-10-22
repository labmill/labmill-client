// Package gui is the Go side of Labmill's desktop app (Wails v2; see
// cmd/labmill-gui). It is pure Go — the Wails glue lives in cmd/labmill-gui —
// so its behavior is unit-tested without a window.
//
// Bridge implements launch.UI for the GUI: approvals, choices (ask_user),
// and login prompts become events for the page, each with an ID, and wait
// for the page's answer through App.Decide / App.Choose / App.Answer. The same guarantees as the TUI
// hold: exactly one reply per request on every path (answered, turn end,
// window closed), requests queued in order (the page shows one at a time),
// no event dropped while the window is open, and all untrusted text cleaned
// with textsafe.Clean before it leaves Go.
package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/launch"
	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
	"labmill-client/internal/textsafe"
	"labmill-client/internal/transfer"
)

// Emitter delivers an event to the page (runtime.EventsEmit in production).
type Emitter func(name string, data any)

// Event names and payloads sent to the page.
const (
	EvText     = "Labmill:text"     // TextEvent: a streamed text delta of the assistant reply
	EvTool     = "Labmill:tool"     // ToolEvent: a tool call, result or decline
	EvApproval = "Labmill:approval" // ApprovalEvent: show an approval dialog
	EvPrompt   = "Labmill:prompt"   // PromptEvent: show a login question
	EvChoice   = "Labmill:choice"   // ChoiceEvent: show a selection dialog (ask_user)
	EvNotify   = "Labmill:notify"   // NotifyEvent: a status line (e.g. approve the sign-in request)
	EvDone     = "Labmill:done"     // DoneEvent: the turn ended
	EvDismiss  = "Labmill:dismiss"  // DismissEvent: these request IDs were answered by Go (turn ended); close them
	EvTurn     = "Labmill:turn"     // TurnEvent: a turn started with this user message (show it, mark the chat busy)
	EvQueued   = "Labmill:queued"   // QueuedEvent: the message waiting for the running turn to end ("" = none)
	EvStatus   = "Labmill:status"   // StatusEvent: the status line changed
	EvJob      = "Labmill:job"      // JobNoticeEvent: a background job notice (show a system line, refresh the jobs)
	EvProgress = "Labmill:progress" // ProgressEvent: live upload/download progress (start, progress, end)
	EvStep     = "Labmill:step"     // StepEvent: what the turn is doing now (the busy indicator's text)
	EvTodos    = "Labmill:todos"    // TodosEvent: the whole to-do list (update_todos); replaces the panel's items
)

type TextEvent struct {
	Text string `json:"text"`
}
type ToolEvent struct {
	Kind    string `json:"kind"` // "call", "result", "declined"
	Tool    string `json:"tool"`
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
	// JobID is set on a successful fetch_results result: the job whose
	// results were downloaded, so the page can offer "Open folder"
	// (App.OpenJobFolder looks the folder up in the job store).
	JobID int64 `json:"jobId,omitempty"`
}
type ApprovalEvent struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Summary  string `json:"summary"`
	Details  string `json:"details"`
	Editable bool   `json:"editable"`
	// Altered is true when cleaning changed Details (hidden or control
	// characters were removed), so the page can warn that what the user sees
	// differs from what the model sent, as the TUI does.
	Altered bool `json:"altered"`
}
type PromptEvent struct {
	ID string `json:"id"`
	// Kind is "username", "password", "passcode", "hostkey", "other", or one
	// of the SSH key setup kinds: "keypassphrase" (secret), "keycreate"
	// (yes/no), "newpassphrase" (secret, may be empty), "keyupload" (shows
	// Data, the public key, with a Copy button; answer "yes" to continue)
	// and "keycopy" (yes/no: copy a private key from the home cluster to
	// this computer; the prompt names file, source, destination and risk).
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Prompt string `json:"prompt"`
	Echo   bool   `json:"echo"`
	// Data is the public key line for kind "keyupload"; "" otherwise.
	Data string `json:"data,omitempty"`
}

// ChoiceOption is one option of a ChoiceEvent.
type ChoiceOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type ChoiceEvent struct {
	ID         string         `json:"id"`
	Question   string         `json:"question"`
	Options    []ChoiceOption `json:"options"`
	AllowOther bool           `json:"allowOther"`
}
type NotifyEvent struct {
	Text string `json:"text"`
}
type DoneEvent struct {
	Error string `json:"error"` // "" on success
}
type DismissEvent struct {
	IDs []string `json:"ids"`
}
type TurnEvent struct {
	Text string `json:"text"`
}
type QueuedEvent struct {
	Text string `json:"text"`
}
type JobNoticeEvent struct {
	Text string `json:"text"`
	// JobID is set when the job's results were just downloaded by
	// themselves (M60.1), so the page can offer "Open folder".
	JobID int64 `json:"jobId,omitempty"`
}

// StepEvent is the current step of a running turn (agent.EventStep), e.g.
// "Waiting for Claude…" or "Connecting to bigcluster…". The page shows it in
// the "Working…" indicator until the reply text starts.
type StepEvent struct {
	Text string `json:"text"`
}

// ProgressEvent is a live transfer notice. Kind "start" carries the rclone-lib
// line in Text, "progress" the rclone-style stats line (and the numbers for a
// bar), "end" removes the progress row (Text is the final summary, "" on failure).
type ProgressEvent struct {
	Kind        string  `json:"kind"`
	Tool        string  `json:"tool"`
	Text        string  `json:"text"`
	Bytes       int64   `json:"bytes"`
	TotalBytes  int64   `json:"totalBytes"`
	Files       int     `json:"files"`
	TotalFiles  int     `json:"totalFiles"`
	Current     string  `json:"current"`
	BytesPerSec float64 `json:"bytesPerSec"`
	ETASeconds  int     `json:"etaSeconds"` // -1 when unknown
}

// errClosed is what Approve and Ask return once the Bridge is closed.
var errClosed = errors.New("Labmill's window was closed")

// errCanceled is the error a prompt gets when the turn ends or the user cancels.
var errCanceled = errors.New("canceled")

// pendingApproval is an approval waiting for the page's answer.
type pendingApproval struct {
	reply    chan agent.Decision
	editable bool
	shown    string // the cleaned Details the page was shown
}

// pendingChoice is a choice waiting for the page's answer.
type pendingChoice struct {
	reply      chan agent.ChoiceAnswer
	options    int
	allowOther bool
}

type promptAnswer struct {
	text string
	err  error
}

// Bridge connects the agent and the SSH layer to the page.
type Bridge struct {
	emit Emitter

	mu        sync.Mutex // guards everything below and serializes emission
	closed    bool
	seq       int
	approvals map[string]*pendingApproval
	choices   map[string]*pendingChoice
	prompts   map[string]chan promptAnswer
	order     []string        // pending IDs in request order
	text      textsafe.Stream // cleans assistant text deltas (guarded by mu)
	fetchJob  int64           // job_id of the running fetch_results call (0 = none)
}

// NewBridge returns a Bridge that emits through emit.
func NewBridge(emit Emitter) *Bridge {
	return &Bridge{
		emit:      emit,
		approvals: map[string]*pendingApproval{},
		choices:   map[string]*pendingChoice{},
		prompts:   map[string]chan promptAnswer{},
	}
}

// emitLocked emits unless closed. b.mu must be held, which keeps events in
// order and guarantees the emitter is never called after Close.
func (b *Bridge) emitLocked(name string, data any) {
	if !b.closed {
		b.emit(name, data)
	}
}

func (b *Bridge) emitNow(name string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.emitLocked(name, data)
}

// Approve implements agent.Approver: it emits an ApprovalEvent (cleaned
// Summary/Details) with a new ID and waits for Decide with that ID, ctx, or
// Close. Edited is honored only for editable proposals (the agent also
// enforces this).
func (b *Bridge) Approve(ctx context.Context, p agent.Proposal) (agent.Decision, error) {
	if err := ctx.Err(); err != nil {
		return agent.Decision{}, err
	}
	pa := &pendingApproval{
		reply:    make(chan agent.Decision, 1),
		editable: p.Editable,
		shown:    textsafe.Clean(p.Details),
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return agent.Decision{}, errClosed
	}
	b.seq++
	id := fmt.Sprintf("a%d", b.seq)
	b.approvals[id] = pa
	b.order = append(b.order, id)
	b.emitLocked(EvApproval, ApprovalEvent{
		ID:       id,
		Tool:     textsafe.Clean(p.Tool),
		Summary:  textsafe.Clean(p.Summary),
		Details:  pa.shown,
		Editable: p.Editable,
		Altered:  pa.shown != p.Details,
	})
	b.mu.Unlock()

	select {
	case d := <-pa.reply:
		return d, nil
	case <-ctx.Done():
		b.forget(id)
		// A reply may have raced in; prefer it so the answer is not lost.
		select {
		case d := <-pa.reply:
			return d, nil
		default:
		}
		return agent.Decision{}, ctx.Err()
	}
}

// forget removes a pending request whose waiter gave up (its ctx ended). If
// the request was still pending, it emits a DismissEvent so the page closes
// the dialog; nothing is emitted when it was already answered or the Bridge
// is closed.
func (b *Bridge) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, a := b.approvals[id]
	_, c := b.choices[id]
	_, p := b.prompts[id]
	delete(b.approvals, id)
	delete(b.choices, id)
	delete(b.prompts, id)
	b.removeOrder(id)
	if a || c || p {
		b.emitLocked(EvDismiss, DismissEvent{IDs: []string{id}})
	}
}

func (b *Bridge) removeOrder(id string) {
	for i, o := range b.order {
		if o == id {
			b.order = append(b.order[:i], b.order[i+1:]...)
			return
		}
	}
}

// Choose implements agent.Chooser: it emits a ChoiceEvent (all text cleaned)
// with a new ID and waits for Pick with that ID, ctx, or Close. When ctx
// ends first, the dialog is dismissed and Choose returns a Skipped answer
// with ctx.Err(); after Close it returns errClosed. Done and Close answer a
// pending choice Skipped.
func (b *Bridge) Choose(ctx context.Context, q agent.Choice) (agent.ChoiceAnswer, error) {
	skipped := agent.ChoiceAnswer{Index: -1, Skipped: true}
	if err := ctx.Err(); err != nil {
		return skipped, err
	}
	pc := &pendingChoice{reply: make(chan agent.ChoiceAnswer, 1), options: len(q.Options), allowOther: q.AllowOther}
	opts := make([]ChoiceOption, 0, len(q.Options))
	for _, o := range q.Options {
		opts = append(opts, ChoiceOption{Label: textsafe.Clean(o.Label), Description: textsafe.Clean(o.Description)})
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return skipped, errClosed
	}
	b.seq++
	id := fmt.Sprintf("c%d", b.seq)
	b.choices[id] = pc
	b.order = append(b.order, id)
	b.emitLocked(EvChoice, ChoiceEvent{
		ID:         id,
		Question:   textsafe.Clean(q.Question),
		Options:    opts,
		AllowOther: q.AllowOther,
	})
	b.mu.Unlock()

	select {
	case a := <-pc.reply:
		return a, nil
	case <-ctx.Done():
		b.forget(id)
		select {
		case a := <-pc.reply:
			return a, nil
		default:
		}
		return skipped, ctx.Err()
	}
}

func promptKind(k sshx.QuestionKind) string {
	switch k {
	case sshx.AskUsername:
		return "username"
	case sshx.AskPassword:
		return "password"
	case sshx.AskPasscode:
		return "passcode"
	case sshx.AskHostKey:
		return "hostkey"
	case sshx.AskKeyPassphrase:
		return "keypassphrase"
	case sshx.AskKeyCreate:
		return "keycreate"
	case sshx.AskNewPassphrase:
		return "newpassphrase"
	case sshx.AskKeyUpload:
		return "keyupload"
	case sshx.AskKeyCopy:
		return "keycopy"
	}
	return "other"
}

// Ask implements sshx.Prompter: emits a PromptEvent and waits for Answer.
func (b *Bridge) Ask(ctx context.Context, q sshx.Question) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	reply := make(chan promptAnswer, 1)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return "", errClosed
	}
	b.seq++
	id := fmt.Sprintf("p%d", b.seq)
	b.prompts[id] = reply
	b.order = append(b.order, id)
	b.emitLocked(EvPrompt, PromptEvent{
		ID:     id,
		Kind:   promptKind(q.Kind),
		Host:   textsafe.Clean(q.Host),
		Prompt: textsafe.Clean(q.Prompt),
		Echo:   q.Echo,
		Data:   textsafe.Clean(q.Data),
	})
	b.mu.Unlock()

	select {
	case a := <-reply:
		return a.text, a.err
	case <-ctx.Done():
		b.forget(id)
		select {
		case a := <-reply:
			return a.text, a.err
		default:
		}
		return "", ctx.Err()
	}
}

// Notify implements sshx.Prompter (cleaned NotifyEvent).
func (b *Bridge) Notify(msg string) {
	b.emitNow(EvNotify, NotifyEvent{Text: textsafe.Clean(msg)})
}

// AutoApprovedLine is the chat line for a step that "Approve everything
// automatically" approved (M43.2).
func AutoApprovedLine(summary string) string { return "Auto-approved: " + summary }

// Event forwards agent events (cleaned) as TextEvent / ToolEvent /
// StepEvent, and EventNote as a NoticeEvent (a system line). A successful
// fetch_results result carries the job_id of its call (JobID).
func (b *Bridge) Event(e agent.Event) {
	if e.Kind == agent.EventText {
		// Cleaned as a stream: an escape sequence split across deltas is
		// removed whole instead of leaving a fragment like "[31m".
		b.mu.Lock()
		defer b.mu.Unlock()
		if t := b.text.Write(e.Text); t != "" {
			b.emitLocked(EvText, TextEvent{Text: t})
		}
		return
	}
	b.flushText()
	switch e.Kind {
	case agent.EventStep:
		b.emitNow(EvStep, StepEvent{Text: textsafe.Clean(e.Text)})
	case agent.EventNote:
		b.emitNow(EvNotice, NoticeEvent{Text: textsafe.Clean(e.Text)})
	case agent.EventAutoApproved:
		b.emitNow(EvNotice, NoticeEvent{Text: AutoApprovedLine(textsafe.Clean(e.Text))})
	case agent.EventToolCall, agent.EventToolResult, agent.EventDeclined:
		ev := ToolEvent{
			Kind:    "call",
			Tool:    textsafe.Clean(e.Tool),
			Text:    textsafe.Clean(e.Text),
			IsError: e.IsError,
		}
		switch e.Kind {
		case agent.EventToolResult:
			ev.Kind = "result"
		case agent.EventDeclined:
			ev.Kind = "declined"
		}
		var callJob int64
		if e.Kind == agent.EventToolCall && e.Tool == fetchTool {
			var in struct {
				JobID int64 `json:"job_id"`
			}
			if json.Unmarshal(e.Input, &in) == nil && in.JobID > 0 {
				callJob = in.JobID
			}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if e.Tool == fetchTool {
			switch e.Kind {
			case agent.EventToolCall:
				b.fetchJob = callJob
			case agent.EventToolResult:
				if !e.IsError {
					ev.JobID = b.fetchJob
				}
				b.fetchJob = 0
			case agent.EventDeclined:
				b.fetchJob = 0
			}
		}
		b.emitLocked(EvTool, ev)
	}
}

// flushText ends the text stream (a tool call, a step, the end of the turn):
// whatever the stream still holds back is cleaned and emitted.
func (b *Bridge) flushText() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.text.Flush(); t != "" {
		b.emitLocked(EvText, TextEvent{Text: t})
	}
}

// fetchTool is the tool whose successful result offers "Open folder".
const fetchTool = "fetch_results"

// StatusEvent carries a new status line.
type StatusEvent struct {
	Status string `json:"status"`
}

// StatusLine implements the optional launch status hook: the status text
// changed after startup.
func (b *Bridge) StatusLine(text string) {
	b.emitNow(EvStatus, StatusEvent{Status: textsafe.Clean(text)})
}

// JobNotice implements launch.UI: a background job monitor sentence
// (cleaned JobNoticeEvent). It never blocks on the page.
func (b *Bridge) JobNotice(text string) {
	b.emitNow(EvJob, JobNoticeEvent{Text: textsafe.Clean(text)})
}

// JobFetched implements launch's optional hook: the job monitor downloaded
// job jobID's results by itself (M60.1). Like JobNotice, with the job's ID so
// the page offers "Open folder".
func (b *Bridge) JobFetched(jobID int64, text string) {
	b.emitNow(EvJob, JobNoticeEvent{Text: textsafe.Clean(text), JobID: jobID})
}

// JobEnded implements launch's optional hook (M64.3): a job ended, show a
// desktop notification (launch has already checked the setting).
func (b *Bridge) JobEnded(text string) { DesktopNotify("labmill", text) }

// Transfer implements launch.UI: a cleaned ProgressEvent. It never blocks on
// the page.
func (b *Bridge) Transfer(u transfer.Update) {
	b.emitNow(EvProgress, ProgressEvent{
		Kind: textsafe.Clean(u.Kind), Tool: textsafe.Clean(u.Tool), Text: textsafe.Clean(u.Text),
		Bytes: u.Bytes, TotalBytes: u.TotalBytes, Files: u.Files, TotalFiles: u.TotalFiles,
		Current: textsafe.Clean(u.Current), BytesPerSec: u.BytesPerSec, ETASeconds: u.ETASeconds,
	})
}

// TodoItem is one to-do item on the page.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | in_progress | completed
	ActiveForm string `json:"activeForm"`
}

// TodosEvent is the whole to-do list; an empty list hides the panel.
type TodosEvent struct {
	Items []TodoItem `json:"items"`
}

// Todos shows the to-do list (cleaned TodosEvent). It never blocks on the
// page. The page keeps the list, so it is per session and in memory only.
func (b *Bridge) Todos(todos []app.Todo) {
	ev := TodosEvent{Items: make([]TodoItem, 0, len(todos))}
	for _, t := range todos {
		ev.Items = append(ev.Items, TodoItem{Content: textsafe.Clean(t.Content), Status: textsafe.Clean(t.Status), ActiveForm: textsafe.Clean(t.ActiveForm)})
	}
	b.emitNow(EvTodos, ev)
}

// answerAllLocked answers every pending request as declined/canceled and
// returns their IDs in request order. b.mu must be held.
func (b *Bridge) answerAllLocked(reason string) []string {
	ids := b.order
	b.order = nil
	for id, pa := range b.approvals {
		pa.reply <- agent.Decision{Approved: false, Reason: reason}
		delete(b.approvals, id)
	}
	for id, pc := range b.choices {
		pc.reply <- agent.ChoiceAnswer{Index: -1, Skipped: true}
		delete(b.choices, id)
	}
	for id, ch := range b.prompts {
		ch <- promptAnswer{err: errCanceled}
		delete(b.prompts, id)
	}
	return ids
}

// Done emits DoneEvent, and first answers every pending request (approvals:
// declined with reason "the turn ended"; choices: skipped; prompts:
// canceled) and emits a
// DismissEvent with their IDs.
func (b *Bridge) Done(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if t := b.text.Flush(); t != "" {
		b.emitLocked(EvText, TextEvent{Text: t})
	}
	if ids := b.answerAllLocked("the turn ended"); len(ids) > 0 {
		b.emit(EvDismiss, DismissEvent{IDs: ids})
	}
	ev := DoneEvent{}
	if err != nil {
		ev.Error = textsafe.Clean(err.Error())
	}
	b.emit(EvDone, ev)
}

// Close is called when the window closes: pending and future requests are
// answered (declined/canceled) and nothing more is emitted. Idempotent.
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.answerAllLocked("the window was closed")
}

// Decide answers approval id. Unknown or already-answered IDs are an error
// (a second click can never approve something twice). Approving (approve
// true) is accepted only for the oldest pending approval, as a guard against
// page bugs: any other approval is answered as declined and an error is
// returned. Any pending approval can be declined. edited is used only
// when approve is true and the proposal was editable and the text differs
// from the (cleaned) Details the page was shown.
//
// For an editable proposal the page must send the FULL edited text whenever
// the user changed it. An empty edited string, or one equal to the shown
// text, means "no edit": the original runs. In particular, clearing the
// field is not an edit, so the page should disable Approve while the edited
// field is empty.
//
// reason is the user's optional explanation for a decline. It is used only
// when approve is false: trimmed, cleaned with textsafe.Clean, newlines
// collapsed to spaces and capped at maxReasonRunes runes. It is ignored on
// approve.
func (b *Bridge) Decide(id string, approve bool, edited, reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	pa, ok := b.approvals[id]
	if !ok {
		return fmt.Errorf("no pending approval %q", id)
	}
	if oldest := b.oldestApprovalLocked(); approve && id != oldest {
		// Guard against page bugs: only the oldest pending approval can be
		// approved. This one is answered as declined (the agent never waits
		// on an approval that has no dialog) and the error is returned.
		delete(b.approvals, id)
		b.removeOrder(id)
		pa.reply <- agent.Decision{Approved: false, Reason: "approved out of order"}
		b.emitLocked(EvDismiss, DismissEvent{IDs: []string{id}})
		return fmt.Errorf("approval %q is not the oldest pending approval (%q); it was declined", id, oldest)
	}
	delete(b.approvals, id)
	b.removeOrder(id)
	d := agent.Decision{Approved: approve}
	if approve {
		// Compare with the text the page showed (cleaned Details): an
		// unedited approval must leave Edited empty so the original runs.
		if pa.editable && edited != "" && edited != pa.shown {
			d.Edited = edited
		}
	} else {
		d.Reason = cleanReason(reason)
		if d.Reason == "" {
			d.Reason = "declined in the dialog"
		}
	}
	pa.reply <- d
	return nil
}

// oldestApprovalLocked returns the ID of the oldest pending approval ("" if
// none). b.mu must be held.
func (b *Bridge) oldestApprovalLocked() string {
	for _, id := range b.order {
		if _, ok := b.approvals[id]; ok {
			return id
		}
	}
	return ""
}

// maxReasonRunes caps a decline reason.
const maxReasonRunes = 500

// cleanReason makes a typed decline reason a safe single line.
func cleanReason(s string) string {
	s = strings.Join(strings.Fields(textsafe.Clean(strings.TrimSpace(s))), " ")
	if r := []rune(s); len(r) > maxReasonRunes {
		s = strings.TrimSpace(string(r[:maxReasonRunes]))
	}
	return s
}

// Pick answers choice id: skip=true skips it; otherwise a non-empty other
// (trimmed, cleaned, collapsed to one line, capped at maxReasonRunes runes)
// is the user's typed answer, which needs AllowOther; otherwise index is the
// chosen option (0-based). Unknown or already-answered IDs are an error and
// change nothing. A known ID is always answered exactly once: invalid input
// (index out of range, a typed answer that isn't allowed) answers Skipped
// and returns an error, so the agent never waits on a dialog the page has
// already closed.
func (b *Bridge) Pick(id string, index int, other string, skip bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	pc, ok := b.choices[id]
	if !ok {
		return fmt.Errorf("no pending choice %q", id)
	}
	delete(b.choices, id)
	b.removeOrder(id)
	a := agent.ChoiceAnswer{Index: -1, Skipped: true}
	var err error
	other = cleanReason(other)
	switch {
	case skip:
	case other != "" && pc.allowOther:
		a = agent.ChoiceAnswer{Index: -1, Other: other}
	case other != "":
		err = fmt.Errorf("choice %q does not take a typed answer", id)
	case index < 0 || index >= pc.options:
		err = fmt.Errorf("choice %q has no option %d", id, index)
	default:
		a = agent.ChoiceAnswer{Index: index}
	}
	pc.reply <- a
	return err
}

// Answer answers prompt id with text, or cancels it when cancel is true.
func (b *Bridge) Answer(id string, text string, cancel bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.prompts[id]
	if !ok {
		return fmt.Errorf("no pending prompt %q", id)
	}
	delete(b.prompts, id)
	b.removeOrder(id)
	if cancel {
		ch <- promptAnswer{err: errCanceled}
	} else {
		ch <- promptAnswer{text: text}
	}
	return nil
}

var (
	_ launch.UI                      = (*Bridge)(nil)
	_ interface{ Todos([]app.Todo) } = (*Bridge)(nil)
)

// Session is the conversation runner (launch.Session satisfies it).
type Session interface {
	Send(ctx context.Context, text string)
	Wait(timeout time.Duration) bool
}

// JobLister lists jobs for the job list (store.Store.Jobs in production).
type JobLister func(ctx context.Context) ([]store.Job, error)

// App is the struct bound to the page (its exported methods are callable
// from JavaScript).
//
// The chat never blocks: a Send while a turn runs queues the message (at
// most one; a newer one replaces it) and it starts by itself when the turn
// ends, also after Cancel. The page learns what happens from events, never
// from Send's return: EvTurn when a turn starts (show the user message),
// EvQueued when the waiting message changes, EvDone when a turn ends.
type App struct {
	b      *Bridge
	s      Session
	jobs   JobLister
	status string

	mu      sync.Mutex
	folders FolderHost // the folder buttons and "Open folder" (SetFolderHost)
	running bool
	closed  bool // set by Shutdown; Send then fails
	cancel  context.CancelFunc
	queued  string // the message waiting for the running turn ("" = none)
}

// NewApp returns the bound App.
func NewApp(b *Bridge, s Session, jobs JobLister, status string) *App {
	return &App{b: b, s: s, jobs: jobs, status: status}
}

// Send starts a turn with the user's text in the background (it returns at
// once), emitting EvTurn first. While a turn runs it queues the text instead
// (replacing an earlier queued message) and emits EvQueued; the queued text
// starts as the next turn when the running one ends. An empty text or a
// Send after Shutdown is an error.
func (a *App) Send(text string) error { return a.send(text, false) }

// ErrQueueBusy is returned by JobAction when a turn is running and the user
// already has a message queued: the button never replaces that message.
var ErrQueueBusy = errors.New("Your queued message was kept; try the button again when Labmill is free")

// send is Send; with keepQueued it fails with ErrQueueBusy instead of
// replacing a message that is already queued.
func (a *App) send(text string, keepQueued bool) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to send")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errors.New("Labmill is shutting down")
	}
	if a.running {
		if keepQueued && a.queued != "" {
			return ErrQueueBusy
		}
		// Emitted under a.mu, so the page always sees EvQueued before the
		// EvTurn that sends this text.
		a.queued = text
		a.b.emitNow(EvQueued, QueuedEvent{Text: textsafe.Clean(text)})
		return nil
	}
	a.running = true
	go a.loop(a.startLocked(text))
	return nil
}

// startLocked prepares the turn for text: a fresh cancelable context and the
// EvTurn event. a.mu must be held.
func (a *App) startLocked(text string) turn {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.b.emitNow(EvTurn, TurnEvent{Text: textsafe.Clean(text)})
	return turn{ctx: ctx, cancel: cancel, text: text}
}

type turn struct {
	ctx    context.Context
	cancel context.CancelFunc
	text   string
	// action, when set, runs instead of a chat turn (RunAction).
	action func(ctx context.Context) (string, error)
}

// ErrBusy is returned by RunAction while a turn runs.
var ErrBusy = errors.New("Labmill is busy with a reply: wait for it to finish (or cancel it), then try again")

// RunAction runs fn as a turn of its own, for actions that don't go through
// the assistant: EvTurn with
// label (shown like a message the user sent), then fn with a cancelable
// context (Cancel cancels it), its reply as assistant text and EvDone with
// its error. Dialogs fn opens (an SSH login, a choice)
// belong to this turn, so no other turn's end can close them. While another
// turn runs it returns ErrBusy instead of interrupting the conversation; a
// message sent while the action runs is queued as usual.
func (a *App) RunAction(label string, fn func(ctx context.Context) (string, error)) error {
	if strings.TrimSpace(label) == "" || fn == nil {
		return errors.New("nothing to run")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errors.New("Labmill is shutting down")
	}
	if a.running {
		return ErrBusy
	}
	a.running = true
	t := a.startLocked(label)
	t.action = fn
	go a.loop(t)
	return nil
}

// runAction runs t's action and reports it like a turn: the reply as text,
// then Done.
func (a *App) runAction(t turn) {
	reply, err := t.action(t.ctx)
	if reply != "" {
		a.b.Event(agent.Event{Kind: agent.EventText, Text: reply})
	}
	if err == nil && t.ctx.Err() != nil {
		err = t.ctx.Err()
	}
	a.b.Done(err)
}

// loop runs t, then the queued message (if any) as the next turn, until
// nothing is queued or the App shut down.
func (a *App) loop(t turn) {
	for {
		if t.action != nil {
			a.runAction(t)
		} else {
			a.s.Send(t.ctx, t.text)
		}
		t.cancel()
		a.mu.Lock()
		if a.closed || a.queued == "" {
			a.running = false
			a.cancel = nil
			a.queued = ""
			a.mu.Unlock()
			return
		}
		next := a.queued
		a.queued = ""
		a.b.emitNow(EvQueued, QueuedEvent{})
		t = a.startLocked(next)
		a.mu.Unlock()
	}
}

// Unqueue drops the queued message, if any (EvQueued with "").
func (a *App) Unqueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.queued == "" {
		return
	}
	a.queued = ""
	a.b.emitNow(EvQueued, QueuedEvent{})
}

// Cancel cancels the running turn (no-op when idle). A queued message is
// kept and starts when the canceled turn has ended.
func (a *App) Cancel() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Decide forwards to Bridge.Decide.
func (a *App) Decide(id string, approve bool, edited, reason string) error {
	return a.b.Decide(id, approve, edited, reason)
}

// Choose forwards to Bridge.Pick.
func (a *App) Choose(id string, index int, other string, skip bool) error {
	return a.b.Pick(id, index, other, skip)
}

// Answer forwards to Bridge.Answer.
func (a *App) Answer(id string, text string, cancel bool) error {
	return a.b.Answer(id, text, cancel)
}

// JobView is a job row for the page (cleaned strings).
type JobView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Cluster string `json:"cluster"`
	State   string `json:"state"`
	Updated string `json:"updated"` // RFC 3339
	Message string `json:"message"`
	// Actions are the job buttons to show, in order (see jobActions).
	Actions []string `json:"actions"`
	// Terminal: the job has a folder on the cluster, so "Open terminal
	// here" is offered (Terminals, M62.2).
	Terminal bool `json:"terminal"`
}

// Job button actions (JobView.Actions, App.JobAction).
const (
	ActStatus = "status"
	ActLogs   = "logs"
	ActFetch  = "fetch"
	ActCancel = "cancel"
	// ActOpen opens the job's downloaded results in the file manager
	// (App.OpenJobFolder); it sends no chat message.
	ActOpen = "open"
)

// jobActions lists the buttons that make sense for j: Status and Logs once
// it was submitted (has a remote ID), Fetch when it completed or failed and
// has a remote folder, Cancel while it is submitted, pending or running,
// Open folder once its results were fetched to a local folder.
func jobActions(j store.Job) []string {
	acts := []string{}
	if j.RemoteID == "" {
		return acts
	}
	acts = append(acts, ActStatus, ActLogs)
	switch j.State {
	case store.StateCompleted, store.StateFailed:
		if j.RemoteDir != "" {
			acts = append(acts, ActFetch)
		}
		// A failed job downloaded automatically (M60.1) stays failed but
		// has its folder.
		if j.State == store.StateFailed && j.LocalDir != "" && j.Downloaded {
			acts = append(acts, ActOpen)
		}
	case store.StateSubmitted, store.StatePending, store.StateRunning:
		acts = append(acts, ActCancel)
	case store.StateFetched:
		if j.LocalDir != "" {
			acts = append(acts, ActOpen)
		}
	}
	return acts
}

// jobActionText is the chat message a job button sends for the user.
func jobActionText(id int64, action string) (string, bool) {
	switch action {
	case ActStatus:
		return fmt.Sprintf("Show the status of job #%d", id), true
	case ActLogs:
		return fmt.Sprintf("Show the last lines of the log of job #%d", id), true
	case ActFetch:
		return fmt.Sprintf("Fetch the results of job #%d", id), true
	case ActCancel:
		return fmt.Sprintf("Cancel job #%d", id), true
	}
	return "", false
}

// JobAction sends the chat message for a job button (e.g. "Cancel job
// #12") through Send, so it runs like a typed message: queued while a turn
// runs, and every action still goes through the agent and its approvals.
// If a message is already queued the button does not replace it: JobAction
// returns ErrQueueBusy (the page shows it as a notice), as the TUI keeps the
// user's queued message.
// The action must be one the job currently offers (JobView.Actions).
// ActOpen is the exception: it opens the folder (OpenJobFolder) at once.
func (a *App) JobAction(id int64, action string) error {
	if action == ActOpen {
		return a.OpenJobFolder(id)
	}
	text, ok := jobActionText(id, action)
	if !ok {
		return fmt.Errorf("unknown job action %q", action)
	}
	if a.jobs == nil {
		return fmt.Errorf("there's no job #%d", id)
	}
	js, err := a.jobs(context.Background())
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}
	for _, j := range js {
		if j.ID != id {
			continue
		}
		for _, act := range jobActions(j) {
			if act == action {
				return a.send(text, true)
			}
		}
		return fmt.Errorf("job #%d can't do %q now (it is %s)", id, action, j.State)
	}
	return fmt.Errorf("there's no job #%d", id)
}

// maxJobs is how many jobs Jobs returns.
const maxJobs = 50

// Jobs returns the 50 most recent jobs.
func (a *App) Jobs() ([]JobView, error) {
	if a.jobs == nil {
		return []JobView{}, nil
	}
	js, err := a.jobs(context.Background())
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	if len(js) > maxJobs {
		js = js[:maxJobs]
	}
	out := make([]JobView, 0, len(js))
	for _, j := range js {
		out = append(out, JobView{
			ID:       j.ID,
			Name:     textsafe.Clean(j.Name),
			Cluster:  textsafe.Clean(j.Cluster),
			State:    textsafe.Clean(string(j.State)),
			Updated:  j.UpdatedAt.Format(time.RFC3339),
			Message:  textsafe.Clean(j.Message),
			Actions:  jobActions(j),
			Terminal: jobHasTerminal(j),
		})
	}
	return out, nil
}

// Status returns the status-bar text (site, clusters).
func (a *App) Status() string { return a.status }

// Shutdown drops a queued message, cancels a running turn, waits up to 10s
// for it, and closes the Bridge (called from Wails'
// OnBeforeClose/OnShutdown).
func (a *App) Shutdown() {
	a.mu.Lock()
	a.closed = true
	a.queued = ""
	a.mu.Unlock()
	a.Cancel()
	a.s.Wait(10 * time.Second)
	a.b.Close()
}
