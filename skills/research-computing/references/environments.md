# Software environments: finding software, venv, conda, modules

Goal: the same code runs the same way on the cluster as on your computer, and
you can rebuild the environment from a text file. Pick **one** mechanism per
project; don't mix them in one job.

## Finding software on a cluster: modules first

When a job needs a named program (a compiler, MPI, CUDA, an aligner, a
simulation code, a particular Python or R), look in this order and stop at the
first that fits:

1. **The module system.** Site builds are tuned for the hardware and its MPI,
   are already installed, and cost no storage quota. On the login node:

   ```bash
   module spider <name>            # Lmod: case-insensitive search across all versions, incl. those hidden behind a compiler/MPI toolchain
                                   # (with the older Tcl Environment Modules use `module avail <name>`)
   module spider <name>/<version>  # lists what must be loaded first
   module avail                    # browse what is visible now
   module -t avail 2>&1            # plain list, one per line (Lmod prints it on stderr; `2>&1` sends it to a pipe)
   module keyword <word>           # search the module descriptions
   ```

2. **Software the site installs outside modules** (shared install folders the
   site skill names): `ls <folder your site skill names>` first, then
   `command -v <name>` on the login node (if the site's PATH comes from login
   files, `bash -lc 'command -v <name>'`).
3. **A conda/mamba or pip environment**
   ([conda](#conda--mamba-miniforge), [venv and pip](#python-venv-and-pip)).
4. **A container** (Apptainer/Singularity; [containers.md](containers.md)).

Steps 3 and 4 only when the provided version (steps 1 and 2) is **missing or
older than one year** (the version from `module spider`; its release date from the project's
release notes or changelog). Never delete an environment or
container others can reuse to "clean up".

In the job script, start clean and pin versions, prerequisites first (what
`module spider <name>/<version>` printed), and log the result:

```bash
module purge
module load <prerequisite>/<version>
module load <name>/<version>
module list                      # lands in the job log
```

`module` is a shell function, so a non-interactive shell can say `module:
command not found`; the fix is in
[Environment modules (Lmod)](#environment-modules-lmod) below. From your
own machine, run it through a login shell, options before the subcommand
included: `ssh host 'bash -lc "module -r spider torch"'` (`bash -lc` reads the
login profile that defines `module`; plain `ssh host module …` does not).

## Python venv and pip

```bash
module avail Python                      # pick a Python the cluster provides, if you use modules
python3 -m venv /path/to/project/.venv   # see your site skill for where project space lives
source /path/to/project/.venv/bin/activate
python -m pip install --upgrade pip
python -m pip install numpy pandas        # inside an active venv: NO --user
python -m pip freeze > requirements.txt
deactivate
```

- Install **numpy together with torch** in a fresh venv
  (`python -m pip install torch numpy`); without numpy, torch prints "Failed
  to initialize NumPy" and tensor/array conversion breaks.

- Use `python -m pip` so the pip that runs belongs to the interpreter you
  think it does. `which python pip` should both point into the venv.
- **Never add `--user` inside an active venv.** `pip install --user` means
  "install into `~/.local`", but a venv is isolated from user site-packages,
  so pip stops with `ERROR: Can not perform a '--user' install. User
  site-packages are not visible in this virtualenv.` Drop the flag. Outside any
  venv (system or module Python) `--user` is the way to install without admin
  rights, but a venv is better: it is removable and reproducible. Packages in
  `~/.local` also shadow conda and module Pythons of the same version and
  cause confusing imports (`PYTHONNOUSERSITE=1` turns that off), and distro
  Pythons that follow PEP 668 refuse `--user` installs altogether ("externally
  managed environment"): use a venv there.
- Recreate it elsewhere: `python3 -m venv .venv && . .venv/bin/activate &&
  python -m pip install -r requirements.txt`.
- A venv records an absolute path to its interpreter. Build it with the same
  Python you load in jobs, in a folder that compute nodes can see (network
  storage, not node-local `/tmp`). Don't copy a venv between machines; rebuild
  it.
- `uv` is a fast drop-in: `uv venv .venv && uv pip install -r requirements.txt`
  (installation: <https://docs.astral.sh/uv/>, into your own home folder).

## Requirements and lock files

- `requirements.txt` from `pip freeze` pins everything installed; a hand-written
  one lists only direct dependencies with ranges. Keep both: a short one for
  humans, a frozen one for reproducing a result.
- Also record the Python version (`python --version`) and, for GPU code, the
  CUDA build of the framework (for example the wheel index you used).
- Conda: `conda env export --from-history > environment.yml` (portable) and
  `conda list --explicit > spec.txt` (exact, same platform only).
- Commit these files next to the code. Rebuilding from them is the test.

## Where libraries put their caches and downloaded weights

Many tools download big files on first use into a folder under `$HOME`: pip
(`~/.cache/pip`), Hugging Face (`~/.cache/huggingface`, `HF_HOME`), PyTorch
(`~/.cache/torch`, `TORCH_HOME`), conda packages, and tool-specific ones such
as `~/.cache/<tool>`. On a cluster that is wrong twice: a small home
quota fills up, and a job array of many tasks downloads the same
gigabytes once per task (or hits the download server at once). Instead:

- Pick one folder in project or shared space (see your site skill) and point
  the cache variable there (`export HF_HOME=/path/to/project/hf`; most
  libraries want an **absolute** path), or set `HOME=/path/to/project/home`
  for a job that should keep everything it downloads. `XDG_CACHE_HOME` moves
  most `~/.cache` users at once. Check `env` first: variables a container image
  or module already sets win over `HOME` and `XDG_CACHE_HOME`, so override
  those by name.
- Fetch the files **once**, from one job or from the login node, with the
  library's own download command or by running a tiny example, then let every
  task read them. Check the size afterwards with `du -sh`.
- Compute nodes may have no internet access, or only through a proxy (see
  your site skill): download where there is access and run offline
  (`HF_HUB_OFFLINE=1`).
- A venv that must reuse a framework's already installed packages (for
  example a GPU `torch` that came with a container image) is created with
  `python -m venv --system-site-packages /path/to/venv`, so `pip install`
  adds to it without pulling a second `torch`.

## conda / mamba (Miniforge)

Install Miniforge (conda-forge defaults, no licence limits) into your own
space, not a shared location: <https://github.com/conda-forge/miniforge>.

```bash
# one-time install into a folder you own (path: see your site skill)
bash Miniforge3-Linux-x86_64.sh -b -p /path/to/project/miniforge3
/path/to/project/miniforge3/bin/conda init bash   # edits ~/.bashrc; read shell.md first
# conda is not on PATH in this shell yet: open a new shell, or
source /path/to/project/miniforge3/etc/profile.d/conda.sh
conda create -y -p /path/to/project/envs/myenv python=3.11 numpy
conda activate /path/to/project/envs/myenv
```

- **Environments are big** (many thousands of small files). If the home folder
  has a small quota (see your site skill), keep the conda install, the
  environments and the package cache in project space:
  `conda config --add pkgs_dirs /path/to/project/conda-pkgs`. `du -sh ~/.conda
  ~/miniforge3` shows what to move; `conda clean --all` frees the cache.
- `mamba` solves faster, and recent conda uses the same solver, so
  `conda create` is usually fine too.
- Create environments by **prefix** (`-p path`) when a group should share the
  folder; by name (`-n`) they land in `<install>/envs` for a Miniforge you own
  (`~/.conda/envs` only with a shared, admin-owned install).
- **In a batch script** conda's shell function isn't loaded, so initialize it
  explicitly, then activate:

  ```bash
  #!/bin/bash
  #SBATCH --time=01:00:00
  source "$(conda info --base)/etc/profile.d/conda.sh"   # needs conda on PATH (module load, or the full path to conda)
  conda activate /path/to/project/envs/myenv
  python train.py
  ```

  If `conda` isn't on PATH in the job, source the file by full path:
  `source /path/to/project/miniforge3/etc/profile.d/conda.sh`.
- Don't `conda install` into an environment that running jobs use. Clone
  first: `conda create -p /path/to/new --clone /path/to/old`.
- `pip` inside a conda env is fine **after** the conda packages: install conda
  packages first, then `python -m pip install …`, and don't go back to conda
  for the same packages.

## Environment modules (Lmod)

Modules put pre-built compilers, libraries and applications on PATH. They are
the right choice for MPI, vendor-tuned numerics and tools you don't need to
modify.

```bash
module avail                 # what's visible with the current toolchain
module spider Python         # search everything, including versions hidden behind a toolchain
module spider Python/3.11.3  # shows which modules must be loaded first
module load Python/3.11.3    # always give the version; ml is a short alias
module list                  # what's loaded now
module show Python/3.11.3    # what it changes (paths, variables)
module purge                 # unload everything
```

- **Pin versions.** A bare `module load Python` picks a default that the site
  changes over time, which silently changes your results.
- **Start scripts with `module purge`**, then load exactly what the job needs;
  otherwise a module loaded in your login shell (and inherited by `sbatch`)
  leaks in.
- **One toolchain per job.** Modules built with a compiler/MPI toolchain (the
  suffix of the version, such as `foss-2023a`) only work together when the
  toolchains match. Mixed toolchains cause `GLIBCXX` and `undefined symbol`
  errors.
- **Batch scripts** are non-interactive. If you see `module: command not
  found`, initialize Lmod first (the init file's path is in your site skill):

  ```bash
  source /path/to/lmod/init/bash     # path: see your site skill
  module purge
  module load Python/3.11.3
  ```

- Different node types can have different module trees; the same name may not
  exist everywhere. Test on the node type you submit to
  (`srun --pty -p <partition> bash`, then `module avail`).
- Which modules exist and where: see your site skill.

## Read the README and the build files before you guess

For code you did not write, `README*`, `INSTALL*`, `CMakeLists.txt`,
`Makefile`, `setup.py`, `pyproject.toml`, `requirements*.txt` and
`environment.yml` say how it is built and run. Read them whole (page through
a long README) before choosing modules, an image or a command. Do not guess
file names such as `run.sh`: `ls` the folder and open only files that exist.
Missing scripts are written where the code will run (on the cluster, in the
job folder), never into the original copy.

## Build an environment once, reuse it in every job

Many jobs of one project need the same venv, conda environment or container
image. Build it **once** in a folder that stays (the site's software folder
when the site skill names one, else `$HOME/.labmill/envs`; not a purged
scratch), name it after a hash of its requirements file, and let every job
reuse it. Two jobs that start together must not build it twice, and a build
that stopped half way must never be used. The recipe for a batch script
(run it in the job folder, on the node where the job runs, after the
`module load` lines that provide Python, conda or Apptainer):

```bash
ENVS="${ENVS:-$HOME/.labmill/envs}"             # or the site's software folder
# The hash covers the requirements, the Python that is really found (after the
# module loads) and the CPU type, so python 3.10 and 3.12, or x86 and ARM
# nodes that share a home folder, never share one venv.
H=$({ cat -- requirements.txt; python3 -c 'import sys;print(sys.version,sys.base_prefix)'; uname -m; } | sha256sum | cut -c1-16)
ENV="$ENVS/venv-$H"                           # a new requirements file gives a new folder
L="$ENV.lock.d"; mkdir -p "$ENVS"
until mkdir "$L" 2>/dev/null; do              # one builder at a time; others wait here
  if find "$L" -maxdepth 0 -mmin +60 | grep -q .; then   # left by a killed job
    mv "$L" "$L.stale.$(date +%Y%m%d%H%M%S)"  # move aside, do not delete
  else
    sleep 5                                   # add a limit to the wait in a real script
  fi
done
(
  trap 'kill "$P" 2>/dev/null; rmdir "$L"' EXIT   # stop the refresher, release the lock on every way out
  while sleep 300; do [ -d "$L" ] && touch -c -- "$L" 2>/dev/null || break; done >/dev/null 2>&1 &   # keep the lock fresh during a long build
  P=$!
  if [ ! -f "$ENV/.labmill-ok" ]; then          # the marker is written last
    [ ! -e "$ENV" ] || mv "$ENV" "$ENV.incomplete.$(date +%Y%m%d%H%M%S)"   # a crashed build: move aside, do not delete
    python3 -m venv "$ENV" && "$ENV/bin/pip" install -r requirements.txt && touch "$ENV/.labmill-ok"
  fi
) || exit 1
[ -f "$ENV/.labmill-ok" ] || exit 1
. "$ENV/bin/activate"
```

- **Build in the final folder, not in a temporary one that you move.** A venv
  or conda prefix keeps its own path in `activate`, script headers and
  `conda-meta`, so a moved copy breaks. The `.labmill-ok` marker, written last,
  takes the place of the atomic move.
- **Use a lock folder (`mkdir`), not `flock`.** `flock` fails on shared file
  systems mounted without it (Lustre often is). A job that was killed leaves
  its lock folder behind: a lock folder older than the wait limit (one hour
  here) is moved aside to `<lock>.stale.<time>`, never deleted, and the build
  goes on. List them with `ls -d "$ENVS"/*.lock.d*`. The builder touches its
  lock folder every 5 minutes while it builds (a background loop killed in the
  same trap that releases the lock), so a build longer than an hour is not
  taken for a stale lock and built twice.
- Conda: the same pattern with `conda env create -p "$ENV" -f
  environment.yml` (or `mamba env create`; no `-y`, older conda rejects it and
  the prefix is always new), hash of `environment.yml` plus `uname -m`, then
  `eval "$(conda shell.bash hook)"; conda activate "$ENV"`. Load the site's
  conda module first; do not guess its name, use `module spider conda`.
- Apptainer: pull a `docker://` image once into a SIF file and run the job
  command in it. Pull to a temporary name, then `mv` it (a SIF file can move):

  ```bash
  export APPTAINER_CACHEDIR="$ENVS/apptainer-cache"   # keeps the layer cache out of ~/.apptainer
  SIF="$ENVS/sif/ubuntu_22.04.sif"; mkdir -p "$ENVS/sif" "$APPTAINER_CACHEDIR"
  [ -f "$SIF" ] || { apptainer pull "$SIF.tmp.$$" docker://ubuntu:22.04 && mv "$SIF.tmp.$$" "$SIF"; }
  apptainer exec --nv "$SIF" python train.py   # --nv only for a GPU job
  ```

  Use `singularity` when `apptainer` is not installed. The image needs `bash`
  when you run the command as `bash -c "..."`. Apptainer mounts the home
  folder and the current folder by default; add `--bind <folder>` for data
  elsewhere.
- Python: the hash above already holds the interpreter that is found, so a venv
  built by one Python is never used under another.
- **Never delete these folders** to "clean up": they are reusable software.
  After a job ends, remove only the job's own working files.
- Labmill does all of this for you: `draft_job` takes an `environment` with
  `modules`, `venv`, `conda` or `apptainer` and writes these lines into the
  script, so the review shows them.

## Which one to use

| Need | Use |
|---|---|
| Pure Python, a few packages | module Python + venv |
| Complex binary stack (CUDA, R, bioinformatics) | a module if `module spider` finds one, or the site's own install; otherwise a conda/mamba env or a container |
| Exact reproducibility across sites | container (containers.md) |
| MPI or compiler work | modules with a single toolchain |

## Checks

```bash
which python; python -c "import sys; print(sys.prefix)"                          # where am I?
python -c "import torch; print(torch.__version__, torch.cuda.is_available())"   # GPU frameworks
```

Test in a short interactive job on the node type you will use before
submitting a long one.
