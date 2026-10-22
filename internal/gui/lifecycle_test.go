package gui

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"labmill-client/internal/settings"
	"labmill-client/internal/store"
)

// lifeRig is a Lifecycle with a Closer whose steps are recorded.
type lifeRig struct {
	l                  *Lifecycle
	tray               atomic.Bool
	mu                 sync.Mutex
	hides              []bool // first flag of each tray hide
	closerHides, shuts atomic.Int32
	quits              atomic.Int32
	quit               chan struct{}
	quitOnce           sync.Once
	hidden             chan bool
}

func newLifeRig(tray bool) *lifeRig {
	r := &lifeRig{quit: make(chan struct{}), hidden: make(chan bool, 8)}
	r.tray.Store(tray)
	var l *Lifecycle
	l = &Lifecycle{
		Closer: &Closer{
			Hide:     func() { r.closerHides.Add(1) },
			Shutdown: func() { r.shuts.Add(1) },
			// Wails' Quit re-enters OnBeforeClose synchronously (Windows).
			Quit: func() {
				r.quits.Add(1)
				if !l.BeforeClose() {
					r.quitOnce.Do(func() { close(r.quit) })
				}
			},
		},
		Hide: func(first bool) {
			r.mu.Lock()
			r.hides = append(r.hides, first)
			r.mu.Unlock()
			r.hidden <- first
		},
		Tray: r.tray.Load,
	}
	r.l = l
	return r
}

func (r *lifeRig) waitHidden(t *testing.T) bool {
	t.Helper()
	select {
	case f := <-r.hidden:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("the window was not hidden")
	}
	return false
}

func (r *lifeRig) waitQuit(t *testing.T) {
	t.Helper()
	select {
	case <-r.quit:
	case <-time.After(2 * time.Second):
		t.Fatal("the app never quit")
	}
}

func TestLifecycleCloseHidesWithTray(t *testing.T) {
	r := newLifeRig(true)
	if !r.l.BeforeClose() {
		t.Fatal("close allowed with a tray; want hide")
	}
	if first := r.waitHidden(t); !first {
		t.Error("first hide did not ask for the notice")
	}
	if !r.l.BeforeClose() {
		t.Fatal("second close allowed")
	}
	if first := r.waitHidden(t); first {
		t.Error("second hide asked for the notice again")
	}
	time.Sleep(50 * time.Millisecond)
	if r.shuts.Load() != 0 || r.quits.Load() != 0 || r.l.Quitting() {
		t.Errorf("a close with a tray shut down (shutdown=%d quit=%d)", r.shuts.Load(), r.quits.Load())
	}
}

func TestLifecycleNoticeAlreadyShown(t *testing.T) {
	r := newLifeRig(true)
	r.l.SetNoticeShown()
	r.l.BeforeClose()
	if first := r.waitHidden(t); first {
		t.Error("hide asked for the notice although prefs say it was shown")
	}
}

func TestLifecycleQuitShutsDownAndExits(t *testing.T) {
	r := newLifeRig(true)
	r.l.BeforeClose()
	r.waitHidden(t)
	r.l.Quit()
	r.waitQuit(t)
	if r.closerHides.Load() != 1 || r.shuts.Load() != 1 {
		t.Errorf("closer hide/shutdown = %d/%d, want 1/1", r.closerHides.Load(), r.shuts.Load())
	}
	if !r.l.Quitting() {
		t.Error("Quitting false after Quit")
	}
	// A close arriving after the shutdown finished is let through.
	if r.l.BeforeClose() {
		t.Error("close after shutdown prevented")
	}
	// Quit twice starts nothing new.
	r.l.Quit()
	time.Sleep(50 * time.Millisecond)
	if r.shuts.Load() != 1 {
		t.Errorf("shutdown ran %d times", r.shuts.Load())
	}
}

func TestLifecycleNoTrayCloseQuits(t *testing.T) {
	r := newLifeRig(false)
	if !r.l.BeforeClose() {
		t.Fatal("first close must be prevented while shutting down")
	}
	r.waitQuit(t)
	r.mu.Lock()
	n := len(r.hides)
	r.mu.Unlock()
	if n != 0 || r.shuts.Load() != 1 {
		t.Errorf("tray hides=%d shutdown=%d; want 0/1", n, r.shuts.Load())
	}
}

// A tray that comes up while a no-tray shutdown runs must not turn the
// shutdown's own Quit into a hide.
func TestLifecycleShutdownNotTurnedIntoHide(t *testing.T) {
	r := newLifeRig(false)
	release := make(chan struct{})
	r.l.Closer.Shutdown = func() { r.shuts.Add(1); <-release }
	r.l.BeforeClose()
	r.tray.Store(true)
	close(release)
	r.waitQuit(t)
	r.mu.Lock()
	n := len(r.hides)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("the shutdown's quit became a hide (%d hides)", n)
	}
}

func TestLifecycleNilTrayQuits(t *testing.T) {
	r := newLifeRig(false)
	r.l.Tray = nil
	r.l.BeforeClose()
	r.waitQuit(t)
}

func TestPrefsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "gui.json")
	p, err := LoadPrefs(path)
	if err != nil || p != (Prefs{}) {
		t.Fatalf("missing file: %+v, %v", p, err)
	}
	want := Prefs{TrayNoticeShown: true, StartInTray: true}
	if err := SavePrefs(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPrefs(path)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 && os.PathSeparator == '/' {
		t.Errorf("mode = %o, want 600", m)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrefs(path); err == nil {
		t.Error("bad JSON loaded without error")
	}
}

func TestTrayStatus(t *testing.T) {
	for _, c := range []struct {
		running, waiting int
		aliases          []string
		want             string
	}{
		{0, 0, nil, "no jobs running · not logged in"},
		{1, 0, []string{"hpcone"}, "1 job running · logged in to hpcone"},
		{2, 0, []string{"hpcone"}, "2 jobs running · logged in to hpcone"},
		{1, 3, nil, "1 job running, 3 waiting · not logged in"},
		{0, 1, []string{"a", "b"}, "no jobs running, 1 waiting · logged in to a, b"},
		{0, 0, []string{"a", "b", "c", "d"}, "no jobs running · logged in to a, b +2"},
	} {
		if got := TrayStatus(c.running, c.waiting, c.aliases); got != c.want {
			t.Errorf("TrayStatus(%d, %d, %v) = %q, want %q", c.running, c.waiting, c.aliases, got, c.want)
		}
	}
}

func TestTrayTooltip(t *testing.T) {
	if got := TrayTooltip(TrayStarting); got != "Labmill is starting…" {
		t.Errorf("starting tooltip = %q", got)
	}
	if got := TrayTooltip("no jobs running · not logged in"); got != "Labmill: no jobs running · not logged in" {
		t.Errorf("ready tooltip = %q", got)
	}
}

func TestCountJobs(t *testing.T) {
	jobs := []store.Job{
		{State: store.StateRunning}, {State: store.StateRunning}, {State: store.StatePending},
		{State: store.StateSubmitted}, {State: store.StateCompleted}, {State: store.StateDraft},
	}
	if r, w := CountJobs(jobs); r != 2 || w != 2 {
		t.Errorf("CountJobs = %d, %d; want 2, 2", r, w)
	}
}

func TestAppNoticeCleansText(t *testing.T) {
	var got []any
	var mu sync.Mutex
	b := NewBridge(func(name string, data any) {
		if name == EvNotice {
			mu.Lock()
			got = append(got, data)
			mu.Unlock()
		}
	})
	b.AppNotice("hi\x1b[31m there")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("events = %v", got)
	}
	if ev := got[0].(NoticeEvent); ev.Text == "hi\x1b[31m there" {
		t.Errorf("notice not cleaned: %q", ev.Text)
	}
}

func TestWakeOnAttention(t *testing.T) {
	var hidden atomic.Bool
	shown := make(chan struct{}, 8)
	var emitted atomic.Int32
	emit := WakeOnAttention(func(string, any) { emitted.Add(1) }, hidden.Load, func() { shown <- struct{}{} })
	wake := func() bool {
		select {
		case <-shown:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}
	for _, ev := range []string{EvApproval, EvPrompt, EvChoice} {
		hidden.Store(false)
		emit(ev, nil)
		if wake() {
			t.Errorf("%s woke a visible window", ev)
		}
		hidden.Store(true)
		emit(ev, nil)
		if !wake() {
			t.Errorf("%s did not show the hidden window", ev)
		}
	}
	for _, ev := range []string{EvJob, EvNotify, EvText, EvNotice, EvProgress} {
		emit(ev, nil)
		if wake() {
			t.Errorf("%s woke the hidden window", ev)
		}
	}
	if emitted.Load() != 11 {
		t.Errorf("emitted %d events, want 11", emitted.Load())
	}
}

// The setting "Run commands on the cluster without asking" round-trips
// through gui.json and is read by settings.AutoRunRemote (the terminal's and
// the agent's reader), which has its own copy of the key.
func TestPrefsRunWithoutAsking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if err := SavePrefs(path, Prefs{StartInTray: true, RunWithoutAsking: true}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPrefs(path)
	if err != nil || !got.RunWithoutAsking || !got.StartInTray {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if !settings.AutoRunRemote(path) {
		t.Error("settings.AutoRunRemote doesn't read what SavePrefs wrote")
	}
	if err := SavePrefs(path, Prefs{}); err != nil {
		t.Fatal(err)
	}
	if settings.AutoRunRemote(path) {
		t.Error("off was read as on")
	}
}
