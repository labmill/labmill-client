package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifyAndMailSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gui.json")
	if !NotifyJobEnd(p) || EmailOnJobEnd(p) || !LastSeen(p).IsZero() {
		t.Fatal("defaults: notify on, email off, last seen unknown")
	}
	if err := SetNotifyJobEnd(p, false); err != nil {
		t.Fatal(err)
	}
	if err := SetEmailOnJobEnd(p, true); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	seen := filepath.Join(filepath.Dir(p), "lastseen")
	if err := SetLastSeen(seen, when); err != nil {
		t.Fatal(err)
	}
	if NotifyJobEnd(p) || !EmailOnJobEnd(p) || !LastSeen(seen).Equal(when) {
		t.Errorf("read back: %v %v %v", NotifyJobEnd(p), EmailOnJobEnd(p), LastSeen(seen))
	}
	if err := SetAutoApproveAll(p, true); err != nil || !EmailOnJobEnd(p) || !AutoApproveAll(p) {
		t.Errorf("other keys must survive: %v", err)
	}
	if err := os.WriteFile(p, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !NotifyJobEnd(p) {
		t.Error("an unreadable file keeps the default (on)")
	}
}
