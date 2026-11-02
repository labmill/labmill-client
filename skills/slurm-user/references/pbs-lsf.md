# PBS and LSF instead of Slurm

Most of this skill holds for any batch cluster: log in, copy data, write a job
script, submit it, watch it, read the output. Only the commands and the script
header lines change. First find out which scheduler runs: `command -v sbatch
qsub bsub`. Use the table of that scheduler; where a site skill names the
queues, accounts and limits, it wins.

## Commands

| Purpose | Slurm | PBS (OpenPBS, PBS Pro) | LSF |
|---|---|---|---|
| Submit | `sbatch job.sh` | `qsub job.sh` | `bsub < job.sh` |
| Your jobs | `squeue -u $USER` | `qstat -u $USER` | `bjobs` |
| One job, with detail | `scontrol show job ID` | `qstat -f ID` | `bjobs -l ID` |
| A finished job | `sacct -j ID` | `qstat -x -f ID` | `bjobs -a ID`, then `bhist -l ID` |
| Cancel | `scancel ID` | `qdel ID` | `bkill ID` |
| Queues | `sinfo` | `qstat -Q -f` | `bqueues -w` |
| Nodes | `sinfo -N` | `pbsnodes -a` | `bhosts` |

`bsub` reads the script from standard input (`<`); `bsub job.sh` would run the
file as a command and ignore the `#BSUB` lines. A PBS job ID looks like
`123.server`; an LSF job ID is a number. LSF forgets finished jobs after
`CLEAN_PERIOD` (one hour by default): read `bhist` or the output file after that.

## Job script header

| Need | Slurm | PBS | LSF |
|---|---|---|---|
| Name | `#SBATCH --job-name=x` | `#PBS -N x` | `#BSUB -J x` |
| Queue | `--partition=q` | `#PBS -q q` | `#BSUB -q q` |
| CPUs and memory | `--cpus-per-task=8 --mem=32G` | `#PBS -l select=1:ncpus=8:mem=32gb` | `#BSUB -n 8` and `#BSUB -R "rusage[mem=32768]"` (MB; the unit is a site setting) |
| One GPU type | `--gres=gpu:a100:2` | `#PBS -l select=1:ngpus=2:gpu_type=a100` (resource names differ per site) | `#BSUB -gpu "num=2:gmodel=a100"` |
| Time limit | `--time=1-02:00:00` | `#PBS -l walltime=26:00:00` | `#BSUB -W 26:00` (minutes granularity) |
| Output file | `--output=x.out` | `#PBS -j oe` and `#PBS -o x.out` | `#BSUB -oo x.out` |
| Work folder | `--chdir=DIR` | `cd DIR` in the script (PBS starts in `$HOME`) | `#BSUB -cwd DIR` |
| Allow requeue | `--requeue` | `#PBS -r y` | `#BSUB -r` |
| Mail at the end | `--mail-type=END,FAIL` | `#PBS -m ae` | `#BSUB -N` |

## Differences that cause failures

- PBS starts the job in `$HOME`, not in the folder you submitted from: `cd` first.
  `$PBS_O_WORKDIR` is the folder of the `qsub` call.
- PBS and LSF have no `srun`. Run the program directly, or with `mpirun` (the
  site skill names the MPI module).
- Resource names (`ngpus`, `gpu_type`, `mem` units, `-R` strings) are chosen by
  each site. If a submit is refused with "Unknown resource" or "Illegal attribute",
  read the site skill or ask the cluster (`qstat -Qf`, `bqueues -l`, `lsinfo`).
- A job in `Q` (PBS) or `PEND` (LSF) shows why in `qstat -f` (`comment`) or
  `bjobs -l` (the PENDING REASONS block).
- Exit codes: PBS Pro `Exit_status` 271 means the job was killed with `qdel`;
  LSF `EXIT` with `TERM_OWNER` means `bkill`, `TERM_MEMLIMIT` and `TERM_RUNLIMIT`
  mean memory and time limits.

In Labmill, `draft_job` writes these lines for a cluster of type `pbs` or `lsf`.
It does not yet support job arrays, QOS or preemptible queues, node-local scratch
or `environment`, and LSF jobs on more than one node. Labmill reports that instead
of trying.
