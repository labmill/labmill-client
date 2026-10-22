# Where a job's data goes, which resource to ask for, preemption

Global rules for every Slurm cluster. Which of these resources exist, their
paths, sizes and purge times: see your site skill. Many clusters have no
preemptible partition and no node-local scratch; then the normal rules apply.

## Where a job's working data goes (in this order)

1. **Node-local scratch, for a single-node job** when the compute nodes have
   local disk. Stage the input in, run there, copy the results back to the job
   folder on shared storage, and remove the job's own local folder at the end
   of the job. The node is unreachable to you afterwards, so the job cleans up
   itself with `trap ... EXIT` (it runs on success, failure and `scancel`;
   a node crash or `kill -9` skips it).
2. **Otherwise a global scratch** that is mounted on the compute nodes (Lustre,
   GPFS, BeeGFS, CephFS, NFS). Its purge policy cleans up, so cleanup is optional:
   remove the job's own working files only after the results are downloaded
   and their checksums match.
3. **Home only as a last resort** (small quota, slow for heavy I/O).

**Never remove software** that can be reused: venvs, conda environments,
containers, caches meant for reuse. Cleanup touches only the job's own folder.

### Find out what the cluster has

```bash
sinfo -N -o "%N %d"                          # TmpDisk (MB) per node; 0 means "not configured", not "no disk"
scontrol show config | grep -i TmpFS         # the path Slurm counts as node-local
srun -n1 --time=00:05:00 bash -c 'echo TMPDIR=$TMPDIR SLURM_TMPDIR=$SLURM_TMPDIR; df -hT /tmp ${TMPDIR:-/tmp}'
                                             # real size and type on a compute node, in a short job
findmnt -t lustre,gpfs,beegfs,ceph,fuse.ceph-fuse,wekafs,nfs,nfs4   # shared file systems (run in the short job too: login and compute differ)
```

Local disk is real only if `df` on a compute node shows it (a local type such
as xfs or ext4 on `/dev/...`, not `nfs`). Some sites need a request such as
`--gres=disk:<GB>` or `--tmp=<MB>`; the site docs say. Record what you find in
the site profile.

### Example: single-node job on node-local scratch

```bash
#!/bin/bash
#SBATCH --job-name=example
#SBATCH --nodes=1
#SBATCH --time=04:00:00
#SBATCH --output=/path/to/project/job1/logs/%x_%j.out   # shared path, folder made before sbatch

set -euo pipefail
JOB=/path/to/project/job1                      # shared storage: inputs, results
WORK=${SLURM_TMPDIR:-${TMPDIR:-/tmp}}/job_${SLURM_JOB_ID}   # node-local, this job only
mkdir -p "$WORK/results"
trap 'if rsync -a "$WORK/results/" "$JOB/results/"; then rm -rf -- "$WORK";
      else echo "stage-out failed; results kept in $WORK on $(hostname)" >&2; exit 1; fi' EXIT
                                               # stage out, and clean up only after a good copy

rsync -a "$JOB/input/" "$WORK/input/"          # stage in
cd "$WORK"
./myprog input/ results/ &                     # run on the fast local disk
wait $!                                        # in the background, so signal traps fire on time
```

If the program writes its results anywhere in the job folder (not only into
`results/`), copy the whole folder in and out instead (`rsync -a "$JOB/"
"$WORK/job/"` and back), and **exclude the job's live output file**
(`--exclude="/${SLURM_JOB_NAME}-${SLURM_JOB_ID}.out"` for the usual
`%x-%j.out`) from both copies: Slurm is still writing it in the job folder,
and the stale copy from the start of the job would overwrite it at stage-out.
For a requeued job (`SLURM_RESTART_COUNT` set) the same ID may land on a node
that still has an old `$WORK` from the killed run: start from a clean one.

Stage out with `rsync -a --update`, so a file the program changed directly in
the shared folder (by absolute path) is not replaced by the older staged-in
copy. On preemption with `--requeue`, the partial outputs are copied back (when
the trap runs) and staged in again by the next run: the program must resume
from them or start clean (`SLURM_RESTART_COUNT`).

The trap stages out before it removes anything and removes the work folder
only when the copy succeeded, so a timeout, `scancel` or a failed copy (quota
full, share unavailable) never deletes the only copy: the job then fails and
says where the results were left; they may be lost with the node's scratch,
so free space or fix the share and rerun. After a timeout or `scancel` Slurm waits only `KillWait` (often
30 s) before killing the job, and on preemption or a node crash the trap may
not run at all: for long stage-outs add `#SBATCH --signal=B:USR1@300` and a
`trap` on `USR1` that stages out (bash runs a trap only after the foreground
program returns, which is why the program runs in the background with
`wait`), and signal the program's whole process group, not only the PID from `$!`
(`set -m` before starting it in the background, then `kill -TERM -- -$PID`
in the trap: killing just the subshell leaves the real program running), and write checkpoints to shared storage regularly.

## Cleaning up after a verified download

Cleanup is optional on a global scratch (its purge policy removes old files
anyway), and never needed on node-local scratch (the job removed it). If the
user wants the scratch space back, remove **the job's own working files, and
only after the results are verified on the laptop**. Do not offer it when the
job ran in the user's main project or home space unless they ask.

Three conditions first; if one fails, stop:

1. **The job has ended and nothing else uses the folder.** `squeue -j <id> -h`
   prints nothing (not queued, running or requeued-pending) and
   `sacct -j <id> -X -o JobID,State` shows a final state. No other job of
   yours works in that folder or below it (`squeue --me -h -o "%i %Z"` shows
   each job's work directory; on Slurm older than 20.02, which has no
   `--me`, use `squeue -u "$(id -un)" -h -o "%i %Z"`). Compare against
   both the folder and its real path (`readlink -f -- <folder>`), since the
   scratch may sit behind a symbolic link. A queued job whose script names
   the folder anywhere (`--chdir`, a `cd`, an output path) also uses it:
   check with `scontrol write batch_script <id> -` before you remove
   anything.
2. **The folder is the job's own and well inside the scratch:** an absolute
   path with no `..`, a job folder of the form `<...>/labmill/<job name>`
   (never the scratch root, the user's folder or the folder that holds all
   jobs), below the scratch root by at least the user folder and the job
   folder.
3. **The files are the verified ones.** Verify every downloaded file with
   SHA-256 (see "Getting results back" in `from-your-laptop.md`):
   `sha256sum -- <files>` on the cluster, compare with `sha256sum` (macOS
   `shasum -a 256`) on the laptop. Any difference or a size-only check: stop,
   fetch again. Run `sha256sum` on the cluster **again just before the `rm`**
   and remove only the files whose sum still equals the verified one (a later
   run may have rewritten a file with the same name).

Look for software and links:

```bash
find <jobdir> -name pyvenv.cfg -print -o -name conda-meta -print -o -type l -print
```

Leave every folder with a `pyvenv.cfg` or `conda-meta` (a virtual or conda
environment), anything named `site-packages`, `.conda` or `envs`, caches meant
for reuse, and anything below a symbolic link.

Then remove **exactly the files you verified**, plus the inputs and helper
scripts you uploaded yourself (only while their checksum on the cluster still
equals what you uploaded), never a whole tree:

```bash
rm -f -- <jobdir>/results/a.csv "<jobdir>/results/my file.csv"   # the verified list, nothing else
rmdir --ignore-fail-on-non-empty -- <jobdir>/results             # only the folders that held those files, deepest first
rmdir --ignore-fail-on-non-empty -- <jobdir>                     # the job folder, only if it is empty now
```

No `rm -r`, no wildcards over a folder you haven't listed, no `find -delete`
over the tree (it would also remove empty folders you never touched). Files you
did not download (other results, logs you didn't fetch) stay, and so do the
folders that still hold them. Running it twice removes nothing more.

Show the user the exact list and commands first and run them only after a
yes. Labmill does this with its `cleanup_job` tool: it refuses unless the job
has ended and no other active job uses the folder, lists every path and the
job folder, checks the cluster for environments and symbolic links and
re-checks each file's SHA-256 just before removing, removes only the folders
that held removed files, and records a `cleaned` event. After a verified
download of a job that used the global scratch, Labmill's message about the
download offers it; with "Approve everything automatically" on it runs
without the question and says so in the chat.

## Which resource to ask for

1. **Preemptible first, if the cluster has one** (a partition or QOS, often
   covering all or most nodes at low priority). Many clusters have none; check
   `sinfo -o "%P %a %l %D %f"` and the site skill. Narrow it with
   `--constraint=<feature>`, `--gres=gpu:<type>:N` or the partition's features.
2. **The smallest viable node**, or the one that starts soonest: cores,
   memory and GPU type just above what the job needs. Estimate the start:

   ```bash
   sbatch --test-only job.sh          # expected start and nodes; submits nothing
   squeue --start -u $USER            # estimated start of pending jobs
   sinfo -o "%P %a %D %T %f"          # nodes by state and feature per partition
   sinfo -N -t idle -o "%N %P %c %m %G %f"   # idle nodes with size and features
   ```

3. A job that needs more than the preemptible resource gives (long, not
   restartable, many nodes) goes to the normal partition.

## Preemption

A preempted job is killed, suspended or requeued by a higher-priority job.

- Submit with `--requeue` where the site allows it; without it a preempted job
  is usually just gone. With `--requeue` (or `PreemptMode=REQUEUE`, or
  `JobRequeue=1`) **Slurm requeues the same job ID**. `SLURM_RESTART_COUNT` is
  set on a requeued run: start from a clean working folder or resume from a
  checkpoint.
- **Never resubmit while the job is still alive**: a second copy would write to
  the same folder. Resubmit only when the job has really ended:

  ```bash
  squeue -j <id> -h                      # empty = not queued, running or requeued-pending
  sacct -j <id> -X -o JobID,State,Restarts   # a final state: PREEMPTED, NODE_FAIL, FAILED, TIMEOUT...
  sacct -j <id> -D -o JobID,State,Start,End  # -D lists each requeued run of the same ID
  ```

- **Count Slurm's own requeues as attempts** (`Restarts` above, `-D` rows,
  `SLURM_RESTART_COUNT`). After the third failed preemptible attempt, `scancel
  <id>` any requeued pending copy, then submit once on a non-preemptible
  partition (`sbatch -p <non-preemptible> job.sh`) and tell the user. Without
  `--requeue`, resubmit by hand (or let the tool do it) each time the job ended
  in `PREEMPTED` or `NODE_FAIL`, up to the same limit.
- Labmill does these steps by itself when the approved review said so: its job
  monitor resubmits the exact approved script after `PREEMPTED`, `NODE_FAIL`,
  `BOOT_FAIL` or a requeue loop that ended `FAILED` with exit status `0:0`
  (never after `TIMEOUT`, `OUT_OF_MEMORY`, a program failure, a signal or a
  cancel), counts Slurm's own requeues (also when Slurm requeued the job
  too often and the pending copy still waits: only a pending copy is
  cancelled, a running one is left alone), then submits once on the fallback
  partition (only the partition, QOS and `--requeue` lines change) and says
  each step in the chat. It claims the old job ID in its database before
  `sbatch`, so one ended job is never submitted twice; if the connection
  drops while `sbatch` runs it does not submit again, but later looks in the
  queue (`TZ=UTC squeue --me -h -o "%i|%j|%Z|%V"`, then, if it shows
  nothing, `TZ=UTC sacct -X -n -P -S <time> --format=JobID,JobName,WorkDir,Submit`)
  for a job with the same name, work folder and a newer submit time, and
  records that one. A Claude Code
  user runs the commands above by hand, and checks `squeue` before
  resubmitting after a dropped connection.
- **Checkpoint long jobs**: write restart files to shared storage at regular
  intervals and make the program resume from them. Partial outputs on shared
  storage survive a preemption; node-local ones do not.
- Find preempted jobs: `sacct -u $USER -S <start-date> -s PR`.

## Software: the site's first

Use what the site provides before building anything: `module spider <name>`,
the site's software folders, the environment variables the site sets for its
software. Build your own (venv, conda, container) only when the provided
version is **older than one year** (the version comes from `module spider`
or `--version`; its release date comes from the project's release notes or
changelog, since `module spider` shows no dates) or is **missing**
([finding software](../../research-computing/references/environments.md#finding-software-on-a-cluster-modules-first)).

When you do build one, build it once and reuse it: keep it in the site's
software folder (else `$HOME/.labmill/envs`; home, not a purged scratch), name
the folder after a hash of the requirements file, take a `flock` lock while
building, and never clean it up. Environments, containers and caches are
reusable software, so the post-download cleanup above never touches them. The
shell recipe is in
[build an environment once](../../research-computing/references/environments.md#build-an-environment-once-reuse-it-in-every-job).
