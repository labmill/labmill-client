# Example University: the bigcluster cluster

Template for a cluster folder's reference file (docs/dev/sites.md). Name the
sources at the top: official documentation URLs, the upstream skill, or a
confirmed answer from the site. Mark anything not confirmed by an official page
**(unverified)**.

Put here only what is specific to this one cluster. Facts shared by every
cluster of the institution (accounts, two-factor sign-in, the shared login
and transfer hosts, the campus proxy, general storage rules) belong in the
site's top-level `References/`.

## What to cover

- Partitions, their limits and who may use them
- GPU types and the exact `--gres` / `--gpus` strings
- Modules, compilers and CUDA installs; local wrapper commands
- Cluster-specific paths (node-local scratch, software trees)
- GPU node driver and its CUDA version
