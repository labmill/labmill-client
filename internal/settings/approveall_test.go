package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M43.2: "Approve everything automatically" is off by default, persists in
// gui.json without touching the other keys, and a broken file means off.
func TestAutoApproveAllPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "gui.json")
	if AutoApproveAll(path) {
		t.Error("a missing file turned it on")
	}
	if err := SetAutoApproveAll(path, true); err != nil {
		t.Fatal(err)
	}
	if !AutoApproveAll(path) {
		t.Error("saved on, read off")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi, err)
	}
	// Other keys survive, and the old setting is independent.
	if err := os.WriteFile(path, []byte(`{"runWithoutAsking": true, "startInTray": true, "approveAll": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetAutoApproveAll(path, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !AutoApproveAll(path) || !AutoRunRemote(path) || !strings.Contains(string(b), "startInTray") {
		t.Errorf("file after save: %s", b)
	}
	if err := SetAutoApproveAll(path, false); err != nil {
		t.Fatal(err)
	}
	if AutoApproveAll(path) || !AutoRunRemote(path) {
		t.Error("turning approve-all off changed the other setting, or did not turn off")
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if AutoApproveAll(path) {
		t.Error("a broken file turned it on")
	}
	if err := SetAutoApproveAll(path, true); err == nil {
		t.Error("a broken file was overwritten")
	}
	if err := SetAutoApproveAll("", true); err == nil {
		t.Error("an empty path was accepted")
	}
	v := (&Service{Paths: Paths{Prefs: path}}).Get(Info{ApproveAll: true})
	if !v.App.ApproveAll || !strings.Contains(v.Text(), "approve everything automatically: ON") {
		t.Errorf("the view lacks the setting:\n%s", v.Text())
	}
}
