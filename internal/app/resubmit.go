package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// M61.3: automatic resubmission of a preempted job. The job monitor (and
// job_status) hand every monitored Slurm job to Resubmitter.Handle. Only a
// job whose placement plan (eventPlan) says "preemptible", i.e. whose
// approved review announced the resubmissions, is ever touched; a job
// drafted before M61.2 has no plan and is left alone.
//
// Exactly once: the old Slurm ID is claimed in the database before sbatch
// runs (store.ClaimResubmit, which re-reads the job), and the new ID and the
// history event are saved in one transaction (store.FinishResubmit). A claim
// a crash left open is never submitted for again: Handle looks the job up by
// name in squeue (read only) and records it, or says so once.

// eventResubmitted records one automatic resubmission (JSON of
// resubmitEvent) on the job, which keeps its Labmill identity while its Slurm
// job ID changes.
const eventResubmitted = "resubmitted"

// resubmitEvent is the text of an eventResubmitted event.
type resubmitEvent struct {
	Attempt   int    `json:"attempt"`   // the preemptible attempt this submit is (0 for the fallback)
	Old       string `json:"old"`       // Slurm job ID that ended
	New       string `json:"new"`       // Slurm job ID submitted
	Partition string `json:"partition"` // "" is the cluster's default
	Fallback  bool   `json:"fallback,omitempty"`
	// Used is how many preemptible attempts had failed when this was
	// submitted, Slurm's own requeues included.
	Used int    `json:"used"`
	Why  string `json:"why,omitempty"`
}

// claimPayload is stored with the claim so that a crashed resubmission can
// be completed from the database alone.
type claimPayload struct {
	Event  resubmitEvent `json:"event"`
	Script string        `json:"script,omitempty"` // the fallback script; "" keeps the approved one
	Msg    string        `json:"msg"`
}

const (
	// maxResubmitFailures is how many failed resubmit tries (sbatch refused,
	// no connection) are made before the job is let stay failed.
	maxResubmitFailures = 3
	// resubmitSaveTimeout bounds the submit and the save, which run even
	// when Labmill is closing so that a submitted job is never left unrecorded.
	resubmitSaveTimeout = 2 * time.Minute
	// claimStale is how old an open claim must be before it counts as left
	// by a crash and not as another Labmill process still working on it.
	claimStale = 10 * time.Minute
	// claimSkew is the clock difference between laptop and cluster allowed
	// when a queued job's submit time is compared with a claim.
	claimSkew = 2 * time.Minute
	// liveCheckEvery is how often a pending or running job of a requeue plan
	// has its Slurm requeue count read.
	liveCheckEvery = 5 * time.Minute
)

// Resubmitter resubmits preempted jobs. Backend lookups and the sign-in are
// the caller's business: Handle gets a backend that is already connected
// (the monitor's never logs in).
type Resubmitter struct {
	Store *store.Store
	// Notify tells the user (the chat); Log writes the same to labmill.log.
	Notify func(text string)
	Log    func(text string)
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	jobMu    map[int64]*sync.Mutex
	failures map[int64]int           // failed tries per job
	told     map[string]bool         // messages said once
	unsaved  map[string]*unsavedSave // submitted but not yet saved, by job ID + old ID
	lastLive map[int64]time.Time
}

// unsavedSave is a submit whose save failed; it is retried, never submitted
// again.
type unsavedSave struct {
	newID, announce string
	p               claimPayload
}

func (r *Resubmitter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resubmitter) say(text string) {
	if r.Notify != nil {
		r.Notify(text)
	}
	if r.Log != nil {
		r.Log(text)
	}
}

func (r *Resubmitter) logf(format string, a ...any) {
	if r.Log != nil {
		r.Log(fmt.Sprintf(format, a...))
	}
}

// sayOnce says text the first time key is seen.
func (r *Resubmitter) sayOnce(key, text string) {
	r.mu.Lock()
	seen := r.told[key]
	if r.told == nil {
		r.told = map[string]bool{}
	}
	r.told[key] = true
	r.mu.Unlock()
	if !seen {
		r.say(text)
	}
}

// lock serializes the handling of one job inside this process.
func (r *Resubmitter) lock(id int64) func() {
	r.mu.Lock()
	if r.jobMu == nil {
		r.jobMu = map[int64]*sync.Mutex{}
	}
	m := r.jobMu[id]
	if m == nil {
		m = &sync.Mutex{}
		r.jobMu[id] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// preemptionCause says why a failed Slurm status counts as lost to
// preemption or a node failure ("" when it doesn't): the final state
// PREEMPTED, NODE_FAIL or BOOT_FAIL, or a requeue loop that ended FAILED
// (Slurm requeued the job, restarts > 0) with exit status 0:0, i.e. neither
// a program exit code nor a signal. Everything else (TIMEOUT,
// OUT_OF_MEMORY, a FAILED job whose program exited non-zero or was killed
// by a signal, CANCELLED) is the job's own business and never resubmitted.
func preemptionCause(st backend.Status, restarts int) string {
	if st.State != store.StateFailed {
		return ""
	}
	switch st.Reason {
	case "PREEMPTED":
		return "was preempted"
	case "NODE_FAIL":
		return "lost its node"
	case "BOOT_FAIL":
		return "lost its node (boot failure)"
	case "FAILED":
		if restarts > 0 && st.ExitCode == 0 && st.Signal == 0 {
			return fmt.Sprintf("was requeued %d time(s) by Slurm and then failed", restarts)
		}
	}
	return ""
}

func monitoredState(s store.JobState) bool {
	return s == store.StateSubmitted || s == store.StatePending || s == store.StateRunning
}

// Handle looks at a monitored job whose scheduler status is st. It returns
// true when it took the job over and the caller must not record st: it
// resubmitted the job, will try again at a later check, or the job moved on
// since the caller looked (another caller resubmitted it, the user canceled
// it). false means nothing was done: for a failed job that is its final end,
// so the caller records the failure and may download the results.
func (r *Resubmitter) Handle(ctx context.Context, j store.Job, be backend.Backend, st backend.Status) bool {
	if r == nil || r.Store == nil || be == nil || j.Backend != "slurm" || j.RemoteID == "" || !monitoredState(j.State) {
		return false
	}
	failedEnd := st.State == store.StateFailed
	defer r.lock(j.ID)()

	// The caller's copy may be stale; the database decides.
	cur, err := r.Store.Job(ctx, j.ID)
	if err != nil {
		return false
	}
	if cur.RemoteID != j.RemoteID || !monitoredState(cur.State) {
		return true // somebody else moved the job on; st is about an old ID
	}
	j = cur
	evs, err := r.Store.Events(ctx, j.ID, 0)
	if err != nil {
		return false
	}
	var plan placementPlan
	var spec backend.JobSpec
	havePlan, haveSpec := false, false
	prior := 0
	for _, e := range evs {
		switch e.Kind {
		case eventPlan:
			plan = placementPlan{}
			havePlan = json.Unmarshal([]byte(e.Text), &plan) == nil
		case eventSpec:
			spec = backend.JobSpec{}
			haveSpec = json.Unmarshal([]byte(e.Text), &spec) == nil
		case eventResubmitted:
			var re resubmitEvent
			if json.Unmarshal([]byte(e.Text), &re) == nil {
				if re.Fallback {
					return false // the one fallback submit is done
				}
				prior = re.Used
			}
		}
	}
	if !havePlan || !plan.Preemptible {
		return false
	}
	if !haveSpec {
		spec = backend.JobSpec{WorkDir: j.RemoteDir}
	}
	// A claimed ID is resolved whatever its status says: a cancelled one
	// is the copy Labmill cancelled itself, and the job is not marked canceled.
	if claim, open := store.OpenResubmitClaim(evs); open {
		return r.resolveOpen(ctx, j, be, spec, claim)
	}
	if !failedEnd && (!plan.Requeue || st.State != store.StatePending) {
		// A requeue loop is acted on only while the requeued copy waits;
		// a running copy is never killed.
		return false
	}
	limit := 1 + plan.MaxResubmits
	if plan.MaxResubmits <= 0 {
		limit = 1 + maxResubmits
	}

	// Whether this is a final failure to act on, or a job Slurm keeps
	// requeueing past the limit.
	restarts := 0
	readRestarts := func() {
		if rc, ok := be.(backend.RestartCounter); ok {
			if n, err := rc.Restarts(ctx, j.RemoteID); err == nil {
				restarts = n
			}
		}
	}
	var cause string
	used, forceFallback := 0, false
	if failedEnd {
		readRestarts()
		if cause = preemptionCause(st, restarts); cause == "" {
			return false
		}
		used = prior + 1 + restarts
	} else {
		r.mu.Lock()
		if r.lastLive == nil {
			r.lastLive = map[int64]time.Time{}
		}
		last, seen := r.lastLive[j.ID]
		due := !seen || r.now().Sub(last) >= liveCheckEvery
		if due {
			r.lastLive[j.ID] = r.now()
		}
		r.mu.Unlock()
		if !due {
			return false
		}
		readRestarts()
		if prior+restarts < limit {
			return false
		}
		cause = fmt.Sprintf("was requeued %d time(s) by Slurm", restarts)
		used, forceFallback = limit, true
	}
	if ctx.Err() != nil {
		return true // closing: decided at the next start
	}

	where := nonEmpty(plan.Partition, "the default partition")
	what := fmt.Sprintf("Job #%d (%s) %s on %s", j.ID, j.Name, cause, where)
	p := claimPayload{Event: resubmitEvent{Old: j.RemoteID, Partition: plan.Partition, Used: used, Why: cause}}
	script := j.Script
	var announce string
	fallback := forceFallback || used >= limit
	if !fallback {
		p.Event.Attempt = used + 1
		announce = fmt.Sprintf("%s; resubmitted (attempt %d of %d)", what, p.Event.Attempt, limit)
		p.Msg = fmt.Sprintf("resubmitted (attempt %d of %d)", p.Event.Attempt, limit)
	} else {
		if plan.FallbackNeedsPartition && plan.FallbackPartition == "" {
			r.sayOnce(fmt.Sprintf("needspart:%d:%s", j.ID, j.RemoteID), fmt.Sprintf("%s for the %s time and the plan has no fallback partition (this cluster has no default one). Tell me which non-preemptible partition to use and I submit it there.", what, ordinal(used)))
			return false
		}
		var changes []string
		script, changes = fallbackScript(j.Script, plan.FallbackPartition)
		p.Script = script
		p.Event.Fallback, p.Event.Partition = true, plan.FallbackPartition
		fb := nonEmpty(plan.FallbackPartition, "the default partition")
		announce = fmt.Sprintf("%s for the %s time; moving to %s as planned", what, ordinal(used), fb)
		if len(changes) > 0 {
			announce += ". Changed in the script: " + strings.Join(changes, "; ")
		}
		p.Msg = "moved to " + fb + " after the preemptible attempts"
	}

	// From here on the work is finished even when Labmill is closing.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resubmitSaveTimeout)
	defer cancel()
	payload, _ := json.Marshal(p)
	ok, err := r.Store.ClaimResubmit(sctx, j.ID, j.RemoteID, string(payload))
	if err != nil {
		r.logf("Job #%d: %v", j.ID, err)
		return true
	}
	if !ok {
		return true // another caller or process has it
	}
	if fallback {
		// A copy Slurm requeued is cancelled first so two never run; only
		// this job's own ID. Nothing to cancel is not an error.
		if err := be.Cancel(sctx, j.RemoteID); err != nil {
			r.logf("Job #%d: scancel %s before the fallback: %v", j.ID, j.RemoteID, err)
		}
	}
	newID, err := be.Submit(sctx, script, spec)
	if err != nil && (errors.Is(err, backend.ErrSubmitUncertain) || sctx.Err() != nil) {
		// The job may exist: the claim stays open and nothing submits again
		// for this ID; resolveOpen looks for the job after the claim ages.
		r.sayOnce(fmt.Sprintf("uncertain:%d:%s", j.ID, j.RemoteID), fmt.Sprintf("%s, but I can't tell whether the resubmission reached the cluster (%s). I won't submit again; I'll look for it in the queue shortly.", what, sanitize(err.Error())))
		return true
	}
	if err != nil {
		if rerr := r.Store.ReleaseResubmit(sctx, j.ID, j.RemoteID); rerr != nil {
			r.logf("Job #%d: couldn't release the claim: %v", j.ID, rerr)
		}
		r.mu.Lock()
		if r.failures == nil {
			r.failures = map[int64]int{}
		}
		r.failures[j.ID]++
		n := r.failures[j.ID]
		r.mu.Unlock()
		switch {
		case n == 1:
			r.say(fmt.Sprintf("%s, but I couldn't resubmit it now (%s). I'll try again at the next check.", what, sanitize(err.Error())))
			return true
		case n < maxResubmitFailures:
			r.logf("Job #%d: resubmit try %d failed: %v", j.ID, n, err)
			return true
		}
		r.say(fmt.Sprintf("I gave up resubmitting job #%d (%s). It stays failed; ask me to resubmit it.", j.ID, sanitize(err.Error())))
		return false
	}
	r.mu.Lock()
	delete(r.failures, j.ID)
	r.mu.Unlock()
	r.finish(sctx, be, j, j.RemoteID, newID, announce, p, false)
	return true
}

// finish saves the submitted newID. A failed save is kept and retried by
// resolveOpen, never submitted again.
func (r *Resubmitter) finish(ctx context.Context, be backend.Backend, j store.Job, old, newID, announce string, p claimPayload, adopted bool) bool {
	p.Event.New = newID
	text, _ := json.Marshal(p.Event)
	err := r.Store.FinishResubmit(ctx, j.ID, old, newID, p.Script, p.Msg, eventResubmitted, string(text))
	key := fmt.Sprintf("%d:%s", j.ID, old)
	switch {
	case err == nil:
		r.mu.Lock()
		delete(r.unsaved, key)
		r.mu.Unlock()
		r.say(fmt.Sprintf("%s, new Slurm job %s.", announce, newID))
		return true
	case errors.Is(err, store.ErrJobChanged):
		// Look again: another caller may have saved this very ID (then it
		// is never cancelled). Only a job the user canceled or that ended
		// meanwhile, and that still has the old ID, makes the copy just
		// submitted unwanted; an adopted job is never cancelled.
		cur, gerr := r.Store.Job(ctx, j.ID)
		switch {
		case gerr == nil && cur.RemoteID == newID:
			r.say(fmt.Sprintf("%s, new Slurm job %s.", announce, newID))
		case !adopted && gerr == nil && cur.RemoteID == old && !monitoredState(cur.State):
			if cerr := be.Cancel(ctx, newID); cerr != nil {
				r.logf("Job #%d: couldn't cancel the unwanted Slurm job %s: %v", j.ID, newID, cerr)
			}
			_ = r.Store.ReleaseResubmit(ctx, j.ID, old)
			r.say(fmt.Sprintf("Job #%d changed while I resubmitted it; I cancelled the new Slurm job %s.", j.ID, newID))
		default:
			_ = r.Store.ReleaseResubmit(ctx, j.ID, old)
			r.say(fmt.Sprintf("Job #%d changed while I resubmitted it; Slurm job %s was left as it is. Ask me to look at it.", j.ID, newID))
		}
		return true
	}
	r.mu.Lock()
	if r.unsaved == nil {
		r.unsaved = map[string]*unsavedSave{}
	}
	r.unsaved[key] = &unsavedSave{newID: newID, announce: announce, p: p}
	r.mu.Unlock()
	r.sayOnce("save:"+key, fmt.Sprintf("Job #%d was resubmitted as Slurm job %s, but I couldn't save that (%s). I'll keep trying.", j.ID, newID, sanitize(err.Error())))
	return false
}

// resolveOpen handles a job with an open claim: a save that failed here is
// retried; a fresh claim belongs to another Labmill process; an old one is a
// crash, and the job is looked up by name in the queue.
func (r *Resubmitter) resolveOpen(ctx context.Context, j store.Job, be backend.Backend, spec backend.JobSpec, claim store.ResubmitClaim) bool {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resubmitSaveTimeout)
	defer cancel()
	key := fmt.Sprintf("%d:%s", j.ID, claim.Old)
	r.mu.Lock()
	u := r.unsaved[key]
	r.mu.Unlock()
	if u != nil {
		return r.finish(sctx, be, j, claim.Old, u.newID, u.announce, u.p, false) || true
	}
	if r.now().Sub(claim.At) < claimStale {
		return true // another process is working on it
	}
	var p claimPayload
	if json.Unmarshal([]byte(claim.Payload), &p) != nil || claim.Old != j.RemoteID {
		return false
	}
	if best := r.findAdoptable(sctx, j, be, spec, claim); best == errLater {
		return true // try again at the next check
	} else if best != "" {
		return r.finish(sctx, be, j, claim.Old, best, fmt.Sprintf("Job #%d (%s) was resubmitted before Labmill closed; I found it again", j.ID, j.Name), p, true) || true
	}
	r.sayOnce("lost:"+key, fmt.Sprintf("Job #%d (%s) was being resubmitted when Labmill stopped, and I can't find the new Slurm job. I did not submit it again; ask me to look and to resubmit it if it is gone.", j.ID, j.Name))
	return false
}

// errLater is findAdoptable's answer when the queue couldn't be read.
const errLater = "\x00later"

// findAdoptable looks, read only, for the job a crashed resubmission
// submitted: one of the user's queued jobs with the job's name and work
// folder, a higher ID than the replaced one, submitted no earlier than the
// claim (a couple of minutes of clock skew allowed), and not recorded by any
// other Labmill job. "" means none; errLater means ask again at the next check.
func (r *Resubmitter) findAdoptable(ctx context.Context, j store.Job, be backend.Backend, spec backend.JobSpec, claim store.ResubmitClaim) string {
	qf, ok := be.(backend.QueueFinder)
	if !ok {
		return ""
	}
	dir := spec.WorkDir
	if dir == "" {
		dir = j.RemoteDir
	}
	if dir == "" {
		return ""
	}
	queued, err := qf.QueuedJobs(ctx)
	if err != nil {
		r.logf("Job #%d: looking for an unrecorded resubmission: %v", j.ID, err)
		return errLater
	}
	// Nothing queued: the job may have started and ended already.
	hist := func() ([]backend.QueuedJob, error) {
		hf, ok := be.(backend.HistoryFinder)
		if !ok {
			return nil, nil
		}
		h, err := hf.JobsSince(ctx, claim.At.Add(-claimSkew))
		if err != nil {
			r.logf("Job #%d: looking in the job history: %v", j.ID, err)
		}
		return h, err
	}
	taken := map[string]bool{}
	if all, err := r.Store.Jobs(ctx, store.JobFilter{}); err == nil {
		for _, o := range all {
			if o.RemoteID != "" {
				taken[o.RemoteID] = true
			}
		}
	}
	name := nonEmpty(spec.Name, j.Name)
	oldN, _ := strconv.Atoi(claim.Old)
	pick := func(list []backend.QueuedJob) string {
		best, bestN := "", 0
		for _, q := range list {
			n, err := strconv.Atoi(q.ID)
			if err != nil || n <= oldN || n <= bestN || taken[q.ID] {
				continue
			}
			if q.Name != name || path.Clean(q.WorkDir) != path.Clean(dir) {
				continue
			}
			if q.Submitted.IsZero() || q.Submitted.Before(claim.At.Add(-claimSkew)) {
				continue
			}
			best, bestN = q.ID, n
		}
		return best
	}
	if best := pick(queued); best != "" {
		return best
	}
	h, err := hist()
	if err != nil {
		// A failed lookup isn't "not found": ask again at the next check.
		return errLater
	}
	return pick(h)
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ordinal is n as "3rd" for the chat.
func ordinal(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}

// fallbackScript turns the approved preemptible script into the one for the
// fallback partition: only the #SBATCH header lines for the partition, the
// QOS and --requeue change (the partition is set to partition, or removed
// for "" = the cluster's default). Trailing "# comments" of an option line
// are ignored when reading it; changes names each edit for the chat.
func fallbackScript(script, partition string) (string, []string) {
	var out, changes []string
	seenPartition := false
	inHeader := true
	for _, l := range strings.Split(script, "\n") {
		t := strings.TrimSpace(l)
		if inHeader && t != "" && !strings.HasPrefix(t, "#") {
			inHeader = false
		}
		if !inHeader || !strings.HasPrefix(t, "#SBATCH") {
			out = append(out, l)
			continue
		}
		body, _, _ := strings.Cut(strings.TrimPrefix(t, "#SBATCH"), " #")
		opt := strings.Fields(body)
		key := ""
		if len(opt) > 0 {
			key, _, _ = strings.Cut(opt[0], "=")
		}
		switch {
		case len(opt) == 1 && (key == "--qos" || key == "--requeue"), len(opt) == 2 && (key == "--qos" || key == "-q"):
			changes = append(changes, "removed "+t)
		case len(opt) == 1 && key == "--partition", len(opt) == 2 && (key == "--partition" || key == "-p"):
			if partition == "" {
				changes = append(changes, "removed "+t+" (the cluster's default partition is used)")
				seenPartition = true
				continue
			}
			if seenPartition {
				changes = append(changes, "removed "+t)
				continue
			}
			seenPartition = true
			nl := "#SBATCH --partition=" + partition
			if nl != t {
				changes = append(changes, t+" -> "+nl)
			}
			out = append(out, nl)
		default:
			out = append(out, l)
		}
	}
	if !seenPartition && partition != "" {
		at := 0
		if len(out) > 0 && strings.HasPrefix(out[0], "#!") {
			at = 1
		}
		nl := "#SBATCH --partition=" + partition
		out = append(out[:at], append([]string{nl}, out[at:]...)...)
		changes = append(changes, "added "+nl)
	}
	return strings.Join(out, "\n"), changes
}
