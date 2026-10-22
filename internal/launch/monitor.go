package launch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"labmill-client/internal/app"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
	"labmill-client/internal/textsafe"
)

// Background job monitor timing.
const (
	monitorInterval    = 45 * time.Second // pause between two checks
	monitorCallTimeout = 20 * time.Second // per Status call

	// A job whose checks fail (the scheduler no longer knows it, an expired
	// login) this many times in a row is marked unknownMessage and checked
	// only every monitorBackoff from then on.
	monitorMaxFailures = 5
	monitorBackoff     = 10 * time.Minute
	unknownMessage     = "unknown \u2014 check with job_status"
)

// monitoredStates are the states the monitor asks the scheduler about: the
// job was handed over and hasn't finished yet.
var monitoredStates = []store.JobState{store.StateSubmitted, store.StatePending, store.StateRunning}

func monitored(s store.JobState) bool {
	for _, m := range monitoredStates {
		if s == m {
			return true
		}
	}
	return false
}

// jobMonitor watches this store's submitted, unfinished jobs in the
// background so the chat never has to wait for them. Every interval it asks
// each job's backend for its status (at most one Status call per job per
// check, each bounded by timeout), saves a changed state, and tells the UI
// once when a job finishes, fails or is canceled. Checks never overlap: the
// next pause starts when a check ends.
//
// It never logs in: backend returns false unless the cluster can be reached
// without a login or a question (launch uses connCache.backendNoLogin), and
// such clusters are simply skipped until a later check.
type jobMonitor struct {
	store   *store.Store
	cfg     *config.Config
	backend func(ctx context.Context, c config.Cluster) (backend.Backend, bool)
	notify  func(text string)
	// release, when set, runs after every check: it closes the helper
	// connections the check's backends used, so the monitor holds none
	// between checks.
	release func()
	// autoOn, autoFetch and fetched are the automatic download of results
	// (M60.1). When autoOn reports the setting is on, a job that completes or
	// fails is handed to autoFetch (in its own goroutine, so the checks go
	// on); a downloaded job's sentence goes to fetched (with the job's ID, so
	// the GUI can offer "Open folder"), any other to notify. Any of them nil:
	// no automatic download.
	autoOn    func() bool
	autoFetch func(ctx context.Context, jobID int64) app.AutoFetchResult
	// autoPending lists finished jobs whose automatic download was postponed
	// (no sign-in yet, busy connections, Labmill was closing); every check
	// tries them again while the setting is on.
	autoPending func(ctx context.Context) []int64
	fetched     func(jobID int64, text string)

	// resubmit, when set, gets every monitored job whose status is failed,
	// pending or running (M61.3: a failed one may be a preemption to
	// resubmit; a pending or running one a requeue loop to move to the
	// fallback). true means it took the job over (resubmitted it, will try
	// again at the next check, or the job moved on): the status is not
	// recorded, nothing is announced as finished and no automatic download
	// starts. It runs under a timeout of a few status calls; its submit and
	// save are not cancelled by it.
	resubmit func(ctx context.Context, j store.Job, be backend.Backend, st backend.Status) bool

	// ended, when set, gets the text of a desktop notification when a job
	// ends by itself (M64.3: completed, failed, timed out, canceled by the
	// scheduler). A job about to be resubmitted never reaches it.
	ended func(text string)
	// awaySince, when not zero, makes the monitor check once at start and
	// then tell the UI in one sentence which jobs completed or failed since
	// that time ("while you were away", M64.3). The notices of that first
	// check are held back for it. alive saves "Labmill is running now"; it
	// runs after every check and at Close.
	awaySince time.Time
	alive     func()
	away      bool        // the first check is running; only the monitor goroutine uses it
	awayEnded []awayEntry // jobs the first check saw end

	interval, timeout time.Duration // 0 means monitorInterval / monitorCallTimeout

	// now is the clock for the back-off; nil means time.Now (tests inject one).
	now func() time.Time
	// failing tracks jobs whose checks keep failing, by job ID. Only the
	// monitor goroutine (check) touches it.
	failing map[int64]*jobFailures

	fetching sync.WaitGroup // running automatic downloads
	fetchMu  sync.Mutex
	inflight map[int64]bool // jobs with an automatic download running
	ctx      context.Context
	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan struct{}
}

// jobFailures counts one job's consecutive failed checks.
type jobFailures struct {
	n    int
	next time.Time // no check before this once n >= monitorMaxFailures
}

func (m *jobMonitor) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// start runs the monitor in a goroutine until Close. after returns the
// channel that ends a pause (nil means time.After); tests pass a fake clock.
func (m *jobMonitor) start(after func(time.Duration) <-chan time.Time) {
	if after == nil {
		after = time.After
	}
	interval := m.interval
	if interval <= 0 {
		interval = monitorInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.ctx = ctx
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		if !m.awaySince.IsZero() {
			m.away = true
			m.check(ctx)
			m.away = false
			m.tellAway()
			if m.release != nil {
				m.release()
			}
			if m.alive != nil {
				m.alive()
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-after(interval):
			}
			m.check(ctx)
			if m.release != nil {
				m.release()
			}
			if m.alive != nil {
				m.alive()
			}
		}
	}()
}

// Close stops the monitor and waits for a running check to end (its calls
// are canceled). Safe to call more than once, and without start.
func (m *jobMonitor) Close() {
	m.stopOnce.Do(func() {
		if m.cancel == nil {
			return
		}
		m.cancel()
		<-m.done
		m.fetching.Wait()
		if m.alive != nil {
			m.alive()
		}
	})
}

// errUnchanged aborts an UpdateJob whose job changed meanwhile (a tool or
// another Labmill process saved a newer state), so nothing is written.
var errUnchanged = errors.New("job changed meanwhile")

// check runs one pass over the monitored jobs.
func (m *jobMonitor) check(ctx context.Context) {
	if m.store == nil || m.cfg == nil || m.backend == nil {
		return
	}
	defer m.retryAutoFetch(ctx)
	jobs, err := m.store.Jobs(ctx, store.JobFilter{States: monitoredStates})
	if err != nil {
		return
	}
	timeout := m.timeout
	if timeout <= 0 {
		timeout = monitorCallTimeout
	}
	live := make(map[int64]bool, len(jobs))
	for _, j := range jobs {
		live[j.ID] = true
	}
	for id := range m.failing {
		if !live[id] {
			delete(m.failing, id) // job finished or removed: stop tracking it
		}
	}
	type beResult struct {
		be backend.Backend
		ok bool
	}
	backends := map[string]beResult{} // per cluster, looked up at most once per check
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if j.RemoteID == "" {
			continue
		}
		c, ok := m.cfg.Cluster(j.Cluster)
		if !ok {
			continue
		}
		r, seen := backends[c.Name]
		if !seen {
			r.be, r.ok = m.backendWithin(ctx, c, timeout)
			backends[c.Name] = r
		}
		if !r.ok {
			continue
		}
		if f := m.failing[j.ID]; f != nil && f.n >= monitorMaxFailures && m.clock().Before(f.next) {
			continue // backing off
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		st, err := r.be.Status(cctx, j.RemoteID)
		cancel()
		if err != nil || !st.State.Valid() {
			if ctx.Err() == nil {
				m.failed(ctx, j, err)
			}
			continue
		}
		delete(m.failing, j.ID)
		if m.resubmit != nil {
			rctx, rcancel := context.WithTimeout(ctx, 4*timeout)
			handled := m.resubmit(rctx, j, r.be, st)
			rcancel()
			if handled {
				continue
			}
		}
		m.apply(ctx, j, st)
	}
}

// failed records a failed check of j. The monitorMaxFailures-th failure in a
// row marks the job unknownMessage (its state stays as it was, so a later
// good answer still notifies); from then on it is checked only every
// monitorBackoff.
func (m *jobMonitor) failed(ctx context.Context, j store.Job, cause error) {
	if m.failing == nil {
		m.failing = map[int64]*jobFailures{}
	}
	f := m.failing[j.ID]
	if f == nil {
		f = &jobFailures{}
		m.failing[j.ID] = f
	}
	f.n++
	if f.n < monitorMaxFailures {
		return
	}
	f.next = m.clock().Add(monitorBackoff)
	if f.n > monitorMaxFailures {
		return
	}
	msg := unknownMessage
	_ = m.store.UpdateJob(ctx, j.ID, func(cur *store.Job) error {
		if !monitored(cur.State) || cur.RemoteID != j.RemoteID || cur.Message == msg {
			return errUnchanged
		}
		cur.Message = msg
		return nil
	})
}

// backendWithin looks up a backend with a deadline (route checks may probe
// the network).
func (m *jobMonitor) backendWithin(ctx context.Context, c config.Cluster, timeout time.Duration) (backend.Backend, bool) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return m.backend(cctx, c)
}

// apply saves st for j when it changed, and notifies when the job reached a
// finished state in this save.
func (m *jobMonitor) apply(ctx context.Context, j store.Job, st backend.Status) {
	var from store.JobState
	var fromMsg string
	err := m.store.UpdateJob(ctx, j.ID, func(cur *store.Job) error {
		from, fromMsg = cur.State, cur.Message
		if !monitored(cur.State) || cur.RemoteID != j.RemoteID {
			return errUnchanged
		}
		if cur.State == st.State && cur.Message == st.Reason {
			return errUnchanged
		}
		cur.State, cur.Message = st.State, st.Reason
		return nil
	})
	if err != nil {
		return
	}
	if m.away && from != st.State && (st.State == store.StateCompleted || st.State == store.StateFailed) {
		m.awayEnded = append(m.awayEnded, awayEntry{j.ID, j.Name, st.State})
	} else if m.ended != nil && from != st.State {
		if text, ok := endedText(j.Name, st.State, st.Reason); ok {
			m.ended(text)
		}
	}
	if m.notify == nil {
		return
	}
	if text := retryNotice(j, fromMsg, st); text != "" {
		m.notify(text)
	}
	if from == st.State {
		return
	}
	auto := m.autoDownload(j, st)
	if text := finishedNotice(j, st); text != "" && !m.away {
		if auto && st.State == store.StateCompleted {
			text = fmt.Sprintf("Job #%d (%s) on %s finished. Downloading its results automatically.", j.ID, j.Name, j.Cluster)
		}
		m.notify(text)
	}
	if auto {
		m.startAutoFetch(ctx, j.ID)
	}
}

// startAutoFetch downloads job id's results in the background (at most one
// download per job at a time) and tells the UI what happened. A postponed
// attempt says nothing here; retryAutoFetch tries it again at a later check.
func (m *jobMonitor) startAutoFetch(ctx context.Context, id int64) {
	m.fetchMu.Lock()
	if m.inflight[id] {
		m.fetchMu.Unlock()
		return
	}
	if m.inflight == nil {
		m.inflight = map[int64]bool{}
	}
	m.inflight[id] = true
	m.fetching.Add(1)
	m.fetchMu.Unlock()
	go func() {
		defer m.fetching.Done()
		defer func() {
			m.fetchMu.Lock()
			delete(m.inflight, id)
			m.fetchMu.Unlock()
		}()
		r := m.autoFetch(ctx, id)
		switch {
		case r.Text == "":
		case r.Downloaded && m.fetched != nil:
			m.fetched(id, r.Text)
		default:
			if m.notify != nil {
				m.notify(r.Text)
			}
		}
	}()
}

// jobEnded is called when job_status saw job id complete or fail before this
// monitor did: the same automatic download, if the setting is on.
func (m *jobMonitor) jobEnded(id int64) {
	if m.ended != nil && m.store != nil {
		if j, err := m.store.Job(context.Background(), id); err == nil {
			if text, ok := endedText(j.Name, j.State, j.Message); ok {
				m.ended(text)
			}
		}
	}
	if m.autoOn == nil || m.autoFetch == nil || !m.autoOn() {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	m.startAutoFetch(ctx, id)
}

// retryAutoFetch starts the postponed automatic downloads (setting on).
func (m *jobMonitor) retryAutoFetch(ctx context.Context) {
	if m.autoPending == nil || m.autoFetch == nil || m.autoOn == nil || !m.autoOn() || ctx.Err() != nil {
		return
	}
	for _, id := range m.autoPending(ctx) {
		m.startAutoFetch(ctx, id)
	}
}

// autoDownload says whether a job that just reached st gets its results
// downloaded by itself: the setting is on, the job finished by itself
// (completed or failed; a canceled one is left alone) and has a folder on a
// cluster.
func (m *jobMonitor) autoDownload(j store.Job, st backend.Status) bool {
	if m.autoOn == nil || m.autoFetch == nil || j.RemoteDir == "" || !m.autoOn() {
		return false
	}
	return st.State == store.StateCompleted || st.State == store.StateFailed
}

// retryNotice is the sentence for a job whose container just started
// failing while the scheduler retries it (the backend's reason ends in
// "retrying"); "" when the previous message already said so or
// the job isn't retrying.
func retryNotice(j store.Job, fromMsg string, st backend.Status) string {
	if !strings.HasSuffix(st.Reason, "retrying") || strings.HasSuffix(fromMsg, "retrying") || st.State.Terminal() {
		return ""
	}
	return fmt.Sprintf("Job #%d (%s) on %s: its container %s. It will probably fail again; ask me to look at the logs, or to cancel it.", j.ID, j.Name, j.Cluster, st.Reason)
}

// finishedNotice is the sentence for a job that reached a finished state
// ("" for any other state).
func finishedNotice(j store.Job, st backend.Status) string {
	what := fmt.Sprintf("Job #%d (%s) on %s", j.ID, j.Name, j.Cluster)
	switch st.State {
	case store.StateCompleted:
		if j.RemoteDir != "" {
			return what + " finished. Ask me to fetch the results."
		}
		return what + " finished. Ask me to show its log."
	case store.StateFailed:
		if st.Reason != "" {
			return fmt.Sprintf("%s failed (%s). Ask me to look at the logs.", what, st.Reason)
		}
		return what + " failed. Ask me to look at the logs."
	case store.StateCanceled:
		return what + " was canceled."
	}
	return ""
}

// endedText is the desktop notification for a job that ended by itself
// ("<job name> completed"); false for any other state. The reason tells a
// time limit from another failure.
func endedText(name string, s store.JobState, reason string) (string, bool) {
	switch s {
	case store.StateCompleted:
		return name + " completed", true
	case store.StateFailed:
		if strings.Contains(strings.ToLower(reason), "timeout") {
			return name + " timed out", true
		}
		return name + " failed", true
	case store.StateCanceled:
		return name + " was canceled", true
	}
	return "", false
}

// maxAway is how many jobs the "while you were away" sentence names.
const maxAway = 10

// awayEntry is one job the first check saw complete or fail.
type awayEntry struct {
	id    int64
	name  string
	state store.JobState
}

// tellAway sends the "while you were away" sentence for the jobs the first
// check saw end, and one desktop notification with the same text; nothing
// when there are none.
func (m *jobMonitor) tellAway() {
	text := awaySummary(m.awayEnded)
	m.awayEnded = nil
	if text == "" {
		return
	}
	if m.notify != nil {
		m.notify(text)
	}
	if m.ended != nil {
		m.ended(text)
	}
}

// awaySummary lists jobs: "While you were away: job 12 (seg) completed,
// job 13 (train) failed." "" when there are none.
func awaySummary(jobs []awayEntry) string {
	var parts []string
	for _, j := range slices.SortedFunc(slices.Values(jobs), func(a, b awayEntry) int { return cmp.Compare(a.id, b.id) }) {
		parts = append(parts, fmt.Sprintf("job %d (%s) %s", j.id, textsafe.Clean(j.name), j.state))
	}
	if len(parts) == 0 {
		return ""
	}
	more := ""
	if len(parts) > maxAway {
		more = fmt.Sprintf(", and %d more", len(parts)-maxAway)
		parts = parts[:maxAway]
	}
	return "While you were away: " + strings.Join(parts, ", ") + more + "."
}
