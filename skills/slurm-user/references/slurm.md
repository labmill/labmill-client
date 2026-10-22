# Slurm: jobs, partitions, GPUs and the common traps

Generic Slurm usage. Partition names, accounts, QOS, GPU models and GRES type
strings, wrapper commands and documentation links for a particular cluster are in
your site skill (`site-<institution domain>`).

## Commands

| Purpose | Command |
|---|---|
| Submit a batch job | `sbatch job.sh` |
| Interactive shell on a compute node | `srun --pty bash` |
| Allocate resources, then run steps in them | `salloc ...` then `srun ...` |
| Your queued and running jobs | `squeue -u $USER` |
| Details of one job | `scontrol show job <jobid>` |
| Finished jobs (state, exit code, time, memory) | `sacct -j <jobid> --format=JobID,JobName,State,ExitCode,Elapsed,MaxRSS` |
| Your recent jobs | `sacct -u $USER -S now-7days` |
| Only pending, running or failed jobs | `sacct -u $USER -S now-7days -s PD,R,F` (`-s` filters by state: `PD` pending, `R` running, `F` failed, `CD` completed, `CA` cancelled, `TO` timeout, `OOM` out of memory) |
| Preempted jobs (preemptible or requeue-style partitions) | `sacct -u $USER -S <start-date> -E <end-date> -s PR` (state `PR`; with `-s`, give `-S` (and `-E` if needed); without them sacct shows only jobs in that state right now) |
| A different cluster (multi-cluster sites) | add `-M <cluster>` to `sbatch`, `squeue`, `sacct`, `scancel`, `sinfo` (`-M all` works for `squeue`, `sinfo` and `sacct`; `sbatch -M` takes a list and submits to whichever listed cluster can start the job first) |
| Cancel | `scancel <jobid>` (all yours: `scancel -u $USER`) |
| Partitions and node states | `sinfo -s`, `sinfo -Nl` |
| GPUs per node | `sinfo -p <partition> -N -o '%n %G'` |
| Your accounts, partitions, QOS | `sacctmgr -n show assoc user=$USER format=Account,Partition,QOS` |
| Partition limits and who may use it | `scontrol show partition <partition>` |

Some sites ship wrapper commands on top of these (for submitting, status and
free resources); see your site skill. Their flags can differ from `sbatch`'s.

## A job script

```bash
#!/bin/bash
#SBATCH --job-name=myjob
#SBATCH --partition=<partition>
#SBATCH --account=<account>          # only if you have more than one, or the site requires it
#SBATCH --time=04:00:00              # wall-clock limit; shorter jobs often start sooner
#SBATCH --cpus-per-task=8
#SBATCH --mem=32G
#SBATCH --output=%x_%j.out           # %x = job name, %j = job id; relative to the submit dir
#SBATCH --error=%x_%j.err

set -euo pipefail
module load <software>/<version>     # names from `module avail`; see your site skill
export OMP_NUM_THREADS="${SLURM_CPUS_PER_TASK:-1}"

python train.py --data "$HOME/data"
```

- Submit from the directory you want relative paths to resolve against, or
  `cd` to an absolute path in the script. For data uploaded to a job folder,
  `#SBATCH --chdir=<absolute job folder>` plus
  `#SBATCH --output=<absolute job folder>/%x-%j.out` keeps the run and its
  log together, wherever `sbatch` is called from
  ([from-your-laptop.md](from-your-laptop.md#where-things-go-on-the-cluster)).
- Show the whole script to the user before `sbatch`, and submit exactly what
  they approved (`sbatch --parsable job.sh` prints just the job ID).
- **Modules first:** when the job needs a named program, run
  `module spider <name>` before the site's other installs (site skill), a conda environment or a container
  ([finding software](../../research-computing/references/environments.md#finding-software-on-a-cluster-modules-first));
  build your own only if the provided version is older than a year or missing
  ([rule](placement-and-preemption.md#software-the-sites-first)).
  `module avail` lists software, `module list` shows what's loaded. Load modules inside the script so the job
  doesn't depend on your login shell.
- A Python environment (venv, conda) created on the cluster can be activated in
  the script the same way.

### Job arrays

Run the same script over many inputs:

```bash
#SBATCH --array=0-99%10              # 100 tasks, at most 10 at once
#SBATCH --output=%x_%A_%a.out        # %A = array job id, %a = task index

input=$(sed -n "$((SLURM_ARRAY_TASK_ID + 1))p" inputs.txt)
python process.py "$input"
```

`squeue -u $USER` shows array tasks as `<jobid>_<index>`; `scancel <jobid>_7`
cancels one task, `scancel <jobid>` all of them.

## The code has no run script

A sample or someone else's code often comes with a README and a build file
(`CMakeLists.txt`, `Makefile`, `setup.py`, `pyproject.toml`, `requirements.txt`)
but no script that builds and runs it. Do not guess names like `run.sh`;
list the folder, read the README and the build files completely, and write
the scripts yourself. Write them on the cluster, in the job's folder, next
to the results (not on your laptop and not in the shared copy of the code):

1. Copy the code to the cluster as it is, into the job's own folder
   (see "From your laptop").
2. Read the steps there: `ls <folder>`, then `cat <folder>/README.md`
   (page through a long one with `sed -n '100,200p'`).
3. Write the steps as files in the job folder with an ssh heredoc (`set -C`
   makes the shell refuse to overwrite a file that already exists), make them
   executable and look at them before the job runs:

```bash
ssh <login-host> "sh -c 'set -C; cat > <folder>/job-build.sh'" <<'EOF'
#!/bin/bash
set -euo pipefail
module load cmake            # whatever the README needs; see your site skill
cmake -S . -B build && cmake --build build -j "$SLURM_CPUS_ON_NODE"
EOF
ssh <login-host> "sh -c 'set -C; cat > <folder>/job-run.sh'" <<'EOF'
#!/bin/bash
set -euo pipefail
./build/app --input testdata/small.in
EOF
ssh <login-host> 'chmod +x <folder>/job-*.sh && cat <folder>/job-*.sh'
```

4. Have the job script call them: `bash job-build.sh && bash job-run.sh`
   (with `--chdir=<folder>`, or after `cd <folder>`). Use new names; never
   overwrite a script that is already there with different content.
5. Test the build on a short, small job first; a build failure is easier to
   read from the log of one short job than from a long run.

## Partitions, accounts and QOS

Partition access is usually gated by `AllowAccounts` (or groups/QOS) in the
partition definition. Check both sides:

```bash
sacctmgr -n show assoc user=$USER format=Account,Partition,QOS
scontrol show partition <partition> | grep -E 'AllowAccounts|AllowGroups|AllowQos|MaxTime|DefaultTime'
```

Groups that bought their own nodes often have private partitions that only
their members see. The default partition is marked with `*` in `sinfo`.

## Requesting GPUs

```bash
# batch:
#SBATCH --partition=<gpu-partition>
#SBATCH --gres=gpu:<type>:1         # pin the MODEL, not just the count -- see below
#SBATCH --cpus-per-task=8
#SBATCH --mem=32G

# interactive:
srun --pty --partition=<gpu-partition> --gres=gpu:<type>:1 bash
```

Newer Slurm versions also accept `--gpus=<type>:1` or `--gpus-per-node`. Which
form a site expects, and the exact `<type>` strings, are in your site skill or
in `sinfo -p <partition> -N -o '%n %G'`.

Inside the job, `nvidia-smi` shows the GPU you got; `watch nvidia-smi` confirms
your program actually uses it. `CUDA_VISIBLE_DEVICES` is set for you.

### Pin the GPU model on mixed partitions

A partition that holds **more than one GPU model** turns a bare
`--gres=gpu:1` into a lottery: the same script can land on either model and
behave differently. Pin the model whenever the code has a **capability**
requirement, not just a memory-size one (see
[cuda-builds.md](cuda-builds.md) for the shared-memory ceiling that differs per
GPU).

The trade-off: pinning a model that only one node has narrows you to that node,
which can mean a long wait. Another partition with that model (even one with a
different CPU architecture) may be idle.

## Checking a job without waiting

A submitted job runs in the background. Tell the user its job ID, that it now
runs on its own, and how to check on it, then **stop**: don't sit in a polling
loop. Check when the user asks, or, if they explicitly want you to wait, poll
every 15 seconds for at most two minutes. A tool that watches jobs for the
user checks about once a minute, not faster.

One status check, in this order:

```bash
squeue -h -j <jobid> -o '%T|%r|%N|%i'          # still queued or running: state|reason|node|id
env TZ=UTC sacct -n -P -X -j <jobid> --format=State,ExitCode,Start,End,NodeList,JobID   # finished
```

- `squeue` prints a row while the job is queued or running (`PENDING`,
  `CONFIGURING` mean waiting; `RUNNING`, `COMPLETING` mean running). Some
  sites answer `Invalid job id` once a job left the queue; treat that like an
  empty answer.
- Empty: the job finished, so ask `sacct`. `COMPLETED` is done (exit code
  `0:0`); `CANCELLED…` was canceled; `FAILED`, `TIMEOUT`, `OUT_OF_MEMORY`,
  `NODE_FAIL`, `PREEMPTED`, `BOOT_FAIL`, `DEADLINE` are failures, and the
  state word is the reason. The exit code is the part before `:`.
  Any other state with an End time is finished: count it as done and as
  failed ("K unknown (STATE)"), and look at the log. Without an End time,
  treat it as waiting.
- `TZ=UTC` makes sacct's times (which carry no zone) UTC, whatever zone the
  cluster runs in.
- **Arrays:** `squeue`/`sacct` return one row per task (`<id>_<n>`) plus
  ranges of waiting tasks (`<id>_[5-9]`). Sum them up: any task running means
  running; waiting with none done means pending; when nothing waits or runs,
  any failed or unknown task means failed, else any canceled means canceled,
  else completed. Report "N of M tasks done, K failed (TIMEOUT), K unknown
  (STATE)".
- **Heterogeneous jobs:** `100+0`, `100+1` are the components of one job `100`.
  It counts as finished only when every component is (fold the states like an
  array, report "2 components"); `scancel 100` acts on the whole job.
- A site without job accounting (`sacct` errors) only has `squeue`'s answer.

The job's output file, for the last lines of the log:

```bash
scontrol show job <jobid> | tr ' ' '\n' | grep '^StdOut='   # while Slurm still knows the job
sacct -n -P -X -j <jobid> --format=WorkDir,JobName           # later: <WorkDir>/<JobName>-<jobid>.out with --output=%x-%j.out
tail -n 100 <output file>
```

### Your queue and recent jobs

"What is in my queue" and "what ran lately" include jobs started outside any
tool:

```bash
env TZ=UTC squeue --me -h -o '%i|%T|%P|%N|%r|%V|%S|%j'      # id|state|partition|node|reason|submitted|start|name
env TZ=UTC sacct -X -n -P -S now-7days --format=JobID,State,Partition,NodeList,Submit,Start,End,JobName
```

Seven days is a good default; look further back (up to about 30 days) only
when asked.

## Diagnosing a job that won't start

`squeue` reasons can mislead. `(ReqNodeNotAvail, May be reserved for other
job)` usually does **not** mean a reservation; the common cause is that every
GPU of the requested model is already allocated. Check:

```bash
squeue -j <jobid> -o '%.10i %.8T %R'           # state + reason
scontrol show res                              # "No reservations in the system" -> not a reservation
sinfo -p <partition> -o '%20n %10T %10E %G'    # node state + drain reason + GRES
scontrol show node <node> | grep -E 'State|CfgTRES|AllocTRES'
squeue -w <node> -o '%i %u %b %T'              # who holds that node's GPUs
```

The decisive line is `AllocTRES` versus `CfgTRES`: `gres/gpu=4` allocated out
of `gres/gpu=4` configured means the node's GPUs are full, even though its state
reads `MIXED` (Slurm reports `MIXED` on CPU occupancy, so a node with free CPUs
and no free GPUs still looks partly idle).

Other common reasons: `Priority` (others are ahead; wait), `Resources` (you're
next, waiting for resources to free up), `QOSMaxJobsPerUserLimit` /
`AssocGrpGRES` (you hit a limit), `PartitionTimeLimit` (your `--time` exceeds
the partition's `MaxTime`; the job will never start, so lower it).

## Don't wrap a single-task job in `srun`

Inside an `sbatch` script, launching a single process with `srun ./myprog` can
fail before your code runs:

```
srun: error: CPU binding outside of job step allocation,
             allocated CPUs are: 0x…
srun: error: Unable to satisfy cpu bind request
```

On nodes with hyperthreading (`ThreadsPerCore=2` in `scontrol show node`), an
allocation can be a **sparse** CPU mask of hyperthread siblings, and `srun`'s
default affinity can't map onto it, especially with `OMP_PROC_BIND` or
`OMP_PLACES` set.

Fix: for one process, **drop `srun`** and run the program directly. It already
inherits the allocation's CPUs, memory and `CUDA_VISIBLE_DEVICES`.

```bash
export OMP_NUM_THREADS="${SLURM_CPUS_PER_TASK:-8}"
./myprog args...            # not: srun ./myprog args...
```

Keep `srun` for genuinely multi-task or MPI steps. If you need it for a single
task, `srun --cpu-bind=none` sidesteps the error.

## Never point `--output`/`--error` at `/tmp`

`/tmp` (and `/var/tmp`, `/dev/shm`) is **node-local**. Don't use any of them as
the job's working or output folder. A job with

```bash
#SBATCH --output=/tmp/myjob_%j.out
```

runs, ends `COMPLETED`, and leaves you nothing: the file sits on the compute
node's own disk, invisible from anywhere else and eventually deleted. It is easy
to misread as "the job produced no output".

Use a shared path: the submit directory (`--output=%x_%j.out`), your home, or a
project folder. The same goes for results you need after the job. Node-local
`/tmp` (or a site's local scratch) is fine and fast for temporary data *within*
one job. Which shared folders exist, and their quotas: see your site skill.
Staging into node-local scratch with `trap` cleanup, and the storage order:
[placement-and-preemption.md](placement-and-preemption.md).

## Create the `--output`/`--error` folder first

Slurm opens the output file when the job starts but **doesn't create missing
folders**. If `--output=logs/%x_%j.out` points into a folder that doesn't exist
the job fails immediately and leaves no log, so you have nothing to read.
Make the folder before submitting, from the directory you submit in:

```bash
mkdir -p logs
sbatch job.sh
```

A missing `--chdir` folder is worse: Slurm silently falls back to `/tmp`
(node-local, see above). Create it in the same step:
`mkdir -p /shared/path && sbatch job.sh`.

Relative paths are relative to the submit directory. In job arrays, create the
folder once before `sbatch --array`, not inside the tasks.

## Several clusters: `-M <cluster>`

Some sites run more than one Slurm cluster behind one login host, each with its
own queue and accounting. Without `-M`, commands talk to the default cluster
only, so a job submitted elsewhere seems to vanish.

```bash
sbatch -M <cluster> job.sh
squeue -M <cluster> --me
squeue -M all --me                       # every cluster
sacct -M <cluster> -j <jobid>
scancel -M <cluster> <jobid>
sinfo -M <cluster> -s
```

Cluster names are site facts (your site skill). `sacctmgr show cluster format=Cluster,ControlHost` or `sinfo -M all -s`
lists what the controller knows.

## Extending a running job's time limit

Users generally **can't** raise their own time limit. `scontrol update JobId=<id>
TimeLimit=...` works for you only to *reduce* it, unless the site lets users
extend (rare, and often through a special QOS). To extend, ask support (ticket
advice: research-computing `references/getting-help.md`) with the job ID, the
current limit (`squeue -j <id> -o "%L %l"`), what's needed and why, and whether
the job can checkpoint. Don't wait until the last minute: for future runs add a
margin to `--time`, save checkpoints, and ask for a warning five minutes before
the limit:

```bash
#SBATCH --signal=B:USR1@300
trap 'touch STOP; echo "time almost up"' USR1   # without a trap, USR1 kills the batch shell
python train.py &
wait $!
```

The trap runs only while bash is in `wait`, which is why the program runs in
the background; the program itself checks for the `STOP` file and saves a
checkpoint.

## A job that ended badly

```bash
sacct -j <jobid> --format=JobID,State,ExitCode,Elapsed,Timelimit,ReqMem,MaxRSS
```

[failures.md](failures.md) has the full list of facts to collect and the
log lines that name a cause.

- `TIMEOUT`: hit `--time`; ask for more, or checkpoint.
- `OUT_OF_MEMORY` (or `oom-kill` in the error file): `MaxRSS` reached
  `--mem`; ask for more memory.
- `FAILED` with an exit code: read the `.err`/`.out` files from `--output`.
- `CANCELLED by <uid>`: you, an admin, or a dependency cancelled it.

Read the end of the output file before changing anything, then match the
symptom; resubmitting the same script unchanged teaches nothing:

| Symptom in the output or `sacct` | Usual cause | Fix |
|---|---|---|
| `TIMEOUT` | The job hit `--time` | More time (within the partition's `MaxTime`), or checkpoint and resume |
| `OUT_OF_MEMORY`, `oom-kill`, `Killed` | `MaxRSS` reached `--mem` | Ask for more `--mem` (check `MaxRSS` of the failed run), or process less at once |
| `module: command not found` | The script runs in a shell without the module system | Start the script with `#!/bin/bash` and load modules in it; `source /etc/profile` on sites that need it |
| `Lmod has detected the following error: ... unknown` / `module ... not found` | Wrong module name or version | `module spider <name>` for the exact name; load its prerequisites first |
| `<tool>: command not found` (exit 127) | The program isn't on `PATH` in the job | `module load` it in the script, or activate the environment that has it |
| `CUDA driver version is insufficient for CUDA runtime version` | The code was built for (or ships) a newer CUDA than the node's driver | Load an older CUDA module, or pin the GPU type whose nodes have a newer driver (`nvidia-smi` shows the driver's CUDA version) |
| `no kernel image is available for execution on the device` | Built for another GPU architecture | Rebuild with the right `-arch`/`CMAKE_CUDA_ARCHITECTURES` ([cuda-builds.md](cuda-builds.md)) |
| `No such file or directory` with `C:\`, `/Users/...` or a laptop folder | A path from the laptop | Make paths relative to the job's folder ([from-your-laptop.md](from-your-laptop.md#paths-from-the-laptop-dont-exist-on-the-cluster)) |
| `Disk quota exceeded` | A folder is over its quota | Write to scratch and copy the results; remove old files you own ([failures.md](failures.md#match-the-log-line)) |
| `/bin/bash^M: bad interpreter` | Windows line endings | `sed -i 's/\r$//' job.sh` |
| `pip install`, `git clone` or a download **hangs** | No direct internet on compute nodes, or `no_proxy` sends an internal host past the proxy | [proxy-and-github.md](proxy-and-github.md) |
| `COMPLETED` but no output | `--output` on node-local `/tmp`, or the program wrote elsewhere | [Never point `--output` at `/tmp`](#never-point---output--error-at-tmp) |
