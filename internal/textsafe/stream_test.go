package textsafe

import (
	"strings"
	"testing"
)

func TestStreamSplitEscapes(t *testing.T) {
	cases := []struct {
		name   string
		deltas []string
		want   string
	}{
		{"csi split", []string{"a\x1b", "[31mb"}, "ab"},
		{"csi split mid", []string{"a\x1b[3", "1mb"}, "ab"},
		{"csi split before final", []string{"a\x1b[31", "mb"}, "ab"},
		{"osc split", []string{"a\x1b]0;ti", "tle\x07b"}, "ab"},
		{"osc split at ST", []string{"a\x1b]0;x\x1b", "\\b"}, "ab"},
		{"c1 csi split", []string{"a\u009b3", "1mb"}, "ab"},
		{"nF split", []string{"a\x1b(", "Bb"}, "ab"},
		{"plain", []string{"hello ", "world"}, "hello world"},
		{"complete in one", []string{"\x1b[31mred\x1b[0m"}, "red"},
		{"many pieces", []string{"\x1b", "[", "3", "1", "m", "x"}, "x"},
	}
	for _, c := range cases {
		var s Stream
		var got strings.Builder
		for _, d := range c.deltas {
			got.WriteString(s.Write(d))
		}
		got.WriteString(s.Flush())
		if got.String() != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got.String(), c.want)
		}
	}
}

func TestStreamFlushDropsOpenEscape(t *testing.T) {
	var s Stream
	if got := s.Write("a\x1b[3"); got != "a" {
		t.Errorf("Write = %q", got)
	}
	if got := s.Flush(); got != "" {
		t.Errorf("Flush = %q, want empty", got)
	}
	if got := s.Write("b"); got != "b" {
		t.Errorf("after Flush: %q", got)
	}
}

func TestStreamUnendingSequenceIsBounded(t *testing.T) {
	var s Stream
	out := s.Write("\x1b]0;" + strings.Repeat("x", 1000))
	out += s.Write("tail")
	if !strings.Contains(out, "tail") {
		t.Errorf("text after a very long unfinished sequence was held back: %q", out)
	}
}
