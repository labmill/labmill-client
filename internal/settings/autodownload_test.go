package settings

import (
	"path/filepath"
	"strings"
	"testing"
)

// M60.1: "Download results automatically" is off by default, persists in
// gui.json next to the other settings without touching them, and shows in
// the view.
func TestAutoDownloadResultsPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if AutoDownloadResults(path) {
		t.Fatal("on without a file")
	}
	if err := SetAutoApproveAll(path, true); err != nil {
		t.Fatal(err)
	}
	if err := SetAutoDownloadResults(path, true); err != nil {
		t.Fatal(err)
	}
	if !AutoDownloadResults(path) || !AutoApproveAll(path) {
		t.Error("setting lost or the other one overwritten")
	}
	if err := SetAutoDownloadResults(path, false); err != nil {
		t.Fatal(err)
	}
	if AutoDownloadResults(path) || !AutoApproveAll(path) {
		t.Error("turning it off changed the wrong key")
	}
	if err := SetAutoDownloadResults("", true); err == nil {
		t.Error("an empty path was accepted")
	}
	svc := &Service{Paths: Paths{Prefs: path}}
	v := svc.Get(Info{AutoDownload: true})
	if !v.App.AutoDownload || !strings.Contains(v.Text(), "download results automatically: ON") {
		t.Errorf("the view lacks the setting:\n%s", v.Text())
	}
	if t2 := svc.Get(Info{}).Text(); !strings.Contains(t2, "download results automatically: off") || !strings.Contains(t2, "/autodownload on") {
		t.Errorf("the off view:\n%s", t2)
	}
}
