//go:build desktop

// Command labmill-gui is Labmill's desktop app: one chat box, one job list, one
// progress view (Wails v2, system webview). It shares the runtime with the
// terminal interface (internal/launch) and talks to the page through
// internal/gui's Bridge and App.
//
// Build (Linux, webkit2gtk-4.1): make gui-linux
// (go build -tags desktop,production,webkit2_41 ./cmd/labmill-gui).
// macOS and Windows builds use the same command on those platforms
// (without webkit2_41); they can't be produced on this Linux box.
//
// Frontend rules (frontend/index.html, app.css, app.js; plain JS, no build
// step, embedded below):
//   - All text that comes from Go is set with textContent, never innerHTML,
//     so nothing the agent or the cluster says can become markup or script.
//     Assistant text is Markdown rendered by frontend/md.js with DOM nodes
//     only (createElement with fixed tags, text nodes for all text): links
//     and images become the plain text "text (url)" with no href or src,
//     raw HTML stays literal text, and an unterminated code fence renders
//     as a code block. While a reply streams it is re-rendered at most every
//     50 ms (and at once before a tool note and at the end of the turn).
//   - Tool notes: one collapsed line per tool (<details>: "load_skill ·
//     4.2 KB", "job_logs · 120 lines"), expandable to the full text in
//     monospace; an error shows its first line.
//   - The chat box: a message list and an input (Enter sends, Shift+Enter
//     newline). The input is never disabled: while a turn runs (Cancel
//     button and spinner shown, Send reads "Queue"), Send queues the
//     message in Go (gui.App: one message, a newer one replaces it; Cancel
//     keeps it) and a "Queued" line with a Remove button shows it until it
//     starts. User messages appear on Labmill:turn (never on click), the
//     queued line follows Labmill:queued. Tool notes in a muted style.
//   - Job notices (Labmill:job, the background job monitor): a small system
//     line in the chat (above a reply that is still streaming) and an
//     immediate job-list refresh.
//   - The job list: refreshed every 5 s via App.Jobs (#id and name,
//     cluster, state, updated); cells never wrap, long values end in an
//     ellipsis with the full text in a tooltip; the panel is at least 20rem
//     wide. Under each job a row of real <button>s for JobView.Actions
//     (Status, Logs, Fetch, Cancel; labels via textContent, an aria-label
//     naming the job); a click calls PageAPI.JobAction, which sends a plain
//     chat message ("Cancel job #12"), queued while busy, so it goes through
//     the agent and its approvals. The table is rebuilt only when the jobs
//     changed, keeping the focused button. Font sizes are in rem (the chat
//     at 1rem, code at 0.875rem in a monospace stack), so they follow the
//     system font size and zoom.
//   - The progress view: the latest status line (Labmill:notify).
//   - Busy feedback (M26). The startup splash is in index.html itself: a
//     pure-CSS hourglass and "Starting labmill…" (aria-live polite), visible
//     on the first paint before any script runs. Its text follows the
//     startup steps (Labmill:startup {step}, also State.Step for a page that
//     loads late; launch.BuildWithProgress: "Loading your settings…",
//     "Opening the job database…", "Loading skills…", "Checking this
//     computer…", "Ready"); after 10 s a second line says which step it is
//     still waiting for. The chat replaces it as soon as the page has its bindings (M46.2), with the startup step in the status line; a message typed before Labmill:ready waits in the gate and goes out when the runtime exists. After Send,
//     a "Working…" row under the conversation (small hourglass, aria-live
//     polite text) appears at once, before Go answers, and shows the turn's
//     current step: Labmill:step (agent.EventStep: "Waiting for Claude…",
//     "Connecting to <alias>…") and "Running <tool>…" for a tool call; after 5 s the
//     seconds since it appeared tick next to it. It goes when the reply
//     text starts (and comes back for a later step or tool call) and when
//     the turn ends; Cancel stays available, and a transfer keeps its own
//     progress row. prefers-reduced-motion stops the hourglass (still,
//     sand on top). The tray icon comes up before the window with the
//     tooltip "Labmill is starting…" (gui.TrayStarting), refreshed at once
//     when the runtime is ready.
//   - Folders (M20, M24): a "+" button (aria-label "Add folder") at the
//     upper left inside the chat input box opens a menu (Enter, Space or
//     ArrowDown open it, arrows/Home/End move, Esc closes and returns the
//     focus): "Add data folder…" first (many users only have data and want
//     the code written for them), "Add code folder…", a separator, up to 5
//     recent folders labelled with their kind ("Data: ~/…/x"), and
//     "Remove data folder" / "Remove code folder" when set. "Add…" opens
//     the native folder dialog (PageAPI.PickFolder, Wails
//     OpenDirectoryDialog); a recent folder goes through PageAPI.SetFolder
//     (recent-only). Chosen folders show as chips above the text ("Data:
//     …", then "Code: …"; gui.FolderView.Short, the full path in the title
//     tooltip) with an × each (aria-label "Remove the data folder").
//     Dropping a folder onto the input box (Wails file drop, enabled only
//     there through the inherited --wails-drop-target CSS property) shows
//     a small inline choice "Add as data folder" (focused) / "Add as code
//     folder" / × (Esc cancels); PageAPI.DropFolder uses the first path
//     only and never reads or uploads anything. Drops elsewhere do nothing.
//     Go checks every folder with app.CheckFolder (no drive roots, system,
//     hidden or secret-looking folders) and the page shows its refusal as
//     an error line above the input box.
//   - "Open folder": a successful fetch_results tool note (ToolEvent.JobID)
//     gets an "Open folder" button, and a fetched job gets one in the job
//     list (action "open"); both call PageAPI.OpenJobFolder, which takes
//     the folder from the job store, checks it (app.Env.OpenableFolder) and
//     opens it with gui.OpenFolder (explorer.exe / open / xdg-open, no
//     shell, never a file:// URL).
//   - Approval dialog (Labmill:approval, one at a time, others queued in
//     order): Summary; Details in a scrollable <pre> (or an editable
//     <textarea> when editable, with a note that edits are what runs);
//     an optional one-line "Tell Labmill why" input (maxlength 500, never
//     focused on open, Enter in it does nothing) whose text goes with a
//     decline and is ignored on approve; buttons Decline (focused, default)
//     and Approve; Esc declines (with whatever is typed); keyboard
//     and clicks are ignored for 500 ms after the dialog opens (type-ahead
//     protection); exactly one App.Decide call per dialog (buttons disabled
//     after the first click). Labmill:dismiss closes dialogs Go already
//     answered.
//   - Choice dialog (Labmill:choice, ask_user; queued with the others): title
//     "Labmill asks", the question, one button per option (number, label,
//     description), an "Other" text input when allowOther (Enter in it
//     sends the text; empty sends nothing) and a Skip button. The FIRST
//     option has the initial focus (a choice changes nothing, so no
//     Decline-first rule); Up/Down move between options and the input,
//     Enter picks the focused option, number keys 1-9 pick directly (not
//     while typing in the input), Esc skips. The same 500 ms grace applies
//     (keys and clicks ignored, re-armed on window focus), so a keystroke
//     meant for the chat can't pick. Exactly one App.Choose call per dialog.
//   - Code blocks: every fenced code block in the chat has a Copy button
//     (aria-label "Copy") that copies the block's text (md.js copyText).
//   - Login dialog (Labmill:prompt): title "Sign in to <host>", the prompt
//     text, an <input type=password> unless echo, Submit and Cancel; the
//     host-key kind is a yes/no with No focused; the field is cleared after
//     submitting.
//   - Settings page (M29, frontend/settings.js, settings.go): the header's
//     gear button (shown once ready or failed), the tray's "Settings…" and
//     macOS File > Settings… (Labmill:open-settings) show a full-page view
//     with a section list (Institution, Clusters, SSH connections, Folders,
//     App, About & diagnostics; role=tablist, arrows/Home/End) and
//     "Back to chat" (Esc outside a form). It talks to SettingsAPI only
//     (plus PageAPI's folder methods); field errors from Go show under their
//     fields; no network when it opens; links stay text;
//     no secret is ever shown. See internal/settings and docs/dev/gui.md.
//   - Light and dark themes via prefers-color-scheme.
//   - The window title is "labmill"; minimum size 640x480.
//
// Bound to the page (window.go.main.*): PageAPI (Send, Cancel, Unqueue,
// JobAction, Decide, Choose, Answer, LoginCode, Jobs, Status, Folders, PickFolder,
// SetFolder, DropFolder, OpenJobFolder, TerminalClusters, OpenTerminal, OpenJobTerminal,
// TerminalInput, TerminalResize, TerminalAck, CloseTerminal, SystemTerminalAvailable, OpenSystemTerminal
// (the Terminal view, M62.2: frontend/terminal.js), StartupMark, SelfTest, SelfTestRelease, SelfTestReport),
// SettingsAPI (the Settings page: Get, SaveSite, SaveCluster,
// RemoveCluster, SaveHost, RemoveHost, SetStartInTray, SetRunWithoutAsking, ResetNotices,
// OpenLogsFolder, Diagnostics) and Setup. gui.App is not bound
// directly, because Wails binds every exported method (that would expose
// Shutdown to the page).
//
// Content-Security-Policy (index.html): default-src 'none'; script-src
// 'self'; style-src 'self'; style-src-elem 'self' 'unsafe-inline' (xterm.js, M62.2); img-src 'self' data:; connect-src 'self';
// base-uri 'none'; form-action 'none'. No inline script or style anywhere.
//
// Self-test (LABMILL_GUI_SELFTEST=1, make gui-selftest): launch.Build is not
// called; a scripted fake session drives the real dialogs and
// frontend/selftest.js checks the real DOM; see selftest.go.
//
// First run: if no configuration exists, a small setup page asks for the
// cluster username (same rules as the TUI's setup) before the main view.
//
// Tray (M22): closing the window hides it while a tray icon is up
// (gui.Lifecycle; tray_*.go, fyne.io/systray on Windows and Linux, the Dock
// on macOS); only the tray's Quit or the macOS app menu's Quit run the
// shutdown (gui.Closer). sshx.Keeper holds the SSH helper meanwhile, a
// single-instance lock makes a second launch show this window, and --tray
// (the opt-in start-at-login entry, internal/autostart) starts hidden. The
// Windows exe icon comes from cmd/labmill-winres. See docs/dev/gui.md.
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wr "github.com/wailsapp/wails/v2/pkg/runtime"

	"labmill-client/assets/icon"
	"labmill-client/internal/app"
	"labmill-client/internal/autostart"
	"labmill-client/internal/config"
	"labmill-client/internal/gui"
	"labmill-client/internal/launch"
	"labmill-client/internal/settings"
	"labmill-client/internal/site"
	"labmill-client/internal/sshx"
	"labmill-client/internal/startup"
	"labmill-client/internal/store"
)

// singleInstanceID names the single-instance lock: a second launch hands
// over to the running app (which shows its window) and exits.
const singleInstanceID = "org.labmill.client.gui"

// autostartEntry is the start-at-login registration (tray checkbox).
func autostartEntry() (autostart.Entry, error) {
	exe, err := os.Executable()
	if err != nil {
		return autostart.Entry{}, err
	}
	return autostart.Entry{Name: "labmill", Label: "org.labmill.client", Exe: exe, Args: []string{"--tray"}}, nil
}

//go:embed frontend
var assets embed.FS

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

// Events that only this command sends (the rest are in internal/gui).
const (
	evReady   = "Labmill:ready"   // {status}: the runtime is up
	evFatal   = "Labmill:fatal"   // {error}: startup failed
	evStartup = "Labmill:startup" // {step}: the startup step now running (launch.Start*)
)

// State is what the page learns at load (and through Labmill:ready/fatal/startup).
type State struct {
	Phase  string `json:"phase"` // "starting", "setup", "ready", "failed"
	Status string `json:"status"`
	Error  string `json:"error"`
	// Step is the startup step now running (launch.StartLoadingSettings,
	// …), shown under the hourglass; "" before the first one.
	Step string `json:"step"`
	// Note is a plain message shown with the setup form: set when
	// clusters.yaml exists but names no home institution (M55).
	Note string `json:"note"`
	// OnlyInstitution: the form asks only for the home institution (the
	// account name and hosts are hidden): clusters.yaml has clusters and
	// just lacks a known `site:`.
	OnlyInstitution bool `json:"onlyInstitution"`
}

// Setup is bound to the page for the first-run form (window.go.main.Setup).
type Setup struct {
	mu        sync.Mutex
	needs     bool
	submitted bool
	answers   chan launch.SetupAnswers
	state     State
	stateSeen atomic.Bool // the page called State (proves app.js ran)
	// liveStatus, when set, gives the latest status text once ready.
	liveStatus func() string
}

func newSetup(needs bool) *Setup {
	s := &Setup{needs: needs, answers: make(chan launch.SetupAnswers, 1)}
	s.state.Phase = "starting"
	if needs {
		s.state.Phase = "setup"
	}
	return s
}

// setNote sets the message shown with the setup form (State.Note).
func (s *Setup) setNote(note string) {
	s.mu.Lock()
	s.state.Note = note
	s.mu.Unlock()
}

// setOnlyInstitution sets State.OnlyInstitution.
func (s *Setup) setOnlyInstitution(only bool) {
	s.mu.Lock()
	s.state.OnlyInstitution = only
	s.mu.Unlock()
}

// NeedsSetup reports whether the first-run username form should be shown.
func (s *Setup) NeedsSetup() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needs && !s.submitted
}

// SiteChoice is one option of the first-run institution choice.
type SiteChoice struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
	// NeedsHosts: the form must also offer the login host field
	// (launch.NeedsHosts).
	NeedsHosts bool `json:"needsHosts"`
	Default    bool `json:"default"` // preselected (site.Default)
}

// Sites lists the institutions for the first-run form (site.Choices: the
// embedded profiles, the default first, then "my institution isn't listed").
func (s *Setup) Sites() []SiteChoice {
	var out []SiteChoice
	def := site.Default().Domain
	for _, c := range site.Choices() {
		out = append(out, SiteChoice{Domain: c.Domain, Name: c.Name, NeedsHosts: launch.NeedsHosts(c), Default: c.Domain == def})
	}
	return out
}

// SubmitUsername answers the first-run form for the default site. It
// fails when the default is the generic profile, which needs hosts: the
// home institution is required (M55).
func (s *Setup) SubmitUsername(name string) error {
	return s.SubmitSetup(site.Default().Domain, name, "")
}

// SubmitSetup answers the first-run form: the chosen site's domain, the
// account name, and (used only when that site has no login host) the login
// host, which is required then: it names the home institution (M55). An
// empty or unknown domain is refused.
func (s *Setup) SubmitSetup(domain, name, loginHost string) error {
	name = strings.TrimSpace(name)
	loginHost = strings.TrimSpace(loginHost)
	s.mu.Lock()
	only := s.state.OnlyInstitution
	s.mu.Unlock()
	if only { // clusters exist already: only the institution is saved
		name, loginHost = "", ""
	} else if !launch.ValidUsername(name) {
		return errors.New("use only lowercase letters, digits, dots, dashes and underscores (1 to 32 characters)")
	}
	if strings.TrimSpace(domain) == "" {
		return errors.New("please choose your home institution first: Labmill can't be set up without one")
	}
	st, err := site.Get(domain)
	if err != nil {
		return errors.New("please choose your institution from the list")
	}
	if !launch.NeedsHosts(st) || only {
		loginHost = ""
	}
	if !only {
		if err := launch.HostsRequired(st, loginHost); err != nil {
			return err
		}
	}
	if loginHost != "" && !launch.ValidLoginHost(loginHost) {
		return errors.New("enter just the login host name, for example login.example.edu")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.needs || s.submitted {
		return errors.New("setup was already submitted")
	}
	s.submitted = true
	s.state.Phase = "starting"
	s.answers <- launch.SetupAnswers{Site: domain, Username: name, LoginHost: loginHost}
	return nil
}

// State returns the current startup state; the page calls it at load so it
// cannot miss an event that fired before it listened.
func (s *Setup) State() State {
	startup.Once("gui: page's first call into Go (Setup.State)")
	s.stateSeen.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	if st.Phase == "ready" && s.liveStatus != nil {
		st.Status = s.liveStatus()
	}
	if s.needs && !s.submitted {
		st.Phase = "setup"
	}
	return st
}

// phase returns the current phase without marking the page as loaded
// (stateSeen), for the tray.
func (s *Setup) phase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Phase
}

func (s *Setup) set(st State) {
	s.mu.Lock()
	if st.Step == "" {
		st.Step = s.state.Step
	}
	s.state = st
	s.mu.Unlock()
}

// setStep records the running startup step (State.Step).
func (s *Setup) setStep(step string) {
	s.mu.Lock()
	s.state.Step = step
	s.mu.Unlock()
}

// setupFunc is the launch.SetupFunc: it blocks until the page submits the
// form.
func (s *Setup) setupFunc(ctx context.Context, _ site.Site) (launch.SetupAnswers, error) {
	select {
	case a := <-s.answers:
		return a, nil
	case <-ctx.Done():
		return launch.SetupAnswers{}, ctx.Err()
	}
}

// gate stands in for the runtime until launch.Build finishes: it is the
// gui.Session and gui.JobLister the App was built with.
type gate struct {
	mu     sync.Mutex
	rt     *launch.Runtime
	sess   gui.Session // rt.Session; a field so tests can install a fake
	closed bool
	ui     *gui.Bridge
	// settled is closed once the runtime was installed, startup failed or
	// the gate was shut down: a Send that arrived earlier waits on it (M46.2).
	settled chan struct{}
	failErr error // set by fail
}

func newGate(ui *gui.Bridge) *gate { return &gate{ui: ui, settled: make(chan struct{})} }

// settle closes settled once; g.mu must be held.
func (g *gate) settle() {
	select {
	case <-g.settled:
	default:
		close(g.settled)
	}
}

// fail records that startup failed; waiting Sends end with err.
func (g *gate) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failErr = err
	g.settle()
}

var errNotReady = errors.New("Labmill is still starting up")

func (g *gate) runtime() *launch.Runtime {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rt
}

// set installs the runtime; it reports false (and installs nothing) when the
// window was already closed, so the caller must close rt itself.
func (g *gate) set(rt *launch.Runtime) bool { return g.install(rt, rt.Session) }

// install is set with the session given separately (tests pass a fake).
func (g *gate) install(rt *launch.Runtime, sess gui.Session) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.rt = rt
	g.sess = sess
	g.settle()
	return true
}

// session returns the installed session, nil before the runtime exists.
func (g *gate) session() gui.Session {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sess
}

// shutdown marks the gate closed and returns the runtime to close, if any.
func (g *gate) shutdown() *launch.Runtime {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	g.settle()
	return g.rt
}

// Send implements gui.Session. The chat is usable before the runtime exists
// (M46.2): a message sent earlier waits here, as a running turn, and goes out
// when the runtime is ready; cancel ends the wait, a failed startup ends the
// turn with its error (shown in the chat).
func (g *gate) Send(ctx context.Context, text string) {
	sess := g.session()
	if sess == nil {
		select {
		case <-g.settled:
		case <-ctx.Done():
			g.ui.Done(ctx.Err())
			return
		}
		var ferr error
		g.mu.Lock()
		sess, ferr = g.sess, g.failErr
		g.mu.Unlock()
		if sess == nil {
			if ferr == nil {
				ferr = errNotReady
			}
			g.ui.Done(ferr)
			return
		}
	}
	sess.Send(ctx, text)
}

// Wait implements gui.Session.
func (g *gate) Wait(timeout time.Duration) bool {
	if sess := g.session(); sess != nil {
		return sess.Wait(timeout)
	}
	return true
}

func (g *gate) jobs(ctx context.Context) ([]store.Job, error) {
	rt := g.runtime()
	if rt == nil {
		return nil, errNotReady
	}
	return rt.Store.Jobs(ctx, store.JobFilter{Limit: 50})
}

// activeJobs returns the submitted, pending and running jobs for the tray's
// status line.
func (g *gate) activeJobs(ctx context.Context) ([]store.Job, error) {
	rt := g.runtime()
	if rt == nil {
		return nil, errNotReady
	}
	return rt.Store.Jobs(ctx, store.JobFilter{States: []store.JobState{store.StateSubmitted, store.StatePending, store.StateRunning}})
}

// PageAPI is the struct bound to the page as window.go.main.PageAPI. It
// forwards to *gui.App and exposes nothing else.
type PageAPI struct {
	app      *gui.App
	g        *gate
	setup    *Setup
	selftest *selfTest // nil unless LABMILL_GUI_SELFTEST=1
	release  func()    // self-test: ends the "starting" phase
	terms    *gui.Terminals
	// cli is the path of the Labmill command-line binary ("" when not found:
	// the "Open in system terminal" option is then hidden).
	cli string
	// termClusters lists the clusters a terminal can open (the self-test
	// swaps in a fake).
	termClusters func() []TerminalCluster
	openSystem   func(cli, cluster string) error
}

// TerminalCluster is a cluster the page can open a terminal on.
type TerminalCluster struct {
	Name string `json:"name"`
	Host string `json:"host"` // the login host alias
}

// TerminalClusters lists the clusters with a login host, for the page's
// Terminal buttons and picker.
func (p *PageAPI) TerminalClusters() []TerminalCluster {
	if p.termClusters != nil {
		return p.termClusters()
	}
	rt := p.g.runtime()
	if rt == nil {
		return []TerminalCluster{}
	}
	out := []TerminalCluster{}
	for _, c := range rt.TerminalClusters() {
		out = append(out, TerminalCluster{Name: c.Name, Host: c.Alias})
	}
	return out
}

// OpenTerminal opens a terminal tab on the cluster, in dir ("" for the home
// folder), and returns its ID; output, state and exit come as Labmill:term-*
// events. cols and rows are xterm.js's size. A sign-in dialog follows if the
// helper holds no login. Nothing typed or shown goes to Claude or any log.
func (p *PageAPI) OpenTerminal(cluster, dir string, cols, rows int) (string, error) {
	return p.terms.Open(cluster, dir, cols, rows)
}

// OpenJobTerminal opens a terminal in the cluster folder of job id (taken
// from the job store, never from the page).
func (p *PageAPI) OpenJobTerminal(id int64, cols, rows int) (string, error) {
	cluster, dir, err := p.app.JobTerminalTarget(id)
	if err != nil {
		return "", err
	}
	return p.terms.Open(cluster, dir, cols, rows)
}

// TerminalInput sends typed bytes (base64) to terminal id; seq numbers the
// calls of that terminal from 0 (calls may arrive out of order).
func (p *PageAPI) TerminalInput(id string, seq int64, data string) error {
	return p.terms.Input(id, seq, data)
}

// TerminalResize tells terminal id its new size in cells; seq numbers the
// calls from 1 (the newest wins).
func (p *PageAPI) TerminalResize(id string, seq int64, cols, rows int) error {
	return p.terms.Resize(id, seq, cols, rows)
}

// TerminalAck confirms the page wrote n bytes of terminal id's output
// (flow control).
func (p *PageAPI) TerminalAck(id string, n int) error { return p.terms.Ack(id, n) }

// CloseTerminal closes terminal id (its tab); the session ends.
func (p *PageAPI) CloseTerminal(id string) error { return p.terms.Close(id) }

// SystemTerminalAvailable reports whether "Open in system terminal" works
// (the Labmill command-line program was found).
func (p *PageAPI) SystemTerminalAvailable() bool { return p.cli != "" }

// OpenSystemTerminal opens the OS terminal running `labmill shell <cluster>`.
// The cluster must be one of TerminalClusters.
func (p *PageAPI) OpenSystemTerminal(cluster string) error {
	for _, c := range p.TerminalClusters() {
		if c.Name == cluster {
			return p.openSystem(p.cli, cluster)
		}
	}
	return fmt.Errorf("no cluster named %q", cluster)
}

// Send starts a turn, or queues the text while one runs (see gui.App.Send).
func (p *PageAPI) Send(text string) error { return p.app.Send(text) }

// Cancel ends the running turn (a queued message is kept).
func (p *PageAPI) Cancel() { p.app.Cancel() }

// Unqueue drops the queued message.
func (p *PageAPI) Unqueue() { p.app.Unqueue() }

// JobAction sends the chat message for a job button (see gui.App.JobAction);
// "open" opens the job's results folder instead.
func (p *PageAPI) JobAction(id int64, action string) error { return p.app.JobAction(id, action) }

// Folders returns the chosen data and code folders and the recent ones.
func (p *PageAPI) Folders() (gui.FolderState, error) { return p.app.Folders() }

// PickFolder opens the native folder dialog for kind ("data" or "code").
func (p *PageAPI) PickFolder(kind string) (gui.FolderState, error) { return p.app.PickFolder(kind) }

// SetFolder sets kind to a recent folder ("" clears it).
//
// Only "" or a folder in that kind's recent list is accepted (see
// gui.App.SetRecentFolder): the dialog, the drops and the terminal have their
// own setters, and the page can't name an arbitrary path through this one.
func (p *PageAPI) SetFolder(kind, path string) (gui.FolderState, error) {
	return p.app.SetRecentFolder(kind, path)
}

// DropFolder sets kind (the user's choice after a drop onto the chat input
// box) to the first dropped path. Only the path is used: nothing is read or
// uploaded.
func (p *PageAPI) DropFolder(kind string, paths []string) (gui.FolderState, error) {
	if len(paths) == 0 {
		return p.app.Folders()
	}
	return p.app.SetFolder(kind, paths[0])
}

// OpenJobFolder opens a fetched job's results folder in the file manager.
func (p *PageAPI) OpenJobFolder(id int64) error { return p.app.OpenJobFolder(id) }

// Decide answers an approval request.
// reason is the optional text typed with a decline (ignored on approve).
func (p *PageAPI) Decide(id string, approve bool, edited, reason string) error {
	err := p.app.Decide(id, approve, edited, reason)
	if p.selftest != nil {
		p.selftest.decided(err, approve, reason)
	}
	return err
}

// Choose answers a choice dialog (ask_user): skip, a typed answer (other),
// or the chosen option's 0-based index.
func (p *PageAPI) Choose(id string, index int, other string, skip bool) error {
	err := p.app.Choose(id, index, other, skip)
	if p.selftest != nil {
		p.selftest.chose(err, index, other, skip)
	}
	return err
}

// Answer answers a login question.
func (p *PageAPI) Answer(id string, text string, cancel bool) error {
	err := p.app.Answer(id, text, cancel)
	if p.selftest != nil {
		p.selftest.answered(err, text, cancel)
	}
	return err
}

// Jobs returns the job list.
func (p *PageAPI) Jobs() ([]gui.JobView, error) { return p.app.Jobs() }

// Status returns the status-bar text once the runtime is ready ("" before).
func (p *PageAPI) Status() string {
	if rt := p.g.runtime(); rt != nil {
		return rt.Status
	}
	return p.setup.State().Status
}

// SelfTest reports whether the app runs in self-test mode (the page then
// loads selftest.js).
func (p *PageAPI) SelfTest() bool { return p.selftest != nil }

// startupSteps are the steps the page may add to the startup timing
// (PageAPI.StartupMark); anything else is ignored.
var startupSteps = map[string]string{
	"bindings": "page: scripts ran, Go bindings available",
	"chat":     "page: chat input usable",
}

var (
	logStartupOnce sync.Once
	chatMarked     atomic.Bool
)

// tryLogStartup writes the timing lines to labmill.log once, when the page
// showed its chat and the runtime exists. The chat comes first now (M46.2),
// so both the "chat" mark and the end of Build call it.
func tryLogStartup(g *gate) {
	if chatMarked.Load() && g.runtime() != nil {
		logStartupOnce.Do(func() { logStartup(g) })
	}
}

// StartupMark records a startup step the page reached (M46.1): "bindings"
// or "chat". The timing lines go to labmill.log when the chat is shown and
// the runtime is up (tryLogStartup).
func (p *PageAPI) StartupMark(step string) {
	name, ok := startupSteps[step]
	if !ok {
		return
	}
	startup.Once(name)
	if step == "chat" {
		chatMarked.Store(true)
		tryLogStartup(p.g)
	}
}

// SelfTestRelease ends the self-test's "starting" phase (self-test mode only).
func (p *PageAPI) SelfTestRelease() {
	if p.selftest != nil && p.release != nil {
		p.release()
	}
}

// SelfTestReport receives the page's verdict (self-test mode only).
func (p *PageAPI) SelfTestReport(passed bool, details string) {
	if p.selftest != nil {
		p.selftest.report(passed, details)
	}
}

// needsSetup reports whether the institution form must be shown first.
func needsSetup() bool {
	p, err := config.DefaultPath()
	if err != nil {
		return false // Build reports the real error
	}
	return launch.NeedsHomeChoice(p) // no config yet, or none names a home institution (M55)
}

func main() {
	startup.Mark("gui: main() entered (time zero to here: on Windows loader and package init; elsewhere only init after the startup package)")
	launch.Version = version
	if timingMode(os.Getenv("LABMILL_GUI_SELFTEST") == "1") {
		selfTestRunID() // fixes the single-instance name before HOME changes
		timingHome()    // before anything reads a per-user folder
	}

	// sshx re-runs this executable as the SSH connection helper, and rclone
	// runs it as its SSH transport (ssh-exec): handle those before starting
	// any window.
	if launch.HelperSubcommand(os.Args[1:]) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		code := launch.RunSubcommand(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}

	// The start splash (M53.1), as early as possible: on Windows the tray's
	// message loop ends the app-starting cursor at once and the window is
	// only there a few hundred milliseconds later (the first start of a new
	// exe: seconds). Not for a start that hides in the tray (--tray; also
	// when a setup form would show), the tests, or a second launch that only
	// hands over: the single-instance mutex is probed here (microseconds) so
	// that launch never flashes a splash. It closes when the page has loaded
	// (OnDomReady) or the window is shown from the tray.
	closeSplash := func() {}
	{
		c := splashCase{
			Tray:     slices.Contains(os.Args[1:], "--tray"),
			SelfTest: os.Getenv("LABMILL_GUI_SELFTEST") == "1",
			Smoke:    os.Getenv("LABMILL_GUI_SMOKE") == "1",
		}
		c.SecondInstance = !c.SelfTest && !c.Smoke && otherInstanceRunning(singleInstanceID)
		if splashWanted(c) {
			closeSplash = startSplash(splashGeom{Window: savedWindowGeom()})
		}
	}

	// The emitter needs the Wails context, which exists only after startup;
	// nothing can emit before that (no turn runs before the page loads), and
	// nothing after shutdown began.
	var (
		ectx    context.Context
		eclosed bool
		emu     sync.RWMutex
	)
	// observe sees every event in self-test mode (the self-test checks that
	// a typed key passphrase never leaves Go in an event).
	var observe func(name string, data any)
	emit := func(name string, data any) {
		if observe != nil {
			observe(name, data)
		}
		emu.RLock()
		defer emu.RUnlock()
		if ectx != nil && !eclosed {
			wr.EventsEmit(ectx, name, data)
		}
	}
	// hiddenInTray is true while the window is hidden; an approval, login
	// question or choice dialog then shows it (gui.WakeOnAttention).
	var hiddenInTray atomic.Bool
	var showWindow func()
	bridge := gui.NewBridge(gui.WakeOnAttention(emit, hiddenInTray.Load, func() { showWindow() }))
	g := newGate(bridge)
	selftestOn := os.Getenv("LABMILL_GUI_SELFTEST") == "1"
	var st *selfTest
	var sess gui.Session = g
	jobs := g.jobs
	if selftestOn {
		st = newSelfTest(bridge)
		observe = st.observe
		sess = st
		jobs = st.jobs
	}
	gapp := gui.NewApp(bridge, sess, jobs, "")
	// The folder menu and "Open folder" (M20, M24). The dialog needs the
	// Wails context, which exists once the page is up (the menu is only
	// shown then).
	var wctx atomic.Pointer[context.Context]
	folders := gui.FolderHost{
		Env: func() *app.Env {
			if rt := g.runtime(); rt != nil {
				return rt.Env
			}
			return nil
		},
		Pick: func(title, start string) (string, error) {
			c := wctx.Load()
			if c == nil {
				return "", errNotReady
			}
			if fi, err := os.Stat(start); err != nil || !fi.IsDir() {
				start = ""
			}
			return wr.OpenDirectoryDialog(*c, wr.OpenDialogOptions{Title: title, DefaultDirectory: start, CanCreateDirectories: true})
		},
		Open: gui.OpenFolder,
	}
	if st != nil {
		folders = st.folderHost()
	}
	gapp.SetFolderHost(folders)
	setup := newSetup(!selftestOn && needsSetup())
	if setup.NeedsSetup() {
		if p, err := config.DefaultPath(); err == nil {
			if _, lerr := config.Load(p); lerr == nil {
				only := launch.OnlyInstitution(p)
				setup.setNote(launch.HomeNote(only)) // a config without a home institution
				setup.setOnlyInstitution(only)
			}
		}
	}
	page := &PageAPI{app: gapp, g: g, setup: setup, selftest: st}
	// The terminal tabs (M62.2). Their bytes are the user's own and go to
	// the page only.
	termRun := func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		rt := g.runtime()
		if rt == nil {
			return 0, errNotReady
		}
		return rt.RunTerminal(ctx, cluster, opts, connected, allowSignIn)
	}
	if st != nil {
		termRun = st.terminalRunner
		page.termClusters = st.terminalClusters
	}
	page.terms = gui.NewTerminals(emit, termRun)
	page.terms.SetBusy(gapp.Busy) // no sign-in dialog from a terminal while a turn runs
	page.openSystem = gui.OpenSystemTerminal
	if st != nil {
		page.cli = "/selftest/labmill"
		page.openSystem = st.openSystemTerminal
	} else if exe, err := os.Executable(); err == nil {
		page.cli = gui.FindCLI(exe)
	}

	bctx, bcancel := context.WithCancel(context.Background())
	smoke := os.Getenv("LABMILL_GUI_SMOKE") == "1"
	testMode := selftestOn || smoke

	// Desktop-app settings (gui.json next to clusters.yaml). The self-test
	// never touches the real file (on Windows it runs with the real
	// profile): it starts from defaults and writes next to its executable.
	prefsPath, perr := gui.PrefsPath()
	var prefs gui.Prefs
	switch {
	case selftestOn:
		prefsPath = ""
		if exe, err := os.Executable(); err == nil {
			prefsPath = filepath.Join(filepath.Dir(exe), "labmill-selftest-gui.json")
		}
		// A stored window geometry for the restore check (M47.1).
		g := selfTestGeom
		prefs.Window = &g
	case perr != nil:
		prefsPath = ""
		fmt.Fprintln(os.Stderr, "labmill-gui:", perr)
	default:
		if prefs, perr = gui.LoadPrefs(prefsPath); perr != nil {
			fmt.Fprintln(os.Stderr, "labmill-gui:", perr)
		}
	}
	var prefsMu sync.Mutex
	savePrefs := func(change func(*gui.Prefs)) {
		prefsMu.Lock()
		defer prefsMu.Unlock()
		if prefsPath != "" {
			// Only setRunWithoutAsking changes that key: a hand edit of
			// the file (terminal users) must not be undone by an
			// unrelated save of the in-memory copy.
			prefs.RunWithoutAsking = settings.AutoRunRemote(prefsPath)
			prefs.ApproveAll = settings.AutoApproveAll(prefsPath)
			prefs.AutoDownload = settings.AutoDownloadResults(prefsPath)
		}
		change(&prefs)
		if prefsPath != "" {
			if err := gui.SavePrefs(prefsPath, prefs); err != nil {
				fmt.Fprintln(os.Stderr, "labmill-gui:", err)
			}
		}
	}

	// The window geometry (M47.1): restored in OnStartup, saved by geom.
	geom := newGeomWatcher(func() context.Context { return *wctx.Load() }, savePrefs, hiddenInTray.Load)
	var pendingMax atomic.Bool // maximise at the first show (a tray start stays hidden)
	startW, startH := winWidth, winHeight
	var startGeom *gui.WindowGeom // the saved geometry, before any save changes prefs
	if prefs.Window != nil {
		c := *prefs.Window
		startGeom = &c
	}
	if startGeom != nil && startGeom.Valid() {
		// The size the window is created with, so it doesn't open at the
		// default and jump; OnStartup applies the full restore.
		startW, startH = max(startGeom.Width, winMinWidth), max(startGeom.Height, winMinHeight)
	}

	// keeper holds the SSH helper while the app runs (shown or hidden in the
	// tray), so its logins never idle-expire under the app; after quit the
	// helper's normal 10-minute expiry applies. It never logs in.
	keeper := &sshx.Keeper{}

	var shutdownRan atomic.Bool
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			shutdownRan.Store(true)
			bcancel() // also ends the keeper's hold
			emu.Lock()
			eclosed = true
			emu.Unlock()
			page.terms.CloseAll()
			gapp.Shutdown()
			if rt := g.shutdown(); rt != nil {
				rt.Close()
			}
		})
	}

	// OnBeforeClose must return at once (on Windows it runs on the UI
	// thread, and Session.Wait can take up to 10 s), and runtime.Quit calls
	// it again, so the sequence lives in gui.Closer; see there.
	closer := &gui.Closer{
		Hide:     func() { wr.WindowHide(*wctx.Load()) },
		Shutdown: shutdown,
		Quit:     func() { wr.Quit(*wctx.Load()) },
		// A shutdown stuck past this (e.g. an SSH connect ignoring
		// cancellation) exits the hidden process; SQLite's WAL survives it.
		Exit:    func() { os.Exit(1) },
		Timeout: 30 * time.Second,
	}

	// Closing the window hides it to the tray while a tray icon is up; only
	// the tray's Quit (or the macOS app menu's Quit) really quits.
	var trayRef atomic.Pointer[tray]
	life := &gui.Lifecycle{
		Closer: closer,
		// The self-test exercises the hide path even where the desktop has
		// no tray (WSLg).
		Tray: func() bool { return selftestOn || trayRef.Load().Active() },
		Hide: func(first bool) {
			ctx := *wctx.Load()
			if first {
				bridge.AppNotice(gui.TrayNotice)
				savePrefs(func(p *gui.Prefs) { p.TrayNoticeShown = true })
				if !testMode {
					// Shown before hiding, so the box has a visible owner.
					wr.MessageDialog(ctx, wr.MessageDialogOptions{Type: wr.InfoDialog, Title: "Labmill keeps running", Message: gui.TrayNotice})
				}
			}
			geom.Flush() // before hiding: a hidden window isn't read
			hiddenInTray.Store(true)
			wr.WindowHide(ctx)
			if st != nil {
				st.hidden(first)
			}
		},
	}
	if prefs.TrayNoticeShown {
		life.SetNoticeShown()
	}
	if st != nil {
		st.life, st.shutdownStarted, st.prefsPath = life, shutdownRan.Load, prefsPath
		st.trayUp = func() bool { return trayRef.Load().Active() }
		st.shownState = func() bool { return !hiddenInTray.Load() }
	}

	// pendingShow: the tray asked for the window before Wails was up (the
	// tray starts first); OnStartup shows it then.
	var pendingShow atomic.Bool
	showWindow = func() {
		hiddenInTray.Store(false)
		if selftestHidden(selftestOn) {
			return // the Windows self-test never shows a window (desktop stays undisturbed); only the state above changes
		}
		c := wctx.Load()
		if c == nil {
			pendingShow.Store(true)
			if c = wctx.Load(); c == nil || !pendingShow.Swap(false) { // OnStartup may have run in between
				return
			}
		}
		if pendingMax.Swap(false) {
			wr.WindowMaximise(*c)
		}
		wr.WindowShow(*c)
		wr.WindowUnminimise(*c)
		closeSplash()
	}
	// pendingQuit: Quit from the tray before Wails was up; OnStartup quits
	// then. quitApp reports whether it acted (the tray menu keeps running
	// when it didn't).
	var pendingQuit atomic.Bool
	quitApp := func() bool {
		if wctx.Load() != nil {
			life.Quit()
			return true
		}
		pendingQuit.Store(true)
		if wctx.Load() != nil && pendingQuit.Swap(false) { // OnStartup ran in between
			life.Quit()
			return true
		}
		return false
	}
	startAtLogin := func(on bool) error {
		e, err := autostartEntry()
		if err == nil {
			if on {
				err = autostart.Enable(e)
			} else {
				err = autostart.Disable(e)
			}
		}
		if err != nil {
			bridge.AppNotice("Couldn't change \"Start in the tray when I log in\": " + err.Error())
			return err
		}
		savePrefs(func(p *gui.Prefs) { p.StartInTray = on })
		return nil
	}
	trayStatus := func() string {
		if selftestOn {
			r, w := gui.CountJobs(selfJobs)
			return gui.TrayStatus(r, w, nil)
		}
		jobs, err := g.activeJobs(bctx)
		if err != nil {
			if setup.phase() == "failed" {
				return "Labmill couldn't start: open the window for details"
			}
			return gui.TrayStarting
		}
		r, w := gui.CountJobs(jobs)
		aliases, _ := keeper.LoggedIn()
		return gui.TrayStatus(r, w, aliases)
	}

	// The Settings page (M29). The self-test uses its own throwaway files.
	setPaths, sperr := settings.DefaultPaths()
	if st != nil {
		setPaths = st.settingsPaths()
	} else if sperr != nil {
		fmt.Fprintln(os.Stderr, "labmill-gui:", sperr)
	}
	settingsAPI := &SettingsAPI{
		svc:     &settings.Service{Paths: setPaths},
		app:     gapp,
		version: version,
		brokerURL: func() string {
			if rt := g.runtime(); rt != nil {
				return rt.Site.BrokerURL
			}
			return ""
		},
		loggedIn: keeper.LoggedIn,
		prefs: func() gui.Prefs {
			prefsMu.Lock()
			defer prefsMu.Unlock()
			return prefs
		},
		setStartInTray: func(on bool) error {
			if err := startAtLogin(on); err != nil {
				return err
			}
			trayRef.Load().SetStartAtLogin(on)
			return nil
		},
		setRunWithoutAsking: func(on bool) error {
			// Unlike the other prefs, a failed save is an error and the
			// in-memory value goes back: the page must show what the agent
			// (which reads the file) will do.
			prefsMu.Lock()
			defer prefsMu.Unlock()
			old := prefs.RunWithoutAsking
			prefs.ApproveAll = settings.AutoApproveAll(prefsPath)
			prefs.AutoDownload = settings.AutoDownloadResults(prefsPath)
			prefs.RunWithoutAsking = on
			if prefsPath == "" {
				prefs.RunWithoutAsking = old
				return errors.New("Labmill can't find where to save its settings")
			}
			if err := gui.SavePrefs(prefsPath, prefs); err != nil {
				prefs.RunWithoutAsking = old
				return err
			}
			return nil
		},
		setApproveAll: func(on bool) error {
			// "Approve everything automatically" (M43.2): the agent reads
			// the file at every approval, so a saved change applies at once.
			prefsMu.Lock()
			defer prefsMu.Unlock()
			if err := settings.SetAutoApproveAll(prefsPath, on); err != nil {
				return err
			}
			prefs.ApproveAll = on
			return nil
		},
		setAutoDownload: func(on bool) error {
			// "Download results automatically" (M60.1): the job monitor
			// reads the file whenever a job ends.
			prefsMu.Lock()
			defer prefsMu.Unlock()
			if err := settings.SetAutoDownloadResults(prefsPath, on); err != nil {
				return err
			}
			prefs.AutoDownload = on
			return nil
		},
		jobsWidth: func() float64 {
			prefsMu.Lock()
			defer prefsMu.Unlock()
			return prefs.JobsWidth
		},
		setJobsWidth: func(f float64) error {
			savePrefs(func(p *gui.Prefs) { p.JobsWidth = f })
			return nil
		},
		resetNotices: func() error {
			savePrefs(func(p *gui.Prefs) { p.TrayNoticeShown = false })
			life.ResetNotice()
			return nil
		},
		openFolder: gui.OpenFolder,
		planMore: func(name string) (launch.HomeClusterPlan, error) {
			rt := g.runtime()
			if rt == nil {
				return launch.HomeClusterPlan{}, errNotReady
			}
			return rt.PlanHomeCluster(name)
		},
		checkMore: func(name, username string) error {
			rt := g.runtime()
			if rt == nil {
				return errNotReady
			}
			return rt.CheckHomeCluster(name, username)
		},
		addMore: func(ctx context.Context, name, username string) (string, error) {
			rt := g.runtime()
			if rt == nil {
				return "", errNotReady
			}
			return rt.AddHomeCluster(ctx, name, username)
		},
		emit: emit,
	}
	if st != nil {
		st.wireSettings(settingsAPI)
	}
	// openSettings shows the window on the Settings page (tray menu, macOS
	// File menu).
	openSettings := func() {
		showWindow()
		emit(evOpenSettings, map[string]string{})
	}

	// --tray (the start-at-login entry) starts hidden in the tray, but only
	// when a tray can bring the window back and no setup form is needed.
	startHidden := !testMode && slices.Contains(os.Args[1:], "--tray") && trayAvailable() && !setup.NeedsSetup()
	hiddenInTray.Store(startHidden)

	// A second launch shows the running app's window instead of starting
	// another app (and another set of logins). Not in test modes, so a test
	// run never hands over to a real running app.
	var single *options.SingleInstanceLock
	switch {
	case selftestOn:
		// Its own lock name: make gui-selftest launches a second self-test
		// app, which must hand over to this one and exit. The name is per
		// run (HOME and the executable's folder, which the second launch
		// shares), so self-tests running in parallel (worktrees) never hand
		// over to each other.
		single = &options.SingleInstanceLock{
			UniqueId: singleInstanceID + ".selftest." + selfTestRunID(),
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if wctx.Load() != nil {
					showWindow()
				}
				st.secondLaunch()
			},
		}
	case !testMode:
		single = &options.SingleInstanceLock{
			UniqueId: singleInstanceID,
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if wctx.Load() == nil {
					return
				}
				showWindow()
				// Windows may refuse to bring a background app to the front;
				// a short always-on-top raises it.
				ctx := *wctx.Load()
				wr.WindowSetAlwaysOnTop(ctx, true)
				wr.WindowSetAlwaysOnTop(ctx, false)
			},
		}
	}

	// The tray icon comes up first, before the window (it needs nothing from
	// Wails), with the tooltip "Labmill is starting…" until the runtime is
	// ready (kickTray refreshes it at once then). Its Quit does nothing until
	// the window exists.
	trayKick := make(chan struct{}, 1)
	kickTray := func() {
		select {
		case trayKick <- struct{}{}:
		default:
		}
	}
	trayDone := make(chan struct{}) // closed once startTray returned
	// A second Windows launch is about to be exited by Wails' single-instance
	// check inside Run; an early tray would stay behind as a ghost icon.
	secondProcess := single != nil && otherInstanceRunning(single.UniqueId)
	if secondProcess && selftestOn {
		st.noTray()
	}
	if smoke || secondProcess {
		close(trayDone)
	} else {
		go func() {
			endTray := startup.Span("gui: tray icon start (parallel to the window)")
			t := startTray(trayHooks{Open: showWindow, Settings: openSettings, Quit: quitApp, StartAtLogin: startAtLogin, StartAtLoginOn: autostartOn()})
			endTray()
			trayRef.Store(t)
			close(trayDone)
			for {
				t.SetStatus(trayStatus())
				select {
				case <-bctx.Done():
					return
				case <-trayKick:
				case <-time.After(10 * time.Second):
				}
			}
		}()
	}

	// startBuild runs launch.Build in the background (once): the keeper and
	// the runtime. The page needn't wait for it (M46.2): the chat shows as
	// soon as its bindings are up and a message sent earlier waits in the
	// gate. Events emitted before the window exists are dropped; the page
	// reads State when it loads.
	var buildOnce sync.Once
	var timingEarly bool // the self-test's timing Build already started
	startBuild := func() {
		buildOnce.Do(func() {
			go keeper.Run(bctx)
			go func() {
				// Each startup step goes to the page (the status line under
				// the chat) and into State for a page that loads later.
				progress := func(step string) {
					setup.setStep(step)
					emit(evStartup, map[string]string{"step": step})
				}
				rt, err := launch.BuildWithProgress(bctx, bridge, setup.setupFunc, progress)
				if err != nil {
					setup.set(State{Phase: "failed", Error: err.Error()})
					g.fail(err)
					emit(evFatal, map[string]string{"error": err.Error()})
					kickTray()
					return
				}
				if bctx.Err() != nil || !g.set(rt) {
					rt.Close() // the window closed while Build ran
					return
				}
				setup.mu.Lock()
				setup.liveStatus = rt.CurrentStatus
				setup.mu.Unlock()
				setup.set(State{Phase: "ready", Status: rt.Status})
				startup.Mark("gui: ready event sent to the page")
				emit(evReady, map[string]string{"status": rt.CurrentStatus()})
				kickTray()
				tryLogStartup(g)
			}()
		})
	}
	// On Windows the runtime starts before the webview is created (0.3-0.6 s
	// of overlap): the single-instance check is known there, so a second
	// launch, which Wails exits, starts nothing. Elsewhere it starts in
	// OnStartup, after Wails' own single-instance check. Known race: two
	// launches within the same few milliseconds can both pass
	// otherInstanceRunning before either has created Wails' mutex in
	// SetupSingleInstance (Labmill can't take that mutex itself). The loser then
	// starts a Build and a tray that Wails exits right away; the side effects
	// are harmless (a read-only startup, the shared SSH helper, a tray icon
	// that goes with the process).
	if runtime.GOOS == "windows" && !selftestOn && !secondProcess {
		startBuild()
	}
	// The self-test's timing Build starts at the same point, so its numbers
	// show what the app does.
	timingEarly = runtime.GOOS == "windows" && timingMode(selftestOn) && !secondProcess
	if timingEarly {
		go keeper.Run(bctx)
		runTimingBuild(bctx, bridge)
	}

	startup.Mark("gui: window options ready, calling wails.Run")
	err := wails.Run(&options.App{
		Title:       mainTitle,
		Width:       startW,
		Height:      startH,
		MinWidth:    winMinWidth,
		MinHeight:   winMinHeight,
		AssetServer: &assetserver.Options{Assets: assets},
		// Wails file drop gives the page the paths of folders dropped onto
		// the folder buttons (the only elements with --wails-drop-target;
		// app.js ignores drops anywhere else). The webview's own drop stays
		// on because WebView2 delivers no drop at all without it, and app.js
		// cancels every dragover and drop, so the page never navigates to a
		// dropped file and nothing is read or uploaded.
		DragAndDrop:        &options.DragAndDrop{EnableFileDrop: true},
		Bind:               []any{page, setup, settingsAPI},
		StartHidden:        startHidden || selftestHidden(selftestOn),               // the Windows self-test runs without any visible window
		HideWindowOnClose:  nativeHideOnClose,                                       // macOS: the close button hides the app, the Dock icon shows it again
		Menu:               appMenu(func() { wr.Hide(*wctx.Load()) }, openSettings), // macOS: File > Settings… and Close Window (Cmd+W, hides too); nil elsewhere
		SingleInstanceLock: single,
		// The Windows window icon comes from the exe resource (cmd/labmill-winres).
		Linux: &linux.Options{Icon: icon.App256PNG, ProgramName: "labmill"},
		// One WebView2 profile folder for every start (M46.2); "" elsewhere.
		Windows: &windows.Options{WebviewUserDataPath: webviewUserDataPath()},
		OnStartup: func(ctx context.Context) {
			startup.Mark("gui: wails OnStartup (window and webview created)")
			wctx.Store(&ctx)
			emu.Lock()
			ectx = ctx
			emu.Unlock()
			// Where the user left the window (M47.1).
			now, wantMax := applyGeom(ctx, startGeom, !hiddenInTray.Load())
			pendingMax.Store(wantMax && hiddenInTray.Load())
			geom.Seed(now)
			startup.Mark("gui: window geometry restored")
			go geom.Run(bctx.Done(), geomPoll)
			if st != nil {
				st.geomRead = func(context.Context) (gui.WindowGeom, bool) { return geom.Read() }
			}
			if pendingShow.Swap(false) {
				showWindow()
			}
			if pendingQuit.Swap(false) {
				life.Quit()
			}
			if startHidden {
				go func() {
					<-trayDone
					if !trayRef.Load().Active() {
						// The tray never came up (startTray gave up, or the
						// shell wasn't ready at logon): nothing could bring
						// the hidden window back.
						showWindow()
					}
				}()
			}
			if selftestOn {
				// The self-test stays in the "starting" phase until the page
				// calls SelfTestRelease, so it can check the chat that is
				// shown before the runtime is ready (M46.2).
				setup.setStep("Opening the job database…")
				page.release = func() {
					bridge.StatusLine("self-test (status event first)") // the page must prefer it over the ready text
					setup.set(State{Phase: "ready", Status: "self-test"})
					emit(evReady, map[string]string{"status": "self-test"})
				}
				go st.watch(ctx)
				if timingMode(true) && !timingEarly {
					go keeper.Run(bctx)
					runTimingBuild(bctx, bridge)
				}
				return
			}
			startBuild() // a no-op when it already started before the window (Windows)
		},
		OnDomReady: func(ctx context.Context) {
			startup.Once("gui: wails OnDomReady (frontend loaded)")
			closeSplash() // the window is up with its page: the splash has done its job
			if smoke {
				go func() { time.Sleep(3 * time.Second); life.Quit() }()
			}
		},
		OnBeforeClose: func(ctx context.Context) bool {
			wctx.Store(&ctx)
			geom.Flush() // a quit from a shown window; Hide above covers the tray
			return life.BeforeClose()
		},
		OnShutdown: func(context.Context) { closeSplash(); shutdown() },
	})
	closeSplash() // also when Run failed before the page loaded
	trayRef.Load().Stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "labmill-gui:", err)
		os.Exit(1)
	}
	switch {
	case selftestOn:
		os.Exit(st.finish(shutdownRan.Load()))
	case smoke && !setup.stateSeen.Load():
		fmt.Fprintln(os.Stderr, "labmill-gui: smoke test failed: the page never called Setup.State (app.js did not run)")
		os.Exit(3)
	}
}

// selfTestRunID is a short hash of $HOME and the executable's folder: the
// self-test's single-instance lock is per run.
// The timing mode (timingHome) changes HOME, so main takes the ID first.
var selfTestRunIDOnce = sync.OnceValue(computeSelfTestRunID)

func selfTestRunID() string { return selfTestRunIDOnce() }

func computeSelfTestRunID() string {
	exe, _ := os.Executable()
	sum := sha256.Sum256([]byte(os.Getenv("HOME") + "\x00" + os.Getenv("USERPROFILE") + "\x00" + filepath.Dir(exe)))
	return hex.EncodeToString(sum[:6])
}

// autostartOn reports whether the start-at-login entry is registered (the
// tray checkbox's initial state).
func autostartOn() bool {
	e, err := autostartEntry()
	if err != nil {
		return false
	}
	on, _ := autostart.Enabled(e)
	return on
}

// selftestHidden reports whether the self-test must run without showing its
// window. That is Windows, where the loop's test would otherwise pop up on
// the owner's desktop and take focus; Linux runs on a hidden X server.
func selftestHidden(selftestOn bool) bool { return selftestOn && runtime.GOOS == "windows" }
