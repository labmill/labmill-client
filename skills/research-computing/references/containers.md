# Containers: Apptainer, Docker images

A container packs software and its libraries into one image, so it runs the
same on your computer, a cluster and a cloud. On HPC clusters the rootless
runtime is **Apptainer** (formerly Singularity); Docker itself usually isn't
installed because it needs root. Whether Apptainer is a module or a system
command, and which registry mirrors exist: see your site skill.

## Apptainer basics

```bash
module avail Apptainer              # or: apptainer --version (system install)
module load Apptainer               # only if it is a module; pin the version

apptainer pull docker://python:3.11-slim            # -> python_3.11-slim.sif
apptainer exec python_3.11-slim.sif python --version
apptainer exec docker://python:3.11-slim python --version   # pulls and converts on the fly
apptainer shell python_3.11-slim.sif                 # interactive shell inside
apptainer run image.sif args                         # runs the image's default entry point
```

- **GPU:** `--nv` makes the host's NVIDIA driver and libraries visible:
  `apptainer exec --nv image.sif python -c "import torch; print(torch.cuda.is_available())"`.
  Run it on a GPU node. The image only needs the CUDA user-space libraries;
  the host driver must be new enough for them (`nvidia-smi` shows the driver's
  maximum CUDA version).
- **Folders:** by default your home folder, the current directory and `/tmp`
  are visible. Bind anything else explicitly:
  `apptainer exec --bind /path/to/data:/data image.sif ls /data`
  (`--bind host:container[:ro]`, comma-separated for several). Inside the
  container use the container-side path.
- **The image is read-only.** Write results to a bound folder, not into the
  image. Environment variables from your shell are inherited; add `--cleanenv`
  when stray variables (`PYTHONPATH`, `LD_LIBRARY_PATH`) break things.
- **Cache location:** pulls cache layers in `~/.apptainer/cache` and can fill a
  small home quota. Point it at project or scratch space and clean it:

  ```bash
  export APPTAINER_CACHEDIR=/path/to/project/apptainer-cache   # path: see your site skill
  export APPTAINER_TMPDIR=/path/to/scratch/apptainer-tmp       # big temporary space for builds
  mkdir -p "$APPTAINER_CACHEDIR" "$APPTAINER_TMPDIR"
  apptainer cache clean --days 30
  ```

  Older installs read the same settings from `SINGULARITY_CACHEDIR` and
  `SINGULARITY_TMPDIR`.
- **Pull and build on a compute node**, not on a shared login host: they are
  CPU, memory and disk heavy. Use an interactive session
  (`srun --pty -p <partition> --mem=16G --time=01:00:00 bash`) or put the pull
  in a batch script. Pull **once** into a shared folder and reuse the `.sif`
  file from jobs, instead of pulling in every array task.
- **Building from a definition file** (`my.def`):

  ```
  Bootstrap: docker
  From: python:3.11-slim

  %post
      python -m pip install --no-cache-dir numpy pandas
  %environment
      export LC_ALL=C
  ```

  `apptainer build my.sif my.def`. Building as a normal user needs
  `--fakeroot`, which only works if the site enables it; otherwise build
  the image on your own computer with Docker or Podman (they don't produce
  `.sif`): push it to a registry and `apptainer pull docker://<registry>/img:tag`
  on the cluster, or `docker save -o img.tar img:tag`, copy the tar and run
  `apptainer build img.sif docker-archive://img.tar`. A remote build service
  helps if the site offers one.
- Private registries need a login: `apptainer registry login --username <user>
  docker://<registry>` (Apptainer 1.2+), or `apptainer pull --docker-login …`
  for one pull. Keep the password out of shell history and scripts:
  `read -rs APPTAINER_DOCKER_PASSWORD; export APPTAINER_DOCKER_PASSWORD` (with
  `APPTAINER_DOCKER_USERNAME`). Docker Hub rate-limits anonymous pulls, which
  shows up as `toomanyrequests`; log in or use a mirror from your site skill.

## In a Slurm job

```bash
#!/bin/bash
#SBATCH --gres=gpu:1
#SBATCH --time=02:00:00
module purge
module load Apptainer                  # only if needed at your site
apptainer exec --nv --bind /path/to/data:/data /path/to/train.sif \
    python /data/code/train.py --out /data/results
```

Bind both your data and your code folders so results land on shared storage,
not inside the container.

## Docker images versus SIF files

- A Docker image from a registry (`docker://name:tag`) works as the source
  for Apptainer; there's no need to install Docker. Apptainer converts it to a
  single `.sif` file that you can copy like any file.
- Most recipes online say `docker run …`. Translate: `-v a:b` becomes `--bind
  a:b`; `--gpus all` becomes `--nv`; `-e VAR=x` becomes `--env VAR=x`; `-w dir`
  becomes `--pwd dir`.
- Images that run as root or expect to write to system folders usually still
  work, because Apptainer runs you as yourself; but if they try to write inside
  the image, point that path at a bound writable folder.
- Use a pinned tag (`:1.2.3`) or digest (`@sha256:…`), not `:latest`.

## When a container beats modules (and the other way round)

- **Container:** complex dependency stacks (CUDA + framework + system
  libraries), software whose only supported install is an image, results that
  must be reproducible across sites, tools with conflicting dependencies.
- **Modules:** MPI codes that must match the cluster's interconnect, compiled
  code tuned to the site's CPUs, small tools, anything where an admin already
  maintains the right build.
- **Conda or venv:** your own Python code with ordinary dependencies; see
  environments.md.
