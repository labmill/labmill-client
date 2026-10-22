package slurm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"labmill-client/internal/backend"
)

// envHelpers are the shell functions that build a software environment once
// (M67.1). labmill_once <target> <builder> runs <builder> <target> under a lock
// unless the target is complete. A directory is complete when it holds
// .labmill-ok (written last), a file (a SIF image) when it exists. The build
// happens in place: a venv or conda prefix moved after the build keeps its
// old path in scripts and activate files. A directory without .labmill-ok is a
// crashed build: the builder moves it aside (never deletes). Nothing here is
// ever cleaned up: environments are reusable software (M61).
const envHelpers = `labmill_done() { [ -f "$1" ] || [ -f "$1/.labmill-ok" ]; }
labmill_aside() { [ ! -e "$1" ] || mv -- "$1" "$1.incomplete.$(date +%Y%m%d%H%M%S).$$"; }
labmill_once() {
  labmill_done "$1" && return 0
  mkdir -p -- "$(dirname -- "$1")" || return 1
  # A mkdir lock, not flock (flock fails on file systems mounted without it).
  # A lock folder older than the wait limit belongs to a killed job: move it
  # aside (never delete) and try again.
  labmill_l="$1.lock.d"; labmill_n=0
  until mkdir -- "$labmill_l" 2>/dev/null; do
    if find "$labmill_l" -maxdepth 0 -mmin +"${LABMILL_LOCK_STALE_MIN:-60}" 2>/dev/null | grep -q .; then
      mv -- "$labmill_l" "$labmill_l.stale.$(date +%Y%m%d%H%M%S).$$" 2>/dev/null
      continue
    fi
    labmill_n=$((labmill_n + 1)); [ "$labmill_n" -le 720 ] || { echo "Labmill: gave up waiting for the lock $labmill_l" >&2; return 1; }
    sleep 5
  done
  # The subshell releases the lock on every way out, including a signal. A
  # refresher touches the lock folder while the build runs, so a build longer
  # than the wait limit is not taken for stale. It stops with the build (the
  # trap), and by itself when the lock folder is gone. Its output goes to
  # /dev/null so it never holds the job's output open.
  ( labmill_p=; trap 'kill "$labmill_p" 2>/dev/null; rmdir -- "$labmill_l"' EXIT; trap 'exit 143' TERM INT HUP
    ( trap 'kill "$labmill_s" 2>/dev/null; exit 0' TERM
      while :; do sleep "${LABMILL_LOCK_REFRESH:-300}" & labmill_s=$!; wait "$labmill_s"; [ -d "$labmill_l" ] && kill -0 $$ 2>/dev/null && touch -c -- "$labmill_l" 2>/dev/null || break; done ) >/dev/null 2>&1 &
    labmill_p=$!
    labmill_done "$1" || "$2" "$1" ); labmill_r=$?
  [ "$labmill_r" -eq 0 ] || return 1
  labmill_done "$1"
}
`

var nonNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// environmentCommand returns the text that replaces spec.Command in the
// script: the module, venv, conda or Apptainer setup lines, then the user's
// command (run inside the image for Apptainer). With LocalScratch the whole
// text runs inside the subshell that runs the command, so the setup happens
// on the node and in the folder where the command runs.
func environmentCommand(spec backend.JobSpec) string {
	e := spec.Environment
	if e == nil {
		return spec.Command
	}
	q := func(s string) string { return backend.Quote([]string{s}) }
	var b strings.Builder
	b.WriteString("# Labmill: software environment (built once, cached, never cleaned up)\n")
	if len(e.Modules) > 0 {
		fmt.Fprintf(&b, "module load %s || exit 1\n", backend.Quote(e.Modules))
	}
	cmd := spec.Command
	if e.Venv != nil || e.Conda != nil || e.Apptainer != nil {
		dir := "$HOME/.labmill/envs"
		if e.SoftwareDir != "" {
			dir = strings.TrimRight(e.SoftwareDir, "/")
		}
		fmt.Fprintf(&b, "LABMILL_ENVS=\"${LABMILL_ENVS:-%s}\"\n", dir)
		b.WriteString(envHelpers)
	}
	switch {
	case e.Venv != nil:
		py := e.Venv.Python
		// The hash covers the requirements, the interpreter that is really
		// found after the module loads, and the CPU type (home may be shared
		// by x86 and ARM nodes).
		hash := `{ cat -- "$LABMILL_REQ"; "$LABMILL_PY" -c 'import sys;print(sys.version,sys.base_prefix)'; uname -m; } | sha256sum | cut -c1-16`
		if py == "" {
			py = "python3"
		}
		fmt.Fprintf(&b, "LABMILL_REQ=%s\nLABMILL_PY=%s\n", q(e.Venv.Requirements), q(py))
		b.WriteString(`[ -f "$LABMILL_REQ" ] || { echo "Labmill: $LABMILL_REQ not found in $PWD" >&2; exit 1; }` + "\n")
		b.WriteString(`command -v "$LABMILL_PY" >/dev/null 2>&1 || { echo "Labmill: $LABMILL_PY not found; load its module (environment.modules)" >&2; exit 1; }` + "\n")
		fmt.Fprintf(&b, "LABMILL_ENV=\"$LABMILL_ENVS/venv-$(%s)\"\n", hash)
		b.WriteString(`labmill_build_venv() { labmill_aside "$1" && "$LABMILL_PY" -m venv "$1" && "$1/bin/pip" install -r "$LABMILL_REQ" && touch "$1/.labmill-ok"; }` + "\n")
		b.WriteString(`labmill_once "$LABMILL_ENV" labmill_build_venv || { echo "Labmill: could not build the venv $LABMILL_ENV" >&2; exit 1; }` + "\n")
		b.WriteString(`. "$LABMILL_ENV/bin/activate" || exit 1` + "\n")
	case e.Conda != nil:
		fmt.Fprintf(&b, "LABMILL_CONDA_FILE=%s\n", q(e.Conda.File))
		b.WriteString(`command -v conda >/dev/null 2>&1 || { echo "Labmill: conda not found; load the site's conda module (environment.modules)" >&2; exit 1; }` + "\n")
		b.WriteString(`[ -f "$LABMILL_CONDA_FILE" ] || { echo "Labmill: $LABMILL_CONDA_FILE not found in $PWD" >&2; exit 1; }` + "\n")
		b.WriteString(`LABMILL_ENV="$LABMILL_ENVS/conda-$({ cat -- "$LABMILL_CONDA_FILE"; uname -m; } | sha256sum | cut -c1-16)"` + "\n")
		b.WriteString(`labmill_build_conda() { labmill_aside "$1" && if command -v mamba >/dev/null 2>&1; then mamba env create -p "$1" -f "$LABMILL_CONDA_FILE"; else conda env create -p "$1" -f "$LABMILL_CONDA_FILE"; fi && touch "$1/.labmill-ok"; }` + "\n")
		b.WriteString(`labmill_once "$LABMILL_ENV" labmill_build_conda || { echo "Labmill: could not build the conda environment $LABMILL_ENV" >&2; exit 1; }` + "\n")
		b.WriteString(`eval "$(conda shell.bash hook)" && conda activate "$LABMILL_ENV" || exit 1` + "\n")
	case e.Apptainer != nil:
		a := e.Apptainer
		b.WriteString(`LABMILL_APPT="$(command -v apptainer || command -v singularity)" || { echo "Labmill: neither apptainer nor singularity found; load its module (environment.modules)" >&2; exit 1; }` + "\n")
		b.WriteString(`export APPTAINER_CACHEDIR="$LABMILL_ENVS/apptainer-cache" SINGULARITY_CACHEDIR="$LABMILL_ENVS/apptainer-cache"` + "\n")
		b.WriteString(`mkdir -p -- "$APPTAINER_CACHEDIR" || exit 1` + "\n")
		if a.IsDockerImage() {
			sum := sha256.Sum256([]byte(a.Image))
			name := nonNameRe.ReplaceAllString(strings.TrimPrefix(a.Image, "docker://"), "_") + "-" + hex.EncodeToString(sum[:4])
			fmt.Fprintf(&b, "LABMILL_REF=%s\nLABMILL_IMG=\"$LABMILL_ENVS/sif/%s.sif\"\n", q(a.Image), name)
			// A half-pulled file is not reusable software: only our own tmp file is removed.
			b.WriteString(`labmill_build_sif() { "$LABMILL_APPT" pull "$1.tmp.$$" "$LABMILL_REF" && mv -- "$1.tmp.$$" "$1" || { rm -f -- "$1.tmp.$$"; return 1; }; }` + "\n")
			b.WriteString(`labmill_once "$LABMILL_IMG" labmill_build_sif || { echo "Labmill: could not pull $LABMILL_REF" >&2; exit 1; }` + "\n")
		} else {
			fmt.Fprintf(&b, "LABMILL_IMG=%s\n", q(a.Image))
			b.WriteString(`[ -f "$LABMILL_IMG" ] || { echo "Labmill: image $LABMILL_IMG not found" >&2; exit 1; }` + "\n")
		}
		nv := ""
		if a.GPU || spec.GPUs > 0 {
			nv = " --nv"
		}
		cmd = fmt.Sprintf("\"$LABMILL_APPT\" exec%s \"$LABMILL_IMG\" bash -c %s", nv, q(spec.Command))
	}
	b.WriteString(cmd)
	return b.String()
}
