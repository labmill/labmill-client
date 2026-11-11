package app

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"labmill-client/internal/agent"
)

// Secret guard (owner decision 2026-11-11). Every tool result goes through scrubTokens before it reaches the model:
// text that looks like a key, token or private key is replaced by a marker, and the user gets a warning. The keyed
// rule of redactSecrets ("password = ...") stays where it is; this rule finds the secret by its shape, so it also
// catches a token in a log line, a URL or a command line. The patterns are narrow on purpose (a known prefix and a
// long body), so ordinary text and checksums are left alone.

const secretMark = "«secret hidden»"

var secretShapes = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)},
	{"AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})\b`)},
	{"API key", regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|hf_[A-Za-z0-9]{30,}|AIza[0-9A-Za-z_-]{35}|glpat-[A-Za-z0-9_-]{20,})`)},
	{"Slack token", regexp.MustCompile(`\bxox[abeprs]-[A-Za-z0-9-]{10,}`)},
	{"JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"bearer token", regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{20,}`)},
	{"password in a URL", regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@]+:)[^\s/@]+@`)},
}

// scrubTokens replaces secret-shaped text in s. kinds lists what it found, once each.
func scrubTokens(s string) (out string, kinds []string) {
	out = s
	for _, p := range secretShapes {
		if !p.re.MatchString(out) {
			continue
		}
		kinds = append(kinds, p.kind)
		switch p.kind {
		case "bearer token":
			out = p.re.ReplaceAllString(out, "${1} "+secretMark)
		case "password in a URL":
			out = p.re.ReplaceAllString(out, "${1}"+secretMark+"@")
		default:
			out = p.re.ReplaceAllString(out, secretMark)
		}
	}
	return out, kinds
}

// guardSecrets wraps the Run of every tool so that its result and its error text are scrubbed. The user is
// warned through warn (nil: no warning); the model is told that something was hidden.
func guardSecrets(tools []agent.Tool, warn func(string)) []agent.Tool {
	for i := range tools {
		run, name := tools[i].Run, tools[i].Name
		tools[i].Run = func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			res, err := run(ctx, input, d)
			var kinds []string
			res, kinds = scrubTokens(res)
			if err != nil {
				msg, k := scrubTokens(err.Error())
				if len(k) > 0 {
					err, kinds = fmt.Errorf("%s", msg), append(kinds, k...)
				}
			}
			if len(kinds) > 0 {
				note := fmt.Sprintf("Labmill hid text that looks like a secret (%s) in the result of %s; it was not sent to Claude. Do not ask for it again.", joinKinds(kinds), name)
				if warn != nil {
					warn(note)
				}
				if err == nil {
					res += "\n[" + note + "]"
				}
			}
			return res, err
		}
	}
	return tools
}

func joinKinds(k []string) string {
	seen := map[string]bool{}
	out := ""
	for _, s := range k {
		if seen[s] {
			continue
		}
		seen[s] = true
		if out != "" {
			out += ", "
		}
		out += s
	}
	return out
}
