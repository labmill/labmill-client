package sshconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editPaths(t *testing.T, Labmill, user string) Paths {
	t.Helper()
	p := Paths{SSHDir: t.TempDir()}
	if Labmill != "" {
		if err := os.MkdirAll(filepath.Dir(p.LabmillConfig()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.LabmillConfig(), []byte(Labmill), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if user != "" {
		if err := os.WriteFile(p.UserConfig(), []byte(user), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

const labmillFile = `# Managed by Labmill. Edit with care: Labmill may rewrite this file.

Host hpclogin
    HostName login.example.edu
    User alice
    ServerAliveInterval 30
    # my own note
    ControlMaster auto

# the transfer host
Host files
    HostName files.example.edu
    User alice
    Port 2222
    IdentityFile ~/.ssh/id_ed25519
`

const userFile = `Include config.d/labmill

Host *
    User fallback

Host mybox
    HostName box.example.org
    Port 2200

Host gw-*
    User x
`

func TestListHosts(t *testing.T) {
	p := editPaths(t, labmillFile, userFile)
	got, err := ListHosts(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	if e := got[0]; e.Alias != "hpclogin" || !e.InLabmill || e.HostName != "login.example.edu" || e.User != "alice" || e.Port != 0 {
		t.Errorf("hpclogin = %+v", e)
	}
	if e := got[1]; e.Alias != "files" || !e.InLabmill || e.Port != 2222 || e.IdentityFile != "~/.ssh/id_ed25519" {
		t.Errorf("files = %+v", e)
	}
	if e := got[2]; e.Alias != "mybox" || e.InLabmill || e.HostName != "box.example.org" || e.Port != 2200 || e.User != "fallback" {
		t.Errorf("mybox = %+v", e)
	}
}

func TestPutLabmillHostEditKeepsOtherLines(t *testing.T) {
	p := editPaths(t, labmillFile, userFile)
	before, _ := os.ReadFile(p.UserConfig())
	created, err := PutLabmillHost(p, Host{Alias: "hpclogin", HostName: "login.example.edu", User: "bob", Port: 2022, ProxyJump: "gate"}, "linux")
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	data, _ := os.ReadFile(p.LabmillConfig())
	s := string(data)
	for _, want := range []string{"    User bob\n", "    Port 2022\n", "    ProxyJump gate\n", "    # my own note\n", "    ControlMaster auto\n", "# the transfer host\n", "    IdentityFile ~/.ssh/id_ed25519\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "User alice\n    ServerAlive") {
		t.Errorf("old user still there:\n%s", s)
	}
	h, _, err := Lookup(p, "hpclogin")
	if err != nil || h.User != "bob" || h.Port != 2022 || h.ProxyJump != "gate" {
		t.Errorf("Lookup = %+v, %v", h, err)
	}
	// Clearing values removes the lines.
	if _, err := PutLabmillHost(p, Host{Alias: "files", HostName: "files.example.edu"}, "linux"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p.LabmillConfig())
	if strings.Contains(string(data), "Port 2222") || strings.Contains(string(data), "files.example.edu\n    User alice") {
		t.Errorf("cleared values still there:\n%s", data)
	}
	if !strings.Contains(string(data), "IdentityFile ~/.ssh/id_ed25519") {
		t.Errorf("IdentityFile was dropped:\n%s", data)
	}
	after, _ := os.ReadFile(p.UserConfig())
	if string(after) != string(before) {
		t.Error("the user's config changed")
	}
}

// PutLabmillHosts writes all hosts in one step, or none: a host that fails
// validation leaves the file as it was.
func TestPutLabmillHostsAllOrNothing(t *testing.T) {
	p := editPaths(t, "Host a\n    HostName a.example.edu\n    User old\n\nHost b\n    HostName b.example.edu\n    User old\n", "")
	before, _ := os.ReadFile(p.LabmillConfig())
	_, err := PutLabmillHosts(p, []Host{
		{Alias: "a", HostName: "a.example.edu", User: "new"},
		{Alias: "b", HostName: "b.example.edu", User: "bad name"},
	}, "linux")
	if err == nil {
		t.Fatal("invalid host accepted")
	}
	if after, _ := os.ReadFile(p.LabmillConfig()); string(after) != string(before) {
		t.Errorf("file changed:\n%s", after)
	}
	created, err := PutLabmillHosts(p, []Host{
		{Alias: "a", HostName: "a.example.edu", User: "new"},
		{Alias: "b", HostName: "b.example.edu", User: "new"},
		{Alias: "c", HostName: "c.example.edu", User: "new"},
	}, "linux")
	if err != nil || len(created) != 3 || created[0] || created[1] || !created[2] {
		t.Fatalf("created = %v, %v", created, err)
	}
	after, _ := os.ReadFile(p.LabmillConfig())
	if strings.Count(string(after), "User new") != 3 || strings.Contains(string(after), "User old") {
		t.Errorf("file:\n%s", after)
	}
}

func TestPutLabmillHostAppendAndRemove(t *testing.T) {
	p := editPaths(t, "", "")
	created, err := PutLabmillHost(p, Host{Alias: "new", HostName: "new.example.edu", User: "u"}, "windows")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, err := PutLabmillHost(p, Host{Alias: "two", HostName: "two.example.edu"}, "windows"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.LabmillConfig())
	if strings.Count(string(data), "# Managed by Labmill") != 1 || strings.Contains(string(data), "ControlMaster") {
		t.Errorf("unexpected file:\n%s", data)
	}
	if err := RemoveLabmillHost(p, "new"); err != nil {
		t.Fatal(err)
	}
	list, err := ListHosts(p)
	if err != nil || len(list) != 1 || list[0].Alias != "two" {
		t.Errorf("after remove: %+v, %v", list, err)
	}
	if err := RemoveLabmillHost(p, "new"); err == nil {
		t.Error("removing a missing host worked")
	}
	if _, err := PutLabmillHost(p, Host{Alias: "bad", HostName: "x\nProxyCommand evil"}, "linux"); err == nil {
		t.Error("an injected value was accepted")
	}
}

func TestRemoveLabmillHostKeepsNextComment(t *testing.T) {
	p := editPaths(t, labmillFile, "")
	if err := RemoveLabmillHost(p, "hpclogin"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.LabmillConfig())
	s := string(data)
	if strings.Contains(s, "hpclogin") || !strings.Contains(s, "# the transfer host\nHost files") || !strings.HasPrefix(s, "# Managed by Labmill") {
		t.Errorf("unexpected file:\n%s", s)
	}
}
