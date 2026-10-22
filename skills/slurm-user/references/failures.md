# Why did my job fail?

Collect the facts first, then name one cause. Do not resubmit the same script
unchanged: that teaches nothing. The symptom table in
[slurm.md](slurm.md#a-job-that-ended-badly) lists the usual fixes. This page
gives the order of the checks and the patterns that are easy to miss.

## Contents

- [Collect the facts](#collect-the-facts)
- [Read the numbers](#read-the-numbers)
- [Match the log line](#match-the-log-line)
- [Remember the cause](#remember-the-cause)

## Collect the facts

```bash
# State, exit code, reason, time used against the limit, memory asked for
sacct -j <jobid> -X -P -o JobID,State,ExitCode,Reason,Elapsed,Timelimit,ReqMem,MaxRSS,NodeList

# MaxRSS is on the step lines (<jobid>.batch, <jobid>.0), not on the job line.
# Take the largest value.
sacct -j <jobid> -P -o JobID,MaxRSS

# The last lines of the log (the path is StdOut= / StdErr= in scontrol while
# the job is known, else the --output file in the job's folder)
scontrol show job <jobid> | tr ' ' '\n' | grep -E '^Std(Out|Err)='
tail -n 60 <output-file>
```

A job that Slurm requeued (preemption, node failure) shows `Restarts` in
`sacct -o JobID,Restarts`. Read the log of the last attempt.

## Read the numbers

| Fact | Meaning |
|---|---|
| `State` is `OUT_OF_MEMORY`, or `MaxRSS` is close to `ReqMem` (above about 90%) | Out of memory. Ask for more memory (`--mem`), or process less data at once. |
| `State` is `TIMEOUT`, or `Elapsed` equals `Timelimit` | The time limit ended the job. Ask for more time inside the partition's `MaxTime`, or save checkpoints and resume. |
| `State` is `FAILED` and the exit code is `127:0` | A command was not found. See below. |
| `State` is `FAILED` and the exit code is `1:0` | The program stopped with an error. The log has the reason. |
| Exit code `0:9` or `0:15`, or `Killed` in the log | A signal ended the job. With `ReqMem` close to `MaxRSS` it is the memory limit; otherwise `scancel` or a node problem. |
| `State` is `NODE_FAIL` or `PREEMPTED` | Not your code. Resubmit, on a partition that does not preempt if it happens again. |
| `ReqMem` ends in `n` (for example `4000Mn`), several tasks per node | The size is per node, but `MaxRSS` is the largest single task. Multiply `MaxRSS` by the tasks per node before you compare it with `ReqMem`. |
| An array job | `sacct -X` gives one line per task. Look at the first task that did not complete, and query its steps with `sacct -j <jobid>_<task> -P -o JobID,MaxRSS`. |
| `ReqMem` ends in `c` (for example `4Gc`) | The size is per CPU. Multiply it by the CPU count before you compare with `MaxRSS`. |

## Match the log line

| In the log | Cause | Fix |
|---|---|---|
| `oom-kill`, `Out of memory`, `Killed` | Out of memory | More `--mem`; check `MaxRSS` of the failed run first |
| `command not found`, `No module named ...`, `ModuleNotFoundError`, `Lmod has detected the following error` | Missing program, module or Python package | `module spider <name>` for the exact module; load it in the script; activate the environment that has the package ([environments](../../research-computing/references/environments.md#finding-software-on-a-cluster-modules-first)) |
| `CUDA driver version is insufficient`, `no kernel image is available` | CUDA or GPU architecture does not match the node | Load a CUDA module that the node driver supports, or pin the GPU type; rebuild for the right architecture ([cuda-builds.md](cuda-builds.md)) |
| `C:\`, `/Users/`, `/home/<laptop user>` in a `No such file or directory` error | A path from the laptop | Use paths relative to the job's folder ([from-your-laptop.md](from-your-laptop.md#paths-from-the-laptop-dont-exist-on-the-cluster)) |
| `Disk quota exceeded`, `No space left on device` | A folder is full or over its quota | Check with `quota -s` or the site's quota command and `df -h <folder>`; write to scratch, then copy the results; remove old files you own |
| `pip` or `conda` download fails or hangs, `Could not resolve host`, `Connection timed out` | Compute nodes may have no internet | Build the environment on the login node, once, in a shared folder; the job only activates it ([proxy-and-github.md](proxy-and-github.md)) |
| `Permission denied` on the job's folder | The folder belongs to another user or group | Check `ls -ld <folder>`; use a folder you own |

If nothing matches, read the whole log, not only its end. The first error is
often far above the last line.

## Remember the cause

When the same code folder runs again on the same cluster, look at what
failed last time and what fixed it. In Claude Code, keep a short note next to
the code (for example `NOTES.md`): job id, cause, the change that worked. When
a later run completed, compare its script with the failed one:

```bash
diff <(sacct -j <old> -X -P -o ReqMem,Timelimit) <(sacct -j <new> -X -P -o ReqMem,Timelimit)
```

Labmill does this by itself: it stores the cause on the job, on the user's
computer only, and the next draft for the same folder and cluster says what
failed and which later job completed.
