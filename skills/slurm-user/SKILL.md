---
name: slurm-user
description: >-
  Run a researcher's work on a Slurm HPC cluster. Use whenever the user connects
  to or works on a Slurm cluster: SSH to a login host (keys, ProxyJump, 2FA),
  copying files with scp / sftp / rsync or a transfer host, writing a job
  script, sbatch, srun --pty or salloc sessions, squeue / sacct / scontrol,
  scancel, sinfo, choosing a partition, account or QOS, requesting GPUs
  (--gres=gpu:<type>:N, --gpus), job arrays, module load, building CUDA code,
  or reaching the internet or GitHub from a compute node through a proxy. Also
  trigger on goals like "run my script on the HPC cluster", "why is my job
  pending", "my job finished but there is no output", "get me a GPU node",
  "git clone hangs on the cluster". Generic: hosts, partitions, GPU models,
  storage paths and proxy settings come from the user's site skill
  (site-<institution domain>).
---

# Slurm user

Helps a researcher who already runs code on their own computer move that work
to a **Slurm** HPC cluster: log in, copy data, write a job script, submit it,
watch it, and get the results back.

**This skill holds no site facts.** Login and transfer hosts, partition names,
accounts, GPU models, storage folders, module names, proxy addresses and
documentation links differ per institution. They live in the user's **site
skill**, named `site-<institution domain>` with dots as dashes (for example
`site-example-edu`). Load it whenever a value below says "see your site skill".
If no site skill is installed, ask the cluster (`sinfo`, `sacctmgr`, `module
avail`) or the site's own documentation rather than guessing.

## Fast facts

- **Log in:** `ssh <user>@<login-host>`. Some sites put the login host behind a
  bastion (`ProxyJump`), some need a VPN, and most ask for a second factor. Add
  an SSH key and a `ControlMaster` entry so you sign in once. Hosts and the
  two-factor method: see your site skill.
- **Move files:** `scp`, `sftp`, `rsync` or `rclone`. Many sites have a
  separate **transfer host** for data, and some login hosts disable the SFTP
  subsystem altogether. Use the transfer host when there is one, copy exactly
  the approved files, and check what would be overwritten first
  ([references/from-your-laptop.md](references/from-your-laptop.md)).
- **Show before you move or submit:** the file list, size and destination
  before any upload; the full job script before `sbatch`; the job before
  `scancel`. Read only names, sizes and small samples (head only) of the user's data files, never whole data files; documentation and code files may be read in bounded pages, never credential values.
- **Data but no code?** Offer to write the analysis script, in its own folder
  next to the data, and show it before it runs.
- **Never compute on the login node.** Get a compute node first with
  `srun --pty bash` or `salloc`. Some sites forbid particular programs there
  (for example AI coding agents); see your site skill.
- **Check before you submit:** run `sbatch --test-only job.sh` first. It
  submits nothing. Slurm says either when and where the job would start, or
  what is wrong with the script (account, partition, time limit, memory). Fix
  the script, then run `sbatch job.sh`. If the check cannot run, submit anyway.
- **Submit:** `sbatch job.sh`; watch with `squeue -u $USER`; afterwards
  `sacct -j <jobid>`; cancel with `scancel <jobid>`. Report the job ID and let
  it run in the background; check when asked instead of polling
  ([references/slurm.md](references/slurm.md#checking-a-job-without-waiting)).
- **Know when a job ends without watching it.** If the user wants mail, add
  `#SBATCH --mail-type=END,FAIL` and leave out `--mail-user`: Slurm then uses
  the address the site has for the account (add `--mail-user=<address>` only
  when the user names another). Do not ask for mail by default. When the user
  comes back after some time, list what ended meanwhile:
  `sacct -u $USER -S <last time> -X -o JobID,JobName,State,End`.
- **Software: modules first.** Before choosing how to get a program a job
  needs, check the module system (`module spider <name>`); then the site's other
  installs (site skill); only then a conda or pip environment or a container
  ([finding software](../research-computing/references/environments.md#finding-software-on-a-cluster-modules-first)).
  Build your own only if the provided version is older than a year or missing.
- **Where the job's data goes:** node-local scratch for a single-node job (stage
  in and out, `trap` cleanup in the job); else a global scratch on the compute
  nodes; home last; never remove reusable software. **Resources:** the
  preemptible partition first if the cluster has one (many have none), the
  smallest viable or soonest-starting node (`sbatch --test-only`), `--requeue`,
  resubmit only after the job has ended (never beside a requeued copy),
  non-preemptible after more than two failed attempts
  ([references/placement-and-preemption.md](references/placement-and-preemption.md)).
- **Partitions and accounts gate access.** Check what you may use with
  `sacctmgr -n show assoc user=$USER format=Account,Partition,QOS` and
  `sinfo -s`.
- **Pin the GPU model** on partitions that mix GPUs:
  `--gres=gpu:<type>:1`, not just `--gres=gpu:1`.
- **Write job output to a shared path**, never to node-local `/tmp`.
- **Compute nodes often reach the internet only through an HTTP proxy**, and
  outbound SSH on port 22 is often blocked. See
  [references/proxy-and-github.md](references/proxy-and-github.md).

## Reference files

- **[references/from-your-laptop.md](references/from-your-laptop.md)**: the
  data and code folders, writing the code for a user who has only data, what
  may be read, what to show before an upload, submit, cancel or download, the
  per-job folder layout, copying exactly the approved files with rclone through
  the transfer host, checking what would be overwritten, fetching results
  without overwriting, and laptop paths in code. Read before moving any data.

- **[references/access.md](references/access.md)**: Slurm login
  and transfer hosts, two-factor prompts, VPN, copying files, and login node
  versus compute node (keys, `~/.ssh/config`, `ProxyJump` and `ControlMaster`
  are in research-computing → `references/ssh.md`). Read for any
  login or transfer problem.
- **[references/slurm.md](references/slurm.md)**: the Slurm commands, a job
  script template, job arrays, interactive sessions, partitions/accounts/QOS,
  requesting GPUs and pinning the model, reading a misleading pending reason,
  the `srun` CPU-binding trap for single-task jobs, the `/tmp` output trap,
  checking a job without waiting (squeue then sacct, arrays), your queue, and
  a table of failure patterns. Read to write, submit or debug a job.
- **[references/failures.md](references/failures.md)**: why did my job fail?
  The `sacct` facts to collect (state, exit code, time and memory used against
  asked, `MaxRSS` from the step lines), what the numbers mean, the log lines
  that name a cause (out of memory, time limit, missing module, CUDA, laptop
  paths, quota, no internet) and remembering the cause per code folder. Read
  when a job failed.
- **[references/placement-and-preemption.md](references/placement-and-preemption.md)**:
  the storage order (node-local scratch, global scratch, home), finding out what
  the cluster has (`sinfo` TmpDisk, `TmpFS`, `df`, `findmnt`), a node-local
  staging script with `trap` cleanup, preemptible-first resource choice, the
  smallest or soonest node, preemption and resubmission, and the one-year
  software rule. Read before choosing partition, scratch or software.
- **[references/cuda-builds.md](references/cuda-builds.md)**: choosing CUDA
  architectures per GPU, the shared-memory ceiling that makes code fail on some
  GPUs, building with CMake/nvcc on the cluster, and partitions with a
  different CPU architecture (ppc64le, aarch64). Read before building GPU code.
- **[references/proxy-and-github.md](references/proxy-and-github.md)**: the
  compute-node HTTP proxy, the `no_proxy` trap that makes internal hosts time
  out, and reaching GitHub over HTTPS or SSH on port 443. Read whenever
  something can't reach the network from a compute node.

Environments (venv, conda, modules), containers, Jupyter, storage, SSH, shell
hygiene, data transfer and how to ask for help are in the **research-computing**
skill.
