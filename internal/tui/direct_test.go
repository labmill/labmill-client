package tui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"labmill-client/internal/agent"
	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
)

// newDirect builds a model that tests drive synchronously (no program, no
// bridge) at the given window size.
func newDirect(t *testing.T, w, h int) *model {
	t.Helper()
	m := newModel(Options{Status: "hpcone (Slurm)"})
	m.now = advancingClock() // keys are never "typed ahead" unless a test says so
	step(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// step sends msg and runs the returned commands (and the messages they
// produce) synchronously. Commands that block (timers, blinking) are
// abandoned after a short wait.
func step(t *testing.T, m *model, msg tea.Msg) {
	t.Helper()
	_, cmd := m.Update(msg)
	runCmd(t, m, cmd, 0)
}

func runCmd(t *testing.T, m *model, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil || depth > 8 {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		if msg == nil {
			return
		}
		rv := reflect.ValueOf(msg)
		if rv.Kind() == reflect.Slice && rv.Type().Elem() == reflect.TypeOf(cmd) { // Batch or Sequence
			for i := 0; i < rv.Len(); i++ {
				runCmd(t, m, rv.Index(i).Interface().(tea.Cmd), depth+1)
			}
			return
		}
		_, next := m.Update(msg)
		runCmd(t, m, next, depth+1)
	case <-time.After(30 * time.Millisecond):
	}
}

func kp(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func content(m *model) string { return m.View().Content }

func manyLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n-1; i++ {
		fmt.Fprintf(&b, "data/file-%03d.csv\n", i)
	}
	b.WriteString("Total: 299 files, 4.2 GB")
	return b.String()
}

func approvalReq(p agent.Proposal, cap int) (ApprovalRequestMsg, chan agent.Decision) {
	ch := make(chan agent.Decision, cap)
	return ApprovalRequestMsg{Proposal: p, Reply: ch}, ch
}

func TestApprovalLongDetailsScrollsAndKeepsButtons(t *testing.T) {
	m := newDirect(t, 60, 15)
	req, ch := approvalReq(agent.Proposal{Tool: "upload_data", Summary: "Upload 299 files (4.2 GB) to hpcone:labmill/seg", Details: manyLines(300)}, 2)
	step(t, m, req)
	v := content(m)
	for _, want := range []string{"Upload 299 files", "Approve", "Decline", "Esc decline", "PgUp/PgDn", "lines 1-"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Total: 299") {
		t.Fatal("last line visible before scrolling")
	}
	if h := strings.Count(v, "\n") + 1; h > 15 {
		t.Fatalf("view is %d rows tall", h)
	}
	step(t, m, kp(tea.KeyEnd))
	v = content(m)
	if !strings.Contains(v, "Total: 299 files, 4.2 GB") || !strings.Contains(v, "of 300") {
		t.Fatalf("end not shown:\n%s", v)
	}
	if !strings.Contains(v, "Approve") || !strings.Contains(v, "Esc decline") {
		t.Fatalf("buttons/hint lost after scrolling:\n%s", v)
	}
	// PgDn/PgUp move the window.
	step(t, m, kp(tea.KeyPgUp))
	if strings.Contains(content(m), "Total: 299") {
		t.Error("PgUp did not scroll")
	}
	select {
	case d := <-ch:
		t.Fatalf("unexpected decision %+v", d)
	default:
	}
}

func TestApprovalDefaultsToDecline(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, ch := approvalReq(agent.Proposal{Summary: "Cancel job 42"}, 2)
	step(t, m, req)
	step(t, m, kp(tea.KeyEnter))
	select {
	case d := <-ch:
		t.Fatalf("Enter on Decline replied before the reason prompt: %+v", d)
	default:
	}
	step(t, m, kp(tea.KeyEnter)) // an empty reason
	d := <-ch
	if d.Approved {
		t.Fatalf("Enter approved by default: %+v", d)
	}
	if len(ch) != 0 || m.modal != nil {
		t.Fatal("extra reply or modal still open")
	}
}

func TestApprovalTooSmallOnlyEscWorks(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, ch := approvalReq(agent.Proposal{Summary: "Upload files", Details: manyLines(50)}, 2)
	step(t, m, req)
	step(t, m, tea.WindowSizeMsg{Width: 40, Height: 10})
	if !strings.Contains(content(m), "enlarge the window to review") && !strings.Contains(strings.ToLower(content(m)), "enlarge the window to review") {
		t.Fatalf("no enlarge note:\n%s", content(m))
	}
	step(t, m, kp(tea.KeyLeft))
	step(t, m, kp(tea.KeyEnter))
	select {
	case d := <-ch:
		t.Fatalf("Enter at 40x10 replied: %+v", d)
	default:
	}
	step(t, m, kp(tea.KeyEscape))
	if d := <-ch; d.Approved {
		t.Fatalf("Esc approved: %+v", d)
	}
}

func TestApprovalLongSummaryStillFits(t *testing.T) {
	m := newDirect(t, 60, 15)
	long := strings.Repeat("Upload everything to somewhere unexpected ", 12) + "FINAL"
	req, _ := approvalReq(agent.Proposal{Summary: long, Details: "x"}, 2)
	step(t, m, req)
	v := content(m)
	if !strings.Contains(v, "Approve") || !strings.Contains(v, "Esc decline") {
		t.Fatalf("buttons/hint lost:\n%s", v)
	}
	step(t, m, kp(tea.KeyEnd))
	if !strings.Contains(content(m), "FINAL") {
		t.Fatalf("full summary not reachable:\n%s", content(m))
	}
}

func TestEditableProposal(t *testing.T) {
	script := "#!/bin/bash\n#SBATCH -J train\nsrun python train.py\n"
	run := func(t *testing.T, details string, edit func(m *model)) (agent.Decision, string) {
		m := newDirect(t, 100, 30)
		req, ch := approvalReq(agent.Proposal{Summary: "Submit sbatch script", Details: details, Editable: true}, 2)
		step(t, m, req)
		if edit != nil {
			edit(m)
		}
		step(t, m, kp(tea.KeyTab))  // to the buttons
		step(t, m, kp(tea.KeyLeft)) // Decline -> Approve
		v := content(m)
		step(t, m, kp(tea.KeyEnter))
		select {
		case d := <-ch:
			return d, v
		default:
			t.Fatal("no decision")
		}
		return agent.Decision{}, ""
	}
	t.Run("unchanged", func(t *testing.T) {
		d, _ := run(t, script, nil)
		if !d.Approved || d.Edited != "" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("changed", func(t *testing.T) {
		d, _ := run(t, script, func(m *model) {
			step(t, m, tea.KeyPressMsg{Code: 'X', Text: "X"})
		})
		if !d.Approved || d.Edited == "" || !strings.Contains(d.Edited, "X") || !strings.Contains(d.Edited, "srun python train.py") {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("tab warning", func(t *testing.T) {
		d, v := run(t, "#!/bin/sh\n\techo hi\r\n", nil)
		if !strings.Contains(v, "warning") {
			t.Fatalf("no warning:\n%s", v)
		}
		if d.Edited != "" {
			t.Fatalf("unchanged script reported as edited: %q", d.Edited)
		}
	})
	t.Run("no warning when clean", func(t *testing.T) {
		_, v := run(t, script, nil)
		if strings.Contains(v, "warning") {
			t.Fatalf("unexpected warning:\n%s", v)
		}
	})
	t.Run("more than 99 lines", func(t *testing.T) {
		big := manyLines(400) + "\n"
		m := newDirect(t, 100, 30)
		req, ch := approvalReq(agent.Proposal{Summary: "Submit", Details: big, Editable: true}, 2)
		step(t, m, req)
		step(t, m, kp(tea.KeyTab))
		step(t, m, kp(tea.KeyLeft))
		step(t, m, kp(tea.KeyEnter))
		if d := <-ch; !d.Approved || d.Edited != "" {
			t.Fatalf("%+v", d)
		}
		if got := m.modal; got != nil {
			t.Fatal("modal still open")
		}
		// The text area itself holds all 400 lines.
		ap := newApprovalState(agent.Proposal{Details: big, Editable: true}, true)
		if ap.ta.LineCount() < 400 {
			t.Fatalf("text area kept %d lines", ap.ta.LineCount())
		}
	})
}

func TestControlCharsStripped(t *testing.T) {
	m := newDirect(t, 100, 30)
	sum := "Upload\x1b[8mhidden \x1b]8;;http://evil.example\x1b\\click\x1b]8;;\x1b\\ now\x07\x00\x9b31m"
	req, _ := approvalReq(agent.Proposal{Summary: sum, Details: "a\tb\x1b[2Jc\x7f"}, 2)
	step(t, m, req)
	v := content(m)
	if strings.ContainsRune(v, 0x1b) {
		// Only styling the TUI itself adds is allowed; check the text content.
		stripped := stripStyles(v)
		if strings.ContainsRune(stripped, 0x1b) {
			t.Fatalf("ESC in output: %q", stripped)
		}
	}
	for _, bad := range []string{"evil.example", "[8m", "[2J", "\x07", "\x00"} {
		if strings.Contains(v, bad) {
			t.Errorf("%q survived in view", bad)
		}
	}
	if !strings.Contains(v, "Uploadhidden click now") {
		t.Errorf("visible text mangled:\n%s", v)
	}
	if got := sanitize("a\tb\r\nc\x1b[31md\x1b(Be"); got != "a    b\ncde" {
		t.Errorf("sanitize = %q", got)
	}
	if got := sanitize("a\u009b31mb\u009d0;t\u009cc"); got != "abc" {
		t.Errorf("C1 sequences: %q", got)
	}
	if got := sanitize("x\x1b]0;title"); got != "x" {
		t.Errorf("unterminated OSC: %q", got)
	}
}

// stripStyles removes SGR sequences the TUI renders itself.
func stripStyles(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestAssistantTextSanitized(t *testing.T) {
	m := newDirect(t, 100, 30)
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventText, Text: "hello \x1b]8;;http://evil"}})
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventText, Text: ".example\x1b\\world"}})
	step(t, m, flushMsg{})
	v := stripStyles(content(m))
	if strings.Contains(v, "evil") || strings.ContainsRune(v, 0x1b) || !strings.Contains(v, "hello world") {
		t.Fatalf("assistant text not sanitized:\n%s", v)
	}
}

func TestQueuedApprovalsEachReplyOnce(t *testing.T) {
	m := newDirect(t, 100, 30)
	r1, c1 := approvalReq(agent.Proposal{Summary: "first request"}, 2)
	r2, c2 := approvalReq(agent.Proposal{Summary: "second request"}, 2)
	step(t, m, r1)
	step(t, m, r2)
	if v := content(m); !strings.Contains(v, "first request") || strings.Contains(v, "second request") {
		t.Fatalf("second request shown early:\n%s", v)
	}
	step(t, m, kp(tea.KeyLeft))
	step(t, m, kp(tea.KeyEnter)) // approve first
	if d := <-c1; !d.Approved {
		t.Fatalf("first: %+v", d)
	}
	if v := content(m); !strings.Contains(v, "second request") {
		t.Fatalf("second request not shown:\n%s", v)
	}
	step(t, m, kp(tea.KeyEnter)) // default: decline second, opens the reason prompt
	step(t, m, kp(tea.KeyEnter)) // no reason
	if d := <-c2; d.Approved {
		t.Fatalf("second: %+v", d)
	}
	step(t, m, TurnDoneMsg{})
	if len(c1) != 0 || len(c2) != 0 || m.modal != nil || len(m.queue) != 0 {
		t.Fatal("duplicate replies or leftovers")
	}
}

func TestTurnDoneAnswersOpenAndQueued(t *testing.T) {
	m := newDirect(t, 100, 30)
	pr := make(chan PromptAnswer, 2)
	r1, c1 := approvalReq(agent.Proposal{Summary: "pending approval"}, 2)
	r2, c2 := approvalReq(agent.Proposal{Summary: "queued approval"}, 2)
	step(t, m, PromptRequestMsg{Question: sshx.Question{Kind: sshx.AskPassword, Host: "h", Prompt: "Password: "}, Reply: pr})
	step(t, m, r1)
	step(t, m, r2)
	step(t, m, TurnDoneMsg{Err: context.Canceled})
	if a := <-pr; a.Err == nil {
		t.Errorf("prompt answered without error: %+v", a)
	}
	for i, c := range []chan agent.Decision{c1, c2} {
		d := <-c
		if d.Approved || d.Reason != "the turn ended" {
			t.Errorf("request %d: %+v", i, d)
		}
		if len(c) != 0 {
			t.Errorf("request %d answered twice", i)
		}
	}
	if len(pr) != 0 || m.modal != nil || len(m.queue) != 0 {
		t.Fatal("leftovers after turn end")
	}
}

func TestQuitAnswersEverything(t *testing.T) {
	m := newDirect(t, 100, 30)
	r1, c1 := approvalReq(agent.Proposal{Summary: "one"}, 2)
	r2, c2 := approvalReq(agent.Proposal{Summary: "two"}, 2)
	step(t, m, r1)
	step(t, m, r2)
	step(t, m, tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	for _, c := range []chan agent.Decision{c1, c2} {
		if d := <-c; d.Approved || len(c) != 0 {
			t.Fatalf("%+v", d)
		}
	}
}

func TestCtrlCInModalDeclinesAndCancelsTurn(t *testing.T) {
	m := newDirect(t, 100, 30)
	ctx, cancel := context.WithCancel(context.Background())
	m.running, m.cancel = true, cancel
	r1, c1 := approvalReq(agent.Proposal{Summary: "one"}, 2)
	step(t, m, r1)
	step(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if d := <-c1; d.Approved {
		t.Fatalf("%+v", d)
	}
	if ctx.Err() == nil || !m.canceling || m.modal != nil {
		t.Fatal("turn not canceled")
	}
	// A second Ctrl+C while canceling quits.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("no quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("second Ctrl+C did not quit")
	}
}

func TestAskHostKeyDefaultsToNo(t *testing.T) {
	ask := func(t *testing.T, keys ...tea.KeyPressMsg) PromptAnswer {
		m := newDirect(t, 100, 30)
		ch := make(chan PromptAnswer, 2)
		step(t, m, PromptRequestMsg{Question: sshx.Question{Kind: sshx.AskHostKey, Host: "login.example.org", Prompt: "Trust key SHA256:abc?"}, Reply: ch})
		if v := content(m); !strings.Contains(v, "login.example.org") || !strings.Contains(v, "No") {
			t.Fatalf("host key dialog:\n%s", v)
		}
		for _, k := range keys {
			step(t, m, k)
		}
		select {
		case a := <-ch:
			if len(ch) != 0 {
				t.Fatal("two answers")
			}
			return a
		default:
			t.Fatal("no answer")
		}
		return PromptAnswer{}
	}
	if a := ask(t, kp(tea.KeyEnter)); a.Text != "no" || a.Err != nil {
		t.Fatalf("Enter = %+v", a)
	}
	if a := ask(t, kp(tea.KeyLeft), kp(tea.KeyEnter)); a.Text != "yes" {
		t.Fatalf("Left+Enter = %+v", a)
	}
}

func TestEscCancelsPrompt(t *testing.T) {
	m := newDirect(t, 100, 30)
	ch := make(chan PromptAnswer, 2)
	step(t, m, PromptRequestMsg{Question: sshx.Question{Kind: sshx.AskPassword, Host: "h", Prompt: "Password: "}, Reply: ch})
	step(t, m, kp(tea.KeyEscape))
	if a := <-ch; a.Err == nil || len(ch) != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestBackgroundColorSelectsStyle(t *testing.T) {
	m := newDirect(t, 100, 30)
	if !m.dark {
		t.Fatal("not dark by default")
	}
	step(t, m, tea.BackgroundColorMsg{Color: colorWhite{}})
	if m.dark || m.rdark {
		t.Fatalf("light background not applied: dark=%v rdark=%v", m.dark, m.rdark)
	}
	// A form opened afterwards is told the background too.
	step(t, m, PromptRequestMsg{Question: sshx.Question{Kind: sshx.AskPassword, Host: "h", Prompt: "p"}, Reply: make(chan PromptAnswer, 1)})
	step(t, m, tea.BackgroundColorMsg{Color: colorBlack{}})
	if !m.dark {
		t.Fatal("dark not restored")
	}
}

type colorWhite struct{}

func (colorWhite) RGBA() (r, g, b, a uint32) { return 0xffff, 0xffff, 0xffff, 0xffff }

type colorBlack struct{}

func (colorBlack) RGBA() (r, g, b, a uint32) { return 0, 0, 0, 0xffff }

func TestViewsFitTheWindow(t *testing.T) {
	for _, size := range [][2]int{{60, 15}, {100, 30}, {80, 20}} {
		m := newDirect(t, size[0], size[1])
		check := func(what string) {
			t.Helper()
			v := content(m)
			lines := strings.Split(v, "\n")
			if len(lines) > size[1] {
				t.Errorf("%s at %v: %d rows", what, size, len(lines))
			}
			for _, l := range lines {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%s at %v: row of width %d: %q", what, size, w, l)
				}
			}
		}
		check("chat")
		req, _ := approvalReq(agent.Proposal{Summary: "s", Details: manyLines(300)}, 2)
		step(t, m, req)
		check("approval")
	}
}

func TestBridgeCloseUnblocksEverything(t *testing.T) {
	b := NewBridge()
	errc := make(chan error, 2)
	go func() { _, err := b.Approve(context.Background(), agent.Proposal{}); errc <- err }()
	go func() { _, err := b.Ask(context.Background(), sshx.Question{}); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	b.Close()
	b.Close() // idempotent
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err == nil {
				t.Error("blocked call returned nil error after Close")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("call still blocked after Close")
		}
	}
	if _, err := b.Approve(context.Background(), agent.Proposal{}); err == nil {
		t.Error("Approve after Close succeeded")
	}
	if _, err := b.Ask(context.Background(), sshx.Question{}); err == nil {
		t.Error("Ask after Close succeeded")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ { // far more than the channel holds
			b.Event(agent.Event{Kind: agent.EventText, Text: "x"})
		}
		b.Done(nil)
		b.Notify("n")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Event/Done blocked after Close")
	}
}

func TestBridgeEventBlocksUntilRoomOrClose(t *testing.T) {
	b := NewBridge()
	for i := 0; i < cap(b.msgs); i++ {
		b.Event(agent.Event{Kind: agent.EventText, Text: "x"})
	}
	returned := make(chan struct{})
	go func() { b.Event(agent.Event{Kind: agent.EventText, Text: "y"}); close(returned) }()
	select {
	case <-returned:
		t.Fatal("Event dropped or returned while the channel was full")
	case <-time.After(50 * time.Millisecond):
	}
	b.Next()() // make room
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Event still blocked after room was made")
	}
}

func TestRapidTextDeltasNeverDropped(t *testing.T) {
	const n = 2000
	b := NewBridge()
	var want strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&want, "w%d ", i)
	}
	s := &fakeSession{b: b, script: func(ctx context.Context, b *Bridge, text string) {
		for i := 0; i < n; i++ {
			b.Event(agent.Event{Kind: agent.EventText, Text: fmt.Sprintf("w%d ", i)})
		}
		b.Done(nil)
	}}
	tm := newTM(t, s)
	typeLine(tm, "go")
	waitFor(t, tm, fmt.Sprintf("w%d", n-1))
	tm.Quit()
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*model)
	var got string
	for _, e := range fm.entries {
		if e.role == roleAssistant && strings.HasPrefix(e.text, "w0 ") {
			got = e.text
		}
	}
	if got != want.String() {
		t.Fatalf("deltas lost or reordered: got %d bytes, want %d", len(got), want.Len())
	}
	if fm.running {
		t.Error("turn did not end (TurnDone not handled)")
	}
}

func TestStreamingRendersAreThrottledAndCached(t *testing.T) {
	m := newDirect(t, 100, 30)
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventToolCall, Tool: "check_cluster"}})
	cmds := 0
	for i := 0; i < 500; i++ {
		_, cmd := m.Update(AgentEventMsg{Event: agent.Event{Kind: agent.EventText, Text: "word "}})
		if cmd != nil {
			cmds++
		}
	}
	if cmds != 1 {
		t.Errorf("%d flush ticks scheduled for one burst, want 1", cmds)
	}
	// Entries that are not streaming stay cached across a refresh.
	m.refresh()
	for i := range m.entries[:len(m.entries)-1] {
		if !m.entries[i].valid {
			t.Errorf("entry %d not cached", i)
		}
	}
	step(t, m, flushMsg{})
	if m.flushPending {
		t.Error("flushPending still set")
	}
	_, cmd := m.Update(AgentEventMsg{Event: agent.Event{Kind: agent.EventText, Text: "more"}})
	if cmd == nil {
		t.Error("no new flush tick after a flush")
	}
}

func TestStatusKeepsHintsOverStatusText(t *testing.T) {
	m := newModel(Options{Status: strings.Repeat("a very long cluster description ", 5)})
	step(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	bar := stripStyles(m.statusBar())
	if !strings.Contains(bar, "Ctrl+C quit") || !strings.Contains(bar, "Ctrl+D quit") || !strings.Contains(bar, "Enter send") {
		t.Fatalf("hints cut off: %q", bar)
	}
	step(t, m, tea.WindowSizeMsg{Width: 60, Height: 30})
	bar = stripStyles(m.statusBar())
	if !strings.Contains(bar, "Ctrl+C quit") || !strings.Contains(bar, "Ctrl+D quit") {
		t.Fatalf("hints cut off at 60 cols: %q", bar)
	}
}

func TestNoticeIsOneSanitizedLine(t *testing.T) {
	m := newDirect(t, 100, 30)
	step(t, m, NotifyMsg{Text: "approve\x1b[31m the\nDuo push"})
	if m.notice != "approve the Duo push" {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestJobsPanelTruncates(t *testing.T) {
	var jobs []store.Job
	for i := 0; i < 40; i++ {
		jobs = append(jobs, store.Job{Name: fmt.Sprintf("train-%d-with-a-really-long-name-that-overflows-the-panel", i), Cluster: "hpcone", State: "RUNNING", UpdatedAt: time.Now()})
	}
	m := newModel(Options{Jobs: func(context.Context) ([]store.Job, error) { return jobs, nil }})
	step(t, m, tea.WindowSizeMsg{Width: 100, Height: 20})
	step(t, m, jobsMsg{jobs: jobs})
	step(t, m, kp(tea.KeyTab))
	if !m.showJobs {
		t.Fatal("jobs panel not shown")
	}
	v := content(m)
	lines := strings.Split(v, "\n")
	if len(lines) != 20 {
		t.Fatalf("%d rows, want 20:\n%s", len(lines), v)
	}
	for _, l := range lines {
		if lipgloss.Width(l) > 100 {
			t.Fatalf("row too wide (%d): %q", lipgloss.Width(l), l)
		}
	}
	if !strings.Contains(v, "more lines") {
		t.Errorf("no truncation note:\n%s", v)
	}
}

// TestKeySetupDialogs: the SSH key setup questions (M38.6). Creating a key
// defaults to No; the upload question shows the site's instructions and the
// public key, Ctrl+Y copies the key, and Left+Enter confirms; the new-key
// passphrase is masked and may be empty.
func TestKeySetupDialogs(t *testing.T) {
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAbcdefghijklmnopqrstuvwxyz0123456789ABCDEFG labmill-orca"
	ask := func(t *testing.T, q sshx.Question, keys ...tea.KeyPressMsg) (PromptAnswer, string) {
		t.Helper()
		m := newDirect(t, 100, 30)
		ch := make(chan PromptAnswer, 2)
		step(t, m, PromptRequestMsg{Question: q, Reply: ch})
		v := content(m)
		for _, k := range keys {
			step(t, m, k)
		}
		select {
		case a := <-ch:
			return a, v
		default:
			t.Fatal("no answer")
		}
		return PromptAnswer{}, v
	}
	a, v := ask(t, sshx.Question{Kind: sshx.AskKeyCreate, Host: "login.example.org", Prompt: "login.example.org accepts only SSH keys.\n\nCreate a new SSH key? (yes/no)", Echo: true}, kp(tea.KeyEnter))
	if a.Text != "no" || !strings.Contains(v, "accepts only SSH keys") {
		t.Errorf("create: answer %+v, view:\n%s", a, v)
	}
	a, _ = ask(t, sshx.Question{Kind: sshx.AskKeyCreate, Host: "h", Prompt: "p", Echo: true}, kp(tea.KeyLeft), kp(tea.KeyEnter))
	if a.Text != "yes" {
		t.Errorf("create, Left+Enter = %+v", a)
	}

	// Copying a private key from the home cluster (M45.3): the whole prompt
	// is shown, the default answer is No.
	cp := sshx.Question{Kind: sshx.AskKeyCopy, Host: "login.far.example.org", Echo: true,
		Prompt: "login.far.example.org already accepts the SSH key id_ed25519 (SHA256:abc) that is on login.home.example.edu.\n\nWarning: this puts a copy of a private key on this computer."}
	a, v = ask(t, cp, kp(tea.KeyEnter))
	if a.Text != "no" || !strings.Contains(v, "copy of a private key") || !strings.Contains(v, "SHA256:abc") {
		t.Errorf("copy: answer %+v, view:\n%s", a, v)
	}
	a, _ = ask(t, cp, kp(tea.KeyLeft), kp(tea.KeyEnter))
	if a.Text != "yes" {
		t.Errorf("copy, Left+Enter = %+v", a)
	}

	up := sshx.Question{Kind: sshx.AskKeyUpload, Host: "login.example.org", Echo: true, Data: pub,
		Prompt: "Register this public key with the site.\n\nPaste it into the registry.\n\nhttps://registry.example.org/keys\n"}
	a, v = ask(t, up, kp(tea.KeyEnter))
	if a.Text != "no" || !strings.Contains(v, "Paste it into the registry.") || !strings.Contains(v, "https://registry.example.org/keys") || !strings.Contains(v, "AAAAC3NzaC1lZDI1NTE5AAAAIAbcdef") || !strings.Contains(v, "Ctrl+Y") {
		t.Errorf("upload: answer %+v, view:\n%s", a, v)
	}
	a, _ = ask(t, up, kp(tea.KeyLeft), kp(tea.KeyEnter))
	if a.Text != "yes" {
		t.Errorf("upload, Left+Enter = %+v", a)
	}
	m := newDirect(t, 100, 30)
	step(t, m, PromptRequestMsg{Question: up, Reply: make(chan PromptAnswer, 2)})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}); cmd == nil {
		t.Error("Ctrl+Y returned no clipboard command")
	}
	if v := content(m); !strings.Contains(v, "Key copied") {
		t.Errorf("no copy notice:\n%s", v)
	}

	// Passphrase for the new key: empty is a valid answer.
	a, _ = ask(t, sshx.Question{Kind: sshx.AskNewPassphrase, Host: "h", Prompt: "Optional passphrase"}, kp(tea.KeyEnter))
	if a.Err != nil || a.Text != "" {
		t.Errorf("empty passphrase = %+v", a)
	}
}
