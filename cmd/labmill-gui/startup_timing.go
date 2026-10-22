//go:build desktop

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/gui"
	"labmill-client/internal/launch"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/startup"
)

// Startup timing (M46.1). The marks are in internal/startup; the page adds
// its own through PageAPI.StartupMark. In self-test mode with
// LABMILL_GUI_TIMING=1 the self-test also runs the real launch.Build, against
// a persistent home next to the executable, one per site, reused across runs
// (a warm start after the first; never the user's profile), so the numbers
// cover the whole startup; LABMILL_GUI_TIMING_SITE picks the site profile of
// that setup (default: generic).

// timingMode reports whether the self-test should time launch.Build too.
func timingMode(selftestOn bool) bool {
	return selftestOn && os.Getenv("LABMILL_GUI_TIMING") == "1"
}

// timingSite is the site profile of the timing setup.
func timingSite() string {
	if d := os.Getenv("LABMILL_GUI_TIMING_SITE"); d != "" {
		return d
	}
	return "generic"
}

// timingRuntimeDir is the short folder the SSH helper's socket lives in
// during the timing run: an AF_UNIX path holds at most 108 bytes and the
// timing home can sit deep inside a worktree, so the folder is under the
// temp dir with a short hash of base.
func timingRuntimeDir(base string) string {
	sum := sha256.Sum256([]byte(base))
	return filepath.Join(os.TempDir(), "labmill-t-"+hex.EncodeToString(sum[:4]))
}

// timingEnv maps every per-user folder into base, and the SSH helper's
// socket folder into timingRuntimeDir(base).
func timingEnv(base string) map[string]string {
	return map[string]string{
		"HOME":            base,
		"USERPROFILE":     base,
		"APPDATA":         filepath.Join(base, "AppData", "Roaming"),
		"LOCALAPPDATA":    filepath.Join(base, "AppData", "Local"),
		"XDG_CONFIG_HOME": filepath.Join(base, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(base, ".cache"),
		"XDG_STATE_HOME":  filepath.Join(base, ".local", "state"),
		"XDG_DATA_HOME":   filepath.Join(base, ".local", "share"),
		"XDG_RUNTIME_DIR": timingRuntimeDir(base), // sshx.SocketPath prefers it to HOME
	}
}

// timingHome points every per-user folder at the persistent timing home of
// the site, labmill-selftest-timing-home-<domain> next to the executable. It
// must run before anything reads them.
func timingHome() {
	base := "labmill-selftest-timing-home-" + timingSite()
	if exe, err := os.Executable(); err == nil {
		base = filepath.Join(filepath.Dir(exe), base)
	}
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	os.MkdirAll(base, 0o755)
	env := timingEnv(base)
	os.MkdirAll(env["XDG_RUNTIME_DIR"], 0o700)
	for k, v := range env {
		os.Setenv(k, v)
	}
}

// timingDone is closed when the timing Build ended (nil: not running);
// selfTest.finish waits for it before it prints the lines.
var timingDone chan struct{}

// runTimingBuild writes the timing setup once (outside the timed steps)
// and runs launch.Build on it, the way the app's startup does.
func runTimingBuild(ctx context.Context, bridge *gui.Bridge) {
	timingDone = make(chan struct{})
	go func() {
		defer close(timingDone)
		cfgPath, err := config.DefaultPath()
		if err != nil {
			fmt.Println("startup timing: no config path:", err)
			return
		}
		if _, err := os.Stat(cfgPath); err != nil || launch.NeedsHomeChoice(cfgPath) {
			var paths sshconfig.Paths
			paths, err = sshconfig.DefaultPaths()
			if err == nil {
				err = writeTimingSetup(paths, cfgPath, timingSite())
			}
			if err != nil {
				fmt.Println("startup timing: can't write the timing setup:", err)
				return
			}
		}
		startup.Mark("gui: timing build begins")
		rt, err := launch.BuildWithProgress(ctx, bridge, nil, nil)
		if err != nil {
			fmt.Println("startup timing: Build failed:", err)
			return
		}
		startup.Mark("gui: ready")
		rt.Close()
	}()
}

// writeTimingSetup writes the timing setup for the site domain. A site
// without known hosts (generic) gets a placeholder login host, as the home
// institution needs one (M55).
func writeTimingSetup(paths sshconfig.Paths, cfgPath, domain string) error {
	s, err := site.Get(domain)
	if err != nil {
		return err
	}
	a := launch.SetupAnswers{Site: domain, Username: "selftest"}
	if launch.NeedsHosts(s) {
		a.LoginHost = "login.example.edu"
	}
	_, err = launch.WriteSetup(paths, cfgPath, s, a, runtime.GOOS)
	return err
}

// printStartupLines prints the recorded timing lines (self-test output),
// waiting a little for the timing Build.
func printStartupLines() {
	if timingDone != nil {
		select {
		case <-timingDone:
		case <-time.After(30 * time.Second):
			fmt.Println("startup timing: Build still running after 30 s")
		}
	}
	fmt.Println("self-test startup timing (ms since process start; spans show their length):")
	for _, l := range startup.Lines() {
		fmt.Println(app.RedactLine(l, 200))
	}
}

// logStartup writes the timing lines to labmill.log once the runtime is up,
// and again after the background steps had time to run.
func logStartup(g *gate) {
	rt := g.runtime()
	if rt == nil {
		return
	}
	rt.LogStartupTimes()
	time.AfterFunc(20*time.Second, func() {
		if rt := g.runtime(); rt != nil {
			rt.LogStartupTimes()
		}
	})
}
