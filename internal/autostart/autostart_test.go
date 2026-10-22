package autostart

import (
	"strings"
	"testing"
)

func TestWindowsCommand(t *testing.T) {
	e := Entry{Exe: `C:\Program Files\labmill\labmill-gui.exe`, Args: []string{"--tray", "a b"}}
	want := `"C:\Program Files\labmill\labmill-gui.exe" --tray "a b"`
	if got := windowsCommand(e); got != want {
		t.Errorf("windowsCommand = %s, want %s", got, want)
	}
}

func TestDesktopExecQuoting(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"/opt/labmill/labmill-gui", "--tray"}, "/opt/labmill/labmill-gui --tray"},
		{[]string{"/home/a b/labmill-gui"}, `"/home/a b/labmill-gui"`},
		{[]string{`/x/$y"z`}, `"/x/\$y\"z"`},
		{[]string{"/x/100%"}, "/x/100%%"},
	} {
		if got := desktopExec(c.argv); got != c.want {
			t.Errorf("desktopExec(%q) = %s, want %s", c.argv, got, c.want)
		}
	}
}

func TestDesktopEntryHidden(t *testing.T) {
	e := Entry{Name: "labmill", Exe: "/opt/labmill-gui", Args: []string{"--tray"}}
	on, off := string(desktopEntry(e, false)), string(desktopEntry(e, true))
	if !strings.Contains(on, "Exec=/opt/labmill-gui --tray\n") || strings.Contains(on, "Hidden=true") {
		t.Errorf("enabled entry:\n%s", on)
	}
	if !strings.Contains(off, "Hidden=true\n") || !strings.Contains(off, "X-GNOME-Autostart-enabled=false") {
		t.Errorf("disabled entry:\n%s", off)
	}
}

func TestLaunchAgentPlist(t *testing.T) {
	e := Entry{Label: "com.example.labmill", Exe: "/Applications/labmill.app/Contents/MacOS/labmill & co", Args: []string{"--tray"}}
	on, off := string(launchAgentPlist(e, false)), string(launchAgentPlist(e, true))
	for _, want := range []string{"<string>com.example.labmill</string>", "<string>/Applications/labmill.app/Contents/MacOS/labmill &amp; co</string>",
		"<string>--tray</string>", "<key>RunAtLoad</key>\n\t<true/>"} {
		if !strings.Contains(on, want) {
			t.Errorf("plist lacks %q:\n%s", want, on)
		}
	}
	if strings.Contains(on, "Disabled") || !strings.Contains(off, "<key>Disabled</key>\n\t<true/>") {
		t.Errorf("Disabled key wrong:\non:\n%s\noff:\n%s", on, off)
	}
}
