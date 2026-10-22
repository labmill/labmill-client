package sshconfig

import (
	"os"
	"strings"
	"testing"
)

func TestAddLabmillIdentityFile(t *testing.T) {
	p := editPaths(t, labmillFile, userFile)
	changed, err := AddLabmillIdentityFile(p, "hpclogin", "~/.ssh/id_ed25519_labmill_hpclogin")
	if err != nil || !changed {
		t.Fatalf("AddLabmillIdentityFile = %v, %v", changed, err)
	}
	data, _ := os.ReadFile(p.LabmillConfig())
	got := string(data)
	want := "    ControlMaster auto\n    IdentityFile ~/.ssh/id_ed25519_labmill_hpclogin\n\n# the transfer host\n"
	if !strings.Contains(got, want) {
		t.Errorf("IdentityFile line misplaced:\n%s", got)
	}
	// The other block is untouched, and a second call is a no-op.
	if !strings.Contains(got, "Port 2222\n    IdentityFile ~/.ssh/id_ed25519\n") {
		t.Errorf("other block changed:\n%s", got)
	}
	if changed, err := AddLabmillIdentityFile(p, "hpclogin", "~/.ssh/id_ed25519_labmill_hpclogin"); err != nil || changed {
		t.Errorf("second call = %v, %v; want no change", changed, err)
	}
	h, _, err := Lookup(p, "hpclogin")
	if err != nil || len(h.IdentityFiles) != 1 || h.IdentityFiles[0] != "~/.ssh/id_ed25519_labmill_hpclogin" {
		t.Errorf("Lookup identity files = %v, %v", h.IdentityFiles, err)
	}
}

func TestAddLabmillIdentityFileRefusals(t *testing.T) {
	p := editPaths(t, labmillFile, userFile)
	if _, err := AddLabmillIdentityFile(p, "mybox", "~/.ssh/k"); err == nil {
		t.Error("an alias outside Labmill's file must be refused")
	}
	for _, bad := range []string{"", "a\nb", `a"b`} {
		if _, err := AddLabmillIdentityFile(p, "hpclogin", bad); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	// A path with a space is quoted and still read back.
	if _, err := AddLabmillIdentityFile(p, "files", `C:\Users\Jo Doe\.ssh\id_x`); err != nil {
		t.Fatal(err)
	}
	h, _, _ := Lookup(p, "files")
	if len(h.IdentityFiles) != 2 || h.IdentityFiles[1] != `C:\Users\Jo Doe\.ssh\id_x` {
		t.Errorf("identity files = %v", h.IdentityFiles)
	}
}
