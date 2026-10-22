package gui

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func lookIn(names ...string) func(string) (string, error) {
	return func(n string) (string, error) {
		for _, x := range names {
			if x == n {
				return "/usr/bin/" + n, nil
			}
		}
		return "", errors.New("not found")
	}
}

func TestSystemTerminalCommand(t *testing.T) {
	cli := "/opt/labmill/labmill"
	wrap := func(c string) []string {
		return []string{"sh", "-c", `"$0" shell "$1" || { echo; echo '` + failNote + `'; read x; }`, cli, c}
	}
	for _, c := range []struct {
		goos string
		have []string
		want termCommand
	}{
		{"windows", []string{"wt.exe"}, termCommand{Name: "wt.exe", Args: []string{cli, "shell", "bigcluster"}}},
		{"windows", nil, termCommand{Name: cli, Args: []string{"shell", "bigcluster"}, NewConsole: true}},
		{"linux", []string{"xterm", "konsole"}, termCommand{Name: "konsole", Args: append([]string{"-e"}, wrap("bigcluster")...)}},
		{"linux", []string{"x-terminal-emulator", "xterm"}, termCommand{Name: "x-terminal-emulator", Args: append([]string{"-e"}, wrap("bigcluster")...)}},
		{"linux", []string{"gnome-terminal"}, termCommand{Name: "gnome-terminal", Args: append([]string{"--"}, wrap("bigcluster")...)}},
		{"linux", []string{"xfce4-terminal"}, termCommand{Name: "xfce4-terminal", Args: append([]string{"-x"}, wrap("bigcluster")...)}},
		{"linux", []string{"xterm"}, termCommand{Name: "xterm", Args: append([]string{"-e"}, wrap("bigcluster")...)}},
	} {
		got, err := systemTerminalCommand(c.goos, cli, "bigcluster", lookIn(c.have...))
		if err != nil {
			t.Fatalf("%s %v: %v", c.goos, c.have, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v:\n got %+v\nwant %+v", c.goos, c.have, got, c.want)
		}
	}
	// macOS: the user's shell gets `exec /bin/sh -c '<script>' sh <cli> <cluster>`,
	// inside an escaped AppleScript string.
	tc, err := systemTerminalCommand("darwin", `/Apps/my "labmill"/labmill`, "bigcluster", lookIn())
	if err != nil || tc.Name != "osascript" || len(tc.Args) != 4 || tc.Args[0] != "-e" || tc.Args[2] != "-e" {
		t.Fatalf("darwin = %+v, %v", tc, err)
	}
	if !strings.HasPrefix(tc.Args[1], `tell application "Terminal" to do script "exec /bin/sh -c `) ||
		!strings.Contains(tc.Args[1], `my \"labmill\"`) || !strings.HasSuffix(tc.Args[1], ` bigcluster"`) {
		t.Errorf("script = %q", tc.Args[1])
	}
	for _, bad := range []string{"", "-x", "a b", "a;b", "a\nb", "$(id)", "a'b", "a\"b"} {
		if _, err := systemTerminalCommand("linux", cli, bad, lookIn("xterm")); err == nil {
			t.Errorf("cluster %q was accepted", bad)
		}
	}
	if _, err := systemTerminalCommand("linux", "", "c", lookIn("xterm")); err == nil {
		t.Error("no cli: want an error")
	}
	if _, err := systemTerminalCommand("linux", cli, "c", lookIn()); err == nil {
		t.Error("no terminal program: want an error")
	}
	if _, err := systemTerminalCommand("plan9", cli, "c", lookIn("xterm")); err == nil {
		t.Error("unknown OS: want an error")
	}
}

func TestFindCLI(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gui := write("labmill-gui", 0o755)
	none := func(string) (string, error) { return "", errors.New("no") }
	write("labmill-gui-linux-amd64", 0o755)
	write("labmill-notes", 0o755)
	write("labmill-darwin-arm64", 0o755) // another OS
	write("labmill-linux-arm64", 0o755)  // another architecture
	write("labmill-linux-amd64.exe", 0o755)
	write("labmill.exe", 0o755)
	write("labmill-linux-amd64", 0o644) // not executable
	if got := findCLI(gui, "linux", "amd64", none); got != "" {
		t.Errorf("found %q", got)
	}
	want := write("labmill-linux-amd64", 0o755)
	os.Chmod(want, 0o755)
	if got := findCLI(gui, "linux", "amd64", none); got != want {
		t.Errorf("found %q, want %q", got, want)
	}
	// Windows needs the .exe names.
	wdir := t.TempDir()
	wexe := filepath.Join(wdir, "labmill-gui.exe")
	for _, n := range []string{"labmill", "labmill-windows-amd64", "labmill-gui.exe"} {
		os.WriteFile(filepath.Join(wdir, n), []byte("x"), 0o755)
	}
	if got := findCLI(wexe, "windows", "amd64", none); got != "" {
		t.Errorf("windows found %q without .exe", got)
	}
	wantW := filepath.Join(wdir, "labmill-windows-amd64.exe")
	os.WriteFile(wantW, []byte("x"), 0o644)
	if got := findCLI(wexe, "windows", "amd64", none); got != wantW {
		t.Errorf("windows found %q, want %q", got, wantW)
	}
	// The PATH is the fallback.
	if got := findCLI(filepath.Join(t.TempDir(), "labmill-gui"), "linux", "amd64", lookIn("labmill")); got != "/usr/bin/labmill" {
		t.Errorf("PATH fallback = %q", got)
	}
}
