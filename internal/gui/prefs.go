package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// Prefs are the desktop app's own settings, kept next to clusters.yaml in
// gui.json (they are not cluster configuration).
type Prefs struct {
	// TrayNoticeShown: the one-time TrayNotice was shown.
	TrayNoticeShown bool `json:"trayNoticeShown"`
	// StartInTray: "Start Labmill in the tray when I log in" (off by default).
	// Turning it on registers the app to start at login with --tray (see
	// internal/autostart); turning it off unregisters it.
	StartInTray bool `json:"startInTray"`
	// RunWithoutAsking: "Run commands on the cluster without asking" (off
	// by default). The terminal reads the same file (settings.AutoRunRemote).
	RunWithoutAsking bool `json:"runWithoutAsking"`
	// ApproveAll: "Approve everything automatically" (off by default). A
	// superset of RunWithoutAsking: every approval is given at once, except
	// reads of sign-in secret files, questions and logins. The terminal
	// reads the same file (settings.AutoApproveAll).
	ApproveAll bool `json:"approveAll"`
	// AutoDownload: "Download results automatically" (off by default,
	// M60.1). The terminal reads the same file (settings.AutoDownloadResults).
	AutoDownload bool `json:"autoDownload"`
	// Window: where the user left the main window (M47.1); nil until the
	// app has saved it once.
	Window *WindowGeom `json:"window,omitempty"`
	// JobsWidth: the jobs panel's share of the main area, set by dragging
	// the divider between chat and panel (M51.1); 0 means the default
	// (DefaultJobsWidth). Kept out of Window so a size change of the window
	// and a divider move are saved independently.
	JobsWidth float64 `json:"jobs_width,omitempty"`
}

// Limits and default of Prefs.JobsWidth, as fractions of the main area.
const (
	DefaultJobsWidth = 0.40
	MinJobsWidth     = 0.15
	MaxJobsWidth     = 0.60
)

// ClampJobsWidth returns f limited to [MinJobsWidth, MaxJobsWidth]; 0, a
// negative number, NaN or Inf mean "the default" and return 0.
func ClampJobsWidth(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0
	}
	return min(max(f, MinJobsWidth), MaxJobsWidth)
}

// PrefsPath is <config dir>/labmill/gui.json (the folder of clusters.yaml).
func PrefsPath() (string, error) {
	p, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "gui.json"), nil
}

// LoadPrefs reads path; a missing file is the zero Prefs and no error.
func LoadPrefs(path string) (Prefs, error) {
	var p Prefs
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return Prefs{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return p, nil
}

// SavePrefs writes p to path atomically (temp file in the same folder, then
// rename), creating the folder (0700) if needed; the file is 0600.
//
// Keys Prefs doesn't know (the settings that internal/settings writes
// itself, such as notifyJobEnd or lastSeen) are kept as they are in the file.
func SavePrefs(path string, p Prefs) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return fmt.Errorf("save prefs: bad settings")
	}
	if old, err := os.ReadFile(path); err == nil {
		var keep map[string]any
		if json.Unmarshal(old, &keep) == nil {
			for k, v := range keep {
				if _, known := prefKeys[k]; !known {
					m[k] = v
				}
			}
		}
	}
	if b, err = json.MarshalIndent(m, "", "  "); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save prefs: %w", err)
	}
	f, err := os.CreateTemp(dir, ".gui-*.json")
	if err != nil {
		return fmt.Errorf("save prefs: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp) // our own temp file, never the user's
		return fmt.Errorf("save prefs: %w", werr)
	}
	return nil
}

// CountJobs splits jobs into running (StateRunning) and waiting (submitted
// or pending in the scheduler's queue).
func CountJobs(jobs []store.Job) (running, waiting int) {
	for _, j := range jobs {
		switch j.State {
		case store.StateRunning:
			running++
		case store.StateSubmitted, store.StatePending:
			waiting++
		}
	}
	return running, waiting
}

// TrayStarting is the tray's status line and tooltip until the runtime is
// ready (the icon comes up before the window).
const TrayStarting = "Labmill is starting…"

// TrayTooltip is the tray icon's tooltip for a status line: a line that
// already starts with "Labmill" (TrayStarting) as is, else "Labmill: <status>".
func TrayTooltip(status string) string {
	if strings.HasPrefix(status, "Labmill") {
		return status
	}
	return "Labmill: " + status
}

// TrayStatus is the disabled status line of the tray menu, e.g.
// "2 jobs running · logged in to bigcluster", "1 job running, 3 waiting · not
// logged in" or "no jobs running · logged in to bigcluster, login +1". At most
// two aliases are named.
func TrayStatus(running, waiting int, aliases []string) string {
	var jobs string
	switch {
	case running == 1:
		jobs = "1 job running"
	case running > 1:
		jobs = fmt.Sprintf("%d jobs running", running)
	default:
		jobs = "no jobs running"
	}
	if waiting > 0 {
		jobs += fmt.Sprintf(", %d waiting", waiting)
	}
	login := "not logged in"
	if n := len(aliases); n > 0 {
		shown := aliases
		if n > 2 {
			shown = aliases[:2]
		}
		login = "logged in to " + strings.Join(shown, ", ")
		if n > 2 {
			login += fmt.Sprintf(" +%d", n-2)
		}
	}
	return jobs + " · " + login
}

// prefKeys are the JSON keys of Prefs (SavePrefs writes them itself and
// keeps every other key of the file).
var prefKeys = func() map[string]bool {
	t := reflect.TypeOf(Prefs{})
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		keys[name] = true
	}
	return keys
}()
