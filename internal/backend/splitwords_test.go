package backend

import (
	"reflect"
	"testing"
)

func TestSplitWords(t *testing.T) {
	good := map[string][]string{
		`second training submit train-1 -i img:tag`: {"second", "training", "submit", "train-1", "-i", "img:tag"},
		`a 'b c' "d e" f\ g`:                        {"a", "b c", "d e", "f g"},
		`x 'it'\''s'`:                               {"x", "it's"},
		`x "say \"hi\" \$HOME \\ \` + "`" + `"`:     {"x", `say "hi" $HOME \ ` + "`"},
		`x ''`:                                      {"x", ""},
		"  lead   trail  ":                          {"lead", "trail"},
		`bash -c 'python train.py; echo done'`:      {"bash", "-c", "python train.py; echo done"},
		`x "a'b"`:                                   {"x", "a'b"},
		"x 'multi\nline'":                           {"x", "multi\nline"},
	}
	for in, want := range good {
		got, err := SplitWords(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("SplitWords(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		`a; rm -rf ~`, `a && b`, `a | b`, `a > f`, `a < f`, "a `id`", `a $(id)`, `a $HOME`, `(a)`,
		"a\nb", `'unterminated`, `"unterminated`, `trailing\`, `a & `,
	} {
		if got, err := SplitWords(in); err == nil {
			t.Errorf("SplitWords(%q) = %q, want error", in, got)
		}
	}
}

func TestSplitWordsInvertsQuote(t *testing.T) {
	for _, argv := range [][]string{
		{"second", "training", "submit", "x", "--command", "--", "bash", "-c", "python a.py; echo $HOME && ls | wc -l"},
		{"a", "", "it's", `"`, "\\", "new\nline", "$(x)", "`y`", "tab\tsep", "~", "-dash"},
	} {
		got, err := SplitWords(Quote(argv))
		if err != nil || !reflect.DeepEqual(got, argv) {
			t.Errorf("SplitWords(Quote(%q)) = %q, %v", argv, got, err)
		}
	}
}
