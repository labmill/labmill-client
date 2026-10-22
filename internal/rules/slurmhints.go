package rules

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// SlurmHints are a Slurm cluster's placement facts (Cluster.yaml
// preemptible:, local_scratch:, global_scratch:; M61): where a job's working
// data goes and which preemptible resource the cluster has. Every part is
// optional; a nil part means the profile does not say (the normal rules then
// apply). They come from the site profile, never from code.
type SlurmHints struct {
	// Preemptible is the cluster's preemptible partition or QOS, if any.
	Preemptible *Preemptible
	// LocalScratch is the node-local scratch of the compute nodes.
	LocalScratch *LocalScratch
	// GlobalScratch is a shared scratch mounted on the compute nodes.
	GlobalScratch *GlobalScratch
	// SoftwareDir is the folder for reusable software environments (venvs,
	// conda environments, container images; M67.1): absolute and clean. "" means
	// $HOME/.labmill/envs. It should not be a purged scratch.
	SoftwareDir string `yaml:"software_dir"`
}

// Preemptible is a cluster's preemptible resource.
type Preemptible struct {
	// Partition is the preemptible partition ("" when only a QOS is used).
	Partition string `yaml:"partition"`
	// QOS is the QOS to submit with (--qos), "" for none.
	QOS string `yaml:"qos"`
	// Requeue says jobs may be submitted with --requeue (a preempted job is
	// then put back in the queue by Slurm).
	Requeue bool `yaml:"requeue"`
	// FallbackPartition is the non-preemptible partition to use after the
	// failed preemptible attempts; "" means the cluster's default partition.
	FallbackPartition string `yaml:"fallback_partition"`
}

// LocalScratch is the node-local scratch of the compute nodes.
type LocalScratch struct {
	// Path is the node-local folder ("/tmp") or a variable ("$TMPDIR");
	// may be empty when EnvVar is set.
	Path string `yaml:"path"`
	// EnvVar is the environment variable Slurm or the site sets to the job's
	// node-local folder (TMPDIR, SLURM_TMPDIR); it wins over Path at run
	// time, and Path is the fallback.
	EnvVar string `yaml:"env_var"`
	// SizeGB is the size per node in GB; 0 means unknown.
	SizeGB int `yaml:"size_gb"`
}

// GlobalScratch is a shared scratch on the compute nodes.
type GlobalScratch struct {
	// Path is the folder; a <placeholder> (<user>, <group>) marks a part the
	// user's own value replaces.
	Path string `yaml:"path"`
	// PurgeDays is after how many days files are purged; 0 means unknown.
	PurgeDays int `yaml:"purge_days"`
}

// Zero reports whether the profile says nothing.
func (h SlurmHints) Zero() bool {
	return h.Preemptible == nil && h.LocalScratch == nil && h.GlobalScratch == nil && h.SoftwareDir == ""
}

var (
	slurmNameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	envVarNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	localPathRe    = regexp.MustCompile(`^(/|\$[A-Za-z_][A-Za-z0-9_]*/?)[A-Za-z0-9._/+@:~-]*$`)
	placeholderRe  = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9_-]*>`)
	globalPathRe   = regexp.MustCompile(`^/[A-Za-z0-9._/+@:~<>-]*$`)
	softwarePathRe = regexp.MustCompile(`^/[A-Za-z0-9._/+-]*$`)
)

const maxScratchDays = 3650

// Validate checks every part that is set. The errors are joined.
func (h SlurmHints) Validate() error {
	var errs []error
	if p := h.Preemptible; p != nil {
		if p.Partition == "" && p.QOS == "" {
			errs = append(errs, errors.New("preemptible: name a partition or a qos"))
		}
		for _, f := range []struct{ key, val string }{{"partition", p.Partition}, {"qos", p.QOS}, {"fallback_partition", p.FallbackPartition}} {
			if f.val != "" && !slurmNameRe.MatchString(f.val) {
				errs = append(errs, fmt.Errorf("preemptible.%s %q must match [A-Za-z0-9][A-Za-z0-9._-]*", f.key, f.val))
			}
		}
		if p.FallbackPartition != "" && p.FallbackPartition == p.Partition {
			errs = append(errs, errors.New("preemptible.fallback_partition must differ from the preemptible partition"))
		}
	}
	if l := h.LocalScratch; l != nil {
		if l.Path == "" && l.EnvVar == "" {
			errs = append(errs, errors.New("local_scratch: set path or env_var"))
		}
		if l.Path != "" && (!localPathRe.MatchString(l.Path) || l.Path == "/" || strings.Contains(l.Path, "..") || len(l.Path) > 1 && strings.HasSuffix(l.Path, "/")) {
			errs = append(errs, fmt.Errorf("local_scratch.path %q must be an absolute folder (not \"/\", no trailing '/' or '..') or $VAR[/folder]", l.Path))
		}
		if l.EnvVar != "" && !envVarNameRe.MatchString(l.EnvVar) {
			errs = append(errs, fmt.Errorf("local_scratch.env_var %q must be an environment variable name", l.EnvVar))
		}
		if l.SizeGB < 0 {
			errs = append(errs, errors.New("local_scratch.size_gb must not be negative"))
		}
	}
	if d := h.SoftwareDir; d != "" && (!softwarePathRe.MatchString(d) || d == "/" || path.Clean(d) != d || strings.Contains(d, "..")) {
		errs = append(errs, fmt.Errorf("software_dir %q must be an absolute, clean folder, not \"/\"", d))
	}
	if g := h.GlobalScratch; g != nil {
		switch {
		case g.Path == "":
			errs = append(errs, errors.New("global_scratch.path is required"))
		case !globalPathRe.MatchString(g.Path) || g.Path == "/" || path.Clean(g.Path) != g.Path || strings.Contains(g.Path, ".."):
			errs = append(errs, fmt.Errorf("global_scratch.path %q must be an absolute, clean folder, not \"/\" (a <placeholder> may stand for the user's own part)", g.Path))
		case strings.ContainsAny(placeholderRe.ReplaceAllString(g.Path, ""), "<>"):
			errs = append(errs, fmt.Errorf("global_scratch.path %q has a malformed <placeholder>", g.Path))
		}
		if g.PurgeDays < 0 || g.PurgeDays > maxScratchDays {
			errs = append(errs, fmt.Errorf("global_scratch.purge_days must be between 0 and %d", maxScratchDays))
		}
	}
	return errors.Join(errs...)
}

// Fixed returns the part of the global scratch path before its first
// <placeholder> (the whole path when there is none) and whether the path has
// a placeholder.
func (g GlobalScratch) Fixed() (prefix string, placeholder bool) {
	i := strings.Index(g.Path, "<")
	if i < 0 {
		return g.Path, false
	}
	return strings.TrimRight(g.Path[:i], "/"), true
}

// Covers reports whether dir (absolute, clean) is the global scratch or
// lies below it, judging by the part before any placeholder.
func (g GlobalScratch) Covers(dir string) bool {
	p, ph := g.Fixed()
	if p == "" {
		return false
	}
	if ph {
		return strings.HasPrefix(dir, p+"/")
	}
	return dir == p || strings.HasPrefix(dir, p+"/")
}
