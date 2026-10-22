package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/agent"
	"labmill-client/internal/sshx"
)

// advancingClock returns a clock that moves one second per call.
func advancingClock() func() time.Time {
	t := time.Unix(1_700_000_000, 0)
	return func() time.Time { t = t.Add(time.Second); return t }
}

// fakeClock is a clock tests set by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func typeText(t *testing.T, m *model, s string) {
	t.Helper()
	for _, r := range s {
		if r == '\n' {
			step(t, m, kp(tea.KeyEnter))
			continue
		}
		step(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func newClocked(t *testing.T) (*model, *fakeClock) {
	t.Helper()
	m := newDirect(t, 100, 30)
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	m.now = c.now
	return m, c
}

func TestTypedAheadCannotApprove(t *testing.T) {
	// Real clock: everything typed right after the modal opens is dropped.
	m := newDirect(t, 100, 30)
	m.now = time.Now
	req, ch := approvalReq(agent.Proposal{Summary: "Cancel job 42"}, 2)
	step(t, m, req)
	typeText(t, m, "hello\n")
	step(t, m, kp(tea.KeyRight))
	step(t, m, kp(tea.KeyEnter))
	select {
	case d := <-ch:
		t.Fatalf("typed-ahead keys answered the dialog: %+v", d)
	default:
	}
	if m.modal == nil {
		t.Fatal("modal closed")
	}
	// Esc still works inside the grace period.
	step(t, m, kp(tea.KeyEscape))
	if d := <-ch; d.Approved {
		t.Fatalf("Esc approved: %+v", d)
	}
}

func TestLettersDoNotToggleApprovalButtons(t *testing.T) {
	m, c := newClocked(t)
	req, ch := approvalReq(agent.Proposal{Summary: "Cancel job 42"}, 2)
	step(t, m, req)
	c.t = c.t.Add(600 * time.Millisecond)
	typeText(t, m, "hello\n") // Enter on Decline opens the reason prompt
	step(t, m, kp(tea.KeyEnter))
	if d := <-ch; d.Approved {
		t.Fatalf("h/l toggled to Approve: %+v", d)
	}
	// The arrow still toggles.
	m, c = newClocked(t)
	req, ch = approvalReq(agent.Proposal{Summary: "Cancel job 42"}, 2)
	step(t, m, req)
	c.t = c.t.Add(600 * time.Millisecond)
	step(t, m, kp(tea.KeyRight))
	step(t, m, kp(tea.KeyEnter))
	if d := <-ch; !d.Approved {
		t.Fatalf("Right+Enter = %+v", d)
	}
}

func TestGraceBoundary(t *testing.T) {
	m, c := newClocked(t)
	req, ch := approvalReq(agent.Proposal{Summary: "Cancel job 42"}, 2)
	step(t, m, req)
	c.t = c.t.Add(keyGrace - time.Millisecond)
	step(t, m, kp(tea.KeyRight))
	step(t, m, kp(tea.KeyEnter))
	if len(ch) != 0 {
		t.Fatal("answered inside the grace period")
	}
	c.t = c.t.Add(time.Millisecond)
	step(t, m, kp(tea.KeyRight))
	step(t, m, kp(tea.KeyEnter))
	if d := <-ch; !d.Approved {
		t.Fatalf("after grace Right+Enter = %+v", d)
	}
}

func TestQueuedModalGetsFreshGrace(t *testing.T) {
	m, c := newClocked(t)
	r1, ch1 := approvalReq(agent.Proposal{Summary: "first"}, 2)
	r2, ch2 := approvalReq(agent.Proposal{Summary: "second"}, 2)
	step(t, m, r1)
	step(t, m, r2)
	c.t = c.t.Add(time.Second)
	step(t, m, kp(tea.KeyEnter)) // chooses Decline for the first: reason prompt
	step(t, m, kp(tea.KeyEnter)) // no reason: declines the first, opens the second
	<-ch1
	step(t, m, kp(tea.KeyRight))
	step(t, m, kp(tea.KeyEnter))
	if len(ch2) != 0 {
		t.Fatal("second dialog answered by typed-ahead keys")
	}
}

func TestHostKeyNeedsArrowAndEnter(t *testing.T) {
	ask := func(t *testing.T, grace bool, keys func(*testing.T, *model)) (PromptAnswer, bool) {
		m, c := newClocked(t)
		ch := make(chan PromptAnswer, 2)
		step(t, m, PromptRequestMsg{Question: sshx.Question{Kind: sshx.AskHostKey, Host: "login.example.org", Prompt: "Trust key SHA256:abc?"}, Reply: ch})
		if !grace {
			c.t = c.t.Add(time.Second)
		}
		keys(t, m)
		select {
		case a := <-ch:
			return a, true
		default:
			return PromptAnswer{}, false
		}
	}
	// Typed during the grace period: nothing happens.
	if _, got := ask(t, true, func(t *testing.T, m *model) { typeText(t, m, "yes\n") }); got {
		t.Fatal("typed-ahead yes answered the host-key prompt")
	}
	// After the grace period: letters are inert, Enter answers the default No.
	if a, got := ask(t, false, func(t *testing.T, m *model) { typeText(t, m, "yes\n") }); !got || a.Text != "no" {
		t.Fatalf("yes\\n = %+v, %v", a, got)
	}
	for _, k := range []string{"y", "Y", "h", "l"} {
		a, _ := ask(t, false, func(t *testing.T, m *model) { typeText(t, m, k+"\n") })
		if a.Text != "no" {
			t.Fatalf("%q answered %+v", k, a)
		}
	}
	// Arrow then Enter selects yes.
	for _, arrow := range []rune{tea.KeyLeft, tea.KeyRight} {
		a, got := ask(t, false, func(t *testing.T, m *model) {
			step(t, m, kp(arrow))
			step(t, m, kp(tea.KeyEnter))
		})
		if !got || a.Text != "yes" {
			t.Fatalf("arrow %v + Enter = %+v, %v", arrow, a, got)
		}
	}
}

func TestSanitizeDropsInvisibleFormatting(t *testing.T) {
	for _, r := range []rune{0x200e, 0x200f, 0x061c, 0x2028, 0x2029, 0x2060, 0x2061, 0x2062, 0x2063, 0x2064} {
		in := "a" + string(r) + "b"
		if got := sanitize(in); got != "ab" {
			t.Errorf("U+%04X: sanitize = %q", r, got)
		}
	}
	if got := sanitize("naïve 日本 ok\nx"); got != "naïve 日本 ok\nx" {
		t.Errorf("ordinary text changed: %q", got)
	}
}

func TestNonTextEventsRefreshOncePerBatch(t *testing.T) {
	m := newDirect(t, 100, 30)
	m.entries = nil
	var batch bridgeBatch
	for i := 0; i < 20; i++ {
		batch = append(batch, AgentEventMsg{Event: agent.Event{Kind: agent.EventToolCall, Tool: "check_cluster"}})
	}
	m.vp.SetContent("stale")
	m.Update(batch)
	if m.dirty {
		t.Fatal("dirty flag left set")
	}
	if got := len(m.entries); got != 20 {
		t.Fatalf("entries = %d", got)
	}
	if !strings.Contains(m.vp.GetContent(), "check cluster") {
		t.Fatalf("viewport not refreshed: %q", m.vp.GetContent())
	}
	// handleEvent alone only marks the chat dirty.
	m.dirty = false
	m.handleEvent(agent.Event{Kind: agent.EventDeclined})
	if !m.dirty {
		t.Fatal("event did not mark the chat dirty")
	}
}

func TestApprovalHintFollowsFocus(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, _ := approvalReq(agent.Proposal{Summary: "Run", Details: "#!/bin/sh\necho hi", Editable: true}, 2)
	step(t, m, req)
	if v := content(m); !strings.Contains(v, "Tab to buttons · Esc decline") {
		t.Fatalf("text focus hint:\n%s", v)
	}
	step(t, m, kp(tea.KeyTab))
	if v := content(m); !strings.Contains(v, "←/→ choose · Enter confirm") || strings.Contains(v, "Tab to buttons") {
		t.Fatalf("button focus hint:\n%s", v)
	}

	m = newDirect(t, 100, 30)
	req, _ = approvalReq(agent.Proposal{Summary: "Upload", Details: manyLines(200)}, 2)
	step(t, m, req)
	if v := boxOnly(content(m)); !strings.Contains(v, "PgUp/PgDn scroll") {
		t.Fatalf("scrolling hint missing:\n%s", v)
	}
	m = newDirect(t, 100, 30)
	req, _ = approvalReq(agent.Proposal{Summary: "Upload", Details: "one line"}, 2)
	step(t, m, req)
	// (the status bar always lists PgUp/PgDn, so look at the box only)
	if v := boxOnly(content(m)); strings.Contains(v, "PgUp") || !strings.Contains(v, "←/→ choose · Enter confirm · Esc decline") {
		t.Fatalf("short details hint:\n%s", v)
	}
}

// boxOnly drops the status bar (the last line) from a rendered view.
func boxOnly(v string) string {
	v = stripStyles(v)
	if i := strings.LastIndex(v, "\n"); i >= 0 {
		return v[:i]
	}
	return v
}
