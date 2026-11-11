package testcase

import (
	"strings"
	"testing"
	"testing/fstest"
)

const okYAML = `id: a-b
title: T
tags: [python]
prompt: do it
expect: {outcome: completed}
`

func TestParse(t *testing.T) {
	c, err := Parse("a-b", []byte(okYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeBudget != DefaultTimeBudget || c.Group() != "python" {
		t.Errorf("defaults: %+v", c)
	}
	bad := map[string]string{
		"unknown key":      okYAML + "colour: red\n",
		"id differs":       strings.Replace(okYAML, "id: a-b", "id: other", 1),
		"bad id":           strings.Replace(okYAML, "id: a-b", "id: A_B", 1),
		"no prompt":        strings.Replace(okYAML, "prompt: do it", "prompt: ''", 1),
		"budget low":       okYAML + "time_budget: 5\n",
		"budget zero":      okYAML + "time_budget: 0\n",
		"budget high":      okYAML + "time_budget: 901\n",
		"class on success": strings.Replace(okYAML, "completed}", "completed, failure_class: timeout}", 1),
		"failed no class":  strings.Replace(okYAML, "completed}", "failed, agent_should: explain, agent_keywords: [x]}", 1),
		"bad class":        strings.Replace(okYAML, "completed}", "failed, failure_class: boom, agent_should: explain, agent_keywords: [x]}", 1),
		"bad should":       strings.Replace(okYAML, "completed}", "failed, failure_class: timeout, agent_should: shrug}", 1),
		"explain no words": strings.Replace(okYAML, "completed}", "failed, failure_class: timeout, agent_should: explain}", 1),
		"bad outcome":      strings.Replace(okYAML, "completed}", "maybe}", 1),
	}
	for name, y := range bad {
		if _, err := Parse("a-b", []byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	failed := strings.Replace(okYAML, "completed}", "failed, failure_class: timeout, agent_should: fix_and_resubmit}", 1)
	if _, err := Parse("a-b", []byte(failed)); err != nil {
		t.Errorf("valid failed case: %v", err)
	}
}

func TestLoad(t *testing.T) {
	fsys := fstest.MapFS{
		"ok/case.yaml":       {Data: []byte(strings.Replace(okYAML, "a-b", "ok", 1))},
		"ok/gen.sh":          {Data: []byte("#!/bin/sh\n")},
		"ok/check.sh":        {Data: []byte("#!/bin/sh\ntrue\n")},
		"nogen/case.yaml":    {Data: []byte(strings.Replace(okYAML, "a-b", "nogen", 1))},
		"nogen/check.sh":     {Data: []byte("x")},
		"emptychk/case.yaml": {Data: []byte(strings.Replace(okYAML, "a-b", "emptychk", 1))},
		"emptychk/gen.sh":    {Data: []byte("x")},
		"emptychk/check.sh":  {Data: []byte("\n")},
		"stray.txt":          {Data: []byte("x")},
	}
	cases, errs := Load(fsys, ".")
	if len(cases) != 1 || cases[0].ID != "ok" {
		t.Errorf("cases: %+v", cases)
	}
	if len(errs) != 2 {
		t.Errorf("errors: %v", errs)
	}
}

func TestFilter(t *testing.T) {
	cs := []Case{
		{ID: "py-a", Tags: []string{"python", "io"}},
		{ID: "py-b", Tags: []string{"python"}},
		{ID: "fail-a", Tags: []string{"failure"}},
	}
	for _, tc := range []struct {
		glob string
		tags []string
		want int
	}{{"", nil, 3}, {"py-*", nil, 2}, {"", []string{"python", "io"}, 1}, {"fail-*", []string{"python"}, 0}} {
		got, err := Filter(cs, tc.glob, tc.tags)
		if err != nil || len(got) != tc.want {
			t.Errorf("%q %v: %d, %v", tc.glob, tc.tags, len(got), err)
		}
	}
	if _, err := Filter(cs, "[", nil); err == nil {
		t.Error("bad glob accepted")
	}
}
