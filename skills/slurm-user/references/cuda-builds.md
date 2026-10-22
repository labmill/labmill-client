# Building CUDA code for a cluster's GPUs

Generic guidance. Which GPU models a cluster has, where its CUDA toolkits and
newer CMake builds are installed, module names, and any partitions with a
different CPU architecture are in your site skill (`site-<institution domain>`).

## CUDA architecture per GPU

`-arch` / `-gencode` for `nvcc` and `CMAKE_CUDA_ARCHITECTURES` take the compute
capability without the dot. Common datacenter and workstation GPUs (from
NVIDIA's CUDA Programming Guide):

| GPU | Arch | Max shared memory per block (opt-in) |
|---|---|---|
| P100 | 60 | 48 KiB |
| GTX 1080 Ti, P40 | 61 | 48 KiB |
| V100 | 70 | 96 KiB |
| T4, RTX 20xx | 75 | 64 KiB |
| A100, A30 | 80 | 163 KiB |
| A10, A40, RTX 30xx, RTX A6000 | 86 | 99 KiB |
| L4, L40S, RTX 40xx | 89 | 99 KiB |
| H100, H200 | 90 | 227 KiB |

To find what a node has: `nvidia-smi --query-gpu=name,compute_cap --format=csv`
inside a job on that node.

To build one binary for a partition that mixes GPUs, list every arch:
`CMAKE_CUDA_ARCHITECTURES="<arch1>;<arch2>"` (or one `-gencode` per arch). List
each GPU model your partition has (your site skill lists them).

## The shared-memory ceiling

Shared memory per block is a hard architectural limit: no compiler flag,
environment variable or `cudaFuncSetAttribute` call raises it. A kernel that
opts into more than 64 KiB of dynamic shared memory **cannot run on, for
example, a T4 or a 1080 Ti at all** (while an A100 or H100 allows far more). The usable amount is slightly *below* the nominal maximum
(e.g. 64512 B of a T4's 65536 B) because the kernel's static shared memory counts
against the same budget, so a kernel needing exactly 64 KiB still fails.

A higher arch number is not strictly better: a T4 (75) has *less* shared memory
than a V100 (70) or an A100 (80).

Building for several arches does **not** mean the code will *work* on all of
them: a kernel can compile for `75` and still fail at launch on a T4. If a run
dies during GPU setup while the same binary works elsewhere, suspect the GPU
model before the build flags, and pin `--gres=gpu:<type>:1` to the model you
tested on (see [slurm.md](slurm.md)).

## Building on the cluster

The default environment can be thin: `nvcc` may not be on `PATH`, and the system
`cmake` may be older than 3.18 (the first version with
`CMAKE_CUDA_ARCHITECTURES`). Use a module (`module avail cuda`, `module avail
cmake`) or the site's shared installs (paths in your site skill):

```bash
module load <cuda-module> <cmake-module>         # or: export PATH=<cuda-dir>/bin:$PATH
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_CUDA_ARCHITECTURES="<arch1>;<arch2>" \
  -DCMAKE_CUDA_COMPILER="$(command -v nvcc)"
cmake --build build -j "${SLURM_CPUS_PER_TASK:-8}"
```

- `nvcc` **cross-compiles** for a target arch, so you can build on a node
  without a GPU. Build in a job, not on the login node, if the build is heavy.
- At run time, the CUDA runtime libraries must be found: load the same module,
  or `export LD_LIBRARY_PATH=<cuda-dir>/lib64:$LD_LIBRARY_PATH`.

## Partitions with a different CPU architecture

Some clusters have GPU nodes with a different CPU (POWER9 `ppc64le`, ARM
`aarch64`) next to x86_64 ones. Binaries on the shared filesystem built for
x86_64, including shared toolchains, are visible from those nodes and fail the
moment you run one:

```
cannot execute binary file: Exec format error
```

`file <binary>` shows `x86-64`. On such nodes:

- use the node's **native** toolchain (often `/usr/local/cuda` and the system
  `cmake`/`gcc`; your site skill lists them), not the shared x86_64 installs;
- **build on that node itself**, inside a job (there is usually no
  cross-compiler on the login host);
- use a **separate build directory** per architecture (`build-ppc64le/` versus
  `build/`): both see the same filesystem, so one CMake cache would be reused by
  the wrong architecture.

Such partitions are sometimes the quickest route to a scarce GPU model, at the
cost of a rebuild.
