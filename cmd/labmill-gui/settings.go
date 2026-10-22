//go:build desktop

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"labmill-client/internal/gui"
	"labmill-client/internal/launch"
	"labmill-client/internal/settings"
)

// The Settings page (M29). See frontend/settings.js for the page and
// internal/settings for the rules; this file binds them to the window.

// Events of the Settings page.
const (
	evOpenSettings    = "Labmill:open-settings"    // {}: show the Settings page (tray menu "Settings…", macOS File > Settings…)
	evSettingsChanged = "Labmill:settings-changed" // {}: re-read the view
)

// SettingsAPI is bound to the page as window.go.main.SettingsAPI, apart
// from PageAPI so the page's chat code and the settings code each reach only
// their own methods. It reads and saves settings through settings.Service
// (field errors come back in settings.Result for inline display), and opens the logs
// folder with the safe folder opener. Nothing it returns holds a secret.
type SettingsAPI struct {
	svc     *settings.Service
	app     *gui.App
	version string

	// Hooks into the running app; any may be nil (self-test, or still
	// starting).
	brokerURL           func() string
	loggedIn            func() ([]string, bool)
	prefs               func() gui.Prefs
	setStartInTray      func(on bool) error
	setRunWithoutAsking func(on bool) error
	setApproveAll       func(on bool) error
	setAutoDownload     func(on bool) error
	resetNotices        func() error
	jobsWidth           func() float64
	setJobsWidth        func(f float64) error
	openFolder          func(dir string) error
	emit                func(name string, data any)
	// More clusters at the user's institution (M57.2): what adding one
	// writes, and the add itself.
	planMore func(name string) (launch.HomeClusterPlan, error)
	addMore  func(ctx context.Context, name, username string) (string, error)
	// checkMore validates the account name (and a conflict with a reused SSH
	// entry) before the add starts, so the form stays open with the message.
	checkMore func(name, username string) error
}

func (s *SettingsAPI) info() settings.Info {
	in := settings.Info{Version: s.version}
	if s.brokerURL != nil {
		in.BrokerURL = s.brokerURL()
	}
	if s.loggedIn != nil {
		in.LoggedIn, in.LoginKnown = s.loggedIn()
	}
	if s.prefs != nil {
		p := s.prefs()
		in.StartInTray, in.NoticesShown = p.StartInTray, p.TrayNoticeShown
		// What the agent reads, so the page shows what it will do.
		in.RunWithoutAsking = settings.AutoRunRemote(s.svc.Paths.Prefs)
		in.ApproveAll = settings.AutoApproveAll(s.svc.Paths.Prefs)
		in.AutoDownload = settings.AutoDownloadResults(s.svc.Paths.Prefs)
		in.NotifyJobEnd = settings.NotifyJobEnd(s.svc.Paths.Prefs)
		in.EmailOnJobEnd = settings.EmailOnJobEnd(s.svc.Paths.Prefs)
	}
	return in
}

// Get returns every setting (settings.View). It reads only local files: no
// network, no login.
func (s *SettingsAPI) Get() settings.View { return s.svc.Get(s.info()) }

// SaveSite sets the institution.
func (s *SettingsAPI) SaveSite(domain string) settings.Result { return s.svc.SaveSite(domain) }

// SaveCluster saves a cluster (oldName "" adds one).
func (s *SettingsAPI) SaveCluster(oldName string, c settings.Cluster) settings.Result {
	return s.svc.SaveCluster(oldName, c)
}

// RemoveCluster removes a cluster.
func (s *SettingsAPI) RemoveCluster(name string) settings.Result { return s.svc.RemoveCluster(name) }

// SaveHost saves an SSH connection in Labmill's own file (oldAlias "" adds one).
func (s *SettingsAPI) SaveHost(oldAlias string, h settings.HostView) settings.Result {
	return s.svc.SaveHost(oldAlias, h)
}

// RemoveHost removes an SSH connection from Labmill's own file.
func (s *SettingsAPI) RemoveHost(alias string) settings.Result { return s.svc.RemoveHost(alias) }

// SetStartInTray turns "Start in the tray when I log in" on or off (the
// same as the tray menu's checkbox).
func (s *SettingsAPI) SetStartInTray(on bool) settings.Result {
	if s.setStartInTray == nil {
		return settings.Result{Error: "this can't be changed here", Errors: []settings.FieldError{}}
	}
	if err := s.setStartInTray(on); err != nil {
		return settings.Result{Error: err.Error(), Errors: []settings.FieldError{}}
	}
	msg := "Labmill won't start when you log in."
	if on {
		msg = "Labmill starts in the tray when you log in."
	}
	return settings.Result{OK: true, Errors: []settings.FieldError{}, Message: msg}
}

// SetRunWithoutAsking turns "Run commands on the cluster without asking"
// on or off. It applies at once, to the next command.
func (s *SettingsAPI) SetRunWithoutAsking(on bool) settings.Result {
	if s.setRunWithoutAsking == nil {
		return settings.Result{Error: "this can't be changed here", Errors: []settings.FieldError{}}
	}
	if err := s.setRunWithoutAsking(on); err != nil {
		return settings.Result{Error: err.Error(), Errors: []settings.FieldError{}}
	}
	msg := "Labmill shows commands for your OK again."
	if on {
		msg = "Labmill now runs commands on your cluster without asking."
	}
	return settings.Result{OK: true, Errors: []settings.FieldError{}, Message: msg}
}

// ApproveAll reports "Approve everything automatically" (what the agent
// reads), for the chat window's checkbox and banner.
func (s *SettingsAPI) ApproveAll() bool { return settings.AutoApproveAll(s.svc.Paths.Prefs) }

// SetApproveAll turns "Approve everything automatically" on or off. It
// applies at once, to the next approval.
func (s *SettingsAPI) SetApproveAll(on bool) settings.Result {
	if s.setApproveAll == nil {
		return settings.Result{Error: "this can't be changed here", Errors: []settings.FieldError{}}
	}
	if err := s.setApproveAll(on); err != nil {
		return settings.Result{Error: err.Error(), Errors: []settings.FieldError{}}
	}
	msg := "Labmill asks for your OK again."
	if on {
		msg = "Labmill now approves every step for you."
	}
	return settings.Result{OK: true, Errors: []settings.FieldError{}, Message: msg}
}

// AutoDownload reports "Download results automatically" (M60.1; what the
// job monitor reads), for the chat window's checkbox.
func (s *SettingsAPI) AutoDownload() bool { return settings.AutoDownloadResults(s.svc.Paths.Prefs) }

// SetAutoDownload turns "Download results automatically" on or off. It
// applies at once, to the next job that ends.
func (s *SettingsAPI) SetAutoDownload(on bool) settings.Result {
	if s.setAutoDownload == nil {
		return settings.Result{Error: "this can't be changed here", Errors: []settings.FieldError{}}
	}
	if err := s.setAutoDownload(on); err != nil {
		return settings.Result{Error: err.Error(), Errors: []settings.FieldError{}}
	}
	msg := "Labmill leaves results on the cluster until you ask for them."
	if on {
		msg = "Labmill now downloads the results of a finished job by itself."
	}
	return settings.Result{OK: true, Errors: []settings.FieldError{}, Message: msg}
}

// SetNotifyJobEnd turns "Notify me when a job ends" on or off (M64.3). The
// job monitor reads it when a job ends, so it applies at once.
func (s *SettingsAPI) SetNotifyJobEnd(on bool) settings.Result {
	return saveSwitch(settings.SetNotifyJobEnd(s.svc.Paths.Prefs, on), on,
		"Labmill shows a notification when a job ends.", "Labmill won't show a notification when a job ends.")
}

// SetEmailOnJobEnd turns "Email me when a Slurm job ends" on or off
// (M64.3). It applies to the next Slurm script Labmill writes.
func (s *SettingsAPI) SetEmailOnJobEnd(on bool) settings.Result {
	return saveSwitch(settings.SetEmailOnJobEnd(s.svc.Paths.Prefs, on), on,
		"New Slurm jobs ask Slurm to email you when they end or fail.", "New Slurm jobs won't ask Slurm for email.")
}

// saveSwitch turns the result of saving a switch into the page's Result.
func saveSwitch(err error, on bool, onMsg, offMsg string) settings.Result {
	if err != nil {
		return settings.Result{Error: err.Error(), Errors: []settings.FieldError{}}
	}
	msg := offMsg
	if on {
		msg = onMsg
	}
	return settings.Result{OK: true, Errors: []settings.FieldError{}, Message: msg}
}

// JobsWidth is the saved share of the main area for the jobs panel (a
// fraction; 0 means the default), for the divider (M51.1).
func (s *SettingsAPI) JobsWidth() float64 {
	if s.jobsWidth == nil {
		return 0
	}
	return gui.ClampJobsWidth(s.jobsWidth())
}

// SetJobsWidth saves the jobs panel's share of the main area (clamped; 0
// resets to the default) in gui.json next to the window geometry.
func (s *SettingsAPI) SetJobsWidth(f float64) error {
	if s.setJobsWidth == nil {
		return nil
	}
	return s.setJobsWidth(gui.ClampJobsWidth(f))
}

// ResetNotices shows the one-time tips again (the "still running in the
// tray" notice).
func (s *SettingsAPI) ResetNotices() settings.Result {
	if s.resetNotices == nil {
		return settings.Result{Error: "this can't be changed here", Errors: []settings.FieldError{}}
	}
	if err := s.resetNotices(); err != nil {
		return settings.Result{Error: err.Error(), Errors: []settings.FieldError{}}
	}
	return settings.Result{OK: true, Errors: []settings.FieldError{}, Message: "The one-time tips will show again."}
}

// MoreClusterPlan describes adding a cluster of the user's institution that
// isn't set up yet: the account name it would use ("" when the page must
// ask), the SSH hosts and the files that change. It reads local files only.
func (s *SettingsAPI) MoreClusterPlan(name string) (launch.HomeClusterPlan, error) {
	if s.planMore == nil {
		return launch.HomeClusterPlan{}, errNotReady
	}
	return s.planMore(name)
}

// AddMoreCluster adds that cluster for the account username. The click on
// the page's "Add" button, after MoreClusterPlan's preview, is the approval.
// It runs as a turn of its own (shown in the chat) and returns at once; the
// page refreshes with Labmill:settings-changed.
func (s *SettingsAPI) AddMoreCluster(name, username string) error {
	if s.addMore == nil || s.checkMore == nil {
		return errNotReady
	}
	if err := s.checkMore(name, username); err != nil {
		return err
	}
	return s.app.RunAction("Add the cluster "+name, func(ctx context.Context) (string, error) {
		msg, err := s.addMore(ctx, name, username)
		if s.emit != nil {
			s.emit(evSettingsChanged, map[string]string{})
		}
		return msg, err
	})
}

// OpenLogsFolder opens the folder of labmill.log (and the job database) in
// the file manager.
func (s *SettingsAPI) OpenLogsFolder() error {
	if s.openFolder == nil {
		return errors.New("opening folders isn't available")
	}
	dir := filepath.Dir(s.svc.Paths.Log)
	if err := s.openFolder(dir); err != nil {
		if strings.Contains(err.Error(), "isn't a folder") {
			return errors.New("there is no log folder yet (" + dir + ")")
		}
		return err
	}
	return nil
}

// Diagnostics returns the "Copy diagnostics" text (settings.View.Diagnostics:
// no secrets, no tokens, no keys, no user names).
func (s *SettingsAPI) Diagnostics() string { return s.Get().Diagnostics() }
