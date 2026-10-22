package launch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/app"
)

// M60.1: the transfer check of the automatic download never logs in; it
// tells "no sign-in" from "connections busy".
func TestBackgroundTransferCheck(t *testing.T) {
	old := connectCached
	t.Cleanup(func() { connectCached = old })
	connectCached = func(context.Context, string) (*ssh.Client, error) { return nil, errors.New("no login") }

	ctx := context.Background()
	t.Run("no login", func(t *testing.T) {
		cc := newConnCache(nil)
		if err := cc.backgroundTransferCheck(ctx, "files", time.Second); !errors.Is(err, app.ErrSignInNeeded) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("denied", func(t *testing.T) {
		cc := newConnCache(nil)
		cc.markDenied("files")
		err := cc.backgroundTransferCheck(ctx, "files", time.Second)
		if !errors.Is(err, app.ErrSignInNeeded) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("busy is not no login", func(t *testing.T) {
		cc := newConnCache(nil)
		cc.mu.Lock() // a login dialog holds the cache
		defer cc.mu.Unlock()
		start := time.Now()
		err := cc.backgroundTransferCheck(ctx, "files", 250*time.Millisecond)
		if !errors.Is(err, app.ErrTryLater) || errors.Is(err, app.ErrSignInNeeded) {
			t.Errorf("err = %v", err)
		}
		if time.Since(start) < 200*time.Millisecond {
			t.Errorf("gave up after %v without waiting", time.Since(start))
		}
	})
	t.Run("busy then free", func(t *testing.T) {
		cc := newConnCache(nil)
		cc.mu.Lock()
		go func() { time.Sleep(150 * time.Millisecond); cc.mu.Unlock() }()
		err := cc.backgroundTransferCheck(ctx, "files", 5*time.Second)
		if !errors.Is(err, app.ErrSignInNeeded) { // free now, and there is no login
			t.Errorf("err = %v", err)
		}
	})
	t.Run("canceled while busy", func(t *testing.T) {
		cc := newConnCache(nil)
		cc.mu.Lock()
		defer cc.mu.Unlock()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := cc.backgroundTransferCheck(cctx, "files", time.Minute); !errors.Is(err, app.ErrTryLater) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestFreeSpace(t *testing.T) {
	n, err := freeSpace(t.TempDir())
	if err != nil || n <= 0 {
		t.Errorf("freeSpace = %d, %v", n, err)
	}
}

// A postponed download is retried at a later check, once, and jobEnded (from
// job_status) starts the same download.
func TestMonitorRetriesPostponedAndJobEnded(t *testing.T) {
	f := newMonitorFixture(t)
	var mu sync.Mutex
	var calls []int64
	pending := []int64{7}
	f.m.autoOn = func() bool { return true }
	f.m.autoPending = func(context.Context) []int64 { mu.Lock(); defer mu.Unlock(); return pending }
	f.m.autoFetch = func(_ context.Context, id int64) app.AutoFetchResult {
		mu.Lock()
		calls = append(calls, id)
		mu.Unlock()
		return app.AutoFetchResult{Retry: true} // postponed again: silent
	}
	f.m.check(context.Background())
	f.m.fetching.Wait()
	f.m.jobEnded(9)
	f.m.fetching.Wait()
	if len(calls) != 2 || calls[0] != 7 || calls[1] != 9 || len(f.notices.all()) != 0 {
		t.Errorf("calls = %v, notices = %q", calls, f.notices.all())
	}
	f.m.autoOn = func() bool { return false }
	f.m.check(context.Background())
	f.m.jobEnded(9)
	f.m.fetching.Wait()
	if len(calls) != 2 {
		t.Errorf("setting off but calls = %v", calls)
	}
}
