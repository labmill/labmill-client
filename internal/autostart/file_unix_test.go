//go:build !windows

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileEnableDisable(t *testing.T) {
	for _, osName := range []string{"linux", "darwin"} {
		t.Run(osName, func(t *testing.T) {
			home := t.TempDir()
			oldOS, oldHome := goos, homeDir
			goos, homeDir = osName, func() (string, error) { return home, nil }
			t.Cleanup(func() { goos, homeDir = oldOS, oldHome })
			t.Setenv("XDG_CONFIG_HOME", "")
			e := Entry{Name: "labmill", Label: "com.example.labmill", Exe: "/opt/labmill-gui", Args: []string{"--tray"}}

			if on, err := Enabled(e); err != nil || on {
				t.Fatalf("before: %v, %v", on, err)
			}
			if err := Disable(e); err != nil {
				t.Fatalf("Disable without registration: %v", err)
			}
			path := filepath.Join(home, ".config", "autostart", "labmill.desktop")
			if osName == "darwin" {
				path = filepath.Join(home, "Library", "LaunchAgents", "com.example.labmill.plist")
			}
			if _, err := os.Stat(path); err == nil {
				t.Fatal("Disable created a file")
			}
			if err := Enable(e); err != nil {
				t.Fatal(err)
			}
			if on, err := Enabled(e); err != nil || !on {
				t.Fatalf("after Enable: %v, %v", on, err)
			}
			b, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(b), "--tray") {
				t.Fatalf("file %s: %q, %v", path, b, err)
			}
			if err := Disable(e); err != nil {
				t.Fatal(err)
			}
			if on, err := Enabled(e); err != nil || on {
				t.Fatalf("after Disable: %v, %v", on, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("Disable removed the file (it should rewrite it): %v", err)
			}
		})
	}
}

// TestFileEnabledEditedEntry: an entry whose command was edited still counts
// as enabled; Hidden=true (Linux) and Disabled (macOS) turn it off.
func TestFileEnabledEditedEntry(t *testing.T) {
	cases := []struct {
		goos, rel, content string
		want               bool
	}{
		{"linux", ".config/autostart/labmill.desktop", "[Desktop Entry]\nExec=/other/labmill-gui\n", true},
		{"linux", ".config/autostart/labmill.desktop", "[Desktop Entry]\nExec=/other/labmill-gui\nHidden=true\n", false},
		{"linux", ".config/autostart/labmill.desktop", "[Desktop Entry]\nHidden=false\n", true},
		{"darwin", "Library/LaunchAgents/com.example.labmill.plist", "<plist><dict><key>Label</key><string>x</string></dict></plist>", true},
		{"darwin", "Library/LaunchAgents/com.example.labmill.plist", "<plist><dict><key>Disabled</key><true/></dict></plist>", false},
	}
	for _, c := range cases {
		home := t.TempDir()
		oldOS, oldHome := goos, homeDir
		goos, homeDir = c.goos, func() (string, error) { return home, nil }
		t.Setenv("XDG_CONFIG_HOME", "")
		path := filepath.Join(home, c.rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		e := Entry{Name: "labmill", Label: "com.example.labmill", Exe: "/opt/labmill-gui", Args: []string{"--tray"}}
		if on, err := Enabled(e); err != nil || on != c.want {
			t.Errorf("%s %q: Enabled = %v, %v; want %v", c.goos, c.content, on, err, c.want)
		}
		goos, homeDir = oldOS, oldHome
	}
}
