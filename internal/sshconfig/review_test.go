package sshconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestValidationRejectsInjection(t *testing.T) {
	bad := []Host{
		{Alias: "a", HostName: "h\n    ProxyCommand sh -c evil"},
		{Alias: "a", User: "u v"},
		{Alias: "a", ProxyJump: "x#y"},
		{Alias: "a b"}, {Alias: "*"}, {Alias: "a,b"}, {Alias: ""}, {Alias: "a", Port: 70000}, {Alias: "a", Port: -1},
		{Alias: "a", IdentityFile: "k\nProxyCommand x"}, {Alias: "a", IdentityFile: `k"x`},
	}
	for _, h := range bad {
		if err := h.Validate(); err == nil {
			t.Errorf("Validate accepted %+v", h)
		}
	}
	if err := (Host{Alias: "ok", HostName: "h.example.edu", User: "u", Port: 22, ProxyJump: "a,user@b:22", IdentityFile: "/my keys/id"}).Validate(); err != nil {
		t.Errorf("valid host rejected: %v", err)
	}
}

func TestWriteHostsRefusesInvalid(t *testing.T) {
	p := tempPaths(t)
	err := WriteHosts(p, []Host{{Alias: "good", HostName: "h"}, {Alias: "bad", HostName: "h\n ProxyCommand evil"}}, "linux")
	if err == nil {
		t.Fatal("WriteHosts accepted an injected HostName")
	}
	if _, serr := os.Stat(p.LabmillConfig()); serr == nil {
		t.Error("file written despite invalid host")
	}
}

func TestRenderQuotesIdentityFileWithSpace(t *testing.T) {
	out := string(Render([]Host{{Alias: "a", IdentityFile: "/my keys/id"}}, "linux"))
	if !strings.Contains(out, `IdentityFile "/my keys/id"`+"\n") {
		t.Errorf("not quoted:\n%s", out)
	}
	h, _, err := func() (Host, bool, error) {
		p := tempPaths(t)
		writeFile(t, p.LabmillConfig(), out)
		return Lookup(p, "a")
	}()
	if err != nil || h.IdentityFile != "/my keys/id" {
		t.Errorf("lookup identity %q %v", h.IdentityFile, err)
	}
}

func TestUnsupportedMatchDoesNotBreakLookup(t *testing.T) {
	for _, m := range []string{`Match exec "true"`, "Match originalhost x", "Match user bob", "Match canonical", `Match host a exec "true"`} {
		p := tempPaths(t)
		writeFile(t, p.UserConfig(), m+"\n    User evil\n    ProxyJump evil\n\nHost a\n    HostName a.example.edu\n")
		h, found, err := Lookup(p, "a")
		if err != nil || !found || h.HostName != "a.example.edu" || h.User != "" || h.ProxyJump != "" {
			t.Errorf("%q: %+v %v %v", m, h, found, err)
		}
		if al, ok, err := FindAliasForHostName(p, "a.example.edu"); err != nil || !ok || al != "a" {
			t.Errorf("%q: find %q %v %v", m, al, ok, err)
		}
		if al, err := UniqueAlias(p, "a"); err != nil || al != "a-2" {
			t.Errorf("%q: unique %q %v", m, al, err)
		}
	}
}

func TestMatchValuesOnlyWhenAllowed(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Match host * exec \"! on-vpn\"\n    ProxyJump bastion\n\nMatch host a,b\n    User matched\n\nMatch all\n    Port 2200\n\nHost a\n    HostName a.example.edu\n")
	h, found, err := Lookup(p, "a")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if h.ProxyJump != "" {
		t.Errorf("ProxyJump from conditional Match applied: %q", h.ProxyJump)
	}
	if h.User != "matched" || h.Port != 2200 {
		t.Errorf("allowed Match values lost: %+v", h)
	}
	if o, _, _ := Lookup(p, "c"); o.User != "" || o.Port != 2200 {
		t.Errorf("c: %+v", o)
	}
	// Match blocks do not define aliases.
	if al, err := UniqueAlias(p, "b"); err != nil || al != "b" {
		t.Errorf("unique b: %q %v", al, err)
	}
}

func TestIncludeResolvedInsideSSHDir(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Include config.d/*\nInclude ~/.ssh/extra\n\nHost mine\n    HostName mine.example.edu\n")
	writeFile(t, filepath.Join(p.SSHDir, "config.d", "hpc"), "Host hpcone\n    HostName shell.example.edu\n    User wu\n")
	writeFile(t, p.LabmillConfig(), "Host y\n    HostName y.example.edu\n")
	writeFile(t, filepath.Join(p.SSHDir, "extra"), "Match exec \"x\"\n  User no\nHost viaextra\n    HostName e.example.edu\n")
	if a, err := UniqueAlias(p, "hpcone"); err != nil || a != "hpcone-2" {
		t.Errorf("unique: %q %v", a, err)
	}
	if a, ok, err := FindAliasForHostName(p, "shell.example.edu"); err != nil || !ok || a != "hpcone" {
		t.Errorf("find: %q %v %v", a, ok, err)
	}
	if a, ok, _ := FindAliasForHostName(p, "e.example.edu"); !ok || a != "viaextra" {
		t.Errorf("find extra: %q %v", a, ok)
	}
	if h, found, err := Lookup(p, "hpcone"); err != nil || !found || h.User != "wu" {
		t.Errorf("lookup: %+v %v %v", h, found, err)
	}
}

func TestIncludeDepthLimitAndLoops(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Include loop\n")
	writeFile(t, filepath.Join(p.SSHDir, "loop"), "Include loop\nHost l\n    HostName l\n")
	if _, err := UniqueAlias(p, "l"); err != nil {
		t.Fatal(err)
	}
}

func TestIsIncludeLineVariants(t *testing.T) {
	p := Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	yes := []string{
		"Include config.d/labmill", "include=config.d/labmill", "Include = config.d/labmill",
		`Include "config.d/labmill"`, "Include config.d/labmill # managed", "Include ~/.ssh/config.d/labmill",
		"Include " + p.LabmillConfig(),
	}
	for _, l := range yes {
		if !p.isIncludeLine(l) {
			t.Errorf("not recognized: %q", l)
		}
	}
	for _, l := range []string{"Include config.d/other", "Include config.d/labmill extra", "Host config.d/labmill", "# Include config.d/labmill"} {
		if p.isIncludeLine(l) {
			t.Errorf("recognized: %q", l)
		}
	}
	for _, l := range yes {
		q := tempPaths(t)
		writeFile(t, q.UserConfig(), strings.ReplaceAll(l, p.LabmillConfig(), q.LabmillConfig())+"\nHost x\n")
		if changed, err := EnsureInclude(q, time.Now()); err != nil || changed {
			t.Errorf("%q: duplicated include (changed=%v, %v)", l, changed, err)
		}
	}
}

func TestEnsureIncludeBOMAndCRLF(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "\xef\xbb\xbfHost a\r\n    User u\r\n")
	if changed, err := EnsureInclude(p, time.Now()); err != nil || !changed {
		t.Fatal(changed, err)
	}
	got := read(t, p.UserConfig())
	want := "\xef\xbb\xbfInclude config.d/labmill\r\n\r\nHost a\r\n    User u\r\n"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if changed, _ := EnsureInclude(p, time.Now()); changed {
		t.Error("second call changed file")
	}
}

func TestEnsureIncludeDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	p := tempPaths(t)
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(p.SSHDir, "nowhere"), p.UserConfig()); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureInclude(p, time.Now()); err == nil {
		t.Error("expected error for dangling symlink")
	}
	if fi, err := os.Lstat(p.UserConfig()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced")
	}
}

func TestValidateRejectsLeadingDash(t *testing.T) {
	if err := ValidAlias("-oProxyCommand=x"); err == nil {
		t.Error("alias with leading '-' accepted")
	}
	for _, h := range []Host{
		{Alias: "a", HostName: "-oProxyCommand=evil"},
		{Alias: "a", User: "-l"},
		{Alias: "a", ProxyJump: "-J"},
	} {
		if err := h.Validate(); err == nil {
			t.Errorf("%+v accepted", h)
		}
	}
}

func TestMatchExtraCriteriaNeutralized(t *testing.T) {
	for _, m := range []string{
		`Match host a !exec "on-vpn"`, `Match host a !tagged t`, "Match host a version 9",
		`Match host "x a"`, "Match host a b", "Match Host a EXEC x",
	} {
		p := tempPaths(t)
		writeFile(t, p.UserConfig(), m+"\n    ProxyJump evil\n\nHost a\n    HostName a.example.edu\n")
		for _, alias := range []string{"a", "x", "b"} {
			if h, _, err := Lookup(p, alias); err != nil || h.ProxyJump != "" {
				t.Errorf("%q alias %s: ProxyJump %q %v", m, alias, h.ProxyJump, err)
			}
		}
	}
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "MATCH HOST a,b\n    User ok\n")
	if h, _, _ := Lookup(p, "b"); h.User != "ok" {
		t.Errorf("plain Match host lost: %+v", h)
	}
}

func TestIncludeKeepsSourceOrder(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Include inc\n\nHost *\n    User global\n")
	writeFile(t, filepath.Join(p.SSHDir, "inc"), "Host w\n    User wu\n")
	if h, _, err := Lookup(p, "w"); err != nil || h.User != "wu" {
		t.Errorf("w: %+v %v", h, err)
	}
	if h, _, _ := Lookup(p, "other"); h.User != "global" {
		t.Errorf("other: %+v", h)
	}
	// An Include after Host *: the including file's earlier value wins.
	writeFile(t, p.UserConfig(), "Host *\n    User global\nInclude inc\n")
	if h, _, _ := Lookup(p, "w"); h.User != "global" {
		t.Errorf("late include: %+v", h)
	}
}

func TestIncludeInsideHostIsScoped(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host a\n    HostName a.example.edu\n    Include extra\n    Port 2222\n")
	writeFile(t, filepath.Join(p.SSHDir, "extra"), "User fromextra\n")
	if h, _, _ := Lookup(p, "b"); h.User == "fromextra" || h.Port == 2222 {
		t.Errorf("b leaked: %+v", h)
	}
	h, found, err := Lookup(p, "a")
	if err != nil || !found || h.User != "fromextra" || h.Port != 2222 || h.HostName != "a.example.edu" {
		t.Errorf("a: %+v %v %v", h, found, err)
	}
	if al, err := UniqueAlias(p, "a"); err != nil || al != "a-2" {
		t.Errorf("unique: %q %v", al, err)
	}
}

func TestIncludeSkipsDotfiles(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Include config.d/*\n")
	writeFile(t, filepath.Join(p.SSHDir, "config.d", "hpc"), "Host w\n  User wu\n")
	writeFile(t, filepath.Join(p.SSHDir, "config.d", ".hpc.swp"), "Host w\n  User dotfile\nHost evil \"unterminated\n")
	h, found, err := Lookup(p, "w")
	if err != nil || !found || h.User != "wu" {
		t.Errorf("w: %+v %v %v", h, found, err)
	}
}

func TestHostTabSeparatedPatterns(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host\tb\ta # note\n    HostName a.example.edu\n")
	for _, al := range []string{"a", "b"} {
		if h, found, err := Lookup(p, al); err != nil || !found || h.HostName != "a.example.edu" {
			t.Errorf("%s: %+v %v %v", al, h, found, err)
		}
	}
}
