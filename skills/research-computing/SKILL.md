---
name: research-computing
description: >-
  General research computing know-how for moving work from a laptop to an HPC
  cluster or GPU platform. Use for: Python venv/pip, conda/mamba, Lmod modules,
  containers (Apptainer, Singularity, Docker images, --nv, --bind), Jupyter on
  a compute node (tunnels, kernels, nbconvert), file systems and storage
  (home, project, scratch, quotas, permissions, setgid, NFS caveats, purge,
  checksums), SSH (keys, agent, ~/.ssh/config, ProxyJump, host key changed,
  tmux), shell startup files (.bashrc, why scp breaks), data transfer (rclone,
  rsync, S3, AWS credentials) and writing a support ticket. Triggers: "pip
  install fails in my venv", "disk quota exceeded", "run Jupyter on the
  cluster", "can I use Docker", "host key verification failed". Generic: site
  values come from the site skill (site-<institution domain>). For Slurm use
  slurm-user.
---

# Research computing basics

Helps a researcher who runs code on their own computer work well on shared
infrastructure: software environments, containers, notebooks, storage, SSH,
the shell, data movement and getting help.

**This skill holds no site facts.** Which modules exist, where project and
scratch space live, what the quotas and purge rules are, which login host to
use and where the help desk is differ per institution. They live in the user's
**site skill**, named `site-<institution domain>` with dots as dashes (for
example `site-example-edu`). Load it whenever a value below says "see your site
skill". If none is installed, ask the system (`module avail`, `df -h`, `quota`)
or the site's own documentation rather than guessing.

Every step shows the shell commands to run. Run them on the machine named in
the step (your computer or the cluster's login host); never paste secrets
(passwords, passcodes, keys, tokens) into a chat or a file.

## Start from the home institution

Every user has one **home institution**: where their cluster account, their
sign-in and most of their data live. Work goes there by default (its login
host, its storage), and its site skill is the one to load. Use a
cluster of another institution only when the user asks for it by name; the
data then travels from the home side, and the account and 2FA are the other
institution's own. If you don't know the user's home institution (no site
skill is installed, or `~/.ssh/config` or your notes don't say),
ask for it before choosing a cluster or staging data.

## Where to go next (read the reference that matches the task)

- **[references/environments.md](references/environments.md)**: finding
  software (modules first, then site installs, conda/pip, containers), Python venv
  and pip (and why `pip install --user` fails inside a venv), conda/mamba in
  batch jobs, Lmod modules, requirements and lock files. Read before
  installing or loading any software.
- **[references/containers.md](references/containers.md)**: Apptainer pull,
  exec, `--nv`, `--bind`, cache location, building, Docker images versus SIF,
  when a container beats modules. Read when software is easier to ship as an
  image, or when Docker is the only recipe you have.
- **[references/jupyter.md](references/jupyter.md)**: notebooks on a Slurm
  compute node with an SSH tunnel, kernels from venv or conda, turning a
  notebook into a batch script.
- **[references/filesystems-and-storage.md](references/filesystems-and-storage.md)**:
  home, project and scratch tiers, listing the mounted file systems (`df -hT`,
  `findmnt`), quotas and inodes, permissions and shared
  group folders, node-local `/tmp`, network file system caveats, backups and
  purge, checksums. Read before putting data or environments anywhere.
- **[references/ssh.md](references/ssh.md)**: keys, agents, `~/.ssh/config`
  and how `Host` matching works, ProxyJump, ControlMaster, changed host keys,
  tmux on round-robin login hosts.
- **[references/shell.md](references/shell.md)**: `.bashrc` hygiene so scp,
  sftp and batch jobs don't break, login versus non-login shells.
- **[references/data-transfer.md](references/data-transfer.md)**: rclone,
  rsync and sftp, verifying with checksums, S3 and AWS credential hygiene.
- **[references/data-governance.md](references/data-governance.md)**: which
  data classes (student, health, export-controlled, human-subjects, genomic,
  sponsor-confidential) need approval before they go onto a shared platform,
  and what to track about copies and access.
- **[references/getting-help.md](references/getting-help.md)**: what to try
  first and what a support ticket needs.

Related skill: **slurm-user** (jobs, partitions, GPUs, the queue).
