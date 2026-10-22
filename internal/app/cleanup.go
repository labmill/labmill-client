package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// M61.4: cleanup of a job's own working files in the global scratch after a
// verified download. The variant (the safest of those considered): Labmill
// removes exactly the files it can name, never a whole folder tree, and only
// a file whose SHA-256 on the cluster, checked just before removing, still
// equals the sum recorded when it was verified or uploaded:
//
//   - the files downloaded and verified (a "downloaded" event with path and
//     SHA-256 of the verified local copy);
//   - the files Labmill uploaded (the staged manifest) and the helper files it
//     wrote (draft_job's files);
//
// then the parent folders of the removed files (deepest first, stopping at
// the job folder, `rmdir --ignore-fail-on-non-empty`) and the job folder
// itself if that is empty. Never `rm -r`, never a tree-wide delete. Anything
// under an environment (a folder with pyvenv.cfg or conda-meta, found on the
// cluster just before removing) or under a symbolic link, and any path with
// site-packages, .conda, envs or a cache folder in it, is left alone. The
// job must have ended, no open resubmission may exist, no other active
// Labmill job may use the folder, and the folder must be a Labmill job folder
// (<...>/labmill/<job>) inside the profile's global scratch, at least
// cleanupMinDepth components below its root. The tool asks for approval with
// the full list; only "Approve everything automatically" skips the question.

const (
	// eventDownloaded is the JSON []downloadedRecord (paths relative to the
	// job's folder on the cluster, with the SHA-256 of the verified copy) of a
	// download; eventCleaned records a cleanup (JSON cleanedRecord) and ends
	// the pending downloads before it.
	eventDownloaded = "downloaded"
	eventCleaned    = "cleaned"

	// maxRecordedDownloads is the most paths one download records; a larger
	// download is never cleaned up automatically (remove by hand).
	maxRecordedDownloads = 20000

	// cleanupMinDepth is how many components the job folder must lie below
	// the global scratch's root (<root>/<user>/labmill/<job> has 3 or more).
	cleanupMinDepth = 2

	cleanupBatchQuote = 16 << 10 // bytes of quoted paths per rm, sha256sum or rmdir call (as transfer's verifyBatchQuote)
	cleanupShown      = 2000     // path lines in the approval; the result lists all
	cleanupTimeout    = 5 * time.Minute
)

type cleanedRecord struct {
	Dir   string `json:"dir"`
	Files int    `json:"files"`
}

type downloadedRecord struct {
	Path   string `json:"p"`
	SHA256 string `json:"s"`
}

// recordDownloaded notes the files of a download that were all verified by
// SHA-256, with the SHA-256 of each verified local copy (best effort:
// without the note nothing is offered for cleanup).
func (t *toolset) recordDownloaded(ctx context.Context, p fetchPlan, written []string, v transfer.Verification) {
	if p.s3 != nil || len(written) == 0 || len(written) > maxRecordedDownloads {
		return
	}
	if v.Verified != len(written) || len(v.Failed) > 0 || v.Unverified > 0 || v.SizeOnly != "" || len(v.Mismatched) > 0 || len(v.Unrenamed) > 0 {
		return
	}
	recs := make([]downloadedRecord, 0, len(written))
	for _, w := range written {
		r, err := filepath.Rel(p.localDir, w)
		if err != nil {
			return
		}
		sum, err := fileSHA256(w)
		if err != nil {
			return
		}
		recs = append(recs, downloadedRecord{filepath.ToSlash(r), sum})
	}
	b, _ := json.Marshal(recs)
	t.env.Store.AddEvent(ctx, p.job.ID, eventDownloaded, string(b))
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pendingDownloads is the files downloaded and verified since the job's last
// cleanup (the latest record of a path wins).
func (t *toolset) pendingDownloads(ctx context.Context, jobID int64) ([]downloadedRecord, error) {
	evs, err := t.env.Store.Events(ctx, jobID, 0)
	if err != nil {
		return nil, fmt.Errorf("I couldn't read the history of job %d: %w", jobID, err)
	}
	last := -1
	for i, e := range evs {
		if e.Kind == eventCleaned {
			last = i
		}
	}
	idx := map[string]int{}
	var out []downloadedRecord
	for _, e := range evs[last+1:] {
		if e.Kind != eventDownloaded {
			continue
		}
		var recs []downloadedRecord
		if json.Unmarshal([]byte(e.Text), &recs) != nil {
			continue
		}
		for _, r := range recs {
			if i, ok := idx[r.Path]; ok {
				out[i] = r
				continue
			}
			idx[r.Path] = len(out)
			out = append(out, r)
		}
	}
	return out, nil
}

// cleanupRoot checks that dir may be cleaned: absolute and already clean
// (no "..", ".", "//" or trailing slash), a Labmill job folder
// (<...>/labmill/<job>), below the global scratch g and at least
// cleanupMinDepth components below its root (a placeholder in g's path
// counts as one component).
func cleanupRoot(g *rules.GlobalScratch, dir string) error {
	if g == nil || g.Path == "" {
		return errors.New("the site profile names no shared scratch")
	}
	switch {
	case !strings.HasPrefix(dir, "/"), path.Clean(dir) != dir, strings.Contains(dir, ".."):
		return fmt.Errorf("%s is not a clean absolute path", sanitize(dir))
	case !g.Covers(dir) || dir == g.Path:
		return fmt.Errorf("%s is not inside the shared scratch %s", sanitize(dir), g.Path)
	}
	root := strings.Split(strings.Trim(path.Clean(g.Path), "/"), "/")
	have := strings.Split(strings.Trim(dir, "/"), "/")
	if len(have)-len(root) < cleanupMinDepth {
		return fmt.Errorf("%s is too close to the root of the shared scratch %s (cleanup needs at least %d folder levels below it)", sanitize(dir), g.Path, cleanupMinDepth)
	}
	if !rules.IsJobFolder(dir, "") {
		return fmt.Errorf("%s is not a Labmill job folder (<...>/%s/<job>)", sanitize(dir), rules.JobFolderParent)
	}
	return nil
}

// envComponents are folder names below which nothing is ever removed.
var envComponents = map[string]bool{"site-packages": true, ".conda": true, "envs": true, "conda-meta": true}

// cleanablePath reports whether rel (relative to the job folder) may be
// removed: clean, relative, printable, and not inside an environment or cache.
func cleanablePath(rel string) bool {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "/") || path.Clean(rel) != rel {
		return false
	}
	for _, r := range rel {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '�' {
			return false
		}
	}
	for _, c := range strings.Split(rel, "/") {
		if c == ".." || envComponents[c] || cacheDirs[c] {
			return false
		}
	}
	return true
}

// cleanup is what a cleanup of a job would remove.
type cleanup struct {
	job     store.Job
	cluster config.Cluster
	dir     string
	// Each file is removed only while the cluster's SHA-256 still equals
	// sums[path].
	downloaded []string
	inputs     []string
	generated  []string
	sums       map[string]string
}

func (c cleanup) count() int { return len(c.downloaded) + len(c.inputs) + len(c.generated) }

func (c cleanup) all() []string {
	out := append(append(append([]string(nil), c.downloaded...), c.inputs...), c.generated...)
	sort.Strings(out)
	return out
}

// cleanupBlocked says why the job's folder can't be cleaned yet, "" when it
// can: the job must have ended (an open resubmission counts as running), and
// no other active Labmill job may work at or below the folder.
func (t *toolset) cleanupBlocked(ctx context.Context, j store.Job) (string, error) {
	if !j.State.Terminal() && j.State != store.StateCompleted {
		return fmt.Sprintf("Job %d (%s) hasn't ended (it is %s), so its folder may still be in use.", j.ID, sanitize(j.Name), j.State), nil
	}
	evs, err := t.env.Store.Events(ctx, j.ID, 0)
	if err != nil {
		return "", fmt.Errorf("I couldn't read the history of job %d: %w", j.ID, err)
	}
	if _, open := store.OpenResubmitClaim(evs); open {
		return fmt.Sprintf("Job %d (%s) is being resubmitted, so its folder is still in use.", j.ID, sanitize(j.Name)), nil
	}
	all, err := t.env.Store.Jobs(ctx, store.JobFilter{})
	if err != nil {
		return "", fmt.Errorf("I couldn't read Labmill's job list: %w", err)
	}
	for _, o := range all {
		if o.ID == j.ID || o.State.Terminal() || o.State == store.StateCompleted {
			continue
		}
		if (o.RemoteDir != "" && inFolder(j.RemoteDir, o.RemoteDir)) || mentionsFolder(o.Script, j.RemoteDir) {
			return fmt.Sprintf("Job %d (%s) is still active in the same folder, so I left it alone.", o.ID, sanitize(o.Name)), nil
		}
	}
	return "", nil
}

// inFolder reports whether d is dir or lies below it.
func inFolder(dir, d string) bool {
	d = path.Clean(d)
	return d == dir || strings.HasPrefix(d, dir+"/")
}

// mentionsFolder reports whether a script names dir or a path below it
// anywhere (a --chdir, a cd in the body, an output path), which is where a
// job may work even when its own RemoteDir is empty.
func mentionsFolder(script, dir string) bool {
	if dir == "" {
		return false
	}
	for i := 0; ; {
		k := strings.Index(script[i:], dir)
		if k < 0 {
			return false
		}
		end := i + k + len(dir)
		if end == len(script) || !isNameByte(script[end]) {
			return true
		}
		i = end
	}
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// workingIn runs squeue on the login node and says whether any of the
// user's queued or running jobs works in dir or below it.
func workingIn(dir string, out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		id, wd, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && inFolder(dir, strings.TrimSpace(wd)) {
			return id, true
		}
	}
	return "", false
}

// planCleanup works out what a cleanup of job j removes (nothing to do is a
// plan with count 0). Errors are plain messages for the model.
func (t *toolset) planCleanup(ctx context.Context, j store.Job) (cleanup, error) {
	if j.RemoteDir == "" || j.Cluster == "" {
		return cleanup{}, fmt.Errorf("Job %d (%s) has no working folder on a cluster, so there is nothing to clean up.", j.ID, sanitize(j.Name))
	}
	c, err := t.cluster(j.Cluster)
	if err != nil {
		return cleanup{}, err
	}
	if c.Type != config.TypeSlurm {
		return cleanup{}, fmt.Errorf("Cleanup only covers a Slurm job's folder in the shared scratch; job %d is on %s.", j.ID, sanitize(c.Name))
	}
	h := t.env.Site.SlurmFor(c.Name)
	if p, ok := t.jobPlan(ctx, j.ID); ok && p.Scratch == scratchLocal {
		return cleanup{}, fmt.Errorf("Job %d used the node-local scratch, which the job removed itself; nothing is left to clean up.", j.ID)
	}
	if err := cleanupRoot(h.GlobalScratch, j.RemoteDir); err != nil {
		return cleanup{}, fmt.Errorf("I only clean up a Labmill job folder in the shared scratch, and %v. Remove other files by hand if you want them gone.", err)
	}
	why, err := t.cleanupBlocked(ctx, j)
	if err != nil {
		return cleanup{}, err
	}
	if why != "" {
		return cleanup{}, errors.New(why + " Try again when the job has ended.")
	}
	cl := cleanup{job: j, cluster: c, dir: j.RemoteDir, sums: map[string]string{}}
	dl, err := t.pendingDownloads(ctx, j.ID)
	if err != nil {
		return cl, err
	}
	have := map[string]bool{}
	for _, r := range dl {
		if cleanablePath(r.Path) && len(r.SHA256) == 64 && !have[r.Path] {
			have[r.Path] = true
			cl.downloaded = append(cl.downloaded, r.Path)
			cl.sums[r.Path] = strings.ToLower(r.SHA256)
		}
	}
	if len(cl.downloaded) == 0 {
		return cl, nil // nothing downloaded and verified since the last cleanup
	}
	sort.Strings(cl.downloaded)
	entries, err := t.env.Store.Manifest(ctx, j.ID)
	if err != nil {
		return cl, fmt.Errorf("I couldn't read what was uploaded for job %d: %w", j.ID, err)
	}
	for _, m := range entries {
		if cleanablePath(m.Path) && !have[m.Path] && len(m.SHA256) == 64 {
			have[m.Path] = true
			cl.inputs = append(cl.inputs, m.Path)
			cl.sums[m.Path] = strings.ToLower(m.SHA256)
		}
	}
	jf, err := t.jobFilesOf(ctx, j.ID)
	if err != nil {
		return cl, err
	}
	for _, f := range jf {
		if cleanablePath(f.Name) && !have[f.Name] {
			have[f.Name] = true
			cl.generated = append(cl.generated, f.Name)
			sum := sha256.Sum256([]byte(f.Content))
			cl.sums[f.Name] = hex.EncodeToString(sum[:])
		}
	}
	sort.Strings(cl.inputs)
	sort.Strings(cl.generated)
	return cl, nil
}

// cleanupOffer is the sentence that offers a cleanup after a verified
// download, "" when none applies: the job's plan used the shared scratch
// (storage "global"; a job folder in the user's main space is never offered)
// and verified files are waiting.
func (t *toolset) cleanupOffer(ctx context.Context, jobID int64) string {
	j, err := t.env.Store.Job(ctx, jobID)
	if err != nil || !j.Downloaded {
		return ""
	}
	if p, ok := t.jobPlan(ctx, jobID); !ok || p.Scratch != scratchGlobal {
		return ""
	}
	cl, err := t.planCleanup(ctx, j)
	if err != nil || len(cl.downloaded) == 0 {
		return ""
	}
	return fmt.Sprintf("The results are downloaded and verified, so I can remove the job's own working files (%s) in %s on %s now: ask me to clean up job #%d, and you see the exact list first. Software environments are never touched.",
		plural(cl.count(), "file", "files"), sanitize(cl.dir), sanitize(cl.cluster.Name), j.ID)
}

// Commands (argv; the runner quotes each word). The job folder and every
// file are absolute paths made by cleanupAbs, after "--" so a name starting
// with "-" is not an option.
func cleanupFindArgv(dir string) []string {
	return []string{"find", dir, "-name", "pyvenv.cfg", "-print", "-o", "-name", "conda-meta", "-print", "-o", "-type", "l", "-print"}
}
func cleanupSumArgv(abs []string) []string { return append([]string{"sha256sum", "--"}, abs...) }
func cleanupRmArgv(abs []string) []string  { return append([]string{"rm", "-f", "--"}, abs...) }
func cleanupRmdirArgv(dirs []string) []string {
	return append([]string{"rmdir", "--ignore-fail-on-non-empty", "--"}, dirs...)
}

func cleanupAbs(dir string, rels []string) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = dir + "/" + r
	}
	return out
}

// batches splits s so that each part's words, quoted, take at most max bytes
// (a part holds at least one word).
func batches(s []string, max int) [][]string {
	var out [][]string
	var cur []string
	size := 0
	for _, w := range s {
		n := len(backend.Quote([]string{w})) + 1
		if len(cur) > 0 && size+n > max {
			out, cur, size = append(out, cur), nil, 0
		}
		cur, size = append(cur, w), size+n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// parentDirs are the folders between the job folder dir and the removed
// files (absolute), deepest first, never dir itself.
func parentDirs(dir string, files []string) []string {
	set := map[string]bool{}
	for _, f := range files {
		for d := path.Dir(f); len(d) > len(dir) && strings.HasPrefix(d, dir+"/"); d = path.Dir(d) {
			set[d] = true
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if di != dj {
			return di > dj
		}
		return out[i] < out[j]
	})
	return out
}

func (c cleanup) proposal() agent.Proposal {
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster: %s (login node)\nJob folder: %s\n\n", sanitize(c.cluster.Name), sanitize(c.dir))
	fmt.Fprintf(&b, "Removes %s, only these files and nothing else, and only those whose SHA-256 on the cluster, checked just before removing, is still the one recorded:\n", plural(c.count(), "file", "files"))
	fmt.Fprintf(&b, "- %s you downloaded (their SHA-256 matched the cluster's)\n", plural(len(c.downloaded), "file", "files"))
	if len(c.inputs)+len(c.generated) > 0 {
		fmt.Fprintf(&b, "- %s Labmill uploaded and %s it wrote there\n", plural(len(c.inputs), "file", "files"), plural(len(c.generated), "file", "files"))
	}
	all := c.all()
	for i, f := range all {
		if i == cleanupShown {
			fmt.Fprintf(&b, "  ... and %d more (the result lists them all)\n", len(all)-cleanupShown)
			break
		}
		fmt.Fprintf(&b, "  %s\n", sanitize(f))
	}
	b.WriteString("\nThen it removes the folders that held those files and are now empty (deepest first, never above the job folder), and the job folder itself if it is empty. Nothing else is deleted.\n")
	b.WriteString("Software environments (a folder with pyvenv.cfg or conda-meta, site-packages, .conda, envs, caches) and anything below a symbolic link are left alone; Labmill checks that on the cluster just before removing.\n\n")
	b.WriteString("Commands, run on the login node (long lists in several calls):\n")
	fmt.Fprintf(&b, "  %s\n", backend.Quote(cleanupFindArgv(c.dir)))
	fmt.Fprintf(&b, "  %s\n", backend.Quote(cleanupSumArgv([]string{c.dir + "/<file>", "..."})))
	fmt.Fprintf(&b, "  %s\n", backend.Quote(cleanupRmArgv([]string{c.dir + "/<file>", "..."})))
	fmt.Fprintf(&b, "  %s\n", backend.Quote(cleanupRmdirArgv([]string{c.dir + "/<folder>", "..."})))
	fmt.Fprintf(&b, "  %s\n", backend.Quote(cleanupRmdirArgv([]string{c.dir})))
	b.WriteString("\nThis can't be undone. The results are on this computer.")
	return agent.Proposal{
		Tool:    "cleanup_job",
		Summary: fmt.Sprintf("Remove %s of job #%d from %s on %s", plural(c.count(), "working file", "working files"), c.job.ID, sanitize(c.dir), sanitize(c.cluster.Name)),
		Details: b.String(),
	}
}

func (t *toolset) cleanupJob() agent.Tool {
	return agent.Tool{
		Name: "cleanup_job",
		Description: "Remove a job's own working files from its folder in the cluster's shared scratch after the results were downloaded and verified (SHA-256). " +
			"Only for a job that has ended, in a Labmill job folder (<...>/labmill/<job>) inside the shared scratch from cluster_info, that no other active job uses. " +
			"Removes exactly: the files that were downloaded and verified, the files Labmill uploaded and the helper files it wrote there, each only while its SHA-256 on the cluster is unchanged, then the now-empty folders that held them. " +
			"Never a whole folder tree, never software environments (venv, conda, site-packages), nothing outside the job's folder. " +
			"The user must approve the exact list (unless they turned on 'Approve everything automatically'). Offer it after a download of a job that ran in the shared scratch, or when the user asks; node-local scratch is cleaned by the job itself. A second call finds nothing left to remove.",
		Schema:        objSchema(map[string]any{"job_id": intProp("job id")}, "job_id"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			cl, err := t.cleanupFor(context.Background(), input)
			if err != nil {
				return agent.Proposal{}, err
			}
			if len(cl.downloaded) == 0 {
				return agent.Proposal{}, agent.ErrNoApproval // nothing to remove: Run says so
			}
			return cl.proposal(), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			cl, err := t.cleanupFor(ctx, input)
			if err != nil {
				return "", err
			}
			if len(cl.downloaded) == 0 {
				return fmt.Sprintf("Nothing to remove for job #%d: no verified downloaded files are waiting (already cleaned up, or the results weren't downloaded and verified yet).", cl.job.ID), nil
			}
			if !d.Approved {
				return "", errors.New("Removing files needs the user's approval, which it didn't get.")
			}
			if err := sameAsApproved(cl.proposal(), d.Proposal); err != nil {
				return "", errors.New("The list of files changed since the user approved it. Please ask again.")
			}
			// The same lock as a download, so an automatic download and a
			// cleanup of one job never overlap.
			if err := lockJob(ctx, t.env.Store, cl.job.ID, "download"); err != nil {
				return "", err
			}
			defer unlockJob(t.env.Store, cl.job.ID, "download")
			return t.runCleanup(ctx, cl)
		},
	}
}

func (t *toolset) cleanupFor(ctx context.Context, input json.RawMessage) (cleanup, error) {
	var in struct {
		JobID int64 `json:"job_id"`
	}
	if err := decode(input, &in); err != nil {
		return cleanup{}, err
	}
	j, err := t.job(ctx, in.JobID)
	if err != nil {
		return cleanup{}, err
	}
	return t.planCleanup(ctx, j)
}

// runCleanup carries out an approved cleanup.
func (t *toolset) runCleanup(ctx context.Context, cl cleanup) (string, error) {
	if t.env.LoginRunner == nil {
		return "", errors.New("Running commands on the cluster isn't available right now.")
	}
	r, err := t.env.LoginRunner(ctx, cl.cluster)
	if err != nil {
		return "", fmt.Errorf("I couldn't connect to %s: %w", cl.cluster.Name, err)
	}
	run := func(argv []string) (backend.Result, error) {
		cctx, cancel := context.WithTimeout(ctx, cleanupTimeout+remoteSlack)
		defer cancel()
		return r.Run(cctx, timeoutWire(cleanupTimeout, argv), nil)
	}

	// A job of the user's, started outside Labmill or by a work_dir, may be
	// working in the folder: ask the scheduler (read-only), and stop when it
	// can't answer.
	// squeue prints the working folder as the job saw it, which may be the
	// real path behind a symbolic link to the scratch.
	where := []string{cl.dir}
	if rl, err := run([]string{"readlink", "-f", "--", cl.dir}); err == nil && rl.ExitCode == 0 {
		if real := strings.TrimSpace(rl.Stdout); strings.HasPrefix(real, "/") && real != cl.dir {
			where = append(where, path.Clean(real))
		}
	}
	sq, err := run([]string{"squeue", "--me", "-h", "-o", "%i %Z"})
	if err == nil && sq.ExitCode != 0 { // Slurm before 20.02 has no --me
		if u, uerr := run([]string{"id", "-un"}); uerr == nil && u.ExitCode == 0 && strings.TrimSpace(u.Stdout) != "" {
			sq, err = run([]string{"squeue", "-u", strings.TrimSpace(u.Stdout), "-h", "-o", "%i %Z"})
		}
	}
	if err != nil || sq.ExitCode != 0 {
		return "", fmt.Errorf("I couldn't check the queue on %s for jobs working in %s, so I removed nothing.", cl.cluster.Name, sanitize(cl.dir))
	}
	for _, d := range where {
		if id, busy := workingIn(d, sq.Stdout); busy {
			return "", fmt.Errorf("Job %s is queued or running in %s on %s, so I removed nothing. Try again when it has ended.", sanitize(id), sanitize(cl.dir), cl.cluster.Name)
		}
	}

	// Environments and symbolic links in the folder: nothing below them goes.
	res, err := run(cleanupFindArgv(cl.dir))
	if err != nil {
		return "", fmt.Errorf("I couldn't check the folder on %s, so I removed nothing: %w", cl.cluster.Name, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("Looking for environments in %s failed (exit %d), so I removed nothing.", sanitize(cl.dir), res.ExitCode)
	}
	var skip []string // absolute prefixes, each ending in "/"
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
		case line == cl.dir:
			return "", fmt.Errorf("%s is a symbolic link, so I removed nothing.", sanitize(cl.dir))
		case path.Base(line) == "pyvenv.cfg" || path.Base(line) == "conda-meta":
			skip = append(skip, path.Dir(line)+"/")
		default: // a symbolic link
			skip = append(skip, line+"/")
		}
	}
	under := func(abs string) bool {
		for _, s := range skip {
			if strings.HasPrefix(abs, s) {
				return true
			}
		}
		return false
	}

	// Every file goes only while it still has the recorded SHA-256 on the
	// cluster (a job may have changed or replaced it since).
	kept := 0
	var candidates []string
	for _, abs := range cleanupAbs(cl.dir, cl.all()) {
		if under(abs) {
			kept++
			continue
		}
		candidates = append(candidates, abs)
	}
	var files []string
	for _, b := range batches(candidates, cleanupBatchQuote) {
		res, err := run(cleanupSumArgv(b))
		if err != nil {
			return "", fmt.Errorf("I couldn't check the files on %s, so I removed nothing: %w", cl.cluster.Name, err)
		}
		got := map[string]string{}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if len(line) > 66 && line[64:66] == "  " {
				got[line[66:]] = line[:64]
			}
		}
		for _, abs := range b {
			if g := got[abs]; g != "" && g == cl.sums[strings.TrimPrefix(abs, cl.dir+"/")] {
				files = append(files, abs)
			} else {
				kept++
			}
		}
	}
	sort.Strings(files)
	for _, b := range batches(files, cleanupBatchQuote) {
		res, err := run(cleanupRmArgv(b))
		if err != nil {
			return "", fmt.Errorf("Removing files on %s was interrupted: %w", cl.cluster.Name, err)
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("Removing files on %s failed (exit %d): %s", cl.cluster.Name, res.ExitCode, sanitize(strings.TrimSpace(res.Stderr)))
		}
	}
	// The folders that held the removed files, deepest first (a folder that
	// still holds something stays), then the job folder if it is empty.
	var dirs []string
	for _, d := range parentDirs(cl.dir, files) {
		if !under(d + "/") {
			dirs = append(dirs, d)
		}
	}
	for _, b := range batches(dirs, cleanupBatchQuote) {
		run(cleanupRmdirArgv(b))
	}
	run(cleanupRmdirArgv([]string{cl.dir}))

	rec, _ := json.Marshal(cleanedRecord{Dir: cl.dir, Files: len(files)})
	if err := t.env.Store.AddEvent(ctx, cl.job.ID, eventCleaned, string(rec)); err != nil {
		return "", fmt.Errorf("The files were removed but I couldn't record that: %w", err)
	}
	out := fmt.Sprintf("Removed %s from %s on %s.", plural(len(files), "working file", "working files"), sanitize(cl.dir), sanitize(cl.cluster.Name))
	if kept > 0 {
		out += fmt.Sprintf(" Left in place: %s (changed on the cluster, in an environment or below a symbolic link).", plural(kept, "file", "files"))
	}
	if len(files) > cleanupShown {
		out += "\nRemoved:\n"
		for _, f := range files {
			out += sanitize(strings.TrimPrefix(f, cl.dir+"/")) + "\n"
		}
	}
	return out, nil
}
