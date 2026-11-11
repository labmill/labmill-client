package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/agent"
)

func TestScrubTokens(t *testing.T) {
	cases := map[string]string{
		"key AKIAIOSFODNN7EXAMPLE here":                                               "AWS access key",
		"export T=ghp_" + strings.Repeat("a1B2", 9):                                   "GitHub token",
		"ANTHROPIC=sk-ant-" + strings.Repeat("x9", 15):                                "API key",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----": "private key",
		"-----BEGIN RSA PRIVATE KEY-----\nabc (cut)":                                  "private key",
		"curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwx'":                    "bearer token",
		"git clone https://bob:hunter2pass@git.example.edu/x":                         "password in a URL",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk":                "JSON web token",
	}
	for in, kind := range cases {
		out, kinds := scrubTokens(in)
		if len(kinds) == 0 || kinds[0] != kind || !strings.Contains(out, secretMark) {
			t.Errorf("%q: kinds %v out %q", in, kinds, out)
		}
		for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "hunter2pass", "abcdefghijklmnopqrstuvwx", "ghp_"} {
			if strings.Contains(out, leak) {
				t.Errorf("%q still has %q: %q", in, leak, out)
			}
		}
	}
	for _, ok := range []string{"sha256 3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a", "https://example.edu/x", "ssh://git@example.edu/repo", "basic usage of the tool"} {
		if out, kinds := scrubTokens(ok); out != ok || len(kinds) != 0 {
			t.Errorf("%q must stay: %q %v", ok, out, kinds)
		}
	}
	if out, _ := scrubTokens("https://bob:hunter2pass@git.example.edu/x"); out != "https://bob:«secret hidden»@git.example.edu/x" {
		t.Errorf("url: %q", out)
	}
}

func TestGuardSecrets(t *testing.T) {
	var warned []string
	tools := guardSecrets([]agent.Tool{
		{Name: "a", Run: func(context.Context, json.RawMessage, agent.Decision) (string, error) {
			return "log: AKIAIOSFODNN7EXAMPLE end", nil
		}},
		{Name: "b", Run: func(context.Context, json.RawMessage, agent.Decision) (string, error) {
			return "", errors.New("failed with token ghp_" + strings.Repeat("a1B2", 9))
		}},
		{Name: "c", Run: func(context.Context, json.RawMessage, agent.Decision) (string, error) { return "fine", nil }},
	}, func(s string) { warned = append(warned, s) })
	res, err := tools[0].Run(context.Background(), nil, agent.Decision{})
	if err != nil || strings.Contains(res, "AKIA") || !strings.Contains(res, "hid text") {
		t.Errorf("a: %q %v", res, err)
	}
	if _, err := tools[1].Run(context.Background(), nil, agent.Decision{}); err == nil || strings.Contains(err.Error(), "ghp_") {
		t.Errorf("b: %v", err)
	}
	if res, _ := tools[2].Run(context.Background(), nil, agent.Decision{}); res != "fine" {
		t.Errorf("c: %q", res)
	}
	if len(warned) != 2 {
		t.Errorf("warnings: %v", warned)
	}
}
