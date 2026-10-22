package launch

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// maxLiveTime, maxLiveCPUs and maxLiveMemMB bound what TestLiveAgent's guard
// lets the model submit.
const (
	maxLiveTimeMin = 15
	maxLiveCPUs    = 2
	maxLiveMemMB   = 4096
)

// sbatchValue returns (the last of)  the value of "#SBATCH --name=value" (or "--name value").
func sbatchValue(script, name string) (string, bool) {
	re := regexp.MustCompile(`(?m)^#SBATCH\s+--` + regexp.QuoteMeta(name) + `(?:=|\s+)(\S+)\s*$`)
	all := re.FindAllStringSubmatch(script, -1)
	if len(all) == 0 {
		return "", false
	}
	return all[len(all)-1][1], true // the last directive wins, as in sbatch
}

// slurmMinutes parses a Slurm --time value (M, M:S, H:M:S, D-H, D-H:M,
// D-H:M:S) to whole minutes, rounding up.
func slurmMinutes(v string) (int, error) {
	bad := fmt.Errorf("bad time %q", v)
	days, hasDays := 0, false
	if d, rest, ok := strings.Cut(v, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, bad
		}
		days, hasDays, v = n, true, rest
	}
	var nums []int
	for _, p := range strings.Split(v, ":") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, bad
		}
		nums = append(nums, n)
	}
	var h, m, s int
	switch {
	case hasDays && len(nums) <= 3: // D-H, D-H:M, D-H:M:S
		h = nums[0]
		if len(nums) > 1 {
			m = nums[1]
		}
		if len(nums) > 2 {
			s = nums[2]
		}
	case !hasDays && len(nums) == 1:
		m = nums[0]
	case !hasDays && len(nums) == 2:
		m, s = nums[0], nums[1]
	case !hasDays && len(nums) == 3:
		h, m, s = nums[0], nums[1], nums[2]
	default:
		return 0, bad
	}
	return days*24*60 + h*60 + m + (s+59)/60, nil
}

// memMB parses a Slurm --mem value (default unit MB; K, M, G, T suffixes).
func memMB(v string) (int, error) {
	mult := 1.0
	if n := len(v); n > 0 {
		switch v[n-1] {
		case 'K', 'k':
			mult, v = 1.0/1024, v[:n-1]
		case 'M', 'm':
			v = v[:n-1]
		case 'G', 'g':
			mult, v = 1024, v[:n-1]
		case 'T', 't':
			mult, v = 1024*1024, v[:n-1]
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad memory size")
	}
	return int(n * mult), nil
}

// checkLiveScript returns a reason the guard refuses a job script, or "".
func checkLiveScript(script string) string {
	low := strings.ToLower(script)
	switch {
	case strings.Contains(low, "gres") || strings.Contains(low, "gpu"):
		return "this must be a CPU-only job: request no GPU and no GPU partition"
	case !regexp.MustCompile(`(?m)^#SBATCH --job-name=labmill-live[\w-]*\s*$`).MatchString(script):
		return "the job must be named labmill-live-agent"
	}
	var body []string
	for _, l := range strings.Split(low, "\n") {
		if !strings.HasPrefix(l, "#sbatch") {
			body = append(body, l)
		}
	}
	text := strings.Join(body, "\n")
	for _, bad := range []string{"sbatch", "scancel", "rm -r", "rm -f", "curl", "wget", "ssh "} {
		if strings.Contains(text, bad) {
			return fmt.Sprintf("the script may not contain %q; it only counts lines in a file", bad)
		}
	}
	tv, ok := sbatchValue(script, "time")
	if !ok {
		return fmt.Sprintf("please set #SBATCH --time to at most %d minutes", maxLiveTimeMin)
	}
	if m, err := slurmMinutes(tv); err != nil || m > maxLiveTimeMin {
		return fmt.Sprintf("--time must be at most %d minutes", maxLiveTimeMin)
	}
	if v, ok := sbatchValue(script, "cpus-per-task"); ok {
		if n, err := strconv.Atoi(v); err != nil || n > maxLiveCPUs {
			return fmt.Sprintf("--cpus-per-task must be at most %d", maxLiveCPUs)
		}
	}
	for _, opt := range []string{"mem", "mem-per-cpu"} {
		if v, ok := sbatchValue(script, opt); ok {
			if m, err := memMB(v); err != nil || m > maxLiveMemMB {
				return "memory must be at most 4G"
			}
		}
	}
	if v, ok := sbatchValue(script, "ntasks"); ok {
		if n, err := strconv.Atoi(v); err != nil || n > 1 {
			return "--ntasks must be 1"
		}
	}
	if _, ok := sbatchValue(script, "nodes"); ok {
		if v, _ := sbatchValue(script, "nodes"); v != "1" {
			return "--nodes must be 1"
		}
	}
	return ""
}
