package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/launch"
	"labmill-client/internal/settings"
	"labmill-client/internal/store"
	"labmill-client/internal/tui"
)

const welcome = `Hi, I'm **labmill**. Tell me what you'd like to compute in plain language, for example "run my training script on a GPU".

I can find the data on this computer, send it to your cluster, start the job, keep an eye on it and bring the results back. Nothing leaves your computer until you approve the exact files and destination.

Tip: tell me where your data lives with ` + "`/data <folder>`" + ` and, if you have code, where it is with ` + "`/code <folder>`" + ` (Tab completes folder names). No code yet? I can write the analysis code for you; you see it before anything runs.`

// runTUI builds the runtime (first-run setup included) and runs the
// terminal interface until the user quits. Errors before the UI starts are
// printed to stderr in plain language (exit 1).
func runTUI(ctx context.Context, stderr io.Writer) int {
	launch.Version = version
	bridge := tui.NewBridge()
	rt, err := launch.Build(ctx, bridge, askSetup)
	if err != nil {
		fmt.Fprintf(stderr, "Labmill: %v\n", err)
		return 1
	}
	defer rt.Close()
	jobs := func(ctx context.Context) ([]store.Job, error) {
		return rt.Store.Jobs(ctx, store.JobFilter{Limit: 20})
	}
	opts := tui.Options{Session: rt.Session, Bridge: bridge, Jobs: jobs, Status: rt.CurrentStatus(), Welcome: welcome}
	if rt.Env != nil && rt.Env.Workspace != nil {
		opts.Folders = rt.Env.Workspace // /data and /code
		opts.Home = rt.Env.Workspace.Home()
	}
	// /settings shows every setting and the files (editing is in the
	// desktop app's Settings page).
	opts.Settings = func() string {
		p, err := settings.DefaultPaths()
		if err != nil {
			return "Settings can't be shown: " + err.Error()
		}
		return (&settings.Service{Paths: p}).Get(settings.Info{Version: version, BrokerURL: rt.Site.BrokerURL, RunWithoutAsking: settings.AutoRunRemote(p.Prefs), ApproveAll: settings.AutoApproveAll(p.Prefs), AutoDownload: settings.AutoDownloadResults(p.Prefs)}).Text()
	}
	// /autoapprove on|off saves "Approve everything automatically" in gui.json,
	// which the agent reads at every approval.
	if p, perr := settings.DefaultPaths(); perr == nil {
		opts.AutoApprove = func() bool { return settings.AutoApproveAll(p.Prefs) }
		opts.SetAutoApprove = func(on bool) error { return settings.SetAutoApproveAll(p.Prefs, on) }
		// /autodownload on|off saves "Download results automatically" there too.
		opts.AutoDownload = func() bool { return settings.AutoDownloadResults(p.Prefs) }
		opts.SetAutoDownload = func(on bool) error { return settings.SetAutoDownloadResults(p.Prefs, on) }
	}
	// /shell <cluster> suspends the TUI and gives the terminal to a shell on
	// the login node (M62.1).
	opts.Shell = func(ctx context.Context, cluster, dir string) (tea.ExecCommand, error) {
		sh, err := rt.OpenShell(ctx, cluster, dir)
		if err != nil {
			return nil, err
		}
		return sh, nil
	}
	m := tui.NewModel(opts)
	// Libraries write warnings to os.Stderr (e.g. the SDK's tool runner),
	// which would garble the full-screen UI: send them to labmill.log while
	// it runs (and while a last turn finishes; error messages below go to
	// the stderr this function was given).
	restore := redirectStderr(rt.LogPath)
	_, err = tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	bridge.Close()
	// A turn may still be finishing (e.g. a job being submitted); give it a
	// moment so it's recorded before the store closes.
	rt.Session.Wait(10 * time.Second)
	restore()
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "Labmill: %v\n", err)
		return 1
	}
	return 0
}

// redirectStderr points os.Stderr at the log file at path (appended, 0600,
// its folder 0700) and returns the function that puts the old one back and
// closes the file. Only writes through the os.Stderr variable move; when
// the file can't be opened (or path is empty) nothing changes.
func redirectStderr(path string) (restore func()) {
	if path == "" {
		return func() {}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() {}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func() {}
	}
	old := os.Stderr
	os.Stderr = f
	return func() {
		os.Stderr = old
		f.Close()
	}
}
