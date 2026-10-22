package launch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshx/sshtest"
)

// TestMonitorReleasesAfterEachCheck: the release hook runs after every check,
// before the next pause begins.
func TestMonitorReleasesAfterEachCheck(t *testing.T) {
	f := newMonitorFixture(t)
	var released atomic.Int32
	f.m.release = func() { released.Add(1) }
	clock := &fakeTicks{pauses: make(chan chan time.Time, 4)}
	f.m.start(clock.after)
	defer f.m.Close()
	tick := <-clock.pauses
	for i := 1; i <= 2; i++ {
		tick <- time.Now()
		tick = <-clock.pauses // the next pause starts only after the check and its release
		if got := released.Load(); got != int32(i) {
			t.Fatalf("after check %d: released %d times", i, got)
		}
	}
}

// TestReleaseBackgroundClosesOnlyPickedUpClients: a connection taken from the
// helper for a check is closed by releaseBackground (so the helper can idle
// out); the interactive client stays open; the next check picks up a new one.
func TestReleaseBackgroundClosesOnlyPickedUpClients(t *testing.T) {
	oldCached := connectCached
	t.Cleanup(func() { connectCached = oldCached })
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw"})
	var picked []*ssh.Client
	connectCached = func(context.Context, string) (*ssh.Client, error) {
		cl := dialTest(t, srv)
		picked = append(picked, cl)
		return cl, nil
	}
	cc := newConnCache(&promptUI{t: t})
	ctx := context.Background()

	cl, ok := cc.existingClient(ctx, "hpclogin")
	if !ok || cl == nil || len(picked) != 1 {
		t.Fatalf("pick-up: ok=%v picked=%d", ok, len(picked))
	}
	// Within one check the same connection is reused.
	if cl2, ok := cc.existingClient(ctx, "hpclogin"); !ok || cl2 != cl || len(picked) != 1 {
		t.Fatalf("second lookup in the same check opened another connection (picked=%d)", len(picked))
	}
	if cc.clients["hpclogin"] != nil {
		t.Fatal("a monitor-held client went into the interactive cache")
	}
	cc.releaseBackground()
	waited := make(chan struct{})
	go func() { cl.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("releaseBackground left the helper connection open")
	}
	// The next check connects again.
	if _, ok := cc.existingClient(ctx, "hpclogin"); !ok || len(picked) != 2 {
		t.Fatalf("after release: picked=%d", len(picked))
	}

	// An interactive client is used as is and survives a release.
	cc2 := newConnCache(&promptUI{t: t})
	inter := dialTest(t, srv)
	cc2.cacheLocked("hpclogin", inter)
	if got, ok := cc2.existingClient(ctx, "hpclogin"); !ok || got != inter {
		t.Fatal("the interactive client was not reused")
	}
	cc2.releaseBackground()
	if _, _, err := inter.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("the interactive client was closed: %v", err)
	}
}
