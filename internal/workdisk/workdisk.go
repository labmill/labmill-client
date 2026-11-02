// Package workdisk chooses where a Slurm job keeps its working files, and what to try next when that fails.
// It is pure Go on recorded command output (no network, no shell, only the standard library), so the client can
// copy it. Design: docs/work-disk.md. Rule of the owner (2026-10-26): a work directory is always obtained. The ladder
// of Choose never ends in "none": its last step is a folder in the home directory.
//
// Sizes in GB: -1 means unknown. 0 means known to be empty or full.
package workdisk

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kind is where the files live: on one node (local) or on storage that all nodes see (shared).
type Kind string

const (
	Local  Kind = "local"
	Shared Kind = "shared"
)

// Option ids are the technique ids of docs/work-disk.md, section 2a.
const (
	L1 = "L1" // $TMPDIR set by Slurm
	L2 = "L2" // --tmp=<size>
	L3 = "L3" // gres scratch
	L4 = "L4" // fixed local path with a per-job folder
	L5 = "L5" // /tmp
	S1 = "S1" // workspace tool (ws_allocate, run on the login node by an approved tool; Path is its result)
	S2 = "S2" // default scratch folder
	S3 = "S3" // own folder below a probed shared type
	S4 = "S4" // per-job shared gres (the name is a site fact, Facts.PerJobGres)
	S6 = "S6" // home (last)
)

// Option is one way to get a work directory.
type Option struct {
	ID         string
	Kind       Kind
	Path       string // folder (L3 with a probed path, L4, S1 result, S2, S3, L5)
	Gres       string // gres name for L3 and S4
	SizeGB     int    // size to request (L2, L3, S4)
	Days       int    // S1: workspace lifetime
	TmpFS      bool   // the path is memory
	LowSpace   bool   // S6: home may be too small (HomeFreeGB below twice the staged size)
	Confidence string // high | medium | low
}

// TmpBased is true when the work folder is below $TMPDIR. After a full disk or tmpfs there, all of them are out.
func (o Option) TmpBased() bool { return o.ID == L1 || o.ID == L2 || (o.ID == L3 && o.Path == "") }

// Mount is one line of `df -PT`.
type Mount struct {
	Path   string
	FSType string
	SizeGB int
	FreeGB int
}

// Facts are what the probe found.
type Facts struct {
	TmpDirEnv       string  // value of $TMPDIR on the login node ("" when unset)
	Mounts          []Mount // df -PT of the candidate paths
	NodeTmpDiskGB   int     // largest TmpDisk of a compute node (scontrol show node); -1 or 0: unknown
	Gres            []string
	GresPath        string // where the scratch gres is mounted, when the probe shows it (else $TMPDIR is used)
	PerJobGres      string // the site's per-job shared gres name (for example from the profile); "" if none
	HasWorkspace    bool   // ws_allocate exists
	WSMaxDays       int    // site maximum of ws_allocate (0: unknown)
	SharedRoot      string // default shared scratch folder, from the profile or the probe (without the user)
	SharedFreeGB    int    // -1 unknown
	HomeFreeGB      int    // -1 unknown
	FromCompute     bool   // the facts come from a compute node (test job), not from the login node
	Today           string // yyyy-mm-dd; empty: failure records never expire
	FailedByProfile []Failure
}

// Job is what the job needs.
type Job struct {
	NeedGB    int // staged input plus expected output
	MemGB     int // requested memory (decides whether a tmpfs path may be used)
	MultiNode bool
	Hours     int // wall time of the job
}

// Prefs are the user settings (docs/work-disk.md, section 6).
type Prefs struct {
	Prefer      string // "local" (default) or "shared"
	OnFailure   string // "retry_same_class" (default) or "ask"
	MaxAttempts int    // default 3
}

func (p Prefs) maxAttempts() int {
	if p.MaxAttempts < 1 {
		return 3
	}
	return p.MaxAttempts
}

// localPaths are fixed node-local folders that sites use. A path counts only when the probe saw it as a mount.
var localPaths = []string{"/scratch/local", "/lscratch", "/localscratch", "/local", "/scratch-local"}

// scratchGres are gres names that stand for local scratch.
var scratchGres = []string{"lscratch", "scratch", "localtmp", "tmp"}

func tmpfs(fs string) bool { return fs == "tmpfs" || fs == "ramfs" || fs == "devtmpfs" }

// fits is true when a known free space can hold need. Unknown (-1), full (0) and need 0 on a full mount all fail.
func fits(free, need int) bool { return free > 0 && free >= need }

func (f Facts) mount(path string) (Mount, bool) {
	for _, m := range f.Mounts {
		if m.Path == path {
			return m, true
		}
	}
	return Mount{}, false
}

func (f Facts) hasGres(name string) bool {
	for _, g := range f.Gres {
		if g == name || strings.HasPrefix(g, name+":") {
			return true
		}
	}
	return false
}

// profileState: a high failure under 180 days old removes the option; a medium one moves it last; others are notes.
// Without Today nothing expires.
func profileState(fs []Failure, o Option, today string) (drop, last bool) {
	for _, f := range fs {
		if f.Technique != o.ID {
			continue
		}
		if f.Confidence == "high" && !expired(f.Date, today) {
			drop = true
		}
		if f.Confidence == "medium" && !expired(f.Date, today) {
			last = true
		}
	}
	return
}

func expired(date, today string) bool {
	if today == "" {
		return false
	}
	d, err1 := time.Parse("2006-01-02", date)
	t, err2 := time.Parse("2006-01-02", today)
	return err1 == nil && err2 == nil && t.Sub(d) > 180*24*time.Hour
}

// Choose returns the ladder: the options in the order to try them. The first is the best guess. The ladder always
// ends with the home folder (S6), so it is never empty. With Prefer=shared, or a multi-node job, no local option is
// in it. Options with a recent high failure in the profile are left out; medium ones go last (before S6).
func Choose(f Facts, j Job, p Prefs) []Option {
	conf := "medium"
	if !f.FromCompute {
		conf = "low"
	}
	var local, shared []Option

	if !j.MultiNode && p.Prefer != "shared" {
		if f.TmpDirEnv != "" {
			m, seen := f.mount(f.TmpDirEnv)
			ram := seen && tmpfs(m.FSType)
			if !ram || (j.MemGB > 0 && j.NeedGB*2 <= j.MemGB) {
				local = append(local, Option{ID: L1, Kind: Local, Path: f.TmpDirEnv, TmpFS: ram, Confidence: conf})
			}
		} else if f.NodeTmpDiskGB <= 0 && len(f.Mounts) == 0 && len(f.Gres) == 0 {
			// Nothing is known: Slurm often sets $TMPDIR in the job. Script reports it if it is not set.
			local = append(local, Option{ID: L1, Kind: Local, Confidence: "low"})
		}
		if j.NeedGB > 0 && f.NodeTmpDiskGB >= j.NeedGB {
			local = append(local, Option{ID: L2, Kind: Local, SizeGB: j.NeedGB, Confidence: conf})
		}
		for _, g := range scratchGres {
			if f.hasGres(g) {
				local = append(local, Option{ID: L3, Kind: Local, Gres: g, Path: f.GresPath, SizeGB: max(j.NeedGB, 1), Confidence: conf})
				break
			}
		}
		for _, path := range localPaths {
			if m, ok := f.mount(path); ok && !tmpfs(m.FSType) && fits(m.FreeGB, j.NeedGB) {
				local = append(local, Option{ID: L4, Kind: Local, Path: path, Confidence: conf})
				break
			}
		}
		if m, ok := f.mount("/tmp"); ok && !tmpfs(m.FSType) && fits(m.FreeGB, j.NeedGB) {
			local = append(local, Option{ID: L5, Kind: Local, Path: "/tmp", Confidence: conf})
		}
	}

	if f.PerJobGres != "" && f.hasGres(f.PerJobGres) {
		shared = append(shared, Option{ID: S4, Kind: Shared, Gres: f.PerJobGres, SizeGB: j.NeedGB, Confidence: conf})
	}
	if f.HasWorkspace {
		shared = append(shared, Option{ID: S1, Kind: Shared, SizeGB: j.NeedGB, Days: wsDays(j.Hours, f.WSMaxDays), Confidence: conf})
	}
	if f.SharedRoot != "" && (f.SharedFreeGB < 0 || fits(f.SharedFreeGB, j.NeedGB)) {
		shared = append(shared, Option{ID: S2, Kind: Shared, Path: f.SharedRoot, Confidence: conf})
	}
	for _, m := range f.Mounts {
		switch m.FSType {
		case "lustre", "gpfs", "beegfs", "ceph", "nfs4", "nfs", "wekafs", "panfs":
			if m.Path != f.SharedRoot && !strings.HasPrefix(m.Path, "/home") && fits(m.FreeGB, j.NeedGB) {
				shared = append(shared, Option{ID: S3, Kind: Shared, Path: m.Path, Confidence: conf})
			}
		}
	}

	order := append(append([]Option{}, local...), shared...)
	if j.MultiNode || p.Prefer == "shared" {
		order = shared
	}
	var first, late []Option
	for _, o := range order {
		drop, last := profileState(f.FailedByProfile, o, f.Today)
		switch {
		case drop:
		case last:
			late = append(late, o)
		default:
			first = append(first, o)
		}
	}
	// The last step always exists: a folder in home. Twice the staged size must fit; if not, the caller is warned.
	home := Option{ID: S6, Kind: Shared, Confidence: "low", LowSpace: f.HomeFreeGB >= 0 && f.HomeFreeGB < 2*j.NeedGB}
	return append(append(first, late...), home)
}

// ErrNoOption means every option of the ladder has failed or the attempt limit is reached.
var ErrNoOption = errors.New("workdisk: no option left")

// Attempt is one try and its result.
type Attempt struct {
	Option Option
	Code   string // "" for success, else F1..F7
}

// Next picks the option for the next attempt. history holds the failed attempts of this job on this cluster. It
// returns ask=true when the user must approve the resubmit: the class changes (local to shared or back), or the
// user set on_failure=ask. A repeated request is never returned. After the attempt limit it returns ErrNoOption.
// After F1 (a rejected size) the same option is retried once with a smaller size (Shrink). After F4 or F6 on a
// $TMPDIR path, no option below $TMPDIR is returned again. F5 (a local disk too small) leaves the local class.
func Next(ladder []Option, history []Attempt, p Prefs, f Facts) (opt Option, ask bool, err error) {
	if len(history) >= p.maxAttempts() {
		return Option{}, false, ErrNoOption
	}
	if n := len(history); n > 0 && history[n-1].Code == "F1" {
		if s, ok := Shrink(history[n-1].Option, f); ok && !failedBefore(history, s) {
			return s, p.OnFailure == "ask", nil
		}
	}
	skipLocal, skipTmp := false, false
	for _, a := range history {
		switch a.Code {
		case "F5":
			skipLocal = true
		case "F4", "F6":
			if a.Option.TmpBased() {
				skipTmp = true
			}
		}
	}
	for _, o := range ladder {
		if (skipLocal && o.Kind == Local) || (skipTmp && o.TmpBased()) || failedBefore(history, o) {
			continue
		}
		if len(history) > 0 {
			ask = history[len(history)-1].Option.Kind != o.Kind || p.OnFailure == "ask"
		}
		return o, ask, nil
	}
	return Option{}, false, ErrNoOption
}

// failedBefore is true when the same request failed: the same id, path and gres. For F4 and F6 the same path is out
// whatever the technique. A smaller size after F1 is a new request.
func failedBefore(history []Attempt, o Option) bool {
	for _, a := range history {
		same := a.Option.ID == o.ID && a.Option.Path == o.Path && a.Option.Gres == o.Gres
		switch a.Code {
		case "F1":
			if same && a.Option.SizeGB <= o.SizeGB {
				return true
			}
		case "F4", "F6":
			if same || (a.Option.Path != "" && a.Option.Path == o.Path) {
				return true
			}
		default:
			if same {
				return true
			}
		}
	}
	return false
}

// Shrink returns o with a size that fits a rejected --tmp or gres request (80 % of the largest node disk), or false
// when the node size is unknown or the request is already that small. It is the "smaller --tmp" retry of F1.
func Shrink(o Option, f Facts) (Option, bool) {
	if f.NodeTmpDiskGB <= 0 {
		return o, false
	}
	n := f.NodeTmpDiskGB * 8 / 10
	if n < 1 || n >= o.SizeGB {
		return o, false
	}
	o.SizeGB = n
	return o, true
}

// Failure is one record for the site profile (work_disk.failed).
type Failure struct {
	Technique  string
	Code       string
	Date       string // yyyy-mm-dd
	Detail     string
	Confidence string // high | medium | low
}

// Record builds the profile record of a failed attempt. A code that the scheduler or the log names (F1 to F4, F6,
// F7) is "high" when the text is from a compute node log, else "medium"; F5 and unknown are "low".
func Record(a Attempt, detail, date string, fromLog bool) Failure {
	conf := "low"
	switch a.Code {
	case "F1", "F2", "F3", "F4", "F6", "F7":
		conf = "medium"
		if fromLog {
			conf = "high"
		}
	}
	detail = strings.Join(strings.Fields(detail), " ")
	if r := []rune(detail); len(r) > 200 {
		detail = string(r[:200])
	}
	return Failure{Technique: a.Option.ID, Code: a.Code, Date: date, Detail: detail, Confidence: conf}
}

var classRules = []struct {
	code string
	re   *regexp.Regexp
}{
	{"F2", regexp.MustCompile(`(?i)invalid generic resource|unknown gres|gres.*(not (found|configured)|invalid)`)},
	{"F1", regexp.MustCompile(`(?i)invalid (tmp|temporary|disk)|requested (node configuration|tmp)|tmp.*(too large|not available)|invalid.*--tmp|node configuration is not available`)},
	{"F7", regexp.MustCompile(`(?i)ws_allocate.*(fail|error|denied|no such)|workspace.*(expired|not found|does not exist|no longer)`)},
	{"F4", regexp.MustCompile(`(?i)no space left|disk quota exceeded|quota exceeded|not enough (free )?(space|disk)`)},
	{"F3", regexp.MustCompile(`(?i)mkdir: cannot create|cannot create directory|read-only file system|labmill: work dir fell back`)},
}

// Classify maps error text (sbatch stderr, the end of slurm-*.out, sacct lines) to a failure code F1..F7, or "" when
// nothing matches. state is the sacct State (may be empty); o is the option that was used. A full disk is F6 on
// tmpfs or after an out-of-memory kill, F5 on a local option (the local disk is too small), else F4 (quota or full
// shared space). Text is data; it is only matched, never run.
func Classify(text, state string, o Option) string {
	for _, r := range classRules {
		if !r.re.MatchString(text) {
			continue
		}
		if r.code == "F4" {
			switch {
			case o.TmpFS || strings.Contains(text, "tmpfs") || state == "OUT_OF_MEMORY":
				return "F6"
			case o.Kind == Local:
				return "F5"
			}
		}
		return r.code
	}
	if state == "OUT_OF_MEMORY" && (o.TmpFS || strings.Contains(text, "tmpfs")) {
		return "F6"
	}
	return ""
}

var (
	safePath = regexp.MustCompile(`^[A-Za-z0-9_.:/+-]+$`)
	safeName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// Script returns the #SBATCH lines and the setup lines of the job script for an option. The folder is WORKDIR. Paths
// and gres names come from probe output, so they are checked; a bad one gives an error.
//
// Cleanup: only a local folder is removed at exit, and only when the caller's lm_stage_out function exists and
// succeeds (a failed or missing copy keeps the folder). Shared
// folders, workspaces and the home folder are never removed by the script; they go only through the approved
// cleanup tool. If the chosen place cannot be made (for example $TMPDIR is unset), the script reports it on stderr
// ("labmill: work dir fell back", class F3), uses a folder in home and removes nothing at exit.
// S1 needs Path: the workspace that the approved login-node tool allocated (ws_allocate is not run in the job).
func Script(o Option, jobName string) (sbatch, setup []string, err error) {
	for _, v := range []string{o.Path, o.Gres} {
		if v != "" && !safePath.MatchString(v) {
			return nil, nil, fmt.Errorf("workdisk: unsafe value %q", v)
		}
	}
	if !safeName.MatchString(jobName) {
		return nil, nil, fmt.Errorf("workdisk: unsafe job name %q", jobName)
	}
	var base string
	switch o.ID {
	case L1:
		base = `${TMPDIR:-}`
	case L2:
		sbatch = append(sbatch, fmt.Sprintf("#SBATCH --tmp=%dG", max(o.SizeGB, 1)))
		base = `${TMPDIR:-}`
	case L3:
		if o.Gres == "" {
			return nil, nil, errors.New("workdisk: L3 needs a gres name")
		}
		sbatch = append(sbatch, fmt.Sprintf("#SBATCH --gres=%s:%d", o.Gres, max(o.SizeGB, 1)))
		base = `${TMPDIR:-}`
		if o.Path != "" {
			base = o.Path + "/$USER"
		}
	case L4, L5, S2, S3:
		base = o.Path + "/$USER"
	case S1:
		if o.Path == "" {
			return nil, nil, errors.New("workdisk: S1 needs the allocated workspace path")
		}
		base = o.Path
	case S4:
		if o.Gres == "" {
			return nil, nil, errors.New("workdisk: S4 needs the gres name")
		}
		sbatch = append(sbatch, "#SBATCH --gres="+o.Gres)
		base = `${PFSDIR:-}`
	default: // S6
		base = `$HOME/labmill-work`
	}
	setup = append(setup,
		fmt.Sprintf(`LM_BASE=%s`, base),
		`LM_HOME="$HOME/labmill-work"`,
		fmt.Sprintf(`WORKDIR="${LM_BASE:-$LM_HOME}/labmill-%s-${SLURM_JOB_ID:-$$}"`, jobName),
		fmt.Sprintf(`LM_CLEAN=%d`, btoi(o.Kind == Local)),
		`if [ -z "$LM_BASE" ] || ! mkdir -p "$WORKDIR" 2>/dev/null; then`,
		`  echo "labmill: work dir fell back to home (planned: ${WORKDIR:-none})" >&2`,
		`  WORKDIR="$LM_HOME/labmill-${SLURM_JOB_ID:-$$}"; LM_CLEAN=0; mkdir -p "$WORKDIR"`,
		`fi`,
	)
	if o.Kind == Local {
		setup = append(setup, `[ "$LM_CLEAN" = 1 ] && trap 'declare -F lm_stage_out >/dev/null && lm_stage_out && rm -rf -- "$WORKDIR"' EXIT`)
	}
	return sbatch, setup, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// wsDays is the workspace lifetime: job time plus 2 days (owner default), at most the site maximum.
func wsDays(hours, siteMax int) int {
	d := (hours+23)/24 + 2
	if siteMax > 0 && d > siteMax {
		d = siteMax
	}
	return d
}
