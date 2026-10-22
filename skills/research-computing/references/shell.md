# Shell startup files and hygiene

Your shell's startup files run not just when you type commands, but also when
`scp`, `sftp`, `rsync`, `ssh host command`, `sbatch` and Slurm jobs start a
shell. A careless `.bashrc` breaks all of them. These steps run on the cluster
login host (and your own computer for its own files).

## Login, interactive and non-interactive shells

| Shell | Examples | Reads |
|---|---|---|
| **Login, interactive** | `ssh host` terminal, `bash -l` | `~/.bash_profile` (or `~/.profile`); `.bashrc` only if the profile sources it |
| **Non-login, interactive** | a new terminal tab, `bash` | `~/.bashrc` |
| **Non-interactive** | `ssh host cmd`, `scp`, `sftp`, `rsync`, scripts | `~/.bashrc` is read for `ssh host cmd` (a bash quirk) and for scp/rsync; scripts read none |
| **Batch job** | `sbatch` script | starts with a `#!/bin/bash` shell: **not** a login shell, inherits your environment from the submit time |

Practical consequences:

- Put exports and PATH in `~/.bashrc` and make `~/.bash_profile` source it:

  ```bash
  # ~/.bash_profile
  [[ -f ~/.bashrc ]] && source ~/.bashrc
  ```

- A batch job inherits the environment of the shell that ran `sbatch` (loaded
  modules, activated venv or conda env, `PATH`). That's convenient and
  confusing at once. Make scripts self-contained: `module purge`, load, activate
  inside the script. `#SBATCH --export=NONE` starts the job with a clean
  environment, but then the script must set everything up itself (and, in a
  script that needs a login-like environment, `#!/bin/bash -l` reads the
  profile).
- Don't rely on `cd` or aliases from `.bashrc` in scripts. Aliases aren't
  expanded in non-interactive shells.

## Keep `.bashrc` quiet and safe

Anything that prints text in a non-interactive shell corrupts the data stream
of scp, sftp and rsync (symptoms: `scp: Received message too long`,
`Couldn't read packet`, `protocol error`, an immediate failure after login).
Anything that waits for input hangs them.

```bash
# ~/.bashrc, in this order
[ -f /etc/bashrc ] && . /etc/bashrc        # (a) system setup (often provides `module`); /etc/bash.bashrc on some distros
export PATH="$HOME/bin:$PATH"              # (b) quiet exports and PATH: needed by ssh host 'cmd' too
[[ $- == *i* ]] || return                  # (c) stop here for non-interactive shells
# (d) interactive-only: prompts, welcome messages, aliases, tmux, keychain, conda init, cd, echo
```

The guard must come after (a) and (b), otherwise `ssh host 'module avail'` and
jobs lose the system module setup and your PATH.

Check it works:

```bash
ssh mycluster true && echo clean            # should print only "clean"
ssh mycluster 'echo $-'                     # no 'i' in the output = non-interactive
bash -c 'source ~/.bashrc; echo ok'         # should not print anything but ok
```

More rules:

- **Guard Slurm interactive jobs.** `srun --pty bash` also reads `.bashrc` and
  can inherit settings that break it. A common guard is to skip fancy
  start-up when `SLURM_PTY_PORT` is set (an interactive Slurm session), for
  example before auto-starting `tmux`/`screen` or `exec zsh`:

  ```bash
  if [[ $- == *i* && -z "$TMUX" && -z "$SLURM_PTY_PORT" ]]; then
      tmux new-session -A -s main     # auto-attach only in a real login terminal
  fi
  ```

- **Run things that may prompt only in a real terminal.** `SLURM_PTY_PORT` is
  set only inside `srun --pty` sessions; batch jobs have no terminal and
  nothing to answer a prompt. Positive pattern, in the interactive part:

  ```bash
  [ -n "$SLURM_PTY_PORT" ] && eval "$(keychain --quiet --eval id_ed25519)"
  ```

- **Telling interactive from batch.** `SLURM_JOB_ID` is set in *any* Slurm job
  and empty on the login node; `SLURM_PTY_PORT` is set only by `srun --pty`. To
  ask Slurm, `scontrol show job $SLURM_JOB_ID | grep -o 'BatchFlag=[0-9]*'`
  (`1` = `sbatch`, `0` = `srun`/`salloc`). A plain `salloc` shell can have
  `SLURM_JOB_ID` set with `BatchFlag=0` and no `SLURM_PTY_PORT`. A tty test
  (`[ -t 0 ]`) only tests **stdin**, so it misleads with redirected input,
  `ssh host cmd` and `srun` without `--pty`; don't use it to detect a Slurm job.
- **A batch script does not read `.bashrc`.** A `#!/bin/bash` script reads no
  startup file, so a fix that lives only in `~/.bashrc` (proxy, `PATH`) never
  applies in `sbatch` jobs. Put the guard line **inside the script**, or use
  `#!/bin/bash -l` with a `~/.bash_profile` that sources `~/.bashrc`:

  ```bash
  # in the sbatch script itself (after the #SBATCH lines)
  [ -n "$SLURM_JOB_ID" ] && { export no_proxy="${no_proxy//.<internal-domain>/}"; export NO_PROXY="$no_proxy"; }
  ```

  `srun --pty bash` is a non-login shell and reads only `~/.bashrc`, never
  `~/.bash_profile`.
- **Don't `exec` another shell or auto-start tmux unconditionally**; that locks
  out `scp`, scripts and jobs.
- **`conda init` and similar installers append blocks** to `.bashrc`. They're
  fine, but keep them in the interactive part (below the guard) and don't let
  an auto-activated `base` environment shadow job environments: `conda config
  --set auto_activate_base false`.
- **Quote variables in tests**: `[ -n "$VAR" ]`, `[ -f "$file" ]`. Unquoted, an
  empty or space-containing value changes the meaning (`[ -n $VAR ]` is true
  when `VAR` is empty).
- **PATH order matters**: the first match wins. Prepend your own tools
  (`export PATH="$HOME/bin:$PATH"`) and don't append the same entries every
  time `.bashrc` is sourced (`echo $PATH` growing means duplicates; guard with
  `case ":$PATH:" in *":$HOME/bin:"*) ;; *) PATH="$HOME/bin:$PATH";; esac`).
- **Don't put secrets** (tokens, keys) in `.bashrc`; every process, job log and
  `env` dump can leak them. Keep them in files with mode `600`.
- Don't change `HOME`, `TMPDIR` or `LD_LIBRARY_PATH` globally; module-loaded
  software and containers expect their own values. Set these per job.

## Scripts

```bash
#!/bin/bash
set -euo pipefail          # stop on errors, undefined variables and pipeline failures
```

`set -u` only catches **unset** variables, not empty ones. Guard deletes with
`rm -rf -- "${DIR:?DIR is empty}/"`, which aborts when `DIR` is unset or empty. Use `"$@"` and quotes
around paths so spaces don't break them. Run `bash -n script.sh` to check
syntax and `shellcheck script.sh` (if installed) for common traps.

### Sending multi-line code to a remote login shell

The login shell on a cluster is not always bash (some sites use tcsh), and
every layer of quoting on the `ssh` command line is parsed by it. Don't pass
multi-line code as an argument (`ssh host "bash -c '...'"`). Send it on
**standard input** to an interpreter instead; stdin is not parsed by the
login shell, so quotes, `$`, newlines and here-documents arrive unchanged:

```bash
ssh <login-host> bash -s <<'EOF'
set -euo pipefail
for d in run1 run2; do ls -l "$d"; done
EOF
```

Use the same idea for files: `ssh <login-host> 'cat > job.sh' < job.sh`.
Job scripts belong in files that the scheduler runs (and that you can
review), and jobs are submitted with `sbatch`, not from
inside an ad-hoc script.

## Recovery

If a bad `.bashrc` stops you logging in at all (the shell exits at once):
`ssh -t mycluster bash --noprofile --norc`, then fix the file with `nano` or
`vi`; or ask support to reset it (ticket advice: getting-help.md).
