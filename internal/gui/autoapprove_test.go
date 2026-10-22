package gui

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/settings"
	"labmill-client/internal/sshx"
	"labmill-client/internal/sshx/sshtest"
)

// answering answers every login question and shows notices on a Bridge.
type answering struct{ b *Bridge }

func (a answering) Ask(_ context.Context, q sshx.Question) (string, error) {
	if q.Kind == sshx.AskPassword {
		return "pw", nil
	}
	return "", nil
}
func (a answering) Notify(msg string) { a.b.Notify(msg) }

// notifyTexts lists the text of every notify event so far.
func (r *recorder) notifyTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		if e.name == EvNotify {
			out = append(out, e.data.(NotifyEvent).Text)
		}
	}
	return out
}

// M43.1: a real login through the Bridge shows the push notice, then ends
// with an empty notify event, so the page's status line is cleared.
func TestLoginNoticeIsClearedInTheEventStream(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Duo: sshtest.DuoApprove, PushDelay: 30 * time.Millisecond})
	rec := newRecorder()
	b := NewBridge(rec.emit)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sshx.Login(context.Background(), conn, srv.Addr, answering{b}, sshx.LoginOptions{User: "alice", KnownHosts: kh})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	got := rec.notifyTexts()
	if len(got) < 2 || got[0] == "" || got[len(got)-1] != "" {
		t.Fatalf("notify events = %q, want a push notice, then an empty one", got)
	}
}

// M43.2: an automatic approval is a chat line "Auto-approved: <summary>"
// (cleaned), not a dialog.
func TestAutoApprovedEventIsAChatLine(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	b.Event(agent.Event{Kind: agent.EventAutoApproved, Tool: "upload_data", Text: "Upload 2 files\x1b[31m to x"})
	n := rec.waitFor(t, EvNotice).(NoticeEvent)
	if n.Text != "Auto-approved: Upload 2 files to x" {
		t.Errorf("notice = %q", n.Text)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if e.name == EvApproval {
			t.Error("an approval dialog was emitted")
		}
	}
}

// The prefs key the page and the agent share survives a SavePrefs round trip.
func TestPrefsApproveAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if err := SavePrefs(path, Prefs{ApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPrefs(path)
	if err != nil || !got.ApproveAll || !settings.AutoApproveAll(path) {
		t.Errorf("prefs = %+v, %v, read=%v", got, err, settings.AutoApproveAll(path))
	}
}
