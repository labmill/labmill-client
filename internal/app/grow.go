package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// Answer 3 of the owner (2026-11-11): a job that ended in TIMEOUT or OUT_OF_MEMORY may be resubmitted once by
// Labmill itself with more time or memory. The count is saved as an eventResubmitted event with Grow set, and the
// report of the job carries it (account.JobIn.Resubmissions). Labmill adds no limit of its own: when the new
// value is above a site limit, sbatch refuses it and the normal "couldn't resubmit" path tells the user.

// growFactor is how much the time or the memory grows.
const growFactor = 2

// growCause says what a failed Slurm status asks to grow: "time", "memory" or "" (nothing to grow).
func growCause(st backend.Status) string {
	if st.State != store.StateFailed {
		return ""
	}
	switch st.Reason {
	case "TIMEOUT":
		return "time"
	case "OUT_OF_MEMORY":
		return "memory"
	}
	return ""
}

var (
	growTimeRE = regexp.MustCompile(`^(?:(\d+)-)?(\d+)(?::(\d+))?(?::(\d+))?$`)
	growMemRE  = regexp.MustCompile(`^(\d+)([KMGT]?)$`)
)

// growTime doubles a Slurm time value ("MM", "MM:SS", "HH:MM:SS", "D-HH", "D-HH:MM", "D-HH:MM:SS") and writes it
// as D-HH:MM:SS. ok is false for a value it can't read (UNLIMITED, 0).
func growTime(v string) (string, bool) {
	m := growTimeRE.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	n := func(s string) int { i, _ := strconv.Atoi(s); return i }
	var d time.Duration
	switch {
	case m[1] != "": // D-HH[:MM[:SS]]
		d = time.Duration(n(m[1]))*24*time.Hour + time.Duration(n(m[2]))*time.Hour +
			time.Duration(n(m[3]))*time.Minute + time.Duration(n(m[4]))*time.Second
	case m[4] != "": // HH:MM:SS
		d = time.Duration(n(m[2]))*time.Hour + time.Duration(n(m[3]))*time.Minute + time.Duration(n(m[4]))*time.Second
	case m[3] != "": // MM:SS
		d = time.Duration(n(m[2]))*time.Minute + time.Duration(n(m[3]))*time.Second
	default: // MM
		d = time.Duration(n(m[2])) * time.Minute
	}
	if d <= 0 {
		return "", false
	}
	d *= growFactor
	secs := int(d / time.Second)
	return fmt.Sprintf("%d-%02d:%02d:%02d", secs/86400, secs%86400/3600, secs%3600/60, secs%60), true
}

// growMem doubles a Slurm memory value ("4G", "2000M", "512"); a bare number is megabytes.
func growMem(v string) (string, bool) {
	m := growMemRE.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 || n > 1<<40 {
		return "", false
	}
	return strconv.Itoa(n*growFactor) + m[2], true
}

// growScript doubles the time (cause "time": --time, -t) or the memory (cause "memory": --mem, --mem-per-cpu) in
// the #SBATCH header of an approved script. changes names each edit for the chat. ok is false when the header has
// no readable line for it (Slurm then used a default Labmill can't read), so nothing is resubmitted.
func growScript(script, cause string) (out string, changes []string, ok bool) {
	lines := strings.Split(script, "\n")
	inHeader := true
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if inHeader && t != "" && !strings.HasPrefix(t, "#") {
			inHeader = false
		}
		if !inHeader || !strings.HasPrefix(t, "#SBATCH") {
			continue
		}
		body, _, _ := strings.Cut(strings.TrimPrefix(t, "#SBATCH"), " #")
		opt := strings.Fields(body)
		if len(opt) == 0 || len(changes) > 0 {
			continue
		}
		key, val, hasEq := strings.Cut(opt[0], "=")
		switch {
		case len(opt) == 2 && !hasEq:
			val = opt[1]
		case len(opt) != 1 || !hasEq:
			continue
		}
		var nv string
		var good bool
		switch {
		case cause == "time" && (key == "--time" || key == "-t"):
			nv, good = growTime(val)
		case cause == "memory" && (key == "--mem" || key == "--mem-per-cpu"):
			nv, good = growMem(val)
		}
		if !good {
			continue
		}
		if key == "-t" {
			key = "--time"
		}
		nl := "#SBATCH " + key + "=" + nv
		changes = append(changes, t+" -> "+nl)
		lines[i] = nl
	}
	return strings.Join(lines, "\n"), changes, len(changes) > 0
}

// usedGrow tells whether a job already had its one growing resubmission.
func usedGrow(re resubmitEvent) bool { return re.Grow != "" }
