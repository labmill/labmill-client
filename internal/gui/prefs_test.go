package gui

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrefsWindowKeepsOtherFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if err := SavePrefs(path, Prefs{ApproveAll: true, RunWithoutAsking: true, TrayNoticeShown: true}); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPrefs(path)
	if err != nil || p.Window != nil {
		t.Fatalf("load: %+v %v", p, err)
	}
	p.Window = &WindowGeom{X: -1200, Y: 30, Width: 1000, Height: 700, Maximised: true}
	if err := SavePrefs(path, p); err != nil {
		t.Fatal(err)
	}
	q, err := LoadPrefs(path)
	if err != nil {
		t.Fatal(err)
	}
	if q.Window == nil || *q.Window != *p.Window || !q.ApproveAll || !q.RunWithoutAsking || !q.TrayNoticeShown {
		t.Errorf("round trip lost data: %+v", q)
	}
	// Without a window the key is omitted.
	if err := SavePrefs(path, Prefs{StartInTray: true}); err != nil {
		t.Fatal(err)
	}
	if q, _ = LoadPrefs(path); q.Window != nil || !q.StartInTray {
		t.Errorf("unexpected: %+v", q)
	}
}

func TestClampJobsWidth(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0, 0}, {-1, 0}, {math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
		{0.01, MinJobsWidth}, {0.15, 0.15}, {0.4, 0.4}, {0.6, 0.6}, {0.9, MaxJobsWidth}, {5, MaxJobsWidth},
	} {
		if got := ClampJobsWidth(c.in); got != c.want {
			t.Errorf("ClampJobsWidth(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPrefsJobsWidthRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	w := WindowGeom{X: 1, Y: 2, Width: 900, Height: 600}
	if err := SavePrefs(path, Prefs{JobsWidth: 0.33, Window: &w}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPrefs(path)
	if err != nil || got.JobsWidth != 0.33 || got.Window == nil || *got.Window != w {
		t.Fatalf("got %+v, %v", got, err)
	}
	// The default (0) is not written at all.
	if err := SavePrefs(path, Prefs{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "jobs_width") {
		t.Errorf("zero JobsWidth was written: %s", b)
	}
}

// Keys that Prefs doesn't know (written by internal/settings) survive a save.
func TestSavePrefsKeepsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gui.json")
	if err := os.WriteFile(p, []byte(`{"notifyJobEnd": false, "futureKey": "x", "startInTray": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SavePrefs(p, Prefs{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, w := range []string{`"notifyJobEnd": false`, `"futureKey": "x"`, `"startInTray": false`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("missing %s in:\n%s", w, b)
		}
	}
}
