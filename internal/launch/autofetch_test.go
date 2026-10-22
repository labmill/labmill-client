package launch

import (
	"context"
	"sync"
	"testing"

	"labmill-client/internal/app"
	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// M60.1: the monitor hands a job that completed or failed to the automatic
// download when the setting is on, never a canceled one, and only once.
func TestMonitorAutoDownload(t *testing.T) {
	cases := []struct {
		name     string
		on       bool
		final    backend.Status
		dir      string
		wantCall bool
		wantFor  string // first notice
	}{
		{"completed, on", true, backend.Status{State: store.StateCompleted}, "/home/u/x", true, "Job #1 (train) on hpcone finished. Downloading its results automatically."},
		{"failed, on", true, backend.Status{State: store.StateFailed, Reason: "TIMEOUT"}, "/home/u/x", true, "Job #1 (train) on hpcone failed (TIMEOUT). Ask me to look at the logs."},
		{"canceled, on", true, backend.Status{State: store.StateCanceled}, "/home/u/x", false, "Job #1 (train) on hpcone was canceled."},
		{"completed, off", false, backend.Status{State: store.StateCompleted}, "/home/u/x", false, "Job #1 (train) on hpcone finished. Ask me to fetch the results."},
		{"completed, no folder", true, backend.Status{State: store.StateCompleted}, "", false, "Job #1 (train) on hpcone finished. Ask me to show its log."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMonitorFixture(t)
			var mu sync.Mutex
			var calls []int64
			var fetchedID int64
			f.m.autoOn = func() bool { return c.on }
			f.m.autoFetch = func(_ context.Context, id int64) app.AutoFetchResult {
				mu.Lock()
				calls = append(calls, id)
				mu.Unlock()
				return app.AutoFetchResult{Text: "downloaded it", Downloaded: true}
			}
			f.m.fetched = func(id int64, text string) { fetchedID = id; f.notices.notify(text) }
			id := f.job(t, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "100", State: store.StateRunning, RemoteDir: c.dir})
			f.be.set("100", c.final)
			ctx := context.Background()
			f.m.check(ctx)
			f.m.check(ctx) // a later check must not start it again
			f.m.fetching.Wait()
			got := f.notices.all()
			if len(got) == 0 || got[0] != c.wantFor {
				t.Fatalf("notices = %q, want first %q", got, c.wantFor)
			}
			if c.wantCall {
				if len(calls) != 1 || calls[0] != id || fetchedID != id || len(got) != 2 || got[1] != "downloaded it" {
					t.Errorf("calls=%v fetched=%d notices=%q", calls, fetchedID, got)
				}
			} else if len(calls) != 0 || len(got) != 1 {
				t.Errorf("calls=%v notices=%q", calls, got)
			}
		})
	}
}

// A skipped or failed automatic download is a plain notice (no Open folder).
func TestMonitorAutoDownloadSkippedIsPlainNotice(t *testing.T) {
	f := newMonitorFixture(t)
	f.m.autoOn = func() bool { return true }
	f.m.autoFetch = func(context.Context, int64) app.AutoFetchResult {
		return app.AutoFetchResult{Text: "too big"}
	}
	f.m.fetched = func(int64, string) { t.Error("fetched hook used for a skipped download") }
	f.job(t, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "100", State: store.StateRunning, RemoteDir: "/h/x"})
	f.be.set("100", backend.Status{State: store.StateCompleted})
	f.m.check(context.Background())
	f.m.fetching.Wait()
	if got := f.notices.all(); len(got) != 2 || got[1] != "too big" {
		t.Errorf("notices = %q", got)
	}
}
