// Package tui is Labmill's terminal interface (Bubble Tea v2): a chat pane, a
// jobs panel, and modal Huh forms for approvals and the login dialog.
//
// The agent and the SSH layer run outside the Bubble Tea event loop. They
// talk to the UI only through a Bridge, which implements agent.Approver,
// sshx.Prompter and the agent's event sink by turning each call into a
// message for the program and waiting for the user's answer. Every
// interaction is therefore visible and modal.
package tui

import (
	"context"
	"errors"
	"sync"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/agent"
	"labmill-client/internal/sshx"
	"labmill-client/internal/transfer"
)

// Bridge connects background work (agent turns, SSH logins) to the UI.
type Bridge struct {
	msgs   chan tea.Msg
	closed chan struct{}
	once   sync.Once
}

// errClosed is what Approve and Ask return once the Bridge is closed.
var errClosed = errors.New("Labmill's window was closed")

// NewBridge returns a Bridge with a buffered message channel.
func NewBridge() *Bridge {
	return &Bridge{msgs: make(chan tea.Msg, 256), closed: make(chan struct{})}
}

// Next returns a tea.Cmd that waits for the next bridged message. The model
// re-issues it after handling each message (the standard "listen" pattern).
// After Close it returns nil (a nil message is ignored by Bubble Tea).
func (b *Bridge) Next() tea.Cmd {
	return func() tea.Msg {
		select {
		case m := <-b.msgs:
			return m
		case <-b.closed:
			return nil
		}
	}
}

// ApprovalRequestMsg asks the UI to show a Proposal. The UI must send exactly
// one Decision on Reply.
type ApprovalRequestMsg struct {
	Proposal agent.Proposal
	Reply    chan<- agent.Decision
}

// ChoiceRequestMsg asks the UI to show a selection dialog (ask_user). The UI
// must send exactly one ChoiceAnswer on Reply.
type ChoiceRequestMsg struct {
	Choice agent.Choice
	Reply  chan<- agent.ChoiceAnswer
}

// PromptRequestMsg asks the UI to show a login-dialog question. The UI sends
// exactly one answer (or an error, if the user cancels) on Reply.
type PromptRequestMsg struct {
	Question sshx.Question
	Reply    chan<- PromptAnswer
}

// PromptAnswer is the reply to a PromptRequestMsg.
type PromptAnswer struct {
	Text string
	Err  error // non-nil when the user canceled the dialog
}

// NotifyMsg is a status line from the SSH layer (e.g. "approve the sign-in request").
type NotifyMsg struct{ Text string }

// StatusMsg replaces the left part of the status bar: the status text changed
// after startup.
type StatusMsg struct{ Text string }

// JobNoticeMsg is a sentence from the background job monitor (a job
// finished, failed or was canceled).
type JobNoticeMsg struct{ Text string }

// TransferMsg is a live upload/download notice (start, progress, end).
type TransferMsg struct{ Update transfer.Update }

// AgentEventMsg wraps an agent.Event for the UI.
type AgentEventMsg struct{ Event agent.Event }

// TurnDoneMsg reports that an agent turn finished (Err nil on success).
type TurnDoneMsg struct{ Err error }

// Approve implements agent.Approver: it sends an ApprovalRequestMsg and waits
// for the Decision, for ctx to end (then returns ctx.Err()) or for Close.
func (b *Bridge) Approve(ctx context.Context, p agent.Proposal) (agent.Decision, error) {
	if b.isClosed() {
		return agent.Decision{}, errClosed
	}
	reply := make(chan agent.Decision, 1)
	select {
	case b.msgs <- ApprovalRequestMsg{Proposal: p, Reply: reply}:
	case <-ctx.Done():
		return agent.Decision{}, ctx.Err()
	case <-b.closed:
		return agent.Decision{}, errClosed
	}
	select {
	case d := <-reply:
		return d, nil
	case <-ctx.Done():
		return agent.Decision{}, ctx.Err()
	case <-b.closed:
		return agent.Decision{}, errClosed
	}
}

// Choose implements agent.Chooser: it sends a ChoiceRequestMsg and waits for
// the answer, for ctx to end (then a Skipped answer and ctx.Err()) or for
// Close (a Skipped answer and an error).
func (b *Bridge) Choose(ctx context.Context, q agent.Choice) (agent.ChoiceAnswer, error) {
	skipped := agent.ChoiceAnswer{Index: -1, Skipped: true}
	if b.isClosed() {
		return skipped, errClosed
	}
	reply := make(chan agent.ChoiceAnswer, 1)
	select {
	case b.msgs <- ChoiceRequestMsg{Choice: q, Reply: reply}:
	case <-ctx.Done():
		return skipped, ctx.Err()
	case <-b.closed:
		return skipped, errClosed
	}
	select {
	case a := <-reply:
		return a, nil
	case <-ctx.Done():
		return skipped, ctx.Err()
	case <-b.closed:
		return skipped, errClosed
	}
}

// Ask implements sshx.Prompter: it sends a PromptRequestMsg and waits for the
// answer, for ctx to end or for Close.
func (b *Bridge) Ask(ctx context.Context, q sshx.Question) (string, error) {
	if b.isClosed() {
		return "", errClosed
	}
	reply := make(chan PromptAnswer, 1)
	select {
	case b.msgs <- PromptRequestMsg{Question: q, Reply: reply}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-b.closed:
		return "", errClosed
	}
	select {
	case a := <-reply:
		return a.Text, a.Err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-b.closed:
		return "", errClosed
	}
}

// Notify implements sshx.Prompter (non-blocking: drops the message if the
// channel is full rather than stall a login; a dropped notice is cosmetic).
func (b *Bridge) Notify(msg string) {
	select {
	case b.msgs <- NotifyMsg{Text: msg}:
	default:
	}
}

// StatusLine is the optional launch hook for a changed status text: the
// status bar shows it from then on. It never blocks; a full queue drops it
// (the text is also what CurrentStatus returns).
func (b *Bridge) StatusLine(text string) {
	select {
	case b.msgs <- StatusMsg{Text: text}:
	default:
	}
}

// JobNotice shows a background job monitor sentence (launch.UI). Like
// Notify it never blocks: when the UI is far behind, the notice is dropped
// (the jobs panel still shows the new state).
func (b *Bridge) JobNotice(text string) {
	if b.isClosed() {
		return
	}
	select {
	case b.msgs <- JobNoticeMsg{Text: text}:
	default:
	}
}

// Transfer shows live transfer progress in the status line (launch.UI).
// Progress notices never block (a dropped one is replaced by the next); the
// start and end notices wait for room so the line is never left stale.
func (b *Bridge) Transfer(u transfer.Update) {
	if b.isClosed() {
		return
	}
	if u.Kind == "progress" {
		select {
		case b.msgs <- TransferMsg{Update: u}:
		default:
		}
		return
	}
	select {
	case b.msgs <- TransferMsg{Update: u}:
	case <-b.closed:
	}
}

// bridgeBatch carries several bridged messages that were already waiting, so
// a burst (thousands of text deltas) costs the UI one redraw, not one each.
type bridgeBatch []tea.Msg

// maxBatch bounds how many queued messages one batch takes.
const maxBatch = 256

// nextBatch is Next for the model: it waits for one message, then takes
// whatever else is already queued without waiting. A single message is
// returned as is.
func (b *Bridge) nextBatch() tea.Cmd {
	return func() tea.Msg {
		var first tea.Msg
		select {
		case first = <-b.msgs:
		case <-b.closed:
			return nil
		}
		batch := bridgeBatch{first}
		for len(batch) < maxBatch {
			select {
			case m := <-b.msgs:
				batch = append(batch, m)
			default:
				if len(batch) == 1 {
					return first
				}
				return batch
			}
		}
		return batch
	}
}

func (b *Bridge) isClosed() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

// send delivers m, blocking until the UI has room or the Bridge is closed.
func (b *Bridge) send(m tea.Msg) {
	if b.isClosed() {
		return
	}
	select {
	case b.msgs <- m:
	case <-b.closed:
	}
}

// Event is the agent's OnEvent sink. It blocks until the UI has room for the
// event or the Bridge is closed: no event, text deltas included, is ever
// dropped while the UI runs (a missing delta could change a reply's meaning).
func (b *Bridge) Event(e agent.Event) { b.send(AgentEventMsg{Event: e}) }

// Done reports the end of a turn to the UI. Like Event it blocks until
// delivered or the Bridge is closed; it is never dropped.
func (b *Bridge) Done(err error) { b.send(TurnDoneMsg{Err: err}) }

// Close tells the Bridge the UI has exited: pending and future Approve/Ask
// calls return an error ("Labmill's window was closed"), Event/Done/Notify
// return immediately. Front ends call it after the program's Run returns.
// Close is idempotent.
func (b *Bridge) Close() { b.once.Do(func() { close(b.closed) }) }

var (
	_ agent.Approver = (*Bridge)(nil)
	_ agent.Chooser  = (*Bridge)(nil)
	_ sshx.Prompter  = (*Bridge)(nil)
)
