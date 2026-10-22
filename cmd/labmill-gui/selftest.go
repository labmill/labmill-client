//go:build desktop

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wr "github.com/wailsapp/wails/v2/pkg/runtime"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/gui"
	"labmill-client/internal/launch"
	"labmill-client/internal/settings"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
	"labmill-client/internal/startup"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// selfTimeout is how long the page has to report before the run fails.
const selfTimeout = 45 * time.Second

// selfTest is the in-webview self-test (LABMILL_GUI_SELFTEST=1). It is the
// gui.Session of the App: a scripted conversation that goes through the real
// gui.Bridge, while frontend/selftest.js drives and checks the real DOM and
// reports with PageAPI.SelfTestReport. This side counts what reached Go.
// selfTestGeom is the window geometry the self-test stores in its own
// gui.json before the start and expects to find restored.
var selfTestGeom = gui.WindowGeom{X: 120, Y: 90, Width: 820, Height: 610}

type selfTest struct {
	b *gui.Bridge

	// geomRead reads the window's geometry (set in OnStartup); the restore
	// check (M47.1) compares it with selfTestGeom.
	geomRead func(context.Context) (gui.WindowGeom, bool)

	mu         sync.Mutex
	decides    int
	approved   bool // the first Decide
	declined   bool // the second Decide was approve=false
	reasonSent string
	answers    int
	answerText string
	chooses    int
	chosenIdx  int
	chosenSkip bool
	chooseOK   bool     // the session saw option 2 (index 1) chosen
	sessOK     bool     // the script saw Approved and "pw"
	declineOK  bool     // the session saw the second decision declined with the typed reason
	turns      []string // the text of every turn, in order
	reported   chan struct{}
	passed     bool
	details    string
	code       int

	// The tray checks (M22): main sets these before the window opens.
	life            *gui.Lifecycle
	shutdownStarted func() bool
	trayUp          func() bool // a real tray icon is up (Windows; Linux with a tray host)
	prefsPath       string
	hides           chan bool // first flag of each tray hide
	second          atomic.Bool
	shownState      func() bool // the app's internal "window is shown" state (not hidden in the tray)

	// The folder checks (M20): a throwaway home next to the executable with
	// code, data, a hidden folder and a fetched job's results folder; the
	// dialog and the opener are stubs.
	env        *app.Env
	codeDir    string
	dataDir    string
	fetchedDir string
	folderErr  error             // setting up the folders failed
	pickTitles []string          // the titles of the stub folder dialog's calls
	opened     []string          // folders the stub opener was asked to open
	prompts    map[string]string // the workspace prompt at the start of each turn

	leaks []string // events whose payload contained the key passphrase

	// The SSH key setup checks (M38.6): the answers the three questions got
	// (create, passphrase, upload).
	keyAnswers []string

	// The Settings checks (M29): throwaway clusters.yaml and SSH files next
	// to the executable, and stubs for everything that would reach outside.
	settingsSvc      *settings.Service
	settingsErr      error     // writing the fixture files failed
	startInTray      []bool    // what "Start in the tray" was set to
	runWithoutAsking []bool    // what "Run commands on the cluster without asking" was set to
	jobsWidth        float64   // the divider's saved width (M51.1)
	jobsWidthSaves   []float64 // what SetJobsWidth received
	autoDownload     []bool    // what "Download results automatically" was set to (M60.1)
	approveAll       []bool    // what "Approve everything automatically" was set to (M43.2)
	autoSecretOK     bool      // the auto-approve turn: the plain proposal was approved at once, the secret one waited for the page
	autoSecretAsked  bool      // the page answered the secret read's dialog
	noticeResets     int
	moreAdded        []string // "name/account" of each "More clusters at your institution" Add (M57.2)
}

// The Settings fixture: a clusters.yaml with comments (they must survive a
// save), Labmill's SSH file and the user's own (read-only) SSH file.
const (
	selfClustersYAML = `# self-test clusters (this comment must survive)
site: generic
clusters:
  # the HPC cluster
  - name: selftest-hpc
    type: slurm
    ssh_host: selftest-login # the login host
    transfer: sftp
  - name: selftest-gpu
    type: slurm
    ssh_host: selftest-login
`
	selfLabmillSSH = `# Managed by Labmill. Edit with care: Labmill may rewrite this file.

Host selftest-login
    HostName login.selftest.example
    User selftest
    ServerAliveInterval 30

Host selftest-spare
    HostName spare.selftest.example
    User selftest

Host selftest-inner
    HostName inner.selftest.example
    User selftest
    ProxyJump selftest@selftest-login:2222
`
	selfUserSSH = `Include config.d/labmill

Host selftest-mine
    HostName mine.selftest.example
`
	// selfBrokerURL carries user info and a query that the page must never
	// show (settings strips them).
	selfBrokerURL = "https://selftest:hunter2@broker.selftest.example/v1?sig=SELFTESTSIG"
)

// settingsPaths writes the Settings fixture (overwriting the files of an
// earlier run; backups of earlier runs stay, nothing is deleted) under
// <executable dir>/labmill-selftest-folders/settings and returns its paths.
func (t *selfTest) settingsPaths() settings.Paths {
	base := "labmill-selftest-folders"
	if exe, err := os.Executable(); err == nil {
		base = filepath.Join(filepath.Dir(exe), base)
	}
	base, _ = filepath.Abs(filepath.Join(base, "settings"))
	p := settings.Paths{
		Config: filepath.Join(base, "config", "clusters.yaml"),
		SSH:    sshconfig.Paths{SSHDir: filepath.Join(base, "ssh")},
		Prefs:  t.prefsPath,
		State:  filepath.Join(base, "state", "labmill.db"),
		Log:    filepath.Join(base, "state", "labmill.log"),
	}
	for _, f := range []struct{ path, data string }{
		{p.Config, selfClustersYAML}, {p.SSH.LabmillConfig(), selfLabmillSSH}, {p.SSH.UserConfig(), selfUserSSH}, {p.Log, ""},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			t.settingsErr = err
			continue
		}
		if err := os.WriteFile(f.path, []byte(f.data), 0o600); err != nil {
			t.settingsErr = err
		}
	}
	return p
}

// wireSettings replaces every hook of the Settings page that would reach
// outside the self-test with a stub: no autostart entry, no network, no
// folder opened.
func (t *selfTest) wireSettings(s *SettingsAPI) {
	t.settingsSvc = s.svc
	s.brokerURL = func() string { return selfBrokerURL }
	s.loggedIn = func() ([]string, bool) { return []string{"selftest-login"}, true }
	s.setStartInTray = func(on bool) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.startInTray = append(t.startInTray, on)
		return nil
	}
	s.setRunWithoutAsking = func(on bool) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.runWithoutAsking = append(t.runWithoutAsking, on)
		return nil
	}
	s.setApproveAll = func(on bool) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.approveAll = append(t.approveAll, on)
		return settings.SetAutoApproveAll(t.prefsPath, on)
	}
	s.setAutoDownload = func(on bool) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.autoDownload = append(t.autoDownload, on)
		return settings.SetAutoDownloadResults(t.prefsPath, on)
	}
	s.jobsWidth = func() float64 {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.jobsWidth
	}
	s.setJobsWidth = func(f float64) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.jobsWidthSaves = append(t.jobsWidthSaves, f)
		t.jobsWidth = f
		return nil
	}
	s.resetNotices = func() error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.noticeResets++
		return nil
	}
	s.openFolder = func(dir string) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.opened = append(t.opened, dir)
		return nil
	}
	// "More clusters at your institution" (M57.2): the fixture's profile has
	// one cluster more than the fixture clusters.yaml; the add is recorded,
	// not written.
	s.svc.Profile = func(*config.Config) site.Site {
		return site.Site{Domain: "selftest.example", Clusters: []config.Cluster{{Name: "selftest-hpc", Type: config.TypeSlurm}, {Name: "selftest-more", Type: config.TypeSlurm}}}
	}
	s.checkMore = func(name, username string) error {
		if !launch.ValidUsername(username) {
			return errors.New("enter your account name there")
		}
		return nil
	}
	s.planMore = func(name string) (launch.HomeClusterPlan, error) {
		return launch.HomeClusterPlan{Cluster: name, Type: "slurm", Username: "selftest",
			Hosts: []string{"more.selftest.example (login): password and two-factor sign-in"}, Files: []string{"clusters.yaml"}}, nil
	}
	s.addMore = func(_ context.Context, name, username string) (string, error) {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.moreAdded = append(t.moreAdded, name+"/"+username)
		return "Added the cluster " + name + " (self-test).", nil
	}
}

// Secrets the page types in the key setup; they must never be in an event.
const (
	selfKeyPass = "SELFPASSPHRASE"
	// selfPubKey is the public key the key-setup dialog shows (a throwaway
	// value, not a real key).
	selfPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISelfTestOnlyNotARealKey000000000000000000 labmill-selftest"
)

// observe sees every event the Bridge emits (self-test mode): the key
// passphrase must never be in one.
func (t *selfTest) observe(name string, data any) {
	b, err := json.Marshal(data)
	if err != nil || !strings.Contains(string(b), selfKeyPass) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaks = append(t.leaks, name)
}

func newSelfTest(b *gui.Bridge) *selfTest {
	t := &selfTest{b: b, reported: make(chan struct{}), code: 2, hides: make(chan bool, 4), prompts: map[string]string{}, jobsWidth: 0.30}
	t.setupFolders()
	return t
}

// setupFolders creates (or reuses: nothing is ever deleted) the self-test's
// folders under <executable dir>/labmill-selftest-folders and the Env the
// folder menu uses: home with code/, data/, .hidden/ and
// labmill-results/selftest-fetched/. The Workspace keeps no file.
func (t *selfTest) setupFolders() {
	base := "labmill-selftest-folders"
	if exe, err := os.Executable(); err == nil {
		base = filepath.Join(filepath.Dir(exe), base)
	}
	home := filepath.Join(base, "home")
	if real, err := filepath.Abs(home); err == nil {
		home = real
	}
	t.codeDir = filepath.Join(home, "code")
	t.dataDir = filepath.Join(home, "data")
	t.fetchedDir = filepath.Join(home, "labmill-results", "selftest-fetched")
	for _, d := range []string{t.codeDir, t.dataDir, filepath.Join(home, ".hidden"), t.fetchedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.folderErr = err
		}
	}
	// Report the folders as the Workspace will (symlinks resolved).
	for _, p := range []*string{&t.codeDir, &t.dataDir, &t.fetchedDir} {
		if r, err := filepath.EvalSymlinks(*p); err == nil {
			*p = r
		}
	}
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.env = &app.Env{
		LocalRoots: []string{home},
		ResultsDir: filepath.Join(home, "labmill-results"),
		Workspace:  app.NewWorkspace(home, ""),
	}
}

// folderHost is the self-test's gui.FolderHost: the stub dialog returns the
// data folder for "Choose your data folder" and the code folder otherwise,
// the stub opener records the folder.
func (t *selfTest) folderHost() gui.FolderHost {
	return gui.FolderHost{
		Env: func() *app.Env { return t.env },
		Pick: func(title, start string) (string, error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.pickTitles = append(t.pickTitles, title)
			if title == "Choose your data folder" {
				return t.dataDir, nil
			}
			return t.codeDir, nil
		},
		Open: func(dir string) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.opened = append(t.opened, dir)
			return nil
		},
	}
}

func (t *selfTest) decided(err error, approve bool, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.decides++
	switch t.decides {
	case 1:
		t.approved = approve
	case 2:
		t.declined = !approve
		t.reasonSent = reason
	case 3:
		t.autoSecretAsked = approve
	}
	_ = err
}

func (t *selfTest) chose(err error, index int, other string, skip bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chooses++
	t.chosenIdx, t.chosenSkip = index, skip || other != ""
	_ = err
}

func (t *selfTest) answered(err error, text string, cancel bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// answerText is the first answer (the password); the three SSH key setup
	// questions of the "key setup check" turn follow (checked in keyAnswers).
	if t.answers++; t.answers == 1 {
		t.answerText = text
		if cancel {
			t.answerText = "<canceled>"
		}
	}
	_ = err
}

// selfMarkdown is the scripted assistant reply: normal Markdown that must
// render as elements, and hostile Markdown that must stay text. It is sent
// in small deltas, so the page renders half-received fences on the way.
const selfMarkdown = "Self-test: hello. Here is **bold**, *italic* and `inline code`.\n\n" +
	"## A heading\n\n" +
	"- first item\n- second item\n\n" +
	"```\nprint(\"<b>not bold</b>\")\n```\n\n" +
	"> a quote\n\n" +
	"Hostile: [x](javascript:alert(1)) and ![p](data:image/png;base64,AA) and " +
	"**<img src=x onerror=\"window.__pwned=1\">** and <a href=\"javascript:window.__pwned=1\">raw</a>.\n"

// selfJobs is the job list in self-test mode: one running job (Status,
// Logs, Cancel), one completed with a remote folder (Status, Logs, Fetch)
// and a draft (no buttons).
var selfJobs = []store.Job{
	{ID: 12, Name: "selftest-train", Cluster: "selftest-cluster", State: store.StateRunning, RemoteID: "900", RemoteDir: "/scratch/selftest", UpdatedAt: time.Now()},
	{ID: 13, Name: "selftest-old", Cluster: "selftest-cluster", State: store.StateCompleted, RemoteID: "901", RemoteDir: "/scratch/old", UpdatedAt: time.Now()},
	{ID: 14, Name: "selftest-draft", Cluster: "selftest-cluster", State: store.StateDraft, UpdatedAt: time.Now()},
}

// jobs adds a fetched job (Open folder) to selfJobs; its folder lies in the
// self-test's results folder.
func (t *selfTest) jobs(context.Context) ([]store.Job, error) {
	js := append([]store.Job(nil), selfJobs...)
	return append(js, store.Job{ID: 15, Name: "selftest-fetched", Cluster: "selftest-cluster", State: store.StateFetched,
		RemoteID: "902", RemoteDir: "/scratch/fetched", LocalDir: t.fetchedDir, UpdatedAt: time.Now()}), nil
}

// selfNotice is the job notice sent during the first turn (hostile markup
// must stay text).
const selfNotice = `Job #12 (selftest-train) on selftest-cluster finished. Ask me to fetch the results. <img src=x onerror="window.__pwned=1">`

// Send implements gui.Session: the first turn is the scripted dialog run;
// the second ("slow") waits until the page cancels it, while the page queues
// a message; later turns (the queued message, a job button) just end.
func (t *selfTest) Send(ctx context.Context, text string) {
	t.mu.Lock()
	t.turns = append(t.turns, text)
	n := len(t.turns)
	// What a real turn appends to the system prompt (agent.SystemExtra).
	t.prompts[text] = t.env.Workspace.Prompt()
	t.mu.Unlock()
	switch {
	case n == 1:
		t.firstTurn(ctx)
	case text == "fetch check":
		// A successful fetch_results: the page must offer "Open folder".
		t.b.Event(agent.Event{Kind: agent.EventToolCall, Tool: "fetch_results", Input: []byte(`{"job_id":15,"paths":["out.txt"]}`)})
		t.b.Event(agent.Event{Kind: agent.EventToolResult, Tool: "fetch_results", Text: "Downloaded 1 file to " + t.fetchedDir + ":\nout.txt"})
		t.b.Event(agent.Event{Kind: agent.EventText, Text: "Your results are in " + t.fetchedDir + "."})
		t.b.Done(nil)
	case text == "todos check" || text == "todos check 2" || text == "todos check 3":
		// The to-do list (M28): a list with all three states, then an update,
		// then everything done. Hostile markup must stay text.
		todos := []app.Todo{
			{Content: "Scan the code", Status: app.TodoCompleted},
			{Content: `<img src=x onerror="window.__pwned=1">Upload the data`, Status: app.TodoInProgress, ActiveForm: "Uploading the data"},
			{Content: "Submit the job", Status: app.TodoPending},
		}
		switch text {
		case "todos check 2":
			todos[1].Status, todos[2].Status = app.TodoCompleted, app.TodoInProgress
			todos[2].ActiveForm = "Submitting the job"
		case "todos check 3":
			todos[1].Status, todos[2].Status = app.TodoCompleted, app.TodoCompleted
		}
		t.b.Todos(todos)
		t.b.Event(agent.Event{Kind: agent.EventText, Text: "Todos ok."})
		t.b.Done(nil)
	case text == "todos error":
		// A turn that fails with a list in progress: the page hides the list.
		t.b.Todos([]app.Todo{{Content: "Upload", Status: app.TodoInProgress, ActiveForm: "Uploading"}})
		t.b.Done(errors.New("selftest failure"))
	case text == "key setup check":
		// The SSH key setup of a key-only host (M38.6): create, passphrase,
		// then the public key with Copy.
		const host = "login.selftest.example"
		a1, e1 := t.b.Ask(ctx, sshx.Question{Kind: sshx.AskKeyCreate, Host: host, Prompt: "login.selftest.example accepts only SSH keys.\n\nCreate a new SSH key? (yes/no)", Echo: true})
		a2, e2 := t.b.Ask(ctx, sshx.Question{Kind: sshx.AskNewPassphrase, Host: host, Prompt: "Optional passphrase to protect the new key (leave empty for none):"})
		a3, e3 := t.b.Ask(ctx, sshx.Question{Kind: sshx.AskKeyUpload, Host: host, Echo: true, Data: selfPubKey,
			Prompt: "Register this public key with the site, then choose Continue.\n\nAdd the key in the registry.\n\nhttps://registry.selftest.example/keys\n"})
		// M45.3: copy a private key from the home cluster (answered No).
		a4, e4 := t.b.Ask(ctx, sshx.Question{Kind: sshx.AskKeyCopy, Host: host, Echo: true,
			Prompt: "login.selftest.example already accepts the SSH key id_ed25519 (SHA256:SelfTestOnly) that is on home.selftest.example.\n\nWarning: this puts a copy of a private key on this computer."})
		t.mu.Lock()
		if e1 == nil && e2 == nil && e3 == nil && e4 == nil {
			t.keyAnswers = []string{a1, a2, a3, a4}
		}
		t.mu.Unlock()
		t.b.Event(agent.Event{Kind: agent.EventText, Text: "Key setup done."})
		t.b.Done(nil)
	case text == "login notice check":
		// M43.1: the 2FA notice shows, then the login ends and the SSH layer
		// clears it with an empty notice; the page must not keep it.
		pause := func(d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
		t.b.Notify("Approve the sign-in request for selftest on your phone\u2026")
		pause(800 * time.Millisecond)
		t.b.Notify("")
		pause(1200 * time.Millisecond)
		t.b.Event(agent.Event{Kind: agent.EventText, Text: "Login notice done."})
		t.b.Done(nil)
	case text == "auto approve check":
		// M43.2: with the setting on, a plain proposal is approved at once
		// (the chat says so) while a read of a sign-in secret file still
		// shows its dialog.
		ap := &agent.AutoApprover{Next: t.b, On: func() bool { return settings.AutoApproveAll(t.prefsPath) },
			Note: func(p agent.Proposal) {
				t.b.Event(agent.Event{Kind: agent.EventAutoApproved, Tool: p.Tool, Text: p.Summary})
			}}
		d1, e1 := ap.Approve(ctx, agent.Proposal{Tool: "upload_data", Summary: "Upload 2 files to selftest:/scratch"})
		d2, e2 := ap.Approve(ctx, agent.Proposal{Tool: "remote_command", Summary: "Run on selftest-hpc: cat ~/.ssh/id_ed25519", Secret: true})
		t.mu.Lock()
		t.autoSecretOK = e1 == nil && d1.Approved && e2 == nil && d2.Approved
		t.mu.Unlock()
		t.b.Event(agent.Event{Kind: agent.EventText, Text: "Auto approve done."})
		t.b.Done(nil)
	case text == "working check":
		// The busy indicator (M26): steps before the first token, then text.
		pause := func(d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
		pause(300 * time.Millisecond) // nothing from Go yet: the page shows "Working…" by itself
		t.b.Event(agent.Event{Kind: agent.EventStep, Text: "Connecting to selftest.example…"})
		pause(900 * time.Millisecond)
		t.b.Event(agent.Event{Kind: agent.EventStep, Text: agent.StepWaitingForClaude})
		pause(600 * time.Millisecond)
		t.b.Event(agent.Event{Kind: agent.EventText, Text: "Working check done."})
		pause(300 * time.Millisecond)
		t.b.Done(ctx.Err())
	case text == "slow":
		// A transfer is running: start line and one progress update; the page
		// must show the progress row until the end notice.
		t.b.Transfer(transfer.Update{Tool: "upload_data", Kind: "start", Text: "rclone-lib: copy --files-from-raw files.txt <b>x</b>", ETASeconds: -1})
		t.b.Transfer(transfer.Update{Tool: "upload_data", Kind: "progress", Bytes: 1 << 20, TotalBytes: 4 << 20, Files: 1, TotalFiles: 4,
			Current: "data/part-001.csv", BytesPerSec: 45 << 20, ETASeconds: 62,
			Text: "Transferred: 1.0 MiB / 4.0 MiB, 25%, 45.0 MiB/s, ETA 1m02s · Files: 1 / 4 · now: data/part-001.csv"})
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
		t.b.Transfer(transfer.Update{Tool: "upload_data", Kind: "end", Text: "Transferred 1.0 MiB in 2s (512.0 KiB/s), 1 file", ETASeconds: -1})
		t.b.Done(ctx.Err())
	default:
		t.b.Event(agent.Event{Kind: agent.EventText, Text: "Got: " + text})
		t.b.Done(nil)
	}
}

// firstTurn is the scripted dialog run.
func (t *selfTest) firstTurn(ctx context.Context) {
	for i := 0; i < len(selfMarkdown); i += 7 {
		t.b.Event(agent.Event{Kind: agent.EventText, Text: selfMarkdown[i:min(i+7, len(selfMarkdown))]})
	}
	t.b.Event(agent.Event{Kind: agent.EventToolCall, Tool: "load_skill"})
	t.b.Event(agent.Event{Kind: agent.EventToolResult, Tool: "load_skill", Text: strings.Repeat("reference text line\n", 40)})
	d, err := t.b.Approve(ctx, agent.Proposal{
		Tool:    "upload",
		Summary: `<img src=x onerror="window.__pwned=1">Upload 2 files`,
		Details: "a.txt  12 B\nb.txt  34 B\nto: cluster:/scratch/selftest\n(a long line follows) " +
			"0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789",
	})
	d2, err2 := t.b.Approve(ctx, agent.Proposal{Summary: "Second request: submit the job"})
	pw, perr := t.b.Ask(ctx, sshx.Question{Kind: sshx.AskPassword, Host: "selftest.example", Prompt: "Password:"})
	c, cerr := t.b.Choose(ctx, agent.Choice{
		Question: "<b>Which</b> Python setup?",
		Options: []agent.ChoiceOption{
			{Label: "Use a module", Description: "load the cluster's python module"},
			{Label: "Make my own env", Description: `<img src=x onerror="window.__pwned=1">conda`},
			{Label: "Use a container"},
		},
		AllowOther: true,
	})
	// A reply that ends inside an unterminated code fence must not break the page.
	t.b.Event(agent.Event{Kind: agent.EventText, Text: "Final words:\n```\nunterminated <script>window.__pwned=1</script>"})
	// A job notice while the reply is still open: it must go above the reply.
	t.b.JobNotice(selfNotice)
	t.mu.Lock()
	t.chooseOK = cerr == nil && !c.Skipped && c.Other == "" && c.Index == 1
	t.sessOK = err == nil && d.Approved && perr == nil && pw == "pw"
	t.declineOK = err2 == nil && !d2.Approved && d2.Reason == "use the gpu partition"
	t.mu.Unlock()
	t.b.Done(nil)
}

// Wait implements gui.Session.
func (t *selfTest) Wait(time.Duration) bool { return true }

func (t *selfTest) report(passed bool, details string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.reported:
		return
	default:
	}
	t.passed, t.details = passed, details
	close(t.reported)
}

// watch waits for the page's report (or the timeout), checks the Go side,
// closes the window to the tray and checks that, then quits through the
// tray's Quit path (finish prints the result).
func (t *selfTest) watch(ctx context.Context) {
	select {
	case <-t.reported:
	case <-time.After(selfTimeout):
		fmt.Fprintln(os.Stderr, "self-test: TIMEOUT: the page did not report within", selfTimeout)
		t.mu.Lock()
		t.code = 2
		t.mu.Unlock()
		t.life.Quit()
		return
	}
	// Give the script a moment to see the answer to its last request.
	time.Sleep(200 * time.Millisecond)
	t.mu.Lock()
	passed := t.passed
	out := t.details
	check := func(ok bool, msg string) {
		if ok {
			out += "\nPASS (go) " + msg
		} else {
			out += "\nFAIL (go) " + msg
			passed = false
		}
	}
	check(t.decides == 3 && t.approved, fmt.Sprintf("Decide called three times (two scripted approvals, then the secret read of the auto-approve turn), the first with approve=true (calls=%d approve=%v)", t.decides, t.approved))
	check(slices.Equal(t.autoDownload, []bool{true, false}), fmt.Sprintf("Download results automatically was switched on and off, and saved each time (%v)", t.autoDownload))
	check(slices.Equal(t.approveAll, []bool{true, false}) && t.autoSecretOK && t.autoSecretAsked, fmt.Sprintf("Approve everything automatically went on, approved the plain proposal at once, still asked for the secret read, and went off (switch=%v ok=%v asked=%v)", t.approveAll, t.autoSecretOK, t.autoSecretAsked))
	check(len(t.jobsWidthSaves) > 0 && t.jobsWidthSaves[len(t.jobsWidthSaves)-1] == 0 && slices.ContainsFunc(t.jobsWidthSaves, func(f float64) bool { return f > 0.45 && f <= gui.MaxJobsWidth }), fmt.Sprintf("the divider's width reached SetJobsWidth (clamped, last save 0 = default): %v", t.jobsWidthSaves))
	check(t.declined && t.reasonSent == "use the gpu partition", fmt.Sprintf("the second Decide was approve=false with the typed reason (declined=%v reason=%q)", t.declined, t.reasonSent))
	check(t.declineOK, `the session's Decision was declined with Reason "use the gpu partition"`)
	check(t.answers == 5 && t.answerText == "pw", fmt.Sprintf("Answer called for the password \"pw\", then for the four key setup questions (calls=%d first=%q)", t.answers, t.answerText))
	check(t.sessOK, "the session received Approved and the password")
	check(t.chooses == 1 && t.chosenIdx == 1 && !t.chosenSkip, fmt.Sprintf("Choose called once with option index 1 (calls=%d index=%d skip/other=%v)", t.chooses, t.chosenIdx, t.chosenSkip))
	check(t.chooseOK, "the session received ChoiceAnswer{Index: 1}")
	wantTurns := []string{"self-test", "slow", "queued two", "Show the status of job #12", "data only check", "workspace check", "fetch check", "todos check", "todos check 2", "todos check 3", "plain turn", "todos check", "plain turn", "plain turn", "todos error", "working check", "key setup check", "login notice check", "auto approve check"}
	check(slices.Equal(t.turns, wantTurns), fmt.Sprintf("the turns were %q (want %q: the queued message replaced, sent after Cancel; the job button's text)", t.turns, wantTurns))
	check(slices.Equal(t.keyAnswers, []string{"yes", selfKeyPass, "yes", "no"}), fmt.Sprintf("the key setup got yes, the typed passphrase, yes and no to the key copy (got %q)", t.keyAnswers))
	check(len(t.leaks) == 0, fmt.Sprintf("the passphrase never appeared in an event to the page (events with it: %q)", t.leaks))
	// The folders (M20).
	check(t.folderErr == nil, fmt.Sprintf("the self-test folders were created (err=%v)", t.folderErr))
	wantTitles := []string{"Choose your data folder", "Choose your code folder"}
	check(slices.Equal(t.pickTitles, wantTitles), fmt.Sprintf("the + menu's Add data folder, then Add code folder, opened the folder dialog (titles %q, want %q)", t.pickTitles, wantTitles))
	codeLine, dataLine := "The user's code folder is "+strconv.Quote(t.codeDir)+".", "The user's data folder is "+strconv.Quote(t.dataDir)+"."
	dp, wp, fp := t.prompts["data only check"], t.prompts["workspace check"], t.prompts["fetch check"]
	check(strings.Contains(dp, dataLine) && !strings.Contains(dp, "code folder is") && strings.Contains(dp, app.DataOnlyPrompt),
		fmt.Sprintf("with only the data folder chosen, the turn's system prompt carried it and the data-only sentence (got %q)", dp))
	check(strings.Contains(wp, dataLine) && strings.Contains(wp, codeLine) && !strings.Contains(wp, app.DataOnlyPrompt) && strings.Index(wp, dataLine) < strings.Index(wp, codeLine),
		fmt.Sprintf("with both folders chosen, the turn carried both (data first) and no data-only sentence (got %q)", wp))
	check(!strings.Contains(fp, "code folder is") && strings.Contains(fp, dataLine) && strings.Contains(fp, app.DataOnlyPrompt),
		fmt.Sprintf("after removing both and dropping the data folder, the next turn carried only the data folder (got %q)", fp))
	check(t.prompts["self-test"] == "", "the turns before any folder was chosen added nothing to the system prompt")
	ws := t.env.Workspace
	check(ws.Folder(app.FolderCode) == "" && ws.Folder(app.FolderData) == t.dataDir, fmt.Sprintf("at the end the code folder is clear and the data folder is the dropped one (code=%q data=%q)", ws.Folder(app.FolderCode), ws.Folder(app.FolderData)))
	check(slices.Equal(ws.Recent(app.FolderCode), []string{t.codeDir}), fmt.Sprintf("the code folder is remembered as recent (got %q)", ws.Recent(app.FolderCode)))
	check(slices.Equal(ws.Recent(app.FolderData), []string{t.dataDir}), fmt.Sprintf("the data folder is remembered as recent (got %q)", ws.Recent(app.FolderData)))
	logDir := ""
	if t.settingsSvc != nil {
		logDir = filepath.Dir(t.settingsSvc.Paths.Log)
	}
	check(slices.Equal(t.opened, []string{t.fetchedDir, t.fetchedDir, logDir}), fmt.Sprintf("Open folder (chat, then job list) asked the opener for the fetched job's folder twice, then Settings' Open logs folder for %q (got %q)", logDir, t.opened))
	// The Settings page (M29).
	check(t.settingsErr == nil, fmt.Sprintf("the Settings fixture files were written (err=%v)", t.settingsErr))
	check(slices.Equal(t.startInTray, []bool{true, false}), fmt.Sprintf("the Start in the tray switch went on, then off, through the stub (got %v)", t.startInTray))
	check(slices.Equal(t.runWithoutAsking, []bool{true, false}), fmt.Sprintf("the Run commands without asking switch went on, then off, through the stub (got %v)", t.runWithoutAsking))
	check(slices.Equal(t.moreAdded, []string{"selftest-more/selftest"}), fmt.Sprintf("the Add button of More clusters added selftest-more for the account shown, once, after the preview (got %q)", t.moreAdded))
	check(t.noticeResets == 1, fmt.Sprintf("Show the one-time tips again ran once (got %d)", t.noticeResets))
	t.checkSettingsFiles(check)
	t.mu.Unlock()

	// Close to the tray: a window close goes through Wails' Quit and
	// OnBeforeClose exactly like the X button (Windows: WM_CLOSE -> Quit;
	// Linux: delete-event -> Quit). The window must hide, the app must keep
	// running, and the first hide must show the one-time notice.
	if t.trayUp() {
		out += "\nINFO (go) a real tray icon is up"
	} else {
		out += "\nINFO (go) no tray host on this desktop: the hide path runs with a simulated tray"
	}
	// The self-test never shows a window (StartHidden, showWindow only
	// records the state): on Windows prove it with the real window state.
	if sum, shown, found := windowProbe(); sum != "" {
		out += "\nINFO (go) " + sum
		check(found, "the window probe found the app's main window (Windows)")
		check(!shown, "no window of the self-test was visible or in the foreground (Windows)")
	}
	// M53.1: the start splash is skipped in self-test mode (it would show on
	// the owner's desktop), and no splash step is in the timing list.
	check(splashStarts.Load() == 0, "the start splash was not created in self-test mode")
	splashMark := false
	for _, l := range startup.Lines() {
		splashMark = splashMark || strings.Contains(l, "splash")
	}
	check(!splashMark, "no splash step in the startup timing list")
	// M47.1: the stored geometry was applied at startup (the window is
	// still hidden on Windows, shown on Linux). A window manager may shift
	// the position by its decorations, so allow a few pixels.
	if t.geomRead != nil {
		g, ok := t.geomRead(ctx)
		near := func(a, b int) bool { return a-b <= 12 && b-a <= 12 }
		want := selfTestGeom
		check(ok && near(g.Width, want.Width) && near(g.Height, want.Height), fmt.Sprintf("the stored window size was restored (got %dx%d, want %dx%d)", g.Width, g.Height, want.Width, want.Height))
		check(ok && near(g.X, want.X) && near(g.Y, want.Y), fmt.Sprintf("the stored window position was restored (got %d,%d, want %d,%d)", g.X, g.Y, want.X, want.Y))
	}
	wr.Quit(ctx)
	var first, hid bool
	select {
	case first = <-t.hides:
		hid = true
	case <-time.After(3 * time.Second):
	}
	time.Sleep(700 * time.Millisecond) // a close that quit would have started the shutdown by now
	check(hid, "closing the window hid it (gui.Lifecycle.Hide ran)")
	check(!t.shutdownStarted() && !t.life.Quitting(), "closing the window did not shut down or quit the app")
	check(first, "the first hide asked for the one-time tray notice")
	pr, err := gui.LoadPrefs(t.prefsPath)
	check(err == nil && pr.TrayNoticeShown, fmt.Sprintf("the notice was remembered in %s (err=%v)", t.prefsPath, err))

	// Single instance: with the window hidden in the tray, the make target
	// now launches a second copy (it waits for "ready" in the marker file);
	// it must hand over to this app, which shows its window.
	if os.Getenv("LABMILL_GUI_SELFTEST_SECOND") == "1" {
		marker := secondMarker()
		if err := os.WriteFile(marker, []byte("ready"), 0o644); err != nil {
			out += fmt.Sprintf("\nINFO (go) could not write %s: %v", marker, err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for !t.second.Load() && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		check(t.second.Load(), "a second launch while hidden handed over to this app (single-instance lock)")
		check(t.shownState != nil && t.shownState(), "the handover marked the window as shown (internal state; no real window is shown in self-test mode)")
		if sum, shown, found := windowProbe(); sum != "" {
			out += "\nINFO (go) after the handover: " + sum
			check(found, "after the handover the window probe found the app's main window (Windows)")
			check(!shown, "after the handover no window was visible or in the foreground (Windows)")
		}
	}

	t.mu.Lock()
	t.passed, t.details = passed, out
	t.mu.Unlock()
	// Then the tray menu's Quit: the Closer shutdown runs and the app exits;
	// finish checks the rest after wails.Run returns.
	t.life.Quit()
}

// checkSettingsFiles checks what the Settings page saved: clusters.yaml has
// the fixed ssh_host with its comments kept, one backup of the original was
// made this run, the file is 0600 (Unix), and both SSH files are as the page
// left them (the user's own untouched).
func (t *selfTest) checkSettingsFiles(check func(bool, string)) {
	if t.settingsSvc == nil {
		check(false, "the Settings service exists")
		return
	}
	p := t.settingsSvc.Paths
	data, err := os.ReadFile(p.Config)
	s := string(data)
	saved := err == nil && strings.Contains(s, "ssh_host: selftest-spare # the login host") && strings.Contains(s, "# self-test clusters (this comment must survive)") && strings.Contains(s, "# the HPC cluster")
	if !saved {
		s = fmt.Sprintf(" (err=%v):\n%s", err, s)
	} else {
		s = ""
	}
	check(saved, "clusters.yaml has the saved ssh_host and kept its comments"+s)
	cfg, err := config.Load(p.Config)
	c, ok := config.Cluster{}, false
	if err == nil {
		c, ok = cfg.Cluster("selftest-hpc")
	}
	check(ok && c.SSHHost == "selftest-spare" && len(cfg.Clusters) == 2, fmt.Sprintf("clusters.yaml loads with the change (err=%v, cluster=%+v)", err, c))
	b := t.settingsSvc.BackupPath()
	old, berr := os.ReadFile(b)
	check(b != "" && berr == nil && string(old) == selfClustersYAML, fmt.Sprintf("one backup of the original clusters.yaml was made (%q, err=%v)", b, berr))
	if fi, err := os.Stat(p.Config); err == nil && filepath.Separator == '/' {
		check(fi.Mode().Perm() == 0o600, fmt.Sprintf("clusters.yaml is 0600 (got %v)", fi.Mode().Perm()))
	}
	user, _ := os.ReadFile(p.SSH.UserConfig())
	check(string(user) == selfUserSSH, "the user's own SSH file was not touched")
}

// secondMarker is the file through which the make target learns when to
// launch the second copy ("ready"); it sits next to the executable and is
// overwritten on every run.
func secondMarker() string {
	exe, err := os.Executable()
	if err != nil {
		return "labmill-selftest-second"
	}
	return filepath.Join(filepath.Dir(exe), "labmill-selftest-second")
}

// noTray records, for the Windows make target, that this (second) process
// skipped the early tray because another instance holds the lock: it writes
// "skipped" into labmill-selftest-notray next to the executable (the target
// pre-fills the file with "none").
func (t *selfTest) noTray() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(filepath.Dir(exe), "labmill-selftest-notray"), []byte("skipped"), 0o644)
}

// secondLaunch records that a second launch handed over to this app.
func (t *selfTest) secondLaunch() { t.second.Store(true) }

// hidden records a tray hide (gui.Lifecycle.Hide).
func (t *selfTest) hidden(first bool) {
	select {
	case t.hides <- first:
	default:
	}
}

// finish adds the checks of the quit path (the app left wails.Run through
// the tray's Quit after the shutdown ran), prints the result and returns the
// exit status: 0 passed, 1 failed, 2 timed out.
func (t *selfTest) finish(shutdownRan bool) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.reported:
	default:
		return t.code // timed out (already printed)
	}
	passed, out := t.passed, t.details
	check := func(ok bool, msg string) {
		if ok {
			out += "\nPASS (go) " + msg
		} else {
			out += "\nFAIL (go) " + msg
			passed = false
		}
	}
	selfTermMu.Lock()
	sys, running := slices.Clone(selfTermSystem), selfTermRunning
	selfTermMu.Unlock()
	check(len(sys) == 1 && running == 0, fmt.Sprintf("Open in system terminal asked for one terminal (%v) and no fake shell is left running (%d)", sys, running))
	check(shutdownRan, "the tray's Quit ran the shutdown (gui.Closer) and the app exited")
	fmt.Println("self-test details:\n" + out)
	printStartupLines()
	if passed {
		fmt.Println("self-test: PASSED")
		return 0
	}
	fmt.Println("self-test: FAILED")
	return 1
}
