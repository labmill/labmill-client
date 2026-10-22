package app

import (
	"strings"
	"testing"
)

func TestRedactLine(t *testing.T) {
	for _, tc := range []struct{ in, notWant string }{
		{"Run on c: curl --token abc123 https://x", "abc123"},
		{"Run on c: tool --password=hunter2", "hunter2"},
		{"Run on c: tool -api-key  s3cr3t", "s3cr3t"},
		{"Run on c: export TOKEN=zzz9", "zzz9"},
		{"Run on c: tool login --auth\tQWERTY", "QWERTY"},
	} {
		got := RedactLine(tc.in, 0)
		if strings.Contains(got, tc.notWant) || !strings.Contains(got, "«redacted»") {
			t.Errorf("RedactLine(%q) = %q", tc.in, got)
		}
	}
	if got := RedactLine("a\nb   c", 0); got != "a b c" {
		t.Errorf("whitespace: %q", got)
	}
	if got := RedactLine(strings.Repeat("x", 300), 20); len([]rune(got)) != 21 {
		t.Errorf("not cut: %d", len([]rune(got)))
	}
}
