// Package startup times Labmill's startup steps (M46.1): which step of the
// GUI's launch costs how much, from process start to the first usable chat
// and to "ready". It imports nothing from Labmill, so it is initialized early
// (some other package's init may still run before it); step names are fixed
// words, never paths or user names.
package startup

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// start is time zero. On Windows it is the process creation time; elsewhere
// it is when this package was initialized (some package init may run before
// it). Package-level, so it is captured before main runs.
var start = processStart()

// Timer records startup steps relative to the process start. It is safe for
// concurrent use; the zero value is ready to use.
type Timer struct {
	mu      sync.Mutex
	entries []entry
}

type entry struct {
	step string
	at   time.Duration // since process start: the mark, or the start of a span
	dur  time.Duration // span length; negative for a mark
}

// Default is the process-wide timer the package-level functions use.
var Default = &Timer{}

// Mark records on Default that step happened now.
func Mark(step string) { Default.Mark(step) }

// Once is Mark on Default unless step was marked before (for events that
// can repeat, like a page reload).
func Once(step string) { Default.Once(step) }

// Span starts timing step on Default and returns the function that ends it.
func Span(step string) func() { return Default.Span(step) }

// Lines returns Default's lines.
func Lines() []string { return Default.Lines() }

// Since returns the time since the process start.
func Since() time.Duration { return time.Since(start) }

// Mark records that step happened now.
func (t *Timer) Mark(step string) {
	t.add(entry{step: clean(step), at: time.Since(start), dur: -1}, false)
}

// Once is Mark unless step was marked before.
func (t *Timer) Once(step string) {
	t.add(entry{step: clean(step), at: time.Since(start), dur: -1}, true)
}

// Span starts timing step and returns the function that ends it and records
// the step; calling it again does nothing.
func (t *Timer) Span(step string) func() {
	step = clean(step)
	begin := time.Now()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.add(entry{step: step, at: begin.Sub(start), dur: time.Since(begin)}, false)
		})
	}
}

func (t *Timer) add(e entry, unique bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if unique {
		for _, o := range t.entries {
			if o.step == e.step && o.dur < 0 {
				return
			}
		}
	}
	t.entries = append(t.entries, e)
}

// Lines returns one line per recorded step, in the order the steps were
// recorded (a span when it ended):
//
//	startup: <step> <ms> ms                   a mark: ms since process start
//	startup: <step> <ms> ms (from <ms> ms)    a span: its length, and when it began
func (t *Timer) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.entries))
	for _, e := range t.entries {
		if e.dur < 0 {
			out = append(out, fmt.Sprintf("startup: %s %s ms", e.step, ms(e.at)))
		} else {
			out = append(out, fmt.Sprintf("startup: %s %s ms (from %s ms)", e.step, ms(e.dur), ms(e.at)))
		}
	}
	return out
}

// ms formats d in milliseconds with one decimal.
func ms(d time.Duration) string {
	return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000)
}

// clean keeps a step name on one line.
func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
