//go:build desktop

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/config"
	"labmill-client/internal/launch"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
)

// The timing mode must never reach the developer's real SSH helper: with the
// timing environment applied, the helper socket lies inside the timing
// runtime folder, which is not the real one.
func TestTimingEnvKeepsHelperSocketInside(t *testing.T) {
	base := t.TempDir()
	real, err := sshx.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range timingEnv(base) {
		t.Setenv(k, v)
	}
	sock, err := sshx.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	dir := timingRuntimeDir(base)
	if rel, err := filepath.Rel(dir, sock); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("helper socket %q is outside the timing runtime folder %q", sock, dir)
	}
	if sock == real {
		t.Errorf("timing helper socket is the real one: %q", sock)
	}
}

// A deep timing home (a worktree) must not push the socket path over the
// 108-byte AF_UNIX limit.
func TestTimingSocketPathStaysShort(t *testing.T) {
	deep := "/home/user/gh/project/.claude/worktrees/agent-0123456789abcdef/bin/labmill-selftest-timing-home-example.edu"
	t.Setenv("XDG_RUNTIME_DIR", timingEnv(deep)["XDG_RUNTIME_DIR"])
	sock, err := sshx.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if len(sock) >= 100 {
		t.Errorf("socket path %q is %d bytes", sock, len(sock))
	}
	if timingRuntimeDir(deep) == timingRuntimeDir(deep+"x") {
		t.Error("different timing homes share a runtime folder")
	}
}

// The generic timing setup gets a placeholder login host, so it names a home
// institution (M55) and Build doesn't ask again.
func TestTimingSetupGenericHasHosts(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "clusters.yaml")
	paths := sshconfig.Paths{SSHDir: filepath.Join(dir, ".ssh")}
	if err := writeTimingSetup(paths, cfgPath, "generic"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || cfg.Site != "generic" || len(cfg.Clusters) == 0 {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if launch.NeedsHomeChoice(cfgPath) {
		t.Error("the timing setup still needs a home institution")
	}
}
