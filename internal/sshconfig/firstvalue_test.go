package sshconfig

import (
	"path/filepath"
	"testing"
)

// OpenSSH uses the first HostName it obtains: in
//
//	Host a / HostName x / Include f   (f: HostName y)
//
// `ssh -G a` says hostname x, so a is an alias for x and not for y.
func TestAliasFirstHostNameWinsAcrossInclude(t *testing.T) {
	p := tempPaths(t)
	f := filepath.Join(p.SSHDir, "f")
	writeFile(t, p.UserConfig(), "Host a\n  HostName x.example\n  Include "+f+"\n")
	writeFile(t, f, "HostName y.example\n")
	if h, _, _ := Lookup(p, "a"); h.HostName != "x.example" {
		t.Fatalf("Lookup: %+v", h)
	}
	if al, ok, _ := FindAliasForHostName(p, "y.example"); ok {
		t.Errorf("y.example attributed to %q", al)
	}
	if al, _, ok, _ := AliasSource(p, "y.example"); ok {
		t.Errorf("AliasSource: y.example attributed to %q", al)
	}
	if al, ok, err := FindAliasForHostName(p, "x.example"); err != nil || !ok || al != "a" {
		t.Errorf("x.example = %q %v %v", al, ok, err)
	}

	// An earlier wildcard block's HostName also wins over a later one.
	q := tempPaths(t)
	writeFile(t, q.UserConfig(), "Host *\n  HostName first.example\nHost b\n  HostName second.example\n")
	if al, ok, _ := FindAliasForHostName(q, "second.example"); ok {
		t.Errorf("second.example attributed to %q", al)
	}

	// A literal pattern equal to the name does not count when an earlier
	// block sends it elsewhere.
	w := tempPaths(t)
	writeFile(t, w.UserConfig(), "Host *\n  HostName gw.example\nHost shell.example\n  User u\n")
	if al, ok, _ := FindAliasForHostName(w, "shell.example"); ok {
		t.Errorf("shell.example attributed to %q", al)
	}
}
