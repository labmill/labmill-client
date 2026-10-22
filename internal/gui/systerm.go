package gui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"labmill-client/internal/backend"
)

// "Open in system terminal" (M62.2): the OS's own terminal program runs
// `labmill shell <cluster>` with the Labmill command-line binary. The desktop
// app and the terminal interface are separate binaries, so the option is
// offered only when that binary is found (FindCLI).

// cliName reports whether a file name is the terminal interface: the plain
// name (with .exe on Windows) or the release name labmill-<os>-<arch>
// (.exe on Windows).
func cliName(name, goos, goarch string) bool {
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return name == "labmill"+ext || name == "labmill-"+goos+"-"+goarch+ext
}

// FindCLI returns the path of the Labmill terminal-interface binary: in the
// folder of the desktop app (exe), beside the .app bundle on macOS, or on
// the PATH. "" when it isn't found.
func FindCLI(exe string) string {
	return findCLI(exe, runtime.GOOS, runtime.GOARCH, exec.LookPath)
}

func findCLI(exe, goos, goarch string, lookPath func(string) (string, error)) string {
	if exe != "" {
		dirs := []string{filepath.Dir(exe)}
		if goos == "darwin" {
			// labmill.app/Contents/MacOS/labmill-gui: also the folder holding the bundle.
			if d := filepath.Dir(exe); filepath.Base(d) == "MacOS" {
				dirs = append(dirs, filepath.Dir(filepath.Dir(filepath.Dir(d))))
			}
		}
		for _, d := range dirs {
			ents, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if e.IsDir() || !cliName(e.Name(), goos, goarch) {
					continue
				}
				p := filepath.Join(d, e.Name())
				if filepath.Clean(p) == filepath.Clean(exe) {
					continue
				}
				if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && (goos == "windows" || fi.Mode()&0o111 != 0) {
					return p
				}
			}
		}
	}
	if p, err := lookPath("labmill"); err == nil {
		return p
	}
	return ""
}

// safeClusterName is what the system terminal accepts as a cluster name: it
// ends up on a command line of a program that may use a shell.
var safeClusterName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// failNote is shown by the Linux and macOS launchers when `labmill shell`
// failed, so the terminal window doesn't vanish with the message.
const failNote = "Labmill shell failed. Press Enter to close."

// termCommand is a program to start: Name with Args, in a console of its own
// when NewConsole (Windows, where the CLI is a console program).
type termCommand struct {
	Name       string
	Args       []string
	NewConsole bool
}

// systemTerminalCommand returns the command that opens a terminal window on
// goos running `cli shell <cluster>`. lookPath finds programs
// (exec.LookPath). No shell parses the cluster name unquoted.
func systemTerminalCommand(goos, cli, cluster string, lookPath func(string) (string, error)) (termCommand, error) {
	if cli == "" {
		return termCommand{}, errors.New("the Labmill command-line program wasn't found")
	}
	if !safeClusterName.MatchString(cluster) {
		return termCommand{}, fmt.Errorf("the cluster name %q can't be opened in the system terminal", cluster)
	}
	run := []string{cli, "shell", cluster}
	has := func(n string) bool { _, err := lookPath(n); return err == nil }
	switch goos {
	case "windows":
		if has("wt.exe") {
			return termCommand{Name: "wt.exe", Args: run}, nil
		}
		// No Windows Terminal: the CLI itself, in a new console window.
		return termCommand{Name: cli, Args: run[1:], NewConsole: true}, nil
	case "darwin":
		// exec /bin/sh -c '<script>' sh <cli> <cluster> in the user's own
		// shell (fish too); the AppleScript string is escaped.
		inner := `"$1" shell "$2" || { echo; echo '` + failNote + `'; read x; }`
		script := backend.Quote([]string{"exec", "/bin/sh", "-c", inner, "sh", cli, cluster})
		esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(script)
		return termCommand{Name: "osascript", Args: []string{
			"-e", `tell application "Terminal" to do script "` + esc + `"`,
			"-e", `tell application "Terminal" to activate`,
		}}, nil
	case "linux", "freebsd", "openbsd", "netbsd":
		// sh keeps the window open after a failure; cli and cluster are
		// positional parameters, never parsed by a shell.
		wrap := []string{"sh", "-c", `"$0" shell "$1" || { echo; echo '` + failNote + `'; read x; }`, cli, cluster}
		for _, c := range []struct{ name, flag string }{
			{"x-terminal-emulator", "-e"}, {"gnome-terminal", "--"}, {"konsole", "-e"}, {"xfce4-terminal", "-x"}, {"xterm", "-e"},
		} {
			if has(c.name) {
				return termCommand{Name: c.name, Args: append([]string{c.flag}, wrap...)}, nil
			}
		}
		return termCommand{}, errors.New("no terminal program was found (tried x-terminal-emulator, gnome-terminal, konsole, xfce4-terminal, xterm)")
	}
	return termCommand{}, fmt.Errorf("opening the system terminal isn't supported on %s", goos)
}

// OpenSystemTerminal opens the OS terminal running `labmill shell <cluster>`
// with the binary at cli. It returns once the program started; no shell
// parses the cluster name.
func OpenSystemTerminal(cli, cluster string) error {
	tc, err := systemTerminalCommand(runtime.GOOS, cli, cluster, exec.LookPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(tc.Name, tc.Args...)
	if tc.NewConsole {
		setNewConsole(cmd)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open the system terminal: %w", err)
	}
	go cmd.Wait() //nolint:errcheck // the exit status means nothing here
	return nil
}
