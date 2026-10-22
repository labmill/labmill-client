package textsafe

import (
	"testing"
	"unicode"
	"unicode/utf8"
)

var cleanCases = map[string]string{
	"plain text ✓ 日本語 🎉": "plain text ✓ 日本語 🎉",
	"a\tb":               "a    b",
	"line1\r\nline2\rx":  "line1\nline2x",
	"\x1b[31mred\x1b[0m": "red",
	"\x1b]8;;http://evil\x07link\x1b]8;;\x07": "link",
	"\x1b]0;title\x1b\\after":                 "after",
	"bell\x07 del\x7f c1\u009b31mX":           "bell del c1X",
	"a\u009d0;title\u009cb":                   "ab",
	"a\u009d0;title\nb":                       "a0;title\nb", // unterminated C1: only the introducer goes
	"a\x1b(\nb":                               "a\nb",
	"\x1b\x1b[m":                              "",
	"\x1b日本":                                  "日本",
	"ð\u009f\u0098\u0080":                     "ð",
	"id,\u009dname,x":                         "id,name,x",
	"a\u009b\nb":                              "a\nb",
	"a\u009b1;2 é":                            "a1;2 é",
	"a\u009b1mb":                              "ab",
	"a\u009d0;t\x07b":                         "ab",
	"a\u0090q\x1b\\b":                         "ab",
	"a\u0090qdata\x1b\\b\u0085c":              "abc",
	"x\x1b]0;title":                           "x",
	"a\x1b(Bb\x1b7c":                          "abc",
	"\x1b[31;1":                               "",
	"a\u202Eb\u2066c\u200Bd\uFEFFe":           "abcde",
	"x\u2028y\u2029z":                         "xyz",
	"bad\xffutf8":                             "bad�utf8",
	"keep\nnewlines":                          "keep\nnewlines",
	"\x1b":                                    "",
	"\x1b[":                                   "",
	"👨\u200d👩\u200d👧":                         "👨\u200d👩\u200d👧",
	"می\u200cخواهم":                           "می\u200cخواهم",
	"soft\u00adhyphen":                        "softhyphen",
	"tag\U000E0041\U000E0042x":                "tagx",
	"a\u206ab\uFFF9c\u2060d\u200Be":           "abcde",
}

func TestClean(t *testing.T) {
	for in, want := range cleanCases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzClean checks the output invariants for arbitrary input; the seeds run
// under plain go test.
func FuzzClean(f *testing.F) {
	for in := range cleanCases {
		f.Add(in)
	}
	f.Add("\x1b]8;;http://x\x07a\x1b]8;;\x07\u009b2J\u009d\u009c\r\n\t")
	f.Fuzz(func(t *testing.T, in string) {
		out := Clean(in)
		if !utf8.ValidString(out) {
			t.Fatalf("Clean(%q) = %q: invalid UTF-8", in, out)
		}
		for _, r := range out {
			switch {
			case r < 0x20 && r != '\n', r == 0x7f:
				t.Fatalf("Clean(%q) = %q: control %U", in, out, r)
			case r >= 0x80 && r <= 0x9f:
				t.Fatalf("Clean(%q) = %q: C1 %U", in, out, r)
			case r == 0x2028 || r == 0x2029:
				t.Fatalf("Clean(%q) = %q: separator %U", in, out, r)
			case unicode.Is(unicode.Cf, r) && r != 0x200c && r != 0x200d:
				t.Fatalf("Clean(%q) = %q: format char %U", in, out, r)
			}
		}
		if again := Clean(out); again != out {
			t.Fatalf("Clean not idempotent: %q -> %q -> %q", in, out, again)
		}
	})
}
