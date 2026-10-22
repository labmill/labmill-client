package gui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/agent"
)

func testChoice() agent.Choice {
	return agent.Choice{
		Question: "Which \x1b[31menv?",
		Options: []agent.ChoiceOption{
			{Label: "Module", Description: "load ‮python"},
			{Label: "Conda", Description: "own env"},
		},
		AllowOther: true,
	}
}

// startChoice runs Choose in the background and returns its event and a
// channel with the result.
func startChoice(t *testing.T, b *Bridge, rec *recorder, ctx context.Context, q agent.Choice) (ChoiceEvent, chan agent.ChoiceAnswer, chan error) {
	t.Helper()
	got := make(chan agent.ChoiceAnswer, 1)
	errs := make(chan error, 1)
	go func() {
		a, err := b.Choose(ctx, q)
		got <- a
		errs <- err
	}()
	return rec.waitFor(t, EvChoice).(ChoiceEvent), got, errs
}

func TestChooseEventCleaned(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	ev, got, _ := startChoice(t, b, rec, context.Background(), testChoice())
	if ev.ID == "" || !ev.AllowOther || len(ev.Options) != 2 {
		t.Fatalf("event = %+v", ev)
	}
	if strings.Contains(ev.Question, "\x1b") || strings.Contains(ev.Options[0].Description, "‮") {
		t.Errorf("text not cleaned: %+v", ev)
	}
	if ev.Options[1].Label != "Conda" || ev.Options[1].Description != "own env" {
		t.Errorf("options = %+v", ev.Options)
	}
	if err := b.Pick(ev.ID, 1, "", false); err != nil {
		t.Fatal(err)
	}
	if a := <-got; a.Index != 1 || a.Skipped || a.Other != "" {
		t.Errorf("answer = %+v", a)
	}
	if err := b.Pick(ev.ID, 0, "", false); err == nil {
		t.Error("second Pick accepted")
	}
	if err := b.Pick("nope", 0, "", false); err == nil {
		t.Error("unknown id accepted")
	}
}

func TestPickVariants(t *testing.T) {
	cases := []struct {
		name       string
		allowOther bool
		index      int
		other      string
		skip       bool
		want       agent.ChoiceAnswer
		wantErr    bool
	}{
		{"first", true, 0, "", false, agent.ChoiceAnswer{Index: 0}, false},
		{"skip wins", true, 1, "text", true, agent.ChoiceAnswer{Index: -1, Skipped: true}, false},
		{"other", true, -1, "  use\n conda \x1b[1m ", false, agent.ChoiceAnswer{Index: -1, Other: "use conda"}, false},
		{"other not allowed", false, -1, "x", false, agent.ChoiceAnswer{Index: -1, Skipped: true}, true},
		{"blank other falls to index", true, 1, "   ", false, agent.ChoiceAnswer{Index: 1}, false},
		{"index too big", true, 2, "", false, agent.ChoiceAnswer{Index: -1, Skipped: true}, true},
		{"negative index", true, -1, "", false, agent.ChoiceAnswer{Index: -1, Skipped: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			b := NewBridge(rec.emit)
			q := testChoice()
			q.AllowOther = tc.allowOther
			ev, got, _ := startChoice(t, b, rec, context.Background(), q)
			err := b.Pick(ev.ID, tc.index, tc.other, tc.skip)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if a := <-got; a != tc.want {
				t.Errorf("answer = %+v, want %+v", a, tc.want)
			}
		})
	}
}

func TestPickCapsOther(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	ev, got, _ := startChoice(t, b, rec, context.Background(), testChoice())
	b.Pick(ev.ID, -1, strings.Repeat("y", 2000), false)
	if a := <-got; len([]rune(a.Other)) != maxReasonRunes {
		t.Errorf("other has %d runes", len([]rune(a.Other)))
	}
}

func TestChooseDoneSkipsAndDismisses(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	ev, got, errs := startChoice(t, b, rec, context.Background(), testChoice())
	b.Done(nil)
	if a := <-got; !a.Skipped {
		t.Errorf("answer = %+v", a)
	}
	if err := <-errs; err != nil {
		t.Errorf("err = %v", err)
	}
	d := rec.waitFor(t, EvDismiss).(DismissEvent)
	if len(d.IDs) != 1 || d.IDs[0] != ev.ID {
		t.Errorf("dismiss = %+v", d)
	}
	if err := b.Pick(ev.ID, 0, "", false); err == nil {
		t.Error("Pick after Done accepted")
	}
}

func TestChooseCtxCancelDismisses(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	ctx, cancel := context.WithCancel(context.Background())
	ev, got, errs := startChoice(t, b, rec, ctx, testChoice())
	cancel()
	if a := <-got; !a.Skipped {
		t.Errorf("answer = %+v", a)
	}
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if d := rec.waitFor(t, EvDismiss).(DismissEvent); len(d.IDs) != 1 || d.IDs[0] != ev.ID {
		t.Errorf("dismiss = %+v", d)
	}
}

func TestChooseClose(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	_, got, errs := startChoice(t, b, rec, context.Background(), testChoice())
	b.Close()
	if a := <-got; !a.Skipped {
		t.Errorf("answer = %+v", a)
	}
	<-errs
	if a, err := b.Choose(context.Background(), testChoice()); err == nil || !a.Skipped {
		t.Errorf("Choose after Close = %+v, %v", a, err)
	}
}

func TestAppChooseForwards(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	app := NewApp(b, nil, nil, "")
	ev, got, _ := startChoice(t, b, rec, context.Background(), testChoice())
	if err := app.Choose(ev.ID, -1, "my own", false); err != nil {
		t.Fatal(err)
	}
	if a := <-got; a.Other != "my own" {
		t.Errorf("answer = %+v", a)
	}
}
