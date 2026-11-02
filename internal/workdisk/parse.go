package workdisk

import (
	"strconv"
	"strings"
)

// sizeGB converts "100G", "1.5T", "500GB", "2048M", "1024K" or a bare number to GB (rounded down). A bare number is
// taken in the unit given by bare (bytes per unit). ok is false for text that is no size.
func sizeGB(s string, bare float64) (int, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := bare
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult, s = 1<<10, s[:n-1]
		case 'M':
			mult, s = 1<<20, s[:n-1]
		case 'G':
			mult, s = 1<<30, s[:n-1]
		case 'T':
			mult, s = 1<<40, s[:n-1]
		case 'P':
			mult, s = 1<<50, s[:n-1]
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return int(v * mult / (1 << 30)), true
}

// ParseDF reads the output of `df -PT` (also with -BG or -h). Lines that do not parse are skipped, so empty or
// error output gives no mounts. Bare numbers are 1 KiB blocks when the header says "1024-blocks", else bytes.
func ParseDF(out string) []Mount {
	var ms []Mount
	bare := float64(1)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "Filesystem" {
			if strings.Contains(line, "1024-blocks") {
				bare = 1 << 10
			}
			continue
		}
		if len(f) < 7 {
			continue
		}
		size, ok1 := sizeGB(f[2], bare)
		free, ok2 := sizeGB(f[4], bare)
		if !ok1 || !ok2 {
			continue
		}
		// The mount point is the rest of the line (it may hold spaces).
		ms = append(ms, Mount{Path: strings.Join(f[6:], " "), FSType: f[1], SizeGB: size, FreeGB: free})
	}
	return ms
}

// ParseNodes reads `scontrol show node -o` and returns the largest TmpDisk in GB (-1 when no node shows one) and the
// gres names seen (without the "(S:...)" suffix). TmpDisk is in MB.
func ParseNodes(out string) (tmpGB int, gres []string) {
	tmpGB = -1
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		for _, tok := range strings.Fields(line) {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			switch k {
			case "TmpDisk":
				if mb, err := strconv.Atoi(v); err == nil && mb > 0 {
					tmpGB = max(tmpGB, mb/1024)
				}
			case "Gres":
				for _, g := range strings.Split(v, ",") {
					if i := strings.Index(g, "("); i >= 0 {
						g = g[:i]
					}
					if g != "" && g != "(null)" && !seen[g] {
						seen[g] = true
						gres = append(gres, g)
					}
				}
			}
		}
	}
	return tmpGB, gres
}

// FreeGB returns the free GB of the mount that holds path (the longest mount point that is a path prefix), or -1
// when no mount matches or path is empty.
func FreeGB(ms []Mount, path string) int {
	best, free := -1, -1
	for _, m := range ms {
		p := strings.TrimRight(m.Path, "/")
		if path != "" && (path == m.Path || strings.HasPrefix(path, p+"/")) && len(p) > best {
			best, free = len(p), m.FreeGB
		}
	}
	return free
}

// Probe is the raw read-only probe output on the login node.
type Probe struct {
	TmpDir       string // $TMPDIR
	Home         string // $HOME
	SharedRoot   string // shared scratch folder from the profile, "" if none
	DF           string // df -PT of the candidate paths
	Nodes        string // scontrol show node -o
	HasWorkspace bool
	WSMaxDays    int
	Today        string
}

// BuildFacts parses the probe output into Facts. Unknown sizes are -1. Empty or hidden output gives no mounts and
// unknown sizes, so the ladder falls back to home (S6).
func BuildFacts(p Probe) Facts {
	ms := ParseDF(p.DF)
	tmp, gres := ParseNodes(p.Nodes)
	return Facts{
		TmpDirEnv: p.TmpDir, Mounts: ms, NodeTmpDiskGB: tmp, Gres: gres,
		HasWorkspace: p.HasWorkspace, WSMaxDays: p.WSMaxDays, SharedRoot: p.SharedRoot,
		SharedFreeGB: FreeGB(ms, p.SharedRoot), HomeFreeGB: FreeGB(ms, p.Home), Today: p.Today,
	}
}
