// Package launch is the runtime wiring shared by Labmill's front ends (the TUI
// and the GUI): configuration, store, skills, backends, SSH logins and the
// agent. A front end supplies a UI and a first-run SetupFunc and gets a
// Runtime back.
package launch

import (
	"context"
	"errors"
	"fmt"
	"labmill-client/internal/settings"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/skill"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/startup"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
	"labmill-client/skills"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/backend"
	"labmill-client/internal/broker"
	"labmill-client/internal/config"
	"labmill-client/internal/knowledge"
	"labmill-client/internal/site"
	"labmill-client/internal/sshx"
)

// Version is reported to the broker in the User-Agent header; the main
// package sets it from its build-time version.
var Version = "dev"

// UI is what a front end provides: approvals, choices (ask_user), login
// prompts, events.
type UI interface {
	agent.Approver
	// Chooser shows ask_user's selection dialog: exactly one answer per
	// question; dismissed, ctx canceled, turn ended or window closed answers
	// Skipped (or returns an error).
	agent.Chooser
	sshx.Prompter
	// Event receives agent progress events.
	Event(e agent.Event)
	// Done is called when a chat turn finishes, with its error (if any).
	Done(err error)
	// JobNotice shows a sentence from the background job monitor, e.g.
	// "Job #12 (train) on bigcluster finished. Ask me to fetch the results."
	// It is called from the monitor's goroutine, during a turn or between
	// turns, and must not block; the front end also refreshes its job list.
	JobNotice(text string)
	// Transfer shows live upload/download notices (start with the rclone-lib
	// line, throttled progress, end). It must not block the transfer.
	Transfer(u transfer.Update)
}

// SetupAnswers are what the first-run form collects.
type SetupAnswers struct {
	// Site is the chosen institution's domain (a site.Choices entry, or
	// site.GenericDomain): the home institution (M55), required. Empty is
	// refused; there is no setup without one.
	Site     string
	Username string // the user's cluster account name
	// LoginHost is asked for only when the chosen site has no login host
	// (NeedsHosts, e.g. the generic profile); then it is required, since it
	// names the home institution.
	LoginHost string
}

// SetupFunc shows the first-run form; it is front-end specific. s is the
// preselected site (site.Default()); the form offers site.Choices() and
// returns the chosen domain in SetupAnswers.Site. It returns an error if the
// user cancels.
type SetupFunc func(ctx context.Context, s site.Site) (SetupAnswers, error)

// Runtime is a running Labmill session.
type Runtime struct {
	Site    site.Site
	Env     *app.Env
	Agent   *agent.Agent
	Session *Session
	Store   *store.Store
	Status  string // status-bar text at startup; CurrentStatus has the latest
	// LogPath is labmill.log next to the job database (startup warnings;
	// the TUI also sends the process's stderr writes there).
	LogPath string
	close   func() error

	conns *connCache // the SSH connection cache
}

// CurrentStatus is the latest status-bar text.
func (r *Runtime) CurrentStatus() string { return r.Status }

// Close stops the background job monitor, releases the SSH logins and
// closes the job store.
func (r *Runtime) Close() error { return r.close() }

// LogStartupTimes writes the startup timing lines (internal/startup, M46.1)
// recorded so far to labmill.log, redacted like the other log lines. The GUI
// calls it once the page has its chat; the lines hold step names and
// milliseconds only.
func (r *Runtime) LogStartupTimes() error {
	lines := startup.Lines()
	for i, l := range lines {
		lines[i] = app.RedactLine(l, 200)
	}
	return appendLog(r.LogPath, "startup timing", lines)
}

// DefaultMaxTurns is the model calls per user message in the TUI and GUI. A
// full sample run (find, stage, image, upload, draft, submit, wait, logs,
// fetch) needs far more than the agent's own default of 25.
const DefaultMaxTurns = 80

// maxTurns is DefaultMaxTurns unless LABMILL_MAX_TURNS names a positive number.
func maxTurns(getenv func(string) string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(getenv("LABMILL_MAX_TURNS"))); err == nil && n > 0 {
		return n
	}
	return DefaultMaxTurns
}

// Build loads (or, on first run, creates via setup) the configuration and
// wires the pieces together:
//   - the site: on first run the setup form picks it (preselected
//     site.Default()) and WriteSetup records it in clusters.yaml; later
//     runs use site.ForConfig(cfg). The home institution is required (M55):
//     a clusters.yaml without `site:`, or with one this build doesn't know,
//     goes back to the institution form (SetupNote carries the plain
//     message) and only `site:` is added to it;
//   - store.Open(store.DefaultPath()); skills via skill.LoadWithOverride
//     (site.SkillsFSAll(skills.FS): the generic skills plus the home site skill and the skills of the other institutions (M45.1),
//     <config dir>/labmill/skills) with Preamble = app.SkillPreamble
//     (warnings are logged to the status, never fatal);
//   - the model (newModel): broker.New with site.BrokerURL and the
//     LABMILL_BROKER_TOKEN token when a broker is configured, else the
//     Anthropic Messages API with the user's ANTHROPIC_API_KEY (neither is
//     reported by every chat turn, see below);
//   - app.Env with Backend = the per-runtime backendFor, RemoteHome via
//     `printenv HOME` over the cluster's runner, Upload/Download =
//     transfer.Upload/Download, SSHCommand = [os.Executable(), "ssh-exec",
//     alias], Choose = ui.Choose, LocalRoots = [home], Workspace =
//     app.LoadWorkspace(home, app.WorkspacePath(clusters.yaml)) (the chosen
//     code and data folders, shared with the other front end; a load problem
//     is a startup warning), ResultsDir = <home>/labmill-results;
//   - an agent.Agent with Tools = app.Tools + agent.SkillTools, Approver = ui,
//     OnEvent = ui.Event (via stepTracker.observe), System = app.SystemPrompt, SystemExtra =
//     Workspace.Prompt (the chosen folders, every turn);
//   - Status like "bigcluster (Slurm) · othercluster (Slurm)";
//   - the background job monitor (monitor.go), which checks submitted jobs
//     every 45 s without ever logging in (connCache.backendNoLogin) and
//     reports finished ones through ui.JobNotice.
//
// SSH logins prompt through ui (sshx.Connect with ui as Prompter). The SSH
// client cache belongs to the returned Runtime.
//
// Deviation: a missing model (no API key and no broker, or a broker.New
// failure) is not fatal.
// The UI still starts, and every chat turn fails with a plain message
// (unavailableModel), so the user sees Labmill and learns what is missing.
//
// During turns the runtime reports the slow steps below the agent as
// agent.EventStep through ui.Event: "Connecting to <alias>…" while an SSH
// login is opened; the step shown before comes back when they
// end (stepTracker).
func Build(ctx context.Context, ui UI, setup SetupFunc) (*Runtime, error) {
	return BuildWithProgress(ctx, ui, setup, nil)
}

// HomeInstitutionNote is the plain message shown with the institution choice
// when clusters.yaml has no home institution (or one this Labmill doesn't know).
const HomeInstitutionNote = "Labmill needs to know your home institution: your data mostly lives there, and the assistant knows you through your sign-in there. Please choose it."

// HomeNote is the note a form shows when clusters.yaml names no home
// institution: HomeInstitutionNote, plus (only true: it has clusters) that
// only the institution is saved.
func HomeNote(only bool) string {
	if only {
		return HomeInstitutionNote + " Only your institution is saved; your clusters stay as they are."
	}
	return HomeInstitutionNote
}

type setupNoteKey struct{}
type setupOnlyKey struct{}

// SetupOnlyInstitution reports whether the form is asked only for the home
// institution: clusters.yaml has clusters already and just lacks a (known)
// `site:`. The form then needs no account name or hosts, and only the
// institution is saved.
func SetupOnlyInstitution(ctx context.Context) bool {
	b, _ := ctx.Value(setupOnlyKey{}).(bool)
	return b
}

// OnlyInstitution is SetupOnlyInstitution for the configuration at cfgPath,
// for front ends that show the form before Build runs.
func OnlyInstitution(cfgPath string) bool {
	cfg, err := config.Load(cfgPath)
	return err == nil && needsHomeChoice(cfg) && len(cfg.Clusters) > 0
}

// SetupNote returns the message a SetupFunc shows with its form ("" on a
// plain first run): set when clusters.yaml exists but names no (known) home
// institution.
func SetupNote(ctx context.Context) string {
	s, _ := ctx.Value(setupNoteKey{}).(string)
	return s
}

// needsHomeChoice reports whether cfg lacks a home institution this build
// knows: no `site:`, one without a profile, or `generic` with no clusters
// (which names no institution at all).
func needsHomeChoice(cfg *config.Config) bool {
	if cfg == nil || cfg.Site == "" {
		return true
	}
	if cfg.Site == site.GenericDomain && len(cfg.Clusters) == 0 {
		return true
	}
	_, err := siteForConfig(cfg)
	return err != nil
}

// NeedsHomeChoice reports whether the first-run institution form must be
// shown for the configuration at cfgPath: there is none yet, or it names no
// home institution this build knows. A file that can't be read or parsed
// returns false (Build reports that error).
func NeedsHomeChoice(cfgPath string) bool {
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, config.ErrNoConfig) {
		return true
	}
	return err == nil && needsHomeChoice(cfg)
}

// firstRun asks the setup form for the home institution and records it.
// existing is the loaded clusters.yaml that lacks a (known) home site, nil
// on a first run. With clusters already configured only `site:` is set in
// the file (comments and clusters stay; the file is backed up first);
// otherwise WriteSetup writes the whole setup.
func firstRun(ctx context.Context, setup SetupFunc, cfgPath string, existing *config.Config) (*config.Config, error) {
	if setup == nil {
		if existing != nil {
			return nil, errors.New("clusters.yaml names no home institution yet, and this front end has no setup step to choose one")
		}
		return nil, errors.New("Labmill isn't set up yet and this front end has no setup step")
	}
	def := site.Default()
	if existing != nil {
		ctx = context.WithValue(ctx, setupNoteKey{}, HomeNote(len(existing.Clusters) > 0))
		ctx = context.WithValue(ctx, setupOnlyKey{}, len(existing.Clusters) > 0)
	}
	answers, err := setup(ctx, def)
	if err != nil {
		return nil, err
	}
	if answers.Site == "" {
		return nil, errors.New("please choose your home institution: Labmill can't be set up without one")
	}
	chosen, err := site.Get(answers.Site)
	if err != nil {
		return nil, fmt.Errorf("choose your home institution: %w", err)
	}
	if existing != nil && len(existing.Clusters) > 0 {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", cfgPath, err)
		}
		out, err := config.SetSiteYAML(data, chosen.Domain)
		if err != nil {
			return nil, fmt.Errorf("record your home institution: %w", err)
		}
		if _, err := config.Backup(cfgPath, time.Now()); err != nil {
			return nil, err
		}
		if err := config.WriteFile(cfgPath, out); err != nil {
			return nil, err
		}
		return config.Parse(out)
	}
	paths, err := sshconfig.DefaultPaths()
	if err != nil {
		return nil, err
	}
	return WriteSetup(paths, cfgPath, chosen, answers, goruntime.GOOS)
}

// BuildWithProgress is Build that reports its startup steps to progress
// (nil: none), in this order: StartLoadingSettings (before the
// configuration is read, and before the first-run form),
// StartOpeningDatabase, StartLoadingSkills and
// StartReady when the runtime is complete. A failed Build stops at the step that failed.
func BuildWithProgress(ctx context.Context, ui UI, setup SetupFunc, progress Progress) (*Runtime, error) {
	report := func(step string) {
		if progress != nil {
			progress(step)
		}
	}
	startup.Mark("build: start")
	report(StartLoadingSettings)
	endStep := startup.Span("build: settings (config, first-run form included)")
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return nil, err
	}
	var warnings []string
	cfg, err := config.Load(cfgPath)
	switch {
	case errors.Is(err, config.ErrNoConfig):
		cfg, err = firstRun(ctx, setup, cfgPath, nil)
	case err == nil && needsHomeChoice(cfg):
		// A clusters.yaml without a known home institution (hand-edited or
		// from an old version) goes back to the institution choice (M55).
		cfg, err = firstRun(ctx, setup, cfgPath, cfg)
	}
	if err != nil {
		return nil, err
	}
	endStep()
	endStep = startup.Span("build: site profile")
	st, serr := siteForConfig(cfg)
	endStep()
	if serr != nil {
		warnings = append(warnings, serr.Error())
	}
	ok := false
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find your home folder: %w", err)
	}
	report(StartOpeningDatabase)
	dbPath, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	endStep = startup.Span("build: job database open")
	db, err := store.Open(dbPath)
	endStep()
	if err != nil {
		return nil, fmt.Errorf("open Labmill's job database: %w", err)
	}
	defer func() {
		if !ok {
			db.Close()
		}
	}()

	report(StartLoadingSkills)
	skillsDir := filepath.Join(filepath.Dir(cfgPath), "skills")
	endStep = startup.Span("build: skills filesystem copy (SkillsFS)")
	others := othersForSite(st.Domain)
	skillFS, skillWarns, err := site.SkillsFSSession(skills.FS, st, others)
	endStep()
	warnings = append(warnings, skillWarns...)
	if err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	// Source priority: the override folder (LoadWithOverride) > the cached,
	// verified broker bundle > the embedded copy.
	embeddedVersion := ""
	endStep = startup.Span("build: embedded knowledge bundle (knowledge.Build)")
	if kb, kerr := knowledge.Build(skills.FS, st); kerr != nil {
		warnings = append(warnings, kerr.Error())
	} else {
		embeddedVersion = kb.Version
	}
	endStep()
	base := skillBase{FS: skillFS, Version: embeddedVersion}
	endStep = startup.Span("build: cached bundle choose and verify")
	if root, cerr := knowledgeCacheDir(); cerr == nil {
		if cacheDir, derr := siteCacheDir(root, st.Domain); derr == nil {
			var kw []string
			base, kw = chooseSkillBase(cacheDir, st, skillFS, embeddedVersion, others...)
			warnings = append(warnings, kw...)
		}
	}
	endStep()
	overridden := overrideInUse(skillsDir)
	base.Overridden = overridden
	endStep = startup.Span("build: skills load (skill.LoadWithOverride)")
	set, warns, err := skill.LoadWithOverride(base.FS, skillsDir)
	if err != nil && base.Cached {
		warnings = append(warnings, "cached cluster know-how not used: "+err.Error())
		base = skillBase{FS: skillFS, Version: embeddedVersion, Overridden: overridden}
		set, warns, err = skill.LoadWithOverride(base.FS, skillsDir)
	}
	endStep()
	if err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	notes := &knowledgeNotes{notice: ui.JobNotice, inUse: base.Version, overDir: skillsDir}
	for _, w := range warns {
		warnings = append(warnings, w.Error())
	}
	set.Preamble = app.SkillPreamble

	endStep = startup.Span("build: workspace load")
	ws, werr := app.LoadWorkspace(home, app.WorkspacePath(cfgPath))
	endStep()
	if werr != nil {
		warnings = append(warnings, werr.Error())
	}

	steps := newStepTracker(ui)
	conns := newConnCache(ui)
	conns.steps = steps
	conns.homeDomain = st.Domain
	conns.clusters = func() []config.Cluster { return cfg.List() }
	conns.keys = keySiteOf(st)
	for _, c := range homeClusters(cfg.Clusters, st.Domain) {
		conns.homeSSHHosts = append(conns.homeSSHHosts, c.SSHHost)
	}
	for _, o := range sitesOfConfig(st, cfg, others)[1:] {
		conns.keys = conns.keys.merged(o)
	}
	env := &app.Env{
		Config: cfg, Store: db, Skills: set,
		Site: siteInfo(st, cfg.Clusters, others...),
		Backend: func(ctx context.Context, c config.Cluster) (backend.Backend, error) {
			return conns.backendFor(ctx, c)
		},
		RemoteHome: func(ctx context.Context, c config.Cluster) (string, error) {
			return conns.remoteHome(ctx, c)
		},
		LoginRunner: conns.loginRunner,
		// The "Run commands on the cluster without asking" setting lives in
		// gui.json next to clusters.yaml; read per command, so a change
		// applies at once.
		AutoRunRemote: func() bool {
			return settings.AutoRunRemote(filepath.Join(filepath.Dir(cfgPath), "gui.json"))
		},
		// Every transfer keeps the laptop awake and is retried when the
		// connection breaks (M64.1).
		Upload: func(ctx context.Context, root string, entries []store.ManifestEntry, dst transfer.Remote, progress func(transfer.Progress)) error {
			return transfer.WithRetry(ctx, progress, func(pr func(transfer.Progress)) error { return transfer.Upload(ctx, root, entries, dst, pr) })
		},
		Download: func(ctx context.Context, src transfer.Remote, paths []string, localDir string, progress func(transfer.Progress)) (w []string, err error) {
			err = transfer.WithRetry(ctx, progress, func(pr func(transfer.Progress)) (e error) {
				w, e = transfer.Download(ctx, src, paths, localDir, pr)
				return
			})
			return w, err
		},
		UploadS3: func(ctx context.Context, root string, entries []store.ManifestEntry, dst transfer.S3Remote, progress func(transfer.Progress)) error {
			return transfer.WithRetry(ctx, progress, func(pr func(transfer.Progress)) error { return transfer.UploadS3(ctx, root, entries, dst, pr) })
		},
		DownloadS3: func(ctx context.Context, src transfer.S3Remote, paths []string, localDir string, progress func(transfer.Progress)) (w []string, err error) {
			err = transfer.WithRetry(ctx, progress, func(pr func(transfer.Progress)) (e error) {
				w, e = transfer.DownloadS3(ctx, src, paths, localDir, pr)
				return
			})
			return w, err
		},
		// The automatic download of results (M60.1) lists and sizes them first.
		ListRemote:   transfer.List,
		ListRemoteS3: transfer.ListS3,
		FreeSpace:    freeSpace,
		TransferCheck: func(ctx context.Context, alias string) error {
			// Make sure the helper holds a live login for alias (this logs in
			// through the front end's dialog if needed) so ssh-exec finds it.
			_, err := conns.client(ctx, alias)
			return err
		},
		SSHCommand: func(alias string) []string {
			exe, err := os.Executable()
			if err != nil {
				exe = "labmill"
			}
			return []string{exe, "ssh-exec", alias}
		},
		HostName: func(alias string) string {
			paths, err := sshconfig.DefaultPaths()
			if err != nil {
				return ""
			}
			h, _, err := sshconfig.Lookup(paths, alias)
			if err != nil {
				return ""
			}
			return h.HostName
		},
		Transfer: ui.Transfer,
		Choose:   ui.Choose,
		SecretPrompt: func(ctx context.Context, title string, fields []app.PromptField) ([]string, error) {
			return askSecrets(ctx, ui, title, fields)
		},
		LocalRoots: []string{home},
		Workspace:  ws,
		ResultsDir: filepath.Join(home, "labmill-results"),
	}
	// The profiles add_cluster can draw on: the home profile too, for a
	// cluster it gained after this install was set up (M57.2).
	withHome := append([]site.Site{st}, others...)
	env.PlanAddCluster = func(domain, cluster string) (app.AddClusterPlan, error) {
		paths, err := sshconfig.DefaultPaths()
		if err != nil {
			return app.AddClusterPlan{}, errNoSSHPaths
		}
		return planAddCluster(paths, cfgPath, withHome, domain, cluster)
	}
	wireAccounts(env, conns, cfg, st.Domain, sshconfig.DefaultPaths, goruntime.GOOS)
	env.AddCluster = func(ctx context.Context, plan app.AddClusterPlan, username string) (config.Cluster, error) {
		paths, err := sshconfig.DefaultPaths()
		if err != nil {
			return config.Cluster{}, errNoSSHPaths
		}
		c, err := addSiteCluster(paths, cfgPath, withHome, plan, username, goruntime.GOOS, time.Now())
		if err != nil {
			return config.Cluster{}, err
		}
		// Live: the key-only hosts of the new cluster's institution join the session's connection cache.
		owner := c.Site
		if owner == "" {
			owner = st.Domain
		}
		if o, ok := findSite(withHome, owner); ok {
			conns.noteAddedCluster(st.Domain, c, o)
		}
		return c, nil
	}
	// The to-do list (update_todos) is shown by front ends that implement
	// Todos; others just ignore it.
	if tu, ok := ui.(interface{ Todos([]app.Todo) }); ok {
		env.Todos = tu.Todos
	}

	endStep = startup.Span("build: tool registry")
	reg := agent.NewRegistry()
	for _, t := range append(app.Tools(env), agent.SkillTools(set)...) {
		if err := reg.Register(t); err != nil {
			return nil, fmt.Errorf("register tool: %w", err)
		}
	}
	endStep()
	endStep = startup.Span("build: model client and knowledge sync start")
	var model agent.Model
	var rec *manifestRecorder // the session's manifest, for the prompt choice
	syncCtx, stopSync := context.WithCancel(context.Background())
	defer func() {
		if !ok {
			stopSync()
		}
	}()
	if st.BrokerURL == "" {
		// No broker: the Anthropic Messages API with the user's own key.
		if dm, err := newDirectModel(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv); err != nil {
			model = unavailableModel{errors.New("Labmill has no assistant yet: set ANTHROPIC_API_KEY to your own Anthropic API key, then restart labmill")}
		} else {
			model = dm
		}
	} else if bc, err := newBrokerClient(st.BrokerURL, st.Domain, base.Version, notes.current); err != nil {
		model = unavailableModel{fmt.Errorf("Labmill can't use its assistant address (%s): %w", st.BrokerURL, err)}
	} else {
		model = bc
		// Fetch newer cluster know-how in the background, once a token is
		// there; it is used from the next start.
		if sc, serr := broker.New(broker.Config{BaseURL: st.BrokerURL, Token: envToken, UserAgent: "labmill/" + Version,
			Site: st.Domain, SkillsVersion: base.Version, OnSkillsCurrent: notes.current}); serr == nil {
			rec = &manifestRecorder{knowledgeFetcher: sc}
			startKnowledgeSync(syncCtx, rec, st, embeddedVersion, hasEnvToken, func(line string) {
				appendLog(filepath.Join(filepath.Dir(dbPath), "labmill.log"), "cluster know-how", []string{line})
			})
		}
	}
	endStep()
	prefsPath := filepath.Join(filepath.Dir(cfgPath), "gui.json")
	onEvent := func(e agent.Event) {
		if e.Kind == agent.EventAutoApproved {
			// Recorded like an approval: the tool and a short redacted target.
			appendLog(filepath.Join(filepath.Dir(dbPath), "labmill.log"), "auto-approved", []string{e.Tool + ": " + app.RedactLine(e.Text, 100)})
		}
		steps.observe(e)
		ui.Event(e)
	}
	// "Approve everything automatically" (M43.2), read per proposal.
	autoApprover := &agent.AutoApprover{Next: ui, On: func() bool { return settings.AutoApproveAll(prefsPath) },
		Note: func(p agent.Proposal) {
			onEvent(agent.Event{Kind: agent.EventAutoApproved, Tool: p.Tool, Text: p.Summary})
		}}
	endStep = startup.Span("build: system prompt")
	sysPrompt := app.SystemPrompt(env)
	endStep()
	ag := &agent.Agent{Model: model, Tools: reg, Approver: autoApprover, OnEvent: onEvent, System: sysPrompt,
		SystemExtra: ws.Prompt, MaxTurns: maxTurns(os.Getenv)}

	mon := &jobMonitor{
		store: db, cfg: cfg, notify: ui.JobNotice,
		backend: func(ctx context.Context, c config.Cluster) (backend.Backend, bool) {
			return conns.backendNoLogin(ctx, c)
		},
		release: conns.releaseBackground,
		// "Download results automatically" (M60.1), read when a job ends. The
		// download runs on a copy of the environment whose transfer check
		// never logs in: a missing sign-in is reported in the chat instead
		// of opening a dialog from the background.
		autoOn:      func() bool { return settings.AutoDownloadResults(prefsPath) },
		autoPending: env.PendingAutoFetch,
		autoFetch: func(ctx context.Context, jobID int64) app.AutoFetchResult {
			bg := *env
			bg.TransferCheck = func(ctx context.Context, alias string) error {
				return conns.backgroundTransferCheck(ctx, alias, backgroundCheckWait)
			}
			return bg.AutoFetch(ctx, jobID, func(line string) {
				appendLog(filepath.Join(filepath.Dir(dbPath), "labmill.log"), "automatic download", []string{line})
			})
		},
	}
	// A preempted job of an approved plan is resubmitted (M61.3), by the
	// monitor and by job_status; the monitor's backend never opens a login
	// dialog, and a lost connection defers the resubmission.
	rs := &app.Resubmitter{Store: db, Notify: ui.JobNotice,
		Log: func(line string) {
			_ = appendLog(filepath.Join(filepath.Dir(dbPath), "labmill.log"), "automatic resubmission", []string{line})
		}}
	mon.resubmit = rs.Handle
	env.Resubmitter = rs
	// job_status may see a job end before the monitor does.
	env.OnJobEnded = mon.jobEnded
	// A downloaded job's sentence carries its ID so the GUI can offer "Open
	// folder"; other front ends show it as a plain job notice.
	if jf, ok := ui.(interface {
		JobFetched(jobID int64, text string)
	}); ok {
		mon.fetched = jf.JobFetched
	}
	// M64.3: a desktop notification when a job ends (front ends that have
	// one, read at each job end), "Email me when a Slurm job ends" for new
	// Slurm scripts, and the "while you were away" notice.
	if jn, ok := ui.(interface{ JobEnded(text string) }); ok {
		mon.ended = func(text string) {
			if settings.NotifyJobEnd(prefsPath) {
				jn.JobEnded(text)
			}
		}
	}
	env.MailOnEnd = func() bool { return settings.EmailOnJobEnd(prefsPath) }
	lastSeenPath := filepath.Join(filepath.Dir(cfgPath), "lastseen")
	mon.awaySince = settings.LastSeen(lastSeenPath)
	mon.alive = func() { _ = settings.SetLastSeen(lastSeenPath, time.Now()) }

	warnSuffix := ""
	if len(warnings) > 0 {
		warnSuffix = fmt.Sprintf(" · %d startup warnings (see labmill.log)", len(warnings))
		logPath := filepath.Join(filepath.Dir(dbPath), "labmill.log")
		if err := appendLog(logPath, "startup warnings (site, skills)", warnings); err != nil {
			warnSuffix += " (couldn't write labmill.log)"
		}
	}
	status := statusLine(cfg) + warnSuffix
	// Configs written before files_host existed: offer the site's transfer
	// host once, at the first turn (a dialog needs the front end running).
	var firstTurn func(ctx context.Context)
	if paths, perr := sshconfig.DefaultPaths(); perr == nil && len(pendingFilesHosts(st, cfg, readFilesHostAsked(cfgPath))) > 0 {
		firstTurn = func(ctx context.Context) {
			for _, n := range offerFilesHosts(ctx, ui, paths, cfgPath, st, cfg, goruntime.GOOS) {
				ui.JobNotice(n)
			}
		}
	}
	if base.Overridden {
		prev := firstTurn
		firstTurn = func(ctx context.Context) {
			notes.override()
			if prev != nil {
				prev(ctx)
			}
		}
	}
	// The system prompt is chosen once, at the first Send (promptindex.go):
	// trimmed only when the broker's manifest is already known and says its
	// index carries this client's skills.
	firstTurn = withPromptChoice(firstTurn, ag, env, rec, st, base)
	ok = true
	endStep = startup.Span("build: job monitor start")
	mon.start(nil)
	endStep()
	stopUpload := startUploader(db, func(name string) string {
		c, _ := cfg.Cluster(name)
		return clusterSite(st, c, others).Name
	}, func(line string) {
		appendLog(filepath.Join(filepath.Dir(dbPath), "labmill.log"), "Labmill account", []string{line})
	})
	startup.Mark("build: done")
	report(StartReady)
	rt := &Runtime{
		Site: st, Env: env, Agent: ag, Store: db,
		Session: &Session{agent: ag, ui: ui, onTurn: conns.newTurn, firstTurn: firstTurn},
		Status:  status,
		LogPath: filepath.Join(filepath.Dir(dbPath), "labmill.log"),
		conns:   conns,
		close: func() error {
			stopSync()
			stopUpload()
			mon.Close() // first: it uses the connections and the store
			conns.closeAll()
			return db.Close()
		},
	}
	return rt, nil
}

// siteInfo is what the system prompt learns about s.
// siteForConfig is site.ForConfig; tests replace it to build with a fixture
// profile, since the embedded private profiles may be absent.
var siteForConfig = site.ForConfig

// The per-cluster hints (SiteInfo.Clusters) are the Slurm placement facts of
// the configured clusters.
//
// others are the embedded profiles of the other institutions (site.Others):
// they are listed in the prompt, and a cluster whose `site:` names one of
// them takes its hints from that profile.
func siteInfo(s site.Site, clusters []config.Cluster, others ...site.Site) app.SiteInfo {
	if s.Domain == site.GenericDomain {
		return app.SiteInfo{Skill: s.Skill, Generic: true, Domain: s.Domain, Others: otherSiteInfos(others)}
	}
	info := app.SiteInfo{NoS3Credentials: s.NoS3Credentials, Domain: s.Domain, Name: s.Name, Skill: s.Skill, DocsURL: s.DocsURL, Others: otherSiteInfos(others)}
	info.HomeMissing = homeMissing(s, clusters)
	for _, c := range clusters {
		// M61: a Slurm cluster's preemptible and scratch facts.
		if h := clusterSite(s, c, others).SlurmHintsFor(c.Name); !h.Zero() {
			if info.Clusters == nil {
				info.Clusters = map[string]app.ClusterHints{}
			}
			info.Clusters[c.Name] = app.ClusterHints{Slurm: h}
		}
	}
	return info
}

// clusterSite is the profile cluster c belongs to: the one its `site:`
// names among others, else home (also for a site this build doesn't know).
func clusterSite(home site.Site, c config.Cluster, others []site.Site) site.Site {
	if c.Site != "" && c.Site != home.Domain {
		if o, ok := findSite(others, c.Site); ok {
			return o
		}
	}
	return home
}

// appendLog appends a timestamped block of lines to the log file at path
// (created 0600, with its folder 0700).
func appendLog(path, title string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", time.Now().Format(time.RFC3339), title)
	for _, l := range lines {
		fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(l, "\n", " "))
	}
	_, werr := f.WriteString(b.String())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// unavailableModel is the agent Model used when Labmill has no usable
// connection to its assistant; every call fails with err.
type unavailableModel struct{ err error }

// BetaMessages implements agent.Model.
func (m unavailableModel) BetaMessages() (*anthropic.BetaMessageService, []option.RequestOption, error) {
	return nil, nil, m.err
}

// statusLine renders e.g. "bigcluster (Slurm) · othercluster (Slurm)".
func statusLine(cfg *config.Config) string {
	var parts []string
	for _, c := range cfg.Clusters {
		if c.Type == config.TypeSlurm {
			parts = append(parts, c.Name+" (Slurm)")
		} else {
			parts = append(parts, c.Name)
		}
	}
	return strings.Join(parts, " · ")
}

// remoteHome returns the absolute home directory on the host where c's
// transfers land: files_host when set, else the login host. Relative upload
// folders resolve against it, so the recorded job folder is where the data
// really is.
func (cc *connCache) remoteHome(ctx context.Context, c config.Cluster) (string, error) {
	host := c.SSHHost
	if c.FilesHost != "" {
		host = c.FilesHost
	}
	if host == "" {
		return "", fmt.Errorf("cluster %s has no login host configured", c.Name)
	}
	cl, err := cc.client(ctx, host)
	if err != nil {
		return "", err
	}
	home, err := homeOver(ctx, cl)
	if err != nil {
		return "", fmt.Errorf("couldn't find your home folder on %s: %w", host, err)
	}
	return home, nil
}

// homeOver finds the home directory over an SSH connection: the SFTP
// server's real path of "." (works on transfer-only hosts that refuse
// commands), else `printenv HOME` (works where the sftp subsystem is off).
func homeOver(ctx context.Context, cl *ssh.Client) (string, error) {
	if sc, err := sftp.NewClient(cl); err == nil {
		p, err := sc.RealPath(".")
		sc.Close()
		if err == nil && path.IsAbs(p) {
			return p, nil
		}
	}
	res, err := (&backend.SSHRunner{Client: cl}).Run(ctx, []string{"printenv", "HOME"}, nil)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || !path.IsAbs(home) {
		return "", errors.New("no absolute home folder reported")
	}
	return home, nil
}

// connCache keeps one SSH client per alias for one Runtime; logins prompt
// through ui.
type connCache struct {
	ui UI
	// steps shows "Connecting to <alias>…" while a login is opened (nil:
	// nothing is shown).
	steps   *stepTracker
	mu      sync.Mutex
	closed  bool // set by closeAll: no new connections after the runtime closed
	clients map[string]*cachedClient
	// bg holds the clients the job monitor picked up from the helper's open
	// logins (existingClient). They are kept apart from clients and closed
	// after every check (releaseBackground). While jobs are watched the
	// monitor reconnects at every check (every 45 s), so the helper stays
	// busy; once no job is watched nothing holds a connection and the helper
	// can idle out.
	bg map[string]*cachedClient
	// homeDomain is the user's own institution (the top-level site); a
	// cluster whose `site:` names another one is foreign (foreignCluster).
	homeDomain string
	// keys describes the site's key-only hosts; their first login runs the
	// guided SSH key setup (keysetup.go). Zero when the site has none.
	keys keySite
	// homeSSHHosts are the ssh aliases of the home institution's clusters
	// (config SSHHost): the user's own alias for the home login host may
	// differ from the site's (M45.3 key look-up).
	homeSSHHosts []string
	// staleCheck bounds forget's keepalive check; zero means
	// defaultStaleCheck.
	staleCheck time.Duration
	// sshPaths locates the ssh config; nil means sshconfig.DefaultPaths.
	sshPaths func() (sshconfig.Paths, error)

	// denied holds the hosts whose password the server rejected in this turn;
	// client refuses a new login to them until the next turn (newTurn), so a
	// wrong password is never retried into an account lockout. Guarded by
	// deniedMu (not mu, which is held during a login dialog).
	deniedMu sync.Mutex
	denied   map[string]string // alias -> the refusal message
	// clusters returns the live configured clusters (Build: the session's
	// config, which add_cluster extends); a password rejection marks every
	// host of the same institution (siblingHosts), one account.
	clusters func() []config.Cluster
	// keysMu guards keys once Build returned: add_cluster adds entries (a
	// cluster of another institution) while the monitor and the tools read
	// them.
	keysMu sync.RWMutex
}

type cachedClient struct {
	*ssh.Client
	dead chan struct{} // closed when the connection ends
	born time.Time     // when it was opened (zero: unknown, counts as old)
}

func newConnCache(ui UI) *connCache {
	return &connCache{ui: ui, clients: map[string]*cachedClient{}, bg: map[string]*cachedClient{}}
}

// client returns a live client for alias, connecting (through the helper)
// when there is none or the cached one has closed. Connecting happens under
// the lock so two tools don't prompt for the same login twice.
func (cc *connCache) client(ctx context.Context, alias string) (*ssh.Client, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.closed {
		return nil, errors.New("Labmill is shutting down")
	}
	if c := cc.clients[alias]; c != nil {
		select {
		case <-c.dead:
		default:
			return c.Client, nil
		}
	}
	if msg := cc.deniedMsg(alias); msg != "" {
		return nil, errors.New(msg)
	}
	end := cc.steps.begin("Connecting to " + alias + "…")
	cl, err := cc.connect(ctx, alias)
	end()
	if err != nil {
		if errors.Is(err, sshx.ErrPasswordRejected) {
			cc.markDenied(alias)
			// The name may be one Labmill reused from another cluster of the
			// institution (M59.1): the model can ask whether it differs.
			err = fmt.Errorf("%w\nIf Labmill reused the user name from another cluster of this institution, ask the user whether it differs here (set_cluster_account). Do not retry the password.", err)
		}
		return nil, err
	}
	cc.cacheLocked(alias, cl)
	return cl, nil
}

// deniedMsg is the refusal for alias when a password was rejected for it (or
// a host of its institution) in the current turn, else "".
func (cc *connCache) deniedMsg(alias string) string {
	cc.deniedMu.Lock()
	defer cc.deniedMu.Unlock()
	return cc.denied[alias]
}

// markDenied records a password rejection for alias, and for the other hosts
// of the same institution (one account), until the next turn.
func (cc *connCache) markDenied(alias string) {
	cc.deniedMu.Lock()
	defer cc.deniedMu.Unlock()
	if cc.denied == nil {
		cc.denied = map[string]string{}
	}
	if cc.clusters != nil {
		for _, h := range siblingHosts(cc.clusters(), cc.homeDomain)[alias] {
			cc.denied[h] = "a sign-in at this institution failed a moment ago; check your user name and password, then ask me to try again"
		}
	}
	cc.denied[alias] = "sign-in to " + alias + " failed a moment ago; check your user name and password, then ask me to try again"
}

// newTurn starts a new user turn: a password rejected in the last turn may
// be tried again.
func (cc *connCache) newTurn() {
	cc.deniedMu.Lock()
	cc.denied = nil
	cc.deniedMu.Unlock()
}

// defaultStaleCheck is connCache.staleCheck's default.
const defaultStaleCheck = 5 * time.Second

// forget drops and closes the cached client for alias when it was opened
// before since and doesn't answer a keepalive request within staleCheck
// (the helper answers one only once its login to the cluster answered), and
// reports whether it did. A client opened at or after since (by the
// caller's own connect), or one that answers (a command that was merely
// slow), is kept. After a drop the next client call connects again through
// the helper, which checks its own login and signs in again (with the front
// end's dialog) when that doesn't answer either.
func (cc *connCache) forget(alias string, since time.Time) bool {
	cc.mu.Lock()
	c := cc.clients[alias]
	cc.mu.Unlock()
	if c == nil || !c.born.Before(since) {
		return false
	}
	d := cc.staleCheck
	if d <= 0 {
		d = defaultStaleCheck
	}
	if answers(c.Client, d) {
		return false
	}
	cc.mu.Lock()
	if cc.clients[alias] == c {
		delete(cc.clients, alias)
	}
	cc.mu.Unlock()
	go c.Close() // in the background: Close may wait on a link that stopped answering
	return true
}

// answers reports whether cl replies to a keepalive request within d (the
// reply's yes or no doesn't matter, only that one comes).
func answers(cl *ssh.Client, d time.Duration) bool {
	res := make(chan error, 1)
	go func() {
		_, _, err := cl.SendRequest("keepalive@openssh.com", true, nil)
		res <- err
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-res:
		return err == nil
	case <-t.C:
		return false
	}
}

// cacheLocked remembers cl for alias and notices when it ends. cc.mu must be
// held.
func (cc *connCache) cacheLocked(alias string, cl *ssh.Client) {
	c := &cachedClient{Client: cl, dead: make(chan struct{}), born: time.Now()}
	go func() { cl.Wait(); close(c.dead) }()
	cc.clients[alias] = c
}

// Hooks for tests: the interactive connect (may log in through the UI) and
// the existing-login-only connect (never asks anything).
var (
	sshConnect    = sshx.Connect
	connectCached = func(ctx context.Context, alias string) (*ssh.Client, error) {
		return sshx.ConnectCached(ctx, alias, sshx.ConnectOptions{})
	}
)

// existingClient returns a live client for alias without ever logging in:
// the cached client if it is still open, else a client on a login the
// helper already holds (sshx.ConnectCached), which is kept only until the
// monitor calls releaseBackground. It gives
// up (false) when the cache is busy (a login dialog may be open), the
// runtime is closing, or there is no open login.
func (cc *connCache) existingClient(ctx context.Context, alias string) (*ssh.Client, bool) {
	cl, ok, _ := cc.tryExisting(ctx, alias)
	return cl, ok
}

// tryExisting is existingClient that also says whether it gave up because
// the cache was busy (busy true: nothing is known about the login).
func (cc *connCache) tryExisting(ctx context.Context, alias string) (cl *ssh.Client, ok, busy bool) {
	if alias == "" {
		return nil, false, false
	}
	if !cc.mu.TryLock() {
		return nil, false, true
	}
	defer cc.mu.Unlock()
	if cc.closed {
		return nil, false, false
	}
	if c := cc.clients[alias]; c != nil {
		select {
		case <-c.dead:
		default:
			return c.Client, true, false
		}
	}
	if c := cc.bg[alias]; c != nil {
		select {
		case <-c.dead:
		default:
			return c.Client, true, false
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cl, err := connectCached(ctx, alias)
	if err != nil {
		return nil, false, false
	}
	c := &cachedClient{Client: cl, dead: make(chan struct{})}
	go func() { cl.Wait(); close(c.dead) }()
	cc.bg[alias] = c
	return cl, true, false
}

// backgroundTransferCheck is the transfer check for work nobody is watching
// (the automatic download of results, M60.1): it never logs in and never
// opens a dialog. It returns nil when the helper holds a login for alias,
// app.ErrSignInNeeded (wrapped) when it doesn't or a password for the host
// was just rejected, and app.ErrTryLater when the connections stayed busy
// for wait (a login dialog may be open, so nothing is known) or ctx ended.
func (cc *connCache) backgroundTransferCheck(ctx context.Context, alias string, wait time.Duration) error {
	if msg := cc.deniedMsg(alias); msg != "" {
		return fmt.Errorf("%w: %s", app.ErrSignInNeeded, msg)
	}
	deadline := time.Now().Add(wait)
	for {
		_, ok, busy := cc.tryExisting(ctx, alias)
		switch {
		case ok:
			return nil
		case !busy:
			return fmt.Errorf("%w: no open sign-in to %s", app.ErrSignInNeeded, alias)
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%w: the connections to %s are busy", app.ErrTryLater, alias)
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// backgroundCheckWait is how long the automatic download waits for busy
// connections before it postpones.
const backgroundCheckWait = 30 * time.Second

// releaseBackground closes the connections existingClient took from the
// helper for background work. The interactive connections in clients stay.
// Called by the job monitor after each check.
func (cc *connCache) releaseBackground() {
	// A busy cache (a login dialog is open) is skipped, not waited for; the
	// next check releases.
	if !cc.mu.TryLock() {
		return
	}
	defer cc.mu.Unlock()
	for k, c := range cc.bg {
		c.Close()
		delete(cc.bg, k)
	}
}

// backendNoLogin is backendFor for background work: it returns the backend
// only when that needs no login and no question (false otherwise, so the
// caller skips and tries later). Slurm needs an open SSH login
// (existingClient).
func (cc *connCache) backendNoLogin(ctx context.Context, c config.Cluster) (backend.Backend, bool) {
	switch c.Type {
	case config.TypeSlurm:
		loc, err := c.Location()
		if err != nil {
			return nil, false
		}
		cl, ok := cc.existingClient(ctx, c.SSHHost)
		if !ok {
			return nil, false
		}
		return &slurm.Slurm{Runner: &backend.SSHRunner{Client: cl}, Location: loc}, true
	}
	return nil, false
}

func (cc *connCache) closeAll() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.closed = true
	for k, c := range cc.clients {
		c.Close()
		delete(cc.clients, k)
	}
	for k, c := range cc.bg {
		c.Close()
		delete(cc.bg, k)
	}
}

// backendFor returns the backend for a cluster:
//   - slurm: backend.SSHRunner over sshx.Connect(ctx, c.SSHHost, ui, ...).
func (cc *connCache) backendFor(ctx context.Context, c config.Cluster) (backend.Backend, error) {
	switch c.Type {
	case config.TypeSlurm:
		cl, err := cc.client(ctx, c.SSHHost)
		if err != nil {
			return nil, err
		}
		loc, err := c.Location()
		if err != nil {
			return nil, fmt.Errorf("cluster %s: %w", c.Name, err)
		}
		return &slurm.Slurm{Runner: &backend.SSHRunner{Client: cl}, Location: loc}, nil
	}
	return nil, fmt.Errorf("cluster %s has an unsupported type %q", c.Name, c.Type)
}

// newBrokerClient is the broker client with the LABMILL_BROKER_TOKEN token.
func newBrokerClient(baseURL, siteDomain, skillsVersion string, onCurrent func(string)) (*broker.Client, error) {
	return broker.New(broker.Config{BaseURL: baseURL, Token: envToken, UserAgent: "labmill/" + Version,
		Site: siteDomain, SkillsVersion: skillsVersion, OnSkillsCurrent: onCurrent})
}

// askSecrets shows one login-dialog question per field (masked unless the
// field is not Secret), so the values take the same path as an SSH password:
// UI to caller, never the model or the chat (M66.2).
func askSecrets(ctx context.Context, pr sshx.Prompter, title string, fields []app.PromptField) ([]string, error) {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		a, err := pr.Ask(ctx, sshx.Question{Kind: sshx.AskOther, Host: title, Prompt: f.Label, Echo: !f.Secret})
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
