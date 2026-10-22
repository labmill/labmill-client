# File systems and storage

Cluster storage is not one big disk. Each tier has a purpose, a size limit and
a safety level. Where each tier lives, its quotas, whether it is backed up and
when it is purged: see your site skill.

## The usual tiers

| Tier | For | Typical traits |
|---|---|---|
| **Home** | dotfiles, SSH keys, small scripts, config | small quota, often backed up, slow for heavy I/O |
| **Project / lab / group** | code, the data you keep, shared environments | large quota, shared with your group, backup varies |
| **Scratch / work** | big temporary job data and intermediate files | very fast, large, **not backed up, files expire** |
| **Node-local** (`/tmp`, a local SSD path) | per-job temporary files on one node | fastest, wiped when the job ends, invisible to other nodes |
| **Object storage / archive** | cold data, sharing, backups of results | cheap, accessed via tools (rclone, S3), not a mounted folder |

Rules of thumb:

- **Never keep the only copy of anything in scratch.** Copy results you care
  about to project space or archive when a job finishes.
- Don't build large environments or caches in home if its quota is small
  (conda, pip, Apptainer caches: see environments.md and containers.md).
- Data a job reads many times, or many small files: copy once to node-local
  `$TMPDIR` / `/tmp` at the start of the job and work there; copy results back
  before the job ends.
- **Order for a job's working data:** node-local scratch for a single-node job
  (stage in, stage out, `trap` cleanup in the job), else a global scratch
  mounted on the compute nodes (cleanup optional, only after the results are
  downloaded and verified), home last. Never remove reusable software.
  Commands and a script: slurm-user →
  [placement-and-preemption.md](../../slurm-user/references/placement-and-preemption.md).
- Purge policies exist on scratch tiers (files untouched for N days are
  deleted, and `touch` to dodge it is often against the rules). Read the value
  in your site skill and plan around it.

## Which file systems exist? (before answering "is there scratch / shared storage")

Never answer "there is no shared scratch" from the site docs or from
`/scratch` alone. List what is mounted, with types, on the login node first:

```bash
df -hT -x tmpfs -x devtmpfs                      # mounted file systems, type, size, use
findmnt -t lustre,gpfs,beegfs,ceph,fuse.ceph-fuse,wekafs,nfs,nfs4,cifs      # network and parallel file systems
findmnt -lo TARGET,SOURCE,FSTYPE,SIZE            # everything, compact
df -h /path/to/dir                               # which file system holds this folder
```

- Match each large Lustre/GPFS/BeeGFS/NFS mount to the names in the site docs.
  Two mount points with the same size and server are the **same file system**;
  a site's "global scratch" can be the same file system as its shared work
  folder, reachable under two paths.
- A node-local `/scratch` on the login node is not the compute nodes'
  `/scratch`; check from a job (`srun df -hT /scratch`) before relying on it.

## How full am I? (quotas and inodes)

```bash
df -h /path/to/dir                  # free space on the file system holding the path
du -sh /path/to/dir                 # size of one folder (can be slow on huge trees)
du -h --max-depth=1 /path | sort -h # which sub-folder is big
df -i /path/to/dir                  # inode (file count) use on that file system
find /path/to/dir -type f | wc -l   # number of files in a tree
quota -s                            # per-user quotas, on systems that use classic quotas
```

Many sites use their own quota tool (a command or web page) because
`quota`-style commands differ by file system; see your site skill. Two limits
matter: **bytes** and **inodes** (file count). A tree of millions of tiny files
can hit the inode limit while using little space: tar them
(`tar -cf data.tar folder/`) or use a container image, HDF5, Parquet or Zarr
instead.

## Permissions, groups and shared folders

```bash
ls -l /path                         # owner, group, mode
id                                  # your user and groups
chmod 640 file                      # owner rw, group r, others nothing
chmod -R g+rX /path/shared          # group can read; X sets execute only on folders
chgrp -R mylab /path/shared         # change group (you must be a member)
umask                               # default permission mask (022 = group can't write)
```

- Private by default: `chmod 700 ~/.ssh ~/.aws`, `chmod 600` on key and
  credential files. The SSH client refuses keys that others can read.
- **Shared group folder**: new files should inherit the folder's group and be
  group-writable.

  ```bash
  chgrp mylab /path/shared && chmod 2775 /path/shared   # the leading 2 is setgid: new files inherit the group
  umask 002                                             # in your job script or shell so new files are group-writable
  ```

  `ls -ld /path/shared` shows an `s` in the group execute position when setgid
  is on. Some file systems manage access with ACLs instead
  (`getfacl`, `setfacl -R -m g:mylab:rwX /path/shared`); a site skill says
  which applies.
- Don't `chmod 777`. Fix the group instead.

## Node-local `/tmp` and `$TMPDIR`

```bash
echo "$TMPDIR"                      # Slurm often sets a per-job local folder; see your site skill
df -h /tmp                          # how much local space the node has
```

Local space is small and shared by every job on the node, and it's removed
when the job ends. Write to `$TMPDIR` (not a fixed `/tmp/myfile`) and clean up
after yourself. Don't send job `--output` logs to `/tmp`: they vanish with the
node (see slurm-user).

## Network file system caveats

Home, project and often scratch are **network** file systems shared by
thousands of jobs.

- **Metadata is the bottleneck.** `ls` on a folder with 100,000 files, `find`
  over whole trees, `du` on huge folders and many small reads all load a shared
  server and slow everyone. Keep folders under a few thousand entries; shard by
  sub-folder; use archives for many small files.
- **File locking can be broken or slow.** Tools that rely on locks (SQLite,
  some package managers, `git` on some mounts) can corrupt data or hang.
  **SQLite on network storage, especially in WAL mode, is unsafe**; keep
  databases on node-local `$TMPDIR` and copy the result out, or use a server
  database.
- **Don't keep many jobs appending to one file.** Give each task its own output
  file (`%A_%a` in Slurm names) and merge afterwards.
- **Caching delays visibility.** A file written on one node can take seconds to
  appear on another; a job that reads right after another writes should wait or
  retry.
- `rsync`/`tar` large trees instead of `cp -r` over and over, and don't run
  heavy `find`/`du` in a loop.

## Backup, snapshots and deleting

- Whether a tier has backups or snapshots, and how to restore from them, is a
  site fact (your site skill). Assume **no backup** unless it says otherwise.
- `rm` is permanent; `rm -rf $VAR/` with an empty variable deletes the wrong
  thing. Use `rm -rf -- "${DIR:?DIR is empty}/"` (`set -u` only catches unset
  variables, not empty ones) and check `echo` output before the real
  command.
- Keep irreplaceable data in two places (for example project space plus an
  object-storage copy; data-transfer.md).

## Integrity: checksums

```bash
sha256sum file > file.sha256            # compute and store
sha256sum -c file.sha256                # verify later (prints OK or FAILED)
find data -type f -exec sha256sum {} + > MANIFEST.sha256   # a whole tree
sha256sum -c MANIFEST.sha256 --quiet    # silent when everything matches
md5sum file                             # many object stores report MD5s
```

macOS has `shasum -a 256 file` instead of `sha256sum`. Compare checksums of
the source and the copy after any large transfer; sizes alone can match while
content differs.

Checking a copy you pulled from the cluster, file by file (what Labmill does
after every SFTP download): hash on the cluster, then on your computer, and
compare.

```bash
# on the cluster (one call for many files; -- guards names starting with -)
ssh mycluster 'cd /path/to/results && sha256sum -- model.pt "my log.txt"' > remote.sha256
# on your computer, in the folder you copied into
sha256sum -c remote.sha256              # macOS: shasum -a 256 -c remote.sha256
```

- Every line must say `OK`. A `FAILED` file is a damaged or half-written
  copy: keep it under another name (`mv f f.mismatch`, don't delete it until
  the retry matches) and copy it again.
- No `sha256sum` on the cluster (rare; `command not found`), or a transfer
  host that only serves SFTP and runs no commands (the `ssh` call above
  fails or prints nothing)? Check where else you can (`shasum -a 256`,
  `md5sum`, or run the command on the login node, which sees the same
  folders). If nothing works, compare sizes (`stat -c %s`, `ls -l`) and say
  "verified by size only: sha256sum not available on <host>". Labmill goes
  straight to that size-only result in these cases and says so.
- One `sha256sum` call per ~200 files (or a few KB of names), not one call
  per file; a very large batch needs a long timeout.
- Report what happened: "Verified N files (SHA-256)", or the files that
  differ. A job whose copy differs is not "fetched" until the retry matches.
- Quote file names with spaces or quotes, and never build the command by
  pasting untrusted names into a shell line.
- S3 copies are different: rclone compares the object's MD5 itself
  (`rclone check`, data-transfer.md).
