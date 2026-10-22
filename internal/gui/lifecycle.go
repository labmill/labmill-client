package gui

import (
	"sync"

	"labmill-client/internal/textsafe"
)

// EvNotice is an app notice for the chat (NoticeEvent): a system line that
// isn't about a job, e.g. the one-time "still running in the tray" notice.
const EvNotice = "Labmill:notice"

// NoticeEvent is the payload of EvNotice.
type NoticeEvent struct {
	Text string `json:"text"`
}

// AppNotice shows text as a system line in the chat (EvNotice, cleaned). It
// never blocks on the page.
func (b *Bridge) AppNotice(text string) {
	b.emitNow(EvNotice, NoticeEvent{Text: textsafe.Clean(text)})
}

// TrayNotice is the one-time notice shown the first time closing the window
// hides it to the tray.
const TrayNotice = "Labmill is still running in the tray to keep your cluster login. Right-click the tray icon and choose Quit to exit."

// Lifecycle decides what closing the window does (owner, M22): while a tray
// icon can bring the window back, a close (X, Alt+F4, Cmd+W) only hides the
// window and the app, its SSH logins and the job monitor keep running. Only
// Quit (the tray menu, or an explicit app-menu Quit) runs the Closer's
// shutdown and ends the app. Without a tray a close quits, as before.
//
// BeforeClose is Wails' OnBeforeClose. It never blocks (Windows calls it on
// the UI thread); the Closer's own final runtime.Quit re-enters it and is let
// through once the shutdown is done.
type Lifecycle struct {
	Closer *Closer
	// Hide hides the window to the tray. first is true only for the first
	// hide ever (Prefs.TrayNoticeShown false): the caller then shows
	// TrayNotice. It runs in its own goroutine, so it may block (e.g. on a
	// native message box shown before the window hides).
	Hide func(first bool)
	// Tray reports whether a tray icon is up that can bring the window back.
	// nil means no tray: every close quits.
	Tray func() bool

	mu       sync.Mutex
	quitting bool
	noticed  bool
}

// SetNoticeShown records that TrayNotice was already shown (saved prefs),
// so no later hide passes first=true.
func (l *Lifecycle) SetNoticeShown() {
	l.mu.Lock()
	l.noticed = true
	l.mu.Unlock()
}

// ResetNotice forgets that TrayNotice was shown (Settings: "Show the
// one-time tips again"), so the next hide passes first=true again.
func (l *Lifecycle) ResetNotice() {
	l.mu.Lock()
	l.noticed = false
	l.mu.Unlock()
}

// BeforeClose is for Wails' OnBeforeClose: true keeps the window (hidden or
// shutting down), false lets the app end. It returns at once.
func (l *Lifecycle) BeforeClose() bool {
	l.mu.Lock()
	if l.quitting || l.Tray == nil || !l.Tray() {
		// Once a shutdown starts it is never turned back into a hide, even
		// if a tray appears meanwhile (the Closer's Quit re-enters here).
		l.quitting = true
		l.mu.Unlock()
		return l.Closer.BeforeClose()
	}
	first := !l.noticed
	l.noticed = true
	l.mu.Unlock()
	go l.Hide(first)
	return true
}

// Quit really quits: it starts the Closer's sequence (hide, shutdown, quit)
// and returns at once. Safe to call more than once and together with
// BeforeClose.
func (l *Lifecycle) Quit() {
	l.mu.Lock()
	l.quitting = true
	l.mu.Unlock()
	l.Closer.BeforeClose()
}

// Quitting reports whether a quit was requested (or a close without a tray
// started the shutdown).
func (l *Lifecycle) Quitting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.quitting
}

// WakeOnAttention wraps emit so that an event that needs the user (an
// approval, a login question or a choice dialog) shows the window first when
// it is hidden in the tray; nobody would see it otherwise. Job notices and
// other events never wake it (the tray's status line covers them). show runs
// in its own goroutine, so emit never blocks on it.
func WakeOnAttention(emit Emitter, hidden func() bool, show func()) Emitter {
	return func(name string, data any) {
		switch name {
		case EvApproval, EvPrompt, EvChoice:
			if hidden() {
				go show()
			}
		}
		emit(name, data)
	}
}
