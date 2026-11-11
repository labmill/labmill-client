package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
)

const browseSample = "@@PWD\n/data/mygroup\n@@FREE\n123456\n@@WRITE\nyes\n@@DIRS\nrun1/\nrun\x1b[31m2/\n"

func TestBrowseRemote(t *testing.T) {
	h := newHarness(t)
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: browseSample}, nil
	}}
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) { return fr, nil }
	out, err := h.run(t, "browse_remote", map[string]any{"cluster": "hpcone", "path": "/data/mygroup"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Folder /data/mygroup", "can write here", "123456 KiB", "- run1", "- run?[31m2"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(fr.calls) != 1 || fr.calls[0][4] != "/data/mygroup" || strings.Contains(fr.calls[0][2], "rm ") {
		t.Errorf("one read-only call with the path as an argument: %v", fr.calls)
	}
}

func TestBrowseRemoteBadInput(t *testing.T) {
	h := newHarness(t)
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) {
		return &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
			return backend.Result{ExitCode: 3}, nil
		}}, nil
	}
	if _, err := h.run(t, "browse_remote", map[string]any{"cluster": "hpcone", "path": "relative/x"}); err == nil {
		t.Error("a relative path must be refused")
	}
	if _, err := h.run(t, "browse_remote", map[string]any{"cluster": "hpcone", "path": "/a\nb"}); err == nil {
		t.Error("a newline in the path must be refused")
	}
	if _, err := h.run(t, "browse_remote", map[string]any{"cluster": "hpcone", "path": "/nope"}); err == nil || !strings.Contains(err.Error(), "can't be opened") {
		t.Errorf("a folder that can't be entered: %v", err)
	}
	h.env.LoginRunner = nil
	if _, err := h.run(t, "browse_remote", map[string]any{"cluster": "hpcone"}); err == nil {
		t.Error("no runner must fail")
	}
}
