package account

import (
	"fmt"
	"labmill-client/internal/backend"
	"strings"
	"testing"
)

func size(l []string) int {
	n := 0
	for _, s := range l {
		n += len(s) + 1
	}
	return n
}

func TestCondenseIdenticalLines(t *testing.T) {
	in := make([]string, 10000)
	for i := range in {
		in[i] = "waiting for the queue"
	}
	out := Condense(in, OutputBudget)
	if len(out) != 3 || out[0] != "waiting for the queue" || out[1] != "⋮ (9998 similar lines)" || out[2] != out[0] {
		t.Errorf("got %d lines: %v", len(out), out)
	}
}

func TestCondenseSimilarLinesAndBars(t *testing.T) {
	var in []string
	for i := 0; i < 500; i++ {
		in = append(in, fmt.Sprintf("2026-11-18 10:%02d:%02d step %d loss 0.%d id %08x", i/60, i%60, i, i*7, i*99999))
	}
	in = append(in, "done")
	for _, s := range backend.SplitOutput("Epoch 1 |█▏    | 1/9\rEpoch 1 |███▎  | 4/9\rEpoch 1 |██████| 9/9\n") {
		in = append(in, s)
	}
	out := Condense(in, OutputBudget)
	if len(out) != 7 || out[1] != "⋮ (498 similar lines)" || out[2] != "2026-11-18 10:08:19 step 499 loss 0.3493 id 02f967ed" || out[3] != "done" || out[5] != "⋮ (1 similar lines)" {
		t.Errorf("got %q", out)
	}
}

func TestCondenseKeepsTracebackInTheMiddle(t *testing.T) {
	var in []string
	for i := 0; i < 5000; i++ {
		in = append(in, fmt.Sprintf("line %d %s", i, strings.Repeat(string(rune('g'+i%19)), i%17+1)))
	}
	in[2500] = "Traceback (most recent call last):"
	in[2501] = "  File \"train.py\", line 5, in <module>"
	in[2502] = "ValueError: bad shape"
	out := Condense(in, OutputBudget)
	text := strings.Join(out, "\n")
	for _, want := range []string{"line 0 ", "line 99 ", "Traceback (most recent call last):", "ValueError: bad shape", "line 2497 ", "line 2505 ", "line 4999 ", "line 4700 "} {
		if !strings.Contains(text, want) {
			t.Errorf("lost %q", want)
		}
	}
	if strings.Contains(text, "line 2400 ") || strings.Contains(text, "line 150 ") {
		t.Error("kept lines far from the head, the tail and the error")
	}
	if !strings.Contains(text, "lines skipped)") || size(out) > OutputBudget {
		t.Errorf("size %d", size(out))
	}
}

func TestCondenseBudget(t *testing.T) {
	// Many different lines, and many error lines: the result still fits.
	var in []string
	for i := 0; i < 20000; i++ {
		in = append(in, fmt.Sprintf("error in module %s: %s", strings.Repeat("x", i%50), strings.Repeat(string(rune('a'+i%26)), 100+i%100)))
		in = append(in, strings.Repeat(string(rune('A'+i%26)), 250))
	}
	out := Condense(in, OutputBudget)
	if size(out) > OutputBudget || len(out) > condenseLines {
		t.Errorf("size %d, %d lines", size(out), len(out))
	}
	if len(out) < 10 {
		t.Errorf("only %d lines left", len(out))
	}
}

func TestCondenseOutputRedactsRepeatedSecret(t *testing.T) {
	red := NewRedactor("someone")
	var out, errs []string
	for i := 0; i < 3000; i++ {
		out = append(out, fmt.Sprintf("step %d password=hunter2 user someone mail ada@example.edu", i))
		errs = append(errs, "token: sk-live-abcdefghijklmnop")
	}
	so, se := CondenseOutput(red, out, errs)
	text := strings.Join(so, "\n") + strings.Join(se, "\n")
	noSecrets(t, "condensed output", text)
	if len(so) != 3 || len(se) != 3 {
		t.Errorf("stdout %d lines, stderr %d lines", len(so), len(se))
	}
	if size(so)+size(se) > OutputBudget {
		t.Error("over budget")
	}
}

func TestRedactorLeavesMarkers(t *testing.T) {
	m := "⋮ (123456 similar lines)"
	if got := NewRedactor().Line(m); got != m {
		t.Errorf("marker changed: %q", got)
	}
}

func TestCondenseKeepsLastLineOfRun(t *testing.T) {
	in := []string{"start"}
	for i := 0; i < 10; i++ {
		in = append(in, fmt.Sprintf("loss %d.5", i))
	}
	in = append(in, "loss nan", "Traceback (most recent call last):")
	out := strings.Join(Condense(in, OutputBudget), "\n")
	if !strings.Contains(out, "loss nan") {
		t.Errorf("last line of the run lost: %q", out)
	}
}

func TestCondenseLastResortKeepsTheStart(t *testing.T) {
	var in []string
	for i := 0; i < 5000; i++ {
		in = append(in, fmt.Sprintf("error %d: %s", i, strings.Repeat(string(rune('a'+i%26)), 400)))
	}
	out := Condense(in, 4000)
	if size(out) > 4000 || len(out) > condenseLines {
		t.Fatalf("size %d, %d lines", size(out), len(out))
	}
	if len(out) == 0 || out[0] != in[0] || out[len(out)-1] != in[len(in)-1] {
		t.Errorf("the start and the end must stay: %q", out)
	}
	if len(Condense(nil, 4000)) != 0 {
		t.Error("no lines must give no lines")
	}
}

func TestCondenseLastResortKeepsBigLastLine(t *testing.T) {
	var in []string
	for i := 0; i < 50; i++ {
		in = append(in, fmt.Sprintf("error %d: %s", i, strings.Repeat("x", 200)))
	}
	in[0] = strings.Repeat("h", 3000) // too big for the head half: no head
	in[len(in)-1] = "error last: " + strings.Repeat("y", 3000)
	out := Condense(in, 4000)
	if size(out) > 4000 || out[len(out)-1] != in[len(in)-1] {
		t.Errorf("the big last line must stay: size %d, %d lines", size(out), len(out))
	}
}

func TestCondenseLastResortHeadLeavesRoomForLastLine(t *testing.T) {
	var in []string
	for i := 0; i < 49; i++ {
		in = append(in, fmt.Sprintf("error %d: %s", i, strings.Repeat("x", 150)))
	}
	in = append(in, "error last: "+strings.Repeat("y", 3000))
	out := Condense(in, 4000)
	if size(out) > 4000 || out[len(out)-1] != in[len(in)-1] {
		t.Errorf("the last line must stay: size %d, %d lines", size(out), len(out))
	}
	if out[0] != in[0] {
		t.Errorf("the first line must stay too")
	}
}
