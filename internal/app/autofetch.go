package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// Automatic download of a finished job's results (M60.1, the setting
// "Download results automatically"). The job monitor (or job_status, when it
// is the first to see the end) calls Env.AutoFetch once a job Labmill submitted
// has completed or failed. It takes the same path as fetch_results
// (fetchPlan, download: the usual results folder, checksums verified)
// without a model call and without an approval: a download only brings files
// to this computer. Because nobody looks at the file list first, it brings
// only what the job produced, never hidden or secret-looking files,
// environments and caches, or symbolic links, and never more than the limits
// below.

const (
	// AutoFetchMaxBytes is the most an automatic download brings: larger
	// results are left for the user to fetch ("ask me to fetch the results").
	AutoFetchMaxBytes = int64(2) << 30

	// AutoFetchMaxFiles is the most files an automatic download brings.
	AutoFetchMaxFiles = 2000

	// autoFetchFreeShare is how much of the free space on the disk the
	// results may fill (the rest, 10 %, stays free).
	autoFetchFreeShare = 0.9

	// Events in a job's history. eventAutoFetch means the job was handled for
	// good (downloaded, or skipped with a sentence); eventAutoFetchPending
	// means its end was seen while the setting was on and it still has to be
	// handled (a restart or a later monitor check retries it);
	// eventAutoFetchSignIn means the user was already told that a sign-in is
	// needed.
	eventAutoFetch        = "autofetch"
	eventAutoFetchPending = "autofetch-pending"
	eventAutoFetchSignIn  = "autofetch-signin"
)

// ErrSignInNeeded is what Env.TransferCheck returns (wrapped is fine) in
// background use when the transfer host has no live sign-in: AutoFetch then
// asks the user to sign in rather than a dialog appearing from nowhere. The
// download is tried again at later checks.
var ErrSignInNeeded = errors.New("sign-in needed")

// ErrTryLater is what a background TransferCheck returns when it couldn't
// tell whether there is a sign-in (the connections are busy, for instance a
// login dialog is open). AutoFetch stays quiet and tries again later.
var ErrTryLater = errors.New("busy, try again later")

// AutoFetchResult is what AutoFetch reports.
type AutoFetchResult struct {
	// Text is the chat sentence ("" when there is nothing to say: the job was
	// handled before, isn't finished, has no folder on a cluster, or the
	// attempt is only postponed).
	Text string
	// Downloaded is true when the results came down.
	Downloaded bool
	// Retry is true when the job is not handled yet and a later attempt may
	// succeed (no sign-in, busy connections, Labmill quitting).
	Retry bool
}

// jobLockKey names a per-job lock of one store.
type jobLockKey struct {
	st   *store.Store
	id   int64
	kind string
}

var jobLocks sync.Map // jobLockKey -> chan struct{} (one slot: full = locked)

func jobLock(st *store.Store, id int64, kind string) chan struct{} {
	m, _ := jobLocks.LoadOrStore(jobLockKey{st, id, kind}, make(chan struct{}, 1))
	return m.(chan struct{})
}

// tryLockJob takes the lock without waiting.
func tryLockJob(st *store.Store, id int64, kind string) bool {
	select {
	case jobLock(st, id, kind) <- struct{}{}:
		return true
	default:
		return false
	}
}

// lockJob takes the lock, giving up when ctx ends (a Cancel stops the wait).
func lockJob(ctx context.Context, st *store.Store, id int64, kind string) error {
	select {
	case jobLock(st, id, kind) <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unlockJob(st *store.Store, id int64, kind string) { <-jobLock(st, id, kind) }

// autoFetchBackoff is how long a job whose download was postponed (no
// sign-in, busy connections) waits between further attempts, after its first
// retry. A variable for tests.
var autoFetchBackoff = 10 * time.Minute

type postponement struct {
	n    int       // postponed attempts so far
	last time.Time // the latest one
}

var postponed sync.Map // jobLockKey -> postponement

// PendingAutoFetch lists the jobs whose end was seen while the setting was
// on but that are not handled yet (see AutoFetchResult.Retry): finished
// jobs with a pending event and no handled event.
func (e *Env) PendingAutoFetch(ctx context.Context) []int64 {
	if e.Store == nil {
		return nil
	}
	jobs, err := e.Store.Jobs(ctx, store.JobFilter{States: []store.JobState{store.StateCompleted, store.StateFailed}})
	if err != nil {
		return nil
	}
	t := &toolset{env: e}
	var ids []int64
	for _, j := range jobs {
		if j.RemoteDir == "" {
			continue
		}
		if _, ok, err := t.latestEvent(ctx, j.ID, eventAutoFetchPending); err != nil || !ok {
			continue
		}
		if _, done, err := t.latestEvent(ctx, j.ID, eventAutoFetch); err != nil || done {
			continue
		}
		ids = append(ids, j.ID)
	}
	return ids
}

// AutoFetch downloads the results of job jobID by itself and says what
// happened. Each job is handled at most once: a definite outcome (downloaded,
// or skipped with a sentence) is recorded in the job's history, so a restart
// doesn't repeat it; a postponed attempt (Retry) is recorded as pending and
// tried again later. A failed or skipped download leaves "fetch_results" to
// the user. logf, if set, gets one line per step (for labmill.log). It never
// returns an error: every failure is in Text.
func (e *Env) AutoFetch(ctx context.Context, jobID int64, logf func(string)) AutoFetchResult {
	if logf == nil {
		logf = func(string) {}
	}
	if e.Store == nil {
		return AutoFetchResult{}
	}
	if !tryLockJob(e.Store, jobID, "auto") {
		return AutoFetchResult{} // being handled right now
	}
	defer unlockJob(e.Store, jobID, "auto")
	pkey := jobLockKey{e.Store, jobID, "postponed"}
	t := &toolset{env: e}
	j, err := e.Store.Job(ctx, jobID)
	if err != nil || j.RemoteDir == "" || j.Cluster == "" {
		return AutoFetchResult{}
	}
	if j.State != store.StateCompleted && j.State != store.StateFailed {
		return AutoFetchResult{}
	}
	if _, done, err := t.latestEvent(ctx, jobID, eventAutoFetch); err != nil || done {
		return AutoFetchResult{}
	}
	if _, ok, err := t.latestEvent(ctx, jobID, eventAutoFetchPending); err == nil && !ok {
		if err := e.Store.AddEvent(ctx, jobID, eventAutoFetchPending, "waiting"); err != nil {
			logf(fmt.Sprintf("job #%d: not downloading, couldn't record it: %v", jobID, err))
			return AutoFetchResult{}
		}
	}
	// A postponed job is retried at the next check and then only every
	// autoFetchBackoff, quietly.
	prev := postponement{}
	if v, ok := postponed.Load(pkey); ok {
		prev = v.(postponement)
		if prev.n >= 2 && time.Since(prev.last) < autoFetchBackoff {
			return AutoFetchResult{Retry: true}
		}
	}
	what := fmt.Sprintf("Job #%d (%s)", j.ID, sanitize(j.Name))
	if prev.n == 0 {
		logf(fmt.Sprintf("job #%d (%s) on %s ended as %s: downloading its results", j.ID, j.Name, j.Cluster, j.State))
	}

	// done records a definite outcome.
	done := func(r AutoFetchResult) AutoFetchResult {
		postponed.Delete(pkey)
		if err := e.Store.AddEvent(ctx, jobID, eventAutoFetch, "done"); err != nil {
			logf(fmt.Sprintf("job #%d: couldn't record the outcome: %v", jobID, err))
		}
		return r
	}
	skip := func(why string) AutoFetchResult {
		logf(fmt.Sprintf("job #%d: not downloaded automatically: %s", j.ID, why))
		return done(AutoFetchResult{Text: fmt.Sprintf("%s: its results weren't downloaded automatically: %s Ask me to fetch the results.", what, why)})
	}
	// retry handles an error that may go away: no sign-in (tell the user once),
	// busy connections or Labmill quitting (say nothing).
	retry := func(err error) (AutoFetchResult, bool) {
		switch {
		case ctx.Err() != nil, errors.Is(err, ErrTryLater):
			postponed.Store(pkey, postponement{prev.n + 1, time.Now()})
			if prev.n == 0 {
				logf(fmt.Sprintf("job #%d: postponed: %v", j.ID, err))
			}
			return AutoFetchResult{Retry: true}, true
		case errors.Is(err, ErrSignInNeeded):
			postponed.Store(pkey, postponement{prev.n + 1, time.Now()})
			if prev.n == 0 {
				logf(fmt.Sprintf("job #%d: postponed, no sign-in: %v", j.ID, err))
			}
			if _, told, _ := t.latestEvent(ctx, jobID, eventAutoFetchSignIn); told {
				return AutoFetchResult{Retry: true}, true
			}
			e.Store.AddEvent(ctx, jobID, eventAutoFetchSignIn, "told")
			return AutoFetchResult{Retry: true, Text: fmt.Sprintf("%s finished, but I need you to sign in to the cluster before I can download its results. Once you are signed in, I will download them within a few minutes, or ask me to fetch the results now.", what)}, true
		}
		return AutoFetchResult{}, false
	}
	fail := func(err error) AutoFetchResult {
		if r, ok := retry(err); ok {
			return r
		}
		return skip(strings.TrimRight(sanitize(err.Error()), ".") + ".")
	}

	in, _ := json.Marshal(map[string]any{"job_id": jobID, "paths": []string{"."}})
	p, err := t.fetchPlan(ctx, in)
	if err != nil {
		return fail(err)
	}
	p.auto = true
	listed, err := t.listResults(ctx, p)
	if err != nil {
		return fail(err)
	}
	files, err := t.newFiles(ctx, jobID, listed)
	if err != nil {
		return fail(err)
	}
	files, leftOut := selectResults(files)
	if len(files) == 0 {
		if leftOut > 0 {
			return skip(fmt.Sprintf("the only new files are private ones or in environment and cache folders (%s left out).", plural(leftOut, "file", "files")))
		}
		return skip("I found no files in its folder besides the ones you uploaded.")
	}
	if len(files) > AutoFetchMaxFiles {
		return skip(fmt.Sprintf("there are %d files, more than the %d-file limit for automatic downloads.", len(files), AutoFetchMaxFiles))
	}
	// Each file lands at localDir/<path>: the same checks as for a path the
	// model names. A file that fails (hidden, a sign-in secret) is left out.
	kept := files[:0:0]
	for _, f := range files {
		if err := t.localDirOK(filepath.Join(p.localDir, filepath.FromSlash(f.Path))); err != nil {
			leftOut++
			continue
		}
		kept = append(kept, f)
	}
	files = kept
	if len(files) == 0 {
		return skip(fmt.Sprintf("the only new files are private ones or in environment and cache folders (%s left out).", plural(leftOut, "file", "files")))
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	logf(fmt.Sprintf("job #%d: %s, %s to download into %s (%d left out)", j.ID, plural(len(files), "file", "files"), transfer.FormatBytes(total), p.localDir, leftOut))
	if total > AutoFetchMaxBytes {
		return skip(fmt.Sprintf("they total %s, more than the 2 GB limit for automatic downloads.", transfer.FormatBytes(total)))
	}
	if e.FreeSpace != nil {
		if free, err := e.FreeSpace(existingParent(p.localDir)); err == nil && float64(total) > float64(free)*autoFetchFreeShare {
			return skip(fmt.Sprintf("they total %s but only %s is free on this computer (I keep 10%% free).", transfer.FormatBytes(total), transfer.FormatBytes(free)))
		}
	}

	p.paths = make([]string, len(files))
	for i, f := range files {
		p.paths[i] = f.Path
	}
	dir := p.job.RemoteDir
	if p.s3 != nil {
		dir = ""
	}
	p.rclone = t.rcloneNote(p.cluster, p.s3, dir, p.localDir, p.paths, false)
	written, verified, _, err := t.download(ctx, p)
	if err != nil {
		return fail(err)
	}
	out := fmt.Sprintf("%s: downloaded %s (%s) to %s.", what, plural(len(written), "file", "files"), transfer.FormatBytes(total), sanitize(p.localDir))
	if leftOut > 0 {
		out += fmt.Sprintf(" %s left out (private names, environments and caches).", plural(leftOut, "file was", "files were"))
	}
	fetched := len(verified.Mismatched) == 0 && len(verified.Unrenamed) == 0
	if note := verifyNote(verified); note != "" && !fetched {
		out += strings.ReplaceAll(note, "\n", " ")
	}
	if fetched {
		// No model turn here, so the offer is a sentence; cleanup_job asks.
		if offer := t.cleanupOffer(ctx, jobID); offer != "" {
			out += " " + offer
		}
	}
	logf(fmt.Sprintf("job #%d: downloaded %s to %s (fetched=%v)", j.ID, plural(len(written), "file", "files"), p.localDir, fetched))
	return done(AutoFetchResult{Text: out, Downloaded: fetched})
}

// listResults lists the files in the job's folder (regular files only: the
// listing skips symbolic links).
func (t *toolset) listResults(ctx context.Context, p fetchPlan) ([]transfer.RemoteFile, error) {
	if p.s3 != nil {
		if t.env.ListRemoteS3 == nil {
			return nil, errors.New("Listing the results isn't available right now.")
		}
		return t.env.ListRemoteS3(ctx, *p.s3)
	}
	if t.env.ListRemote == nil {
		return nil, errors.New("Listing the results isn't available right now.")
	}
	src, err := t.remote(p.cluster, p.job.RemoteDir)
	if err != nil {
		return nil, err
	}
	return t.env.ListRemote(ctx, src)
}

// newFiles drops from files what the job's folder held before it ran: the
// files Labmill uploaded (same path and size as in the staged manifest) and the
// extra scripts it wrote. What is left is what the job produced.
func (t *toolset) newFiles(ctx context.Context, jobID int64, files []transfer.RemoteFile) ([]transfer.RemoteFile, error) {
	known := map[string]int64{}
	entries, err := t.env.Store.Manifest(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("I couldn't read what was uploaded for job %d: %w", jobID, err)
	}
	for _, m := range entries {
		known[m.Path] = m.Size
	}
	scripts := map[string]bool{}
	jf, err := t.jobFilesOf(ctx, jobID)
	if err != nil {
		return nil, err
	}
	for _, f := range jf {
		scripts[f.Name] = true
	}
	var out []transfer.RemoteFile
	for _, f := range files {
		if size, ok := known[f.Path]; ok && size == f.Size {
			continue
		}
		if scripts[f.Path] {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// cacheDirs are folder names whose contents are never results: package and
// build caches, Python bytecode, version control.
var cacheDirs = map[string]bool{
	"__pycache__": true, ".cache": true, "node_modules": true, ".git": true,
	"site-packages": true, ".ipynb_checkpoints": true, ".pytest_cache": true, ".mypy_cache": true, ".tox": true,
}

// selectResults drops from files (job output, sorted or not) what an
// automatic download never brings: anything below an environment (a folder
// with a pyvenv.cfg file or a conda-meta folder), below a cache folder, and
// any file whose path is hidden or private by the privacy rules (checkRel:
// dot-names, sign-in secrets such as .netrc, .env or a token file). It
// returns the rest and how many files it left out.
func selectResults(files []transfer.RemoteFile) (kept []transfer.RemoteFile, leftOut int) {
	envRoots := map[string]bool{} // folders ("a/b/") that are environments
	for _, f := range files {
		dir, name := path.Split(f.Path)
		if name == "pyvenv.cfg" {
			envRoots[dir] = true
		}
		parts := strings.Split(f.Path, "/")
		for i, c := range parts[:len(parts)-1] {
			if c == "conda-meta" {
				envRoots[strings.Join(parts[:i], "/")+"/"] = true
				if i == 0 {
					envRoots["/"] = true
				}
			}
		}
	}
	inEnv := func(p string) bool {
		if envRoots["/"] || envRoots[""] {
			return true
		}
		for i := 0; i < len(p); i++ {
			if p[i] == '/' && envRoots[p[:i+1]] {
				return true
			}
		}
		return false
	}
	for _, f := range files {
		parts := strings.Split(f.Path, "/")
		skip := inEnv(f.Path) || checkRel(f.Path) != nil
		for _, c := range parts[:len(parts)-1] {
			if cacheDirs[c] {
				skip = true
			}
		}
		if skip {
			leftOut++
			continue
		}
		kept = append(kept, f)
	}
	return kept, leftOut
}

// existingParent is dir, or its nearest parent that exists (the results
// folder is created by the download).
func existingParent(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || filepath.Dir(d) == d {
			return d
		}
	}
}
