package account

import (
	"regexp"
	"strings"
)

// Redaction on this computer, before anything leaves it. The rules are the
// generic ones the Labmill server runs again after the upload (secrets,
// emails, IP addresses, paths, host names, long numbers). Words are the
// names only this computer knows (the user's login and account names, the
// job's name, the cluster's host names): they become <name>. A miss leaks
// private data, so the rules only err towards removing.
const (
	maxLineLen     = 300      // a longer line is cut after redaction
	maxScriptBytes = 64 << 10 // the server keeps at most this much of a script
	maxMessage     = 2048     // a problem report's message
)

var redactRules = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?i)\b(?:password|passwd|token|secret|api[_-]?key|authorization)\b\s*[:=]?\s*(?:bearer\s+)?\S+`), "<secret>"},
	{regexp.MustCompile(`\b(?:sk|ghp|gho|lmt|xox[a-z]|AKIA)[-_A-Za-z0-9]{12,}`), "<secret>"},
	{regexp.MustCompile(`(?i)(--?[\w-]*(?:token|password|passwd|secret|key|auth)[\w-]*(?:=|\s+))\S+`), "${1}<secret>"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "<email>"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), "<ip>"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}\b`), "<ip>"},
	{regexp.MustCompile(`(?:[A-Za-z]:)?(?:[/\\][^\s/\\:"']+){2,}[/\\]?`), "<path>"},
	{regexp.MustCompile(`\b[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+\.[A-Za-z]{2,}\b`), "<host>"},
	{regexp.MustCompile(`\b\d{6,}\b`), "<num>"},
}

// sbatchNameRE matches the value of an #SBATCH option that names a job,
// account, project or folder.
var sbatchNameRE = regexp.MustCompile(`(?i)^(\s*#SBATCH\s+(?:--(?:job-name|account|comment|wckey|output|error|chdir|workdir|reservation|qos|licenses|partition|mail-user)|-[JAoeDp]))(?:=|\s+)\S+`)

// Redactor cleans text with the generic rules and its words.
type Redactor struct{ words []*regexp.Regexp }

// NewRedactor makes a Redactor for words (case-insensitive; words shorter
// than 3 characters are ignored, they would match everywhere).
func NewRedactor(words ...string) Redactor {
	var r Redactor
	seen := map[string]bool{}
	for _, w := range words {
		w = strings.ToLower(strings.TrimSpace(w))
		if len(w) < 3 || seen[w] {
			continue
		}
		seen[w] = true
		r.words = append(r.words, regexp.MustCompile(`(?i)`+regexp.QuoteMeta(w)))
	}
	return r
}

// Line redacts one line and cuts it to maxLineLen bytes.
func (r Redactor) Line(l string) string {
	l = strings.ToValidUTF8(l, "")
	for _, w := range r.words { // first: a word can be part of a path or host the rules would cut apart
		l = w.ReplaceAllString(l, "<name>")
	}
	for _, rule := range redactRules {
		l = rule.re.ReplaceAllString(l, rule.with)
	}
	if len(l) > maxLineLen {
		l = strings.ToValidUTF8(l[:maxLineLen], "")
	}
	return l
}

// Lines redacts each line.
func (r Redactor) Lines(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = r.Line(l)
	}
	return out
}

// Script redacts an sbatch script line by line and removes the names in
// #SBATCH options; the result is at most maxScriptBytes.
func (r Redactor) Script(body string) string {
	lines := strings.Split(strings.ToValidUTF8(body, ""), "\n")
	for i, l := range lines {
		lines[i] = r.Line(sbatchNameRE.ReplaceAllString(l, "$1 <name>"))
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxScriptBytes {
		out = strings.ToValidUTF8(out[:maxScriptBytes], "")
	}
	return out
}

// Message redacts a short text of several lines and cuts it to maxMessage bytes.
func (r Redactor) Message(s string) string {
	out := strings.Join(r.Lines(strings.Split(s, "\n")), "\n")
	if len(out) > maxMessage {
		out = strings.ToValidUTF8(out[:maxMessage], "")
	}
	return out
}
