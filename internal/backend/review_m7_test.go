package backend

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitWordsRejectsHash(t *testing.T) {
	for _, line := range []string{`echo hi # rm -rf x`, `a#b`, `#x`, `a 'b' #c`} {
		if _, err := SplitWords(line); err == nil {
			t.Errorf("SplitWords(%q): no error", line)
		}
	}
	for _, a := range []string{"#", "a#b", "#!x"} {
		got, err := SplitWords(Quote([]string{"echo", a}))
		if err != nil || !reflect.DeepEqual(got, []string{"echo", a}) {
			t.Errorf("round trip %q: %q, %v", a, got, err)
		}
	}
	if got, err := SplitWords(`echo '#x' "#y" \#z`); err != nil || len(got) != 4 || got[1] != "#x" || got[2] != "#y" || got[3] != "#z" {
		t.Errorf("quoted hash: %q, %v", got, err)
	}
}

func TestSplitWordsBytesAndNUL(t *testing.T) {
	for _, a := range []string{"\xe1", "\xff\xfe", "é", "a\xc3", "\u202e"} {
		got, err := SplitWords(Quote([]string{a}))
		if err != nil || len(got) != 1 || got[0] != a {
			t.Errorf("round trip %q: %q, %v", a, got, err)
		}
	}
	for _, line := range []string{"a\x00b", "'a\x00'", "\"\x00\"", "\x00"} {
		if _, err := SplitWords(line); err == nil {
			t.Errorf("SplitWords(%q): NUL accepted", line)
		}
	}
}

func TestSplitWordsBackslashNewlineInDoubleQuotes(t *testing.T) {
	if _, err := SplitWords("echo \"a\\\nb\""); err == nil {
		t.Error("backslash-newline in double quotes accepted")
	}
	// A plain newline inside quotes remains literal.
	if got, err := SplitWords("echo 'a\nb'"); err != nil || got[1] != "a\nb" {
		t.Errorf("newline in single quotes: %q, %v", got, err)
	}
}

func TestSplitWordsGlobCharsLiteral(t *testing.T) {
	got, err := SplitWords("ls * ? [a] ~ {a,b}")
	if err != nil || !reflect.DeepEqual(got, []string{"ls", "*", "?", "[a]", "~", "{a,b}"}) {
		t.Errorf("got %q, %v", got, err)
	}
}

func FuzzQuoteSplit(f *testing.F) {
	for _, s := range []string{"", "\xe1", "#", "'", "\n", "a b", "$x", "`x`", "\\", "\"", "a=b", "-x=y", "\r", "é", "\u202e", "~", "*"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, a string) {
		if strings.IndexByte(a, 0) >= 0 {
			t.Skip()
		}
		argv := []string{"x", a, a + "'" + a}
		got, err := SplitWords(Quote(argv))
		if err != nil || !reflect.DeepEqual(got, argv) {
			t.Fatalf("argv %q (valid utf8 %v) -> %q, %v", argv, utf8.ValidString(a), got, err)
		}
	})
}

func TestValidateHidingCharacters(t *testing.T) {
	bad := []string{"\r\x1b[2K", "echo \x1b[31m", "a\x7fb", "a\u0085b", "a\u009fb", "a\u202eb", "a\u200bb", "a\u200fb",
		"a\u202ab", "a\u2066b", "a\u2069b", "a\ufeffb", "a\x01b", "a\rb"}
	for _, s := range bad {
		sp := okSpec()
		sp.Command = s
		if err := sp.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("command %q: %v", s, err)
		}
		sp = okSpec()
		sp.Env = map[string]string{"A": s}
		if err := sp.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("env value %q: %v", s, err)
		}
	}
	for _, s := range []string{"a\tb\nc", "héllo \u2713", "a\u200cb"[:1]} {
		sp := okSpec()
		sp.Command = s
		sp.Env = map[string]string{"A": s}
		if err := sp.Validate(); err != nil {
			t.Errorf("%q rejected: %v", s, err)
		}
	}
}

func TestValidateRejectsInvalidUTF8AndInvisibles(t *testing.T) {
	for _, cmd := range []string{"echo \x9b31m hi", "a\u00adb", "a\u2060b", "a\U000E0041b", "a\u2028b", "a\u061cb", "a\r\nb"} {
		s := JobSpec{Name: "j", Command: cmd, WorkDir: "/w"}
		if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("Validate(%q) = %v", cmd, err)
		}
	}
	for _, part := range []string{"-pfoo", "-x"} {
		s := JobSpec{Name: "j", Command: "x", WorkDir: "/w", Partition: part}
		if err := s.Validate(); err == nil {
			t.Errorf("partition %q accepted", part)
		}
	}
	ok := JobSpec{Name: "j", Command: "python a.py\n\techo é ✓", WorkDir: "/w"}
	if err := ok.Validate(); err != nil {
		t.Errorf("legit command rejected: %v", err)
	}
}
