package sshconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func tempPaths(t *testing.T) Paths {
	t.Helper()
	return Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The owner's real config shape, including a Match ... Exec block.
const userConfig = `# SSH multiplexing control paths

Match Host * Exec "! nc -z localhost 1080 2> /dev/null"
    DynamicForward 1080

Host *
    ControlPath ~/.ssh/controlmasters/%r@%h:%p
    ControlPersist 10m
    User otheruser

# Jump Host
Host hpclogin
    HostName login.example.edu
    ControlMaster auto
`

func TestPaths(t *testing.T) {
	p := Paths{SSHDir: "/h/.ssh"}
	if p.UserConfig() != filepath.Join("/h/.ssh", "config") {
		t.Error(p.UserConfig())
	}
	if p.LabmillConfig() != filepath.Join("/h/.ssh", "config.d", "labmill") {
		t.Error(p.LabmillConfig())
	}
}

func TestRender(t *testing.T) {
	hosts := []Host{
		{Alias: "hpcone", HostName: "login.example.edu", User: "alice"},
		{Alias: "inner", HostName: "login.example.edu", User: "alice", Port: 2222, ProxyJump: "hpcone", IdentityFile: "~/.ssh/id_ed25519"},
	}
	unix := string(Render(hosts, "linux"))
	for _, want := range []string{
		"Host hpcone\n", "HostName login.example.edu", "User alice",
		"Host inner\n", "Port 2222", "ProxyJump hpcone", "IdentityFile ~/.ssh/id_ed25519",
		"ServerAliveInterval 30", "ControlMaster auto", "ControlPath ~/.ssh/controlmasters/%r@%h:%p", "ControlPersist 10m",
	} {
		if !strings.Contains(unix, want) {
			t.Errorf("linux render missing %q:\n%s", want, unix)
		}
	}
	if strings.Contains(unix, "Port 22\n") {
		t.Error("default port written")
	}
	if !strings.HasPrefix(unix, "#") {
		t.Error("render does not start with a managed-by comment")
	}
	if strings.Index(unix, "Host hpcone") > strings.Index(unix, "Host inner") {
		t.Error("hosts not in given order")
	}
	win := string(Render(hosts, "windows"))
	for _, bad := range []string{"ControlMaster", "ControlPath", "ControlPersist"} {
		if strings.Contains(win, bad) {
			t.Errorf("windows render contains %s", bad)
		}
	}
	if string(Render(hosts, "linux")) != unix {
		t.Error("render not deterministic")
	}
}

func TestWriteHostsAndLookup(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), userConfig)
	hosts := []Host{{Alias: "hpcone", HostName: "login.example.edu", User: "alice", ProxyJump: "hpclogin"}}
	if err := WriteHosts(p, hosts, "linux"); err != nil {
		t.Fatalf("WriteHosts: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p.LabmillConfig())
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("Labmill file: %v %v", fi, err)
		}
		if di, err := os.Stat(filepath.Join(p.SSHDir, "controlmasters")); err != nil || !di.IsDir() {
			t.Errorf("controlmasters dir not created: %v", err)
		}
	}

	// Labmill file wins for User (alice), even though the user's Host * says otheruser.
	h, found, err := Lookup(p, "hpcone")
	if err != nil || !found {
		t.Fatalf("Lookup hpcone: %+v %v %v", h, found, err)
	}
	if h.HostName != "login.example.edu" || h.User != "alice" || h.Port != 22 || h.ProxyJump != "hpclogin" {
		t.Errorf("hpcone = %+v", h)
	}

	// Only in the user's file; User comes from their Host *.
	h, found, err = Lookup(p, "hpclogin")
	if err != nil || !found || h.HostName != "login.example.edu" || h.User != "otheruser" {
		t.Errorf("hpclogin = %+v %v %v", h, found, err)
	}

	// Unknown alias: plain name, not found.
	h, found, err = Lookup(p, "files.example.edu")
	if err != nil || found && h.HostName != "files.example.edu" {
		t.Errorf("unknown = %+v %v %v", h, found, err)
	}
	if h.HostName != "files.example.edu" || h.Port != 22 {
		t.Errorf("unknown defaults = %+v", h)
	}
}

func TestLookupNoFiles(t *testing.T) {
	h, found, err := Lookup(tempPaths(t), "x.example.edu")
	if err != nil || found || h.HostName != "x.example.edu" || h.Port != 22 {
		t.Fatalf("got %+v %v %v", h, found, err)
	}
}

func TestEnsureIncludeCreates(t *testing.T) {
	p := tempPaths(t)
	changed, err := EnsureInclude(p, time.Unix(0, 0))
	if err != nil || !changed {
		t.Fatalf("EnsureInclude: %v %v", changed, err)
	}
	if got := read(t, p.UserConfig()); strings.TrimSpace(got) != IncludeLine {
		t.Errorf("new config = %q", got)
	}
}

func TestEnsureIncludeInserts(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), userConfig)
	now := time.Date(2026, 10, 3, 18, 30, 5, 0, time.UTC)
	changed, err := EnsureInclude(p, now)
	if err != nil || !changed {
		t.Fatalf("EnsureInclude: %v %v", changed, err)
	}
	got := read(t, p.UserConfig())
	if !strings.HasPrefix(got, IncludeLine+"\n") {
		t.Errorf("Include not first:\n%s", got)
	}
	if !strings.Contains(got, userConfig) {
		t.Errorf("original content not preserved:\n%s", got)
	}
	backup := p.UserConfig() + ".labmill-backup-20261003-183005"
	if read(t, backup) != userConfig {
		t.Errorf("backup content wrong")
	}

	// Idempotent: second call changes nothing and makes no new backup.
	changed, err = EnsureInclude(p, now.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("second EnsureInclude: %v %v", changed, err)
	}
	if _, err := os.Stat(p.UserConfig() + ".labmill-backup-20261003-193005"); err == nil {
		t.Error("second call made a backup")
	}
}

func TestEnsureIncludeMovesExistingLineToTop(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host a\n    User x\n\ninclude config.d/labmill\n")
	changed, err := EnsureInclude(p, time.Unix(1, 0))
	if err != nil || !changed {
		t.Fatalf("EnsureInclude: %v %v", changed, err)
	}
	got := read(t, p.UserConfig())
	if strings.Count(strings.ToLower(got), "config.d/labmill") != 1 || !strings.HasPrefix(got, IncludeLine) {
		t.Errorf("got:\n%s", got)
	}
}

func TestEnsureIncludeAlreadyFirstAfterComments(t *testing.T) {
	p := tempPaths(t)
	orig := "# my config\n\n  Include config.d/labmill\n\nHost a\n    User x\n"
	writeFile(t, p.UserConfig(), orig)
	changed, err := EnsureInclude(p, time.Unix(2, 0))
	if err != nil || changed {
		t.Fatalf("EnsureInclude: %v %v", changed, err)
	}
	if read(t, p.UserConfig()) != orig {
		t.Error("file modified")
	}
}

func TestFindAliasAndUnique(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), userConfig)
	if err := WriteHosts(p, []Host{{Alias: "hpc-files", HostName: "files.example.edu"}}, "linux"); err != nil {
		t.Fatal(err)
	}
	if a, ok, err := FindAliasForHostName(p, "LOGIN.example.edu"); err != nil || !ok || a != "hpclogin" {
		t.Errorf("shell: %q %v %v", a, ok, err)
	}
	if a, ok, err := FindAliasForHostName(p, "files.example.edu"); err != nil || !ok || a != "hpc-files" {
		t.Errorf("files: %q %v %v", a, ok, err)
	}
	if _, ok, _ := FindAliasForHostName(p, "other.example.edu"); ok {
		t.Error("found alias for unknown host")
	}
	if a, err := UniqueAlias(p, "hpcone"); err != nil || a != "hpcone" {
		t.Errorf("free alias: %q %v", a, err)
	}
	if a, err := UniqueAlias(p, "hpclogin"); err != nil || a != "hpclogin-2" {
		t.Errorf("taken alias: %q %v", a, err)
	}
	if a, err := UniqueAlias(p, "HPC-FILES"); err != nil || a != "HPC-FILES-2" {
		t.Errorf("taken alias (case): %q %v", a, err)
	}
}
