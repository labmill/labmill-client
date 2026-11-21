# Workflow managers: Snakemake and Nextflow

A workflow manager runs many steps of a pipeline and submits each step to Slurm
itself. Do not turn such a pipeline into one big `sbatch` script: the manager
already knows the order of the steps, their inputs and how to restart.

## How to tell

Look at the file names in the project folder (no file needs to be read for this):

- Snakemake: `Snakefile`, `workflow/Snakefile`, `*.smk`, `config.yaml` or
  `config/config.yaml`, `profiles/<name>/config.yaml`.
- Nextflow: `main.nf`, `*.nf`, `nextflow.config`, `conf/*.config`; or a
  public pipeline named by the user (`nextflow run nf-core/<name>`).

Read the Snakefile or `nextflow.config` (all pages) for the software it needs
(`conda:`, `container:`, `module` lines) and the paths it uses.

## The pattern

The manager's controller is a long, light process. Run it as **one small
batch job** (1 CPU, a few GB, a time limit longer than the whole pipeline), or
in `tmux` on the login node for a short pipeline if the site allows it
(see your site skill). The controller submits the real steps as their own Slurm
jobs. Never run the steps themselves on the login node.

```bash
# Snakemake 8 or newer, with the Slurm executor plugin installed in the env
snakemake --executor slurm --jobs 50 \
  --default-resources slurm_account=<account> slurm_partition=<partition> \
                      mem_mb=4000 runtime=60 \
  --latency-wait 60 --rerun-incomplete
snakemake -n                       # dry run first: the steps and counts, nothing runs

# Nextflow
module load Nextflow               # or a container; pin the version
nextflow run main.nf -profile slurm -resume     # a profile with process.executor = 'slurm'
```

```groovy
// nextflow.config: the Slurm profile
profiles { slurm { process.executor = 'slurm'
                   process.queue    = '<partition>'
                   process.clusterOptions = '--account=<account>'
                   executor.queueSize = 50 } }
```

- Controller job: `#SBATCH --cpus-per-task=1 --mem=4G --time=<longer than the pipeline>`
  and the commands above as the body. A pipeline that outlives the time limit
  restarts with `snakemake ... --rerun-incomplete` or `nextflow run ... -resume`.
- Put the work folder (`.snakemake/`, Nextflow `work/`) on shared scratch that
  compute nodes mount (see filesystems-and-storage.md), not in home: it grows
  large and has many small files.
- Check the partition, account and time limits of the steps against the
  cluster (`sinfo`, `sacctmgr show assoc user=$USER format=account%20`); the
  controller's own limits do not apply to the steps.
- Per-step memory and time belong in the workflow (`resources:` in Snakemake,
  `process` labels in Nextflow), not in the controller's `#SBATCH` lines.
- Started inside a Slurm job, the Snakemake Slurm plugin may warn, and the steps
  can inherit the controller job's `SLURM_*` settings (memory, CPUs). Check the
  plugin documentation for the installed version.
- Cap the number of parallel jobs (`--jobs`, `queueSize`) at a few dozen
  unless the site says more is fine: one workflow can flood the scheduler.
- Software per step: `conda:` or `container:` (Apptainer, see containers.md);
  on a cluster use `--use-conda` or `--use-apptainer`, and in Nextflow
  `apptainer.enabled = true` (or `-profile apptainer` where the config defines
  it, as nf-core pipelines do); pull images once on the login node.
- Results: the workflow's own output folders. Download them like any other
  results (data-transfer.md).
