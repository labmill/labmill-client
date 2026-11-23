package account

import (
	"fmt"
	"regexp"
)

// Condensing a job's output for its report (after redaction): repeats are
// collapsed, then the first and last lines and the lines around errors are
// kept, within a byte budget.
const (
	OutputBudget  = 64 << 10 // all output text of one report (stderr and stdout)
	maxOutputRead = 3 << 20  // bytes read of one output file (the SSH runner cuts a stream at 4 MB)
	condenseHead  = 100
	condenseTail  = 300
	condenseCtx   = 3    // lines around an error line
	condenseLines = 2000 // at most this many lines of one stream
)

// markerRE matches the lines this file writes. Redaction leaves them alone.
var markerRE = regexp.MustCompile(`^⋮ \(\d+ (?:similar lines|lines skipped)\)$`)

var (
	errorLineRE = regexp.MustCompile(`(?i)traceback|error|exception|\bkilled\b|\boom\b|out of memory|slurmstepd|exit(?:ed)? (?:with )?(?:code|status)|segmentation fault|core dumped|\bfatal\b|\bpanic\b|cancelled|time limit`)
	hexRE       = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	digitsRE    = regexp.MustCompile(`\d+`)
	barRE       = regexp.MustCompile(` ?[█▉▊▋▌▍▎▏░▒▓#=>]+ ?`)
	spaceRE     = regexp.MustCompile(`\s+`)
)

// similarKey is l with the parts that change from line to line (numbers,
// hex ids, timestamps, progress bars) replaced.
func similarKey(l string) string {
	l = hexRE.ReplaceAllString(l, "h")
	l = digitsRE.ReplaceAllString(spaceRE.ReplaceAllString(l, " "), "0")
	return barRE.ReplaceAllString(l, "#")
}

// collapse replaces each run of three or more similar lines by its first
// line, "⋮ (N similar lines)" and its last line (N counts the ones left out).
func collapse(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		k := similarKey(lines[i])
		j := i + 1
		for j < len(lines) && similarKey(lines[j]) == k {
			j++
		}
		if j-i >= 3 {
			out = append(out, lines[i], fmt.Sprintf("⋮ (%d similar lines)", j-i-2), lines[j-1])
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return out
}

type condensePlan struct{ head, tail, blocks int } // blocks: error blocks kept (0: all)

// condensePlans go from the full plan to smaller ones until the text fits.
var condensePlans = []condensePlan{
	{condenseHead, condenseTail, 0}, {condenseHead, condenseTail, 64}, {50, 150, 32}, {25, 75, 16}, {10, 30, 8}, {0, 10, 4},
}

// Condense reduces redacted lines to at most budget bytes: runs of similar
// lines are collapsed, then the first 100 and the last 300 lines are kept
// whole, and error lines with 3 lines around them from the middle; each cut
// is marked "⋮ (N lines skipped)". When that is still too much, the plan
// shrinks. The result is nil for no lines.
func Condense(lines []string, budget int) []string {
	lines = collapse(lines)
	for _, p := range condensePlans {
		if out := pick(lines, p); fits(out, budget) {
			return out
		}
	}
	// Last resort: the end of the text.
	var out []string
	n := 0
	for i := len(lines) - 1; i >= 0 && len(out) < condenseLines-1; i-- {
		if n += len(lines[i]) + 1; n > budget-40 {
			break
		}
		out = append(out, lines[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if skipped := len(lines) - len(out); skipped > 0 {
		out = append([]string{fmt.Sprintf("⋮ (%d lines skipped)", skipped)}, out...)
	}
	return out
}

func fits(lines []string, budget int) bool {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n <= budget && len(lines) <= condenseLines
}

// pick keeps the head, the tail and the error blocks of lines.
func pick(lines []string, p condensePlan) []string {
	n := len(lines)
	if n <= p.head+p.tail {
		return lines
	}
	keep := make([]bool, n)
	for i := 0; i < p.head; i++ {
		keep[i] = true
	}
	for i := n - p.tail; i < n; i++ {
		keep[i] = true
	}
	// Error blocks of the middle, as [from, to) ranges that touch merge.
	var blocks [][2]int
	for i := p.head; i < n-p.tail; i++ {
		if !errorLineRE.MatchString(lines[i]) {
			continue
		}
		from, to := max(i-condenseCtx, p.head), min(i+condenseCtx+1, n-p.tail)
		if k := len(blocks); k > 0 && from <= blocks[k-1][1] {
			blocks[k-1][1] = to
		} else {
			blocks = append(blocks, [2]int{from, to})
		}
	}
	if p.blocks > 0 && len(blocks) > p.blocks { // keep the first and the last ones
		first := p.blocks / 2
		blocks = append(blocks[:first:first], blocks[len(blocks)-(p.blocks-first):]...)
	}
	for _, b := range blocks {
		for i := b[0]; i < b[1]; i++ {
			keep[i] = true
		}
	}
	var out []string
	for i := 0; i < n; {
		if keep[i] {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i
		for j < n && !keep[j] {
			j++
		}
		out = append(out, fmt.Sprintf("⋮ (%d lines skipped)", j-i))
		i = j
	}
	return out
}

// CondenseOutput redacts and condenses both streams within OutputBudget:
// stderr first (at most half), stdout gets what is left.
func CondenseOutput(red Redactor, stdout, stderr []string) (so, se []string) {
	se = Condense(red.Lines(stderr), OutputBudget/2)
	used := 0
	for _, l := range se {
		used += len(l) + 1
	}
	return Condense(red.Lines(stdout), OutputBudget-used), se
}
