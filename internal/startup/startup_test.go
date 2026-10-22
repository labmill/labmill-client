package startup

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOrderAndFormat(t *testing.T) {
	tm := &Timer{}
	tm.Mark("first")
	end := tm.Span("slow step")
	time.Sleep(20 * time.Millisecond)
	end()
	end() // a second call records nothing
	tm.Mark("multi\nline  name")
	lines := tm.Lines()
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	mark := regexp.MustCompile(`^startup: first \d+\.\d ms$`)
	span := regexp.MustCompile(`^startup: slow step (\d+\.\d) ms \(from \d+\.\d ms\)$`)
	if !mark.MatchString(lines[0]) {
		t.Errorf("mark line = %q", lines[0])
	}
	m := span.FindStringSubmatch(lines[1])
	if m == nil {
		t.Fatalf("span line = %q", lines[1])
	}
	var d float64
	fmt.Sscanf(m[1], "%f", &d)
	if d < 19 {
		t.Errorf("span length %v ms, want >= 20", d)
	}
	if !strings.HasPrefix(lines[2], "startup: multi line name ") {
		t.Errorf("name not cleaned: %q", lines[2])
	}
}

func TestMarksNeverGoBack(t *testing.T) {
	tm := &Timer{}
	for i := 0; i < 5; i++ {
		tm.Mark(fmt.Sprint("m", i))
		time.Sleep(time.Millisecond)
	}
	var prev float64
	for i, l := range tm.Lines() {
		var name string
		var v float64
		if _, err := fmt.Sscanf(l, "startup: %s %f ms", &name, &v); err != nil {
			t.Fatal(err)
		}
		if name != fmt.Sprint("m", i) || v < prev {
			t.Errorf("line %d = %q after %v", i, l, prev)
		}
		prev = v
	}
}

func TestOnce(t *testing.T) {
	tm := &Timer{}
	tm.Once("a")
	tm.Once("a")
	tm.Once("b")
	if n := len(tm.Lines()); n != 2 {
		t.Errorf("%d lines, want 2", n)
	}
}

func TestConcurrent(t *testing.T) {
	tm := &Timer{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm.Mark("m")
			tm.Span("s")()
			tm.Once("o")
			_ = tm.Lines()
		}()
	}
	wg.Wait()
	if n := len(tm.Lines()); n != 101 {
		t.Errorf("%d lines, want 101", n)
	}
}

func TestDefault(t *testing.T) {
	Mark("default mark")
	Span("default span")()
	if len(Lines()) < 2 || Since() <= 0 {
		t.Error("package-level timer recorded nothing")
	}
}
