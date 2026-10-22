package keepawake

import "testing"

func TestHoldRefCount(t *testing.T) {
	starts, stops := 0, 0
	old := start
	start = func() func() { starts++; return func() { stops++ } }
	defer func() { start = old }()

	a := Hold()
	b := Hold()
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
	a()
	a() // a second call must not release b's hold
	if stops != 0 {
		t.Fatalf("stopped while a hold is left")
	}
	b()
	if stops != 1 {
		t.Fatalf("stops = %d, want 1", stops)
	}
	Hold()()
	if starts != 2 || stops != 2 {
		t.Fatalf("starts=%d stops=%d, want 2 2", starts, stops)
	}
}
