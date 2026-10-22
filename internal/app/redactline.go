package app

import (
	"regexp"
	"strings"
)

// flagSecretRe matches an option that carries a credential, e.g.
// "--token abc", "--password=abc", "--api-key abc".
var flagSecretRe = regexp.MustCompile(`(?i)(--?[\w-]*(?:token|password|passwd|secret|key|auth)[\w-]*(?:=|\s+))\S+`)

// RedactLine returns a short single-line version of s that is safe to write
// to a log: values assigned to credential-looking keys and the arguments of
// --token/--password/--key style options are hidden, and it is cut to max
// runes (0 means 100).
func RedactLine(s string, max int) string {
	if max <= 0 {
		max = 100
	}
	s = strings.Join(strings.Fields(s), " ")
	s = flagSecretRe.ReplaceAllString(s, "${1}«redacted»")
	s = redactSecrets(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
