package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/store"
)

// Session runs conversation turns. app wiring implements it with an
// agent.Agent (keeping the history); tests use fakes. Send must report
// progress through the Bridge it was built with (Event, Approve, Ask,
// Notify) and finish by calling Bridge.Done.
type Session interface {
	Send(ctx context.Context, text string)
}

// JobLister returns jobs for the jobs panel (store.Store.Jobs in production).
type JobLister func(ctx context.Context) ([]store.Job, error)

// Options configures the TUI model.
type Options struct {
	Session Session
	Bridge  *Bridge
	Jobs    JobLister // nil hides the jobs panel
	Status  string    // left part of the status bar, e.g. "bigcluster (Slurm) · othercluster (Slurm)"
	Welcome string    // first assistant message shown (markdown)
	// Folders is the chosen data and code folders (nil: no /data and /code).
	Folders Folders
	// Home is the home folder for Tab completion of "~" in /code and /data
	// ("" leaves "~" alone).
	Home string
	// Settings returns the text /settings shows (Markdown: every setting
	// without secrets, and the files they live in; settings.View.Text). nil
	// makes /settings say it isn't available.
	Settings func() string
	// AutoApprove reports "Approve everything automatically" (M43.2); the
	// status line shows it while on. SetAutoApprove saves a change made with
	// /autoapprove on|off. nil for either makes /autoapprove say it isn't
	// available.
	AutoApprove    func() bool
	SetAutoApprove func(on bool) error
	// AutoDownload reports "Download results automatically" (M60.1) and
	// SetAutoDownload saves a change made with /autodownload on|off. nil for
	// either makes /autodownload say it isn't available.
	AutoDownload    func() bool
	SetAutoDownload func(on bool) error
	// Shell (M62.1) opens "/shell <cluster> [--dir <folder>]": it connects to
	// the cluster's login node (the helper's existing sign-in; login prompts
	// come through the Bridge as usual) and returns the command that runs the
	// interactive shell on the real terminal. The TUI runs it with tea.Exec
	// (suspended while it runs) and carries on when the shell exits. Nothing
	// typed or shown in the shell reaches the chat or the model. nil makes
	// /shell say it isn't available.
	Shell func(ctx context.Context, cluster, dir string) (tea.ExecCommand, error)
}

// NewModel builds the root model:
//
//   - Chat pane (bubbles viewport): user messages, assistant messages
//     rendered with Glamour as they stream (EventText deltas appended to the
//     current assistant message), and one-line tool notes ("checking
//     cluster bigcluster…" for EventToolCall, "✓ done"/"✗ <error>" for
//     results, "declined" for EventDeclined).
//   - Input (bubbles textarea): Enter sends (when no modal is open),
//     Alt+Enter inserts a newline. Sending calls Session.Send in a goroutine
//     with a cancelable ctx; a spinner shows in the status bar while a turn
//     runs, followed by the turn's current step (M26): "Working…" at first
//     and while the reply streams, the agent's EventStep texts ("Waiting for
//     Claude…", "Connecting to
//     <alias>…"), "Running <tool>…" for a tool call, and the elapsed seconds
//     once the turn ran 5 s; a shorter key hint makes room for it. The input
//     stays usable during a turn: Enter then queues the
//     message (one; a newer one replaces it), shown as a "queued: …" line
//     above the input, and it is sent when the turn ends (also after a
//     cancel).
//   - Ctrl+C: cancels the running turn (a second Ctrl+C quits); when idle,
//     quits. Ctrl+D quits. The key hints in the status bar show these.
//   - JobNoticeMsg (the background job monitor: a job finished, failed or
//     was canceled) adds a highlighted line to the chat (above a reply that
//     is still streaming) and refreshes the jobs panel. The jobs panel has
//     no per-job keys: ask in the chat ("show the log of job 12").
//   - Approval modal for ApprovalRequestMsg. The user must be able to see
//     everything they approve: Summary, then Details in a scrollable
//     viewport sized to the remaining space (PgUp/PgDn/↑/↓, "lines x-y of
//     z" indicator), then the buttons, then a key hint, which are all
//     always visible. For Editable proposals, Details is an editable text
//     area (no line limit; external editor disabled). Edited is set only
//     if the text differs from what the text area showed initially; if the
//     text area normalized the script (tabs, CR), a visible warning says
//     so. The buttons are "Approve" / "Decline", and Decline is the
//     default. Esc declines; Ctrl+C declines and cancels the turn. If the
//     modal can't fit (window below the minimum), keys other than Esc are
//     ignored and the modal asks the user to enlarge the window.
//   - Choice modal for ChoiceRequestMsg (ask_user): the question, then one
//     row per option (number, label, description) and, when AllowOther, an
//     "Other" row with a one-line input. The first option is selected at
//     first; ↑/↓ (and Tab) move, Enter picks the selected row (an empty
//     Other does nothing), number keys pick an option directly, Esc skips.
//     When the window is small, only the selected option keeps its
//     description, so every option and the key hint stay visible.
//   - Login modal for PromptRequestMsg: an input titled with Question.Host
//     and Prompt, masked unless Echo. AskHostKey renders as a yes/no confirm
//     defaulting to no. Esc cancels (PromptAnswer.Err). The SSH key setup of
//     a key-only host (M38.6) uses the same modal: AskKeyCreate is a yes/no
//     confirm, AskKeyPassphrase and AskNewPassphrase are masked inputs (the
//     new-key one may stay empty), and AskKeyUpload draws the site's
//     instructions and the public key (Question.Data) above a confirm
//     ("I've added it, try again" / "Cancel"); Ctrl+Y copies the key. AskKeyCopy
//     (copy a private key from the home cluster, M45.3) draws its prompt above
//     a confirm that defaults to No.
//   - Requests that arrive while a modal is open are queued and shown in
//     order. Exactly one reply is sent per request on every path: answered,
//     Esc, turn end (remaining requests answered as declined/canceled/
//     skipped), and quit. Every modal ignores keys other than Esc, Ctrl+C
//     and Ctrl+D for keyGrace after it opens.
//   - All text from the agent or the cluster shown anywhere (assistant
//     messages, Summary, Details, prompts, notices, tool notes) has control
//     characters other than newline removed (tabs become spaces) before
//     rendering, so escape sequences or OSC 8 links can't alter the display.
//   - Glamour and Huh follow the terminal's background (request it at start;
//     choose the dark or light style; default dark until known).
//   - Rendering stays responsive: rendered entries are cached and only the
//     streaming entry is re-rendered (at most ~10 times a second).
//   - NotifyMsg shows in the status bar (after the step while a turn runs)
//     until the next turn starts.
//   - Jobs panel: Tab toggles a side panel listing jobs (name, cluster,
//     state, updated), refreshed every 5s via Jobs.
//   - Folders (M20): "/data <folder>" and "/code <folder>" set the chosen
//     data and code folders through Folders (a relative folder is taken
//     from the current folder, "~" is home, one pair of quotes is
//     dropped), "/code" alone shows it and "/code -" clears it. They never
//     start a turn (also allowed while one runs); the answer, or why the
//     folder was refused, is a line in the chat. While the input is such a
//     command, Tab completes the folder name instead of toggling the jobs
//     panel (several matches: the common part, the names in the status
//     bar). The status bar shows "Data: …" and "Code: …" (data first,
//     M24) when chosen.
//   - Settings (M29): "/settings" shows Options.Settings() as an assistant
//     entry (above a reply that is still streaming); it never starts a turn
//     and also works while one runs. Editing stays in the desktop app.
//   - WindowSizeMsg relayouts everything; minimum usable size 60x15
//     (smaller shows a "window too small" note).
func NewModel(o Options) tea.Model { return newModel(o) }
