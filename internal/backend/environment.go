package backend

import (
	"path"
	"regexp"
	"strings"
)

// Environment is the software a Slurm job needs (M67.1): environment modules
// plus at most one of a Python venv, a conda environment or an Apptainer
// image. The Slurm renderer writes the setup lines before the command;
// built environments are cached per requirements hash and never cleaned up.
type Environment struct {
	// Modules are loaded with `module load`, in order (prerequisites first).
	Modules []string `json:",omitempty"`
	// Venv builds a Python virtual environment from a requirements file.
	Venv *VenvEnv `json:",omitempty"`
	// Conda builds a conda environment from an environment file.
	Conda *CondaEnv `json:",omitempty"`
	// Apptainer runs the command in an Apptainer (Singularity) image.
	Apptainer *ApptainerEnv `json:",omitempty"`
	// SoftwareDir is the site profile's folder for reusable software, set by
	// Labmill (never by Claude); "" means $HOME/.labmill/envs.
	SoftwareDir string `json:",omitempty"`
}

// VenvEnv is a Python venv built from a requirements file.
type VenvEnv struct {
	// Requirements is a file in the job folder (relative path, no "..").
	Requirements string
	// Python is the interpreter command, e.g. "python3.11"; "" means python3.
	// Put the module that provides it in Environment.Modules.
	Python string `json:",omitempty"`
}

// CondaEnv is a conda environment built from an environment file.
type CondaEnv struct {
	// File is an environment.yml in the job folder (relative path, no "..").
	File string
}

// ApptainerEnv is an Apptainer image the command runs in.
type ApptainerEnv struct {
	// Image is a docker:// reference (pulled once into a SIF cache) or a path
	// to a .sif file.
	Image string
	// GPU adds --nv.
	GPU bool `json:",omitempty"`
}

var (
	moduleRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]*$`)
	relPathRe   = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/+@,:=-]*$`)
	pythonCmdRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	sifPathRe   = regexp.MustCompile(`^/?[A-Za-z0-9._][A-Za-z0-9._/+@,:=-]*\.sif$`)
)

// ValidRelPath reports whether p is a plain relative path inside the job
// folder: safe characters only, no leading "/" or "-", no ".." element.
func ValidRelPath(p string) bool {
	if !relPathRe.MatchString(p) {
		return false
	}
	for _, e := range strings.Split(p, "/") {
		if e == ".." || e == "" {
			return false
		}
	}
	return true
}

// IsDockerImage reports whether the image is a docker:// reference.
func (a ApptainerEnv) IsDockerImage() bool { return strings.HasPrefix(a.Image, "docker://") }

// Validate checks the formats documented on Environment. Errors wrap
// ErrInvalidSpec.
func (e Environment) Validate() error {
	for _, m := range e.Modules {
		if !moduleRe.MatchString(m) {
			return invalid("environment module %q must match ^[A-Za-z0-9][A-Za-z0-9._+/-]*$", m)
		}
	}
	n := 0
	if v := e.Venv; v != nil {
		n++
		if !ValidRelPath(v.Requirements) {
			return invalid("environment venv requirements %q must be a relative path in the job folder (no leading '/', no '..')", v.Requirements)
		}
		if v.Python != "" && !pythonCmdRe.MatchString(v.Python) {
			return invalid("environment venv python %q must be a command name such as python3.11 (load its module with modules)", v.Python)
		}
	}
	if c := e.Conda; c != nil {
		n++
		if !ValidRelPath(c.File) {
			return invalid("environment conda file %q must be a relative path in the job folder (no leading '/', no '..')", c.File)
		}
	}
	if a := e.Apptainer; a != nil {
		n++
		switch {
		case a.IsDockerImage():
			if !ValidImage(strings.TrimPrefix(a.Image, "docker://")) {
				return invalid("environment apptainer image %q must be docker://<name>[:tag] with letters, digits and ._/:@-", a.Image)
			}
		case sifPathRe.MatchString(a.Image) && !strings.Contains(a.Image, "..") && path.Clean(a.Image) == a.Image:
		default:
			return invalid("environment apptainer image %q must be docker://<name>[:tag] or a clean path to a .sif file", a.Image)
		}
	}
	if n > 1 {
		return invalid("environment: use at most one of venv, conda and apptainer")
	}
	if e.SoftwareDir != "" && !workDirRe.MatchString(e.SoftwareDir) {
		return invalid("software dir %q must match ^/[A-Za-z0-9._/+@,:=~-]*$", e.SoftwareDir)
	}
	return nil
}
