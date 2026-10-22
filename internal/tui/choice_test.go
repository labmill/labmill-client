package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/agent"
)

func envChoice() agent.Choice {
	return agent.Choice{
		Question: "How should I set up Python on the cluster?",
		Options: []agent.ChoiceOption{
			{Label: "Use a module", Description: "load the cluster's python module"},
			{Label: "Make my own env", Description: "create a conda environment in your home"},
			{Label: "Use a container", Description: "run inside an image"},
		},
		AllowOther: true,
	}
}

func choiceReq(q agent.Choice) (ChoiceRequestMsg, chan agent.ChoiceAnswer) {
	ch := make(chan agent.ChoiceAnswer, 2)
	return ChoiceRequestMsg{Choice: q, Reply: ch}, ch
}

func onlyAnswer(t *testing.T, ch chan agent.ChoiceAnswer) agent.ChoiceAnswer {
	t.Helper()
	select {
	case a := <-ch:
		if len(ch) != 0 {
			t.Fatal("more than one answer")
		}
		return a
	default:
		t.Fatal("no answer")
	}
	return agent.ChoiceAnswer{}
}

func TestChoiceShowsQuestionAndOptions(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, _ := choiceReq(envChoice())
	step(t, m, req)
	v := content(m)
	for _, want := range []string{"How should I set up Python", "› 1. ", "Use a module", "2. Make my own env", "3. Use a container",
		"create a conda environment", "Other…", "Esc skip", "1-3 pick"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in:\n%s", want, v)
		}
	}
}

func TestChoiceArrowEnterPicks(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, ch := choiceReq(envChoice())
	step(t, m, req)
	step(t, m, kp(tea.KeyDown))
	step(t, m, kp(tea.KeyEnter))
	if a := onlyAnswer(t, ch); a.Skipped || a.Other != "" || a.Index != 1 {
		t.Fatalf("answer = %+v, want option 2", a)
	}
	if m.modal != nil {
		t.Fatal("modal still open")
	}
	// Up from the first row wraps to the last (Other); Up again is the last option.
	m = newDirect(t, 100, 30)
	req, ch = choiceReq(envChoice())
	step(t, m, req)
	step(t, m, kp(tea.KeyUp))
	step(t, m, kp(tea.KeyUp))
	step(t, m, kp(tea.KeyEnter))
	if a := onlyAnswer(t, ch); a.Index != 2 {
		t.Fatalf("answer = %+v, want option 3", a)
	}
}

func TestChoiceNumberKeyPicks(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, ch := choiceReq(envChoice())
	step(t, m, req)
	step(t, m, tea.KeyPressMsg{Code: '9', Text: "9"}) // no such option: nothing
	if len(ch) != 0 {
		t.Fatal("a number without an option answered")
	}
	step(t, m, tea.KeyPressMsg{Code: '3', Text: "3"})
	if a := onlyAnswer(t, ch); a.Index != 2 {
		t.Fatalf("answer = %+v, want option 3", a)
	}
}

func TestChoiceOtherTyped(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, ch := choiceReq(envChoice())
	step(t, m, req)
	for i := 0; i < 3; i++ {
		step(t, m, kp(tea.KeyDown))
	}
	step(t, m, kp(tea.KeyEnter)) // empty: nothing
	if len(ch) != 0 {
		t.Fatal("an empty Other answered")
	}
	typeText(t, m, "use 2 GPUs") // digits go into the text, not the options
	if !strings.Contains(content(m), "use 2 GPUs") {
		t.Fatalf("typed text not shown:\n%s", content(m))
	}
	step(t, m, kp(tea.KeyEnter))
	if a := onlyAnswer(t, ch); a.Other != "use 2 GPUs" || a.Skipped {
		t.Fatalf("answer = %+v", a)
	}
}

func TestChoiceNoOtherRow(t *testing.T) {
	m := newDirect(t, 100, 30)
	q := envChoice()
	q.AllowOther = false
	req, ch := choiceReq(q)
	step(t, m, req)
	if strings.Contains(content(m), "Other") {
		t.Fatal("Other row shown although not allowed")
	}
	for i := 0; i < 3; i++ { // wraps back to the first option
		step(t, m, kp(tea.KeyDown))
	}
	step(t, m, kp(tea.KeyEnter))
	if a := onlyAnswer(t, ch); a.Index != 0 {
		t.Fatalf("answer = %+v, want option 1", a)
	}
}

func TestChoiceEscSkips(t *testing.T) {
	m := newDirect(t, 100, 30)
	req, ch := choiceReq(envChoice())
	step(t, m, req)
	step(t, m, kp(tea.KeyEscape))
	if a := onlyAnswer(t, ch); !a.Skipped {
		t.Fatalf("answer = %+v, want skipped", a)
	}
}

func TestChoiceTypedAheadIgnored(t *testing.T) {
	m, c := newClocked(t)
	req, ch := choiceReq(envChoice())
	step(t, m, req)
	step(t, m, tea.KeyPressMsg{Code: '2', Text: "2"})
	step(t, m, kp(tea.KeyDown))
	step(t, m, kp(tea.KeyEnter))
	if len(ch) != 0 {
		t.Fatal("keys inside the grace period answered the choice")
	}
	c.t = c.t.Add(keyGrace)
	step(t, m, kp(tea.KeyEnter)) // the selection did not move during the grace period
	if a := onlyAnswer(t, ch); a.Index != 0 {
		t.Fatalf("answer = %+v, want option 1", a)
	}
}

func TestChoiceTurnEndAnswersOnce(t *testing.T) {
	m := newDirect(t, 100, 30)
	req1, ch1 := choiceReq(envChoice())
	req2, ch2 := choiceReq(envChoice())
	step(t, m, req1)
	step(t, m, req2) // queued
	m.running = true
	step(t, m, TurnDoneMsg{})
	if a := onlyAnswer(t, ch1); !a.Skipped {
		t.Fatalf("open choice = %+v", a)
	}
	if a := onlyAnswer(t, ch2); !a.Skipped {
		t.Fatalf("queued choice = %+v", a)
	}
	if m.modal != nil {
		t.Fatal("modal still open")
	}
}

func TestChoiceQueuedBehindApproval(t *testing.T) {
	m := newDirect(t, 100, 30)
	areq, ach := approvalReq(agent.Proposal{Summary: "Cancel job 42"}, 2)
	creq, cch := choiceReq(envChoice())
	step(t, m, areq)
	step(t, m, creq)
	step(t, m, kp(tea.KeyEscape)) // declines the approval, opens the choice
	if d := <-ach; d.Approved {
		t.Fatal("approved")
	}
	if !strings.Contains(content(m), "Use a module") {
		t.Fatalf("choice not shown next:\n%s", content(m))
	}
	step(t, m, kp(tea.KeyEnter))
	if a := onlyAnswer(t, cch); a.Index != 0 {
		t.Fatalf("answer = %+v", a)
	}
}

func TestChoiceSanitized(t *testing.T) {
	m := newDirect(t, 100, 30)
	q := envChoice()
	q.Question = "Pick \x1b]8;;http://evil\x07one\x1b]8;;\x07"
	q.Options[0].Label = "A\x1b[31mred\nline"
	req, _ := choiceReq(q)
	step(t, m, req)
	v := content(m)
	if strings.Contains(v, "evil") || strings.Contains(v, "\x1b]8") {
		t.Fatalf("escape sequence survived:\n%q", v)
	}
	if !strings.Contains(v, "Ared line") {
		t.Fatalf("label not cleaned to one line:\n%s", v)
	}
}

func TestChoiceFitsSmallWindow(t *testing.T) {
	m := newDirect(t, 60, 15)
	q := envChoice()
	q.Question = strings.Repeat("a very long question ", 15)
	long := strings.Repeat("a long description of the option ", 9)
	for i := range q.Options {
		q.Options[i].Description = long
	}
	q.Options = append(q.Options, agent.ChoiceOption{Label: "Fourth", Description: long})
	req, _ := choiceReq(q)
	step(t, m, req)
	v := content(m)
	for _, want := range []string{"1. ", "2. Make my own env", "3. Use a container", "4. Fourth", "Other…", "Esc skip"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q at 60x15:\n%s", want, v)
		}
	}
}

func TestBridgeChooseRoundTrip(t *testing.T) {
	b := NewBridge()
	got := make(chan agent.ChoiceAnswer, 1)
	go func() {
		a, _ := b.Choose(context.Background(), envChoice())
		got <- a
	}()
	msg := b.Next()()
	req, ok := msg.(ChoiceRequestMsg)
	if !ok {
		t.Fatalf("got %T", msg)
	}
	req.Reply <- agent.ChoiceAnswer{Index: 2}
	if a := <-got; a.Index != 2 {
		t.Errorf("answer = %+v", a)
	}

	// ctx canceled: Skipped with the context's error.
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		a, err := b.Choose(ctx, envChoice())
		if !a.Skipped {
			err = errors.New("not skipped")
		}
		errs <- err
	}()
	b.Next()()
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Choose err = %v", err)
	}

	// Close: Skipped with an error, also for later calls.
	go func() { _, err := b.Choose(context.Background(), envChoice()); errs <- err }()
	b.Next()()
	b.Close()
	select {
	case err := <-errs:
		if err == nil {
			t.Error("Choose survived Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Choose blocked after Close")
	}
	if a, err := b.Choose(context.Background(), envChoice()); err == nil || !a.Skipped {
		t.Errorf("Choose after Close = %+v, %v", a, err)
	}
}

// eightChoice has ask_user's maximum of 8 options, as the container-image
// question uses it.
func eightChoice() agent.Choice {
	q := agent.Choice{Question: "Which container image should the job use? You can also type your own image URL.", AllowOther: true}
	for i := 1; i <= 8; i++ {
		q.Options = append(q.Options, agent.ChoiceOption{
			Label:       fmt.Sprintf("Image %d", i),
			Description: fmt.Sprintf("reason %d: %s", i, strings.Repeat("why this image fits the code ", 4)),
		})
	}
	return q
}

func TestChoiceEightOptions(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {80, 24}, {60, 20}} {
		m := newDirect(t, size[0], size[1])
		req, ch := choiceReq(eightChoice())
		step(t, m, req)
		v := content(m)
		if !strings.Contains(v, "› 1. ") || !strings.Contains(v, "Image 1") {
			t.Fatalf("%dx%d: option 1 missing:\n%s", size[0], size[1], v)
		}
		for i := 2; i <= 8; i++ {
			if !strings.Contains(v, fmt.Sprintf("%d. Image %d", i, i)) {
				t.Fatalf("%dx%d: option %d missing:\n%s", size[0], size[1], i, v)
			}
		}
		for _, want := range []string{"Other…", "1-8 pick", "Esc skip"} {
			if !strings.Contains(v, want) {
				t.Fatalf("%dx%d: missing %q:\n%s", size[0], size[1], want, v)
			}
		}
		if n := len(strings.Split(v, "\n")); n > size[1] {
			t.Errorf("%dx%d: view has %d lines", size[0], size[1], n)
		}
		step(t, m, tea.KeyPressMsg{Code: '9', Text: "9"}) // no ninth option
		if len(ch) != 0 {
			t.Fatal("9 answered with 8 options")
		}
		step(t, m, tea.KeyPressMsg{Code: '8', Text: "8"})
		if a := onlyAnswer(t, ch); a.Index != 7 {
			t.Fatalf("answer = %+v, want option 8", a)
		}
	}
	// Arrows reach the eighth option and the Other row after it.
	m := newDirect(t, 80, 24)
	req, ch := choiceReq(eightChoice())
	step(t, m, req)
	for i := 0; i < 8; i++ {
		step(t, m, kp(tea.KeyDown))
	}
	if !m.modal.cs.onOther() {
		t.Fatal("eight Downs didn't reach Other")
	}
	step(t, m, kp(tea.KeyUp))
	step(t, m, kp(tea.KeyEnter))
	if a := onlyAnswer(t, ch); a.Index != 7 {
		t.Fatalf("answer = %+v, want option 8", a)
	}
}
