package launch

import "testing"

func TestMaxTurnsSetting(t *testing.T) {
	env := func(v string) func(string) string {
		return func(string) string { return v }
	}
	for _, c := range []struct {
		val  string
		want int
	}{{"", DefaultMaxTurns}, {"abc", DefaultMaxTurns}, {"0", DefaultMaxTurns}, {"-3", DefaultMaxTurns}, {" 120 ", 120}} {
		if got := maxTurns(env(c.val)); got != c.want {
			t.Errorf("LABMILL_MAX_TURNS=%q: %d, want %d", c.val, got, c.want)
		}
	}
	if DefaultMaxTurns != 80 {
		t.Errorf("DefaultMaxTurns = %d, want 80", DefaultMaxTurns)
	}
}
