package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
)

const quotaSample = "@@PWD\n/home/alice\n@@DF\nFilesystem Type 1024-blocks Used Available Capacity Mounted on\nhome nfs 20971520 20000000 971520 96% /home\n@@LFS\n@@QUOTA\nDisk quotas for user alice\n /home 19G 20G 20G\n"

func runQuota(t *testing.T, need int) (string, *fakeRunner) {
	t.Helper()
	h := newHarness(t)
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: quotaSample}, nil
	}}
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) { return fr, nil }
	out, err := h.run(t, "check_quota", map[string]any{"cluster": "hpcone", "need_gb": need})
	if err != nil {
		t.Fatal(err)
	}
	return out, fr
}

func TestCheckQuotaLow(t *testing.T) {
	out, fr := runQuota(t, 5)
	for _, want := range []string{"free by df: 0 GB", "LOW", "quota output", "19G 20G"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(fr.calls) != 1 || strings.Contains(fr.calls[0][2], "rm ") {
		t.Errorf("one read-only call: %v", fr.calls)
	}
}

func TestCheckQuotaFits(t *testing.T) {
	if out, _ := runQuota(t, 0); !strings.Contains(out, "no size given") {
		t.Errorf("zero need:\n%s", out)
	}
	if quotaVerdict(50, 10) != "fits" || !strings.HasPrefix(quotaVerdict(-1, 10), "unknown") {
		t.Error("verdict")
	}
}

func TestCheckQuotaBadPath(t *testing.T) {
	h := newHarness(t)
	if _, err := h.run(t, "check_quota", map[string]any{"cluster": "hpcone", "path": "rel", "need_gb": 1}); err == nil {
		t.Error("relative path must fail")
	}
}
