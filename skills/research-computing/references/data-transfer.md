# Moving data: rclone, rsync, sftp, S3

Which hosts to transfer through (a dedicated transfer host, a gateway, object
storage endpoints) and which tools are installed: see your site skill. Rule of
thumb: transfers go through the **data transfer path your site provides**, not
through the login host, and big jobs read from the cluster's own storage, not
from your laptop.

Before moving anything, list what will move and where it goes, check the
destination's free space (filesystems-and-storage.md), and check what would be
overwritten.

## rsync and sftp over SSH

```bash
rsync -avh --progress --dry-run dir/ mycluster:/path/to/project/dir/   # preview first
rsync -avh --progress dir/ mycluster:/path/to/project/dir/             # then for real
rsync -avh --partial --append-verify big.tar mycluster:/path/to/project/   # resumable big file
rsync -avh mycluster:/path/to/project/results/ ./results/              # pull results back
sftp mycluster                        # interactive: put, get, ls, cd, lcd, mkdir
scp -r dir mycluster:/path/to/project/   # fine for small things
```

- A trailing slash on the source copies the **contents**; without it, the
  folder itself. This is the commonest mistake: check with `--dry-run`.
- `-a` keeps times and permissions; add `-z` for compressible data over slow
  links (not for already-compressed files); `--exclude '*.tmp'`.
- `--delete` makes the destination match the source and deletes extra files
  there. Use it only on purpose, after a dry run.
- `rsync` can be restarted; run it in `tmux` (ssh.md) so a closed laptop lid
  doesn't stop it.
- Windows: `scp`/`sftp` ship with the OpenSSH client; for rsync use WSL or
  rclone.

## rclone

rclone speaks sftp, S3, Google Drive, Azure and many more with one syntax.
Install: <https://rclone.org/install/>. Remotes are named in
`~/.config/rclone/rclone.conf`; create them with `rclone config`.

```bash
rclone listremotes
rclone lsd remote:                           # buckets / top folders
rclone ls remote:bucket/prefix | head
rclone copy --dry-run -P ./data remote:bucket/prefix/data    # preview
rclone copy -P --checksum ./data remote:bucket/prefix/data   # copy: never deletes at the destination
rclone check ./data remote:bucket/prefix/data                # compare sizes and hashes
rclone size remote:bucket/prefix/data
```

- **`copy` versus `sync`**: `copy` adds and updates files; `sync` makes the
  destination identical to the source and **deletes** what is only at the
  destination. Default to `copy`; use `sync` only after `--dry-run`. Source
  and destination arguments order matters.
- `--checksum` compares hashes instead of size+modification time, more reliable
  after a re-run, slower on huge trees. `-M` / `--metadata` also carries file
  metadata (mtime, permissions, where the backend supports it).
- `--transfers 16 --checkers 32` raise parallelism for many files; lower them
  if a shared host complains. `--bwlimit 20M` caps bandwidth.
- **Never put a secret on a command line.** Use `rclone config` (stored with
  mode 600 in `rclone.conf`), `rclone config encryption`, or environment
  variables you set yourself in the shell.
- **S3 and buckets**: `--s3-no-check-bucket` skips the bucket-exists/create
  check, needed when your credentials may read and write objects but not list
  or create buckets (the error is `AccessDenied` on `CreateBucket` /
  `HeadBucket`). Also never create buckets by accident: a typo'd bucket name
  silently becoming a new bucket is how data goes to the wrong place.
- **sftp through an SSH alias**: rclone's `sftp` remote can reuse your
  `~/.ssh/config` entry: `rclone config create hpc sftp host mycluster
  ssh "ssh mycluster"` style set-ups let the alias handle jump hosts and
  ControlMaster. Test with `rclone lsd hpc:`.
- **After copying**, verify: `rclone check` (hash comparison where the backend
  has hashes) or compare checksum manifests (filesystems-and-storage.md).

## S3 and AWS credential hygiene

Object storage (AWS S3 or an S3-compatible service) is the fast way to stage
big data for GPU platforms and to archive results. Credentials are secrets:

```bash
aws configure                               # YOU type the keys, once, in your own terminal
aws configure list                          # shows masked values and where they came from
aws sts get-caller-identity                 # proves the credentials work, shows the account
aws s3 ls s3://bucket/prefix/               # needs the aws CLI; rclone works too
chmod 700 ~/.aws && chmod 600 ~/.aws/credentials ~/.aws/config
```

- Run `aws configure` yourself. **Never paste an access key or secret into
  chat, an issue, a ticket, a script or a git repository.** If one leaked,
  deactivate it in your account's console and create a new one; assume it was
  copied.
- Keys land in `~/.aws/credentials` under a profile name. The `[default]`
  profile is used when none is named; extra profiles (`aws configure --profile
  work`) are selected with `AWS_PROFILE=work` or `--profile work`. Tools like
  rclone with `env_auth = true` read the same chain, so no secret goes into
  `rclone.conf`.
- Prefer short-lived credentials (single sign-on: `aws configure sso`, then
  `aws sso login`) or keys scoped to one bucket, over account-wide keys.
- Credentials in jobs: don't bake them into scripts. Give the job the profile
  name or a read-only mounted credentials file with mode 600, and expire keys
  after a project ends.
- S3 URLs look like `s3://bucket/prefix/`; there are no real folders, only key
  prefixes. List with `--max-depth` carefully on huge prefixes.

## Large transfers

- One big file or millions of small ones are both slow: archive small files
  (`tar -cf`), and use resumable tools (`rsync --partial`, rclone re-runs).
- Throughput rules of thumb: a laptop on Wi-Fi tops out far below the
  cluster's network; start the transfer from a wired or campus connection, and
  run it inside `tmux` on the transfer host so it continues when you disconnect
  (ssh.md).
- A laptop that sleeps breaks the transfer. On macOS run `caffeinate -i rsync ...`;
  on Windows keep the laptop plugged in and set sleep to "never" while plugged
  in (`powercfg /change standby-timeout-ac 0`), and set it back afterwards
  (for example `powercfg /change standby-timeout-ac 30`). If a transfer breaks, run the
  same command again: `rsync --partial` and rclone skip what already arrived.
- Compress what compresses (`gzip`, `zstd`), but not images, video or
  already-compressed formats.
- Pulling results back from the cluster: hash them there and here (SHA-256,
  one batched `sha256sum -- files...` on the cluster; recipe in
  filesystems-and-storage.md, "Integrity: checksums"); keep a differing file
  as `<name>.mismatch` and copy it again.
- Finish with a **checksum comparison**, not just "no error": `sha256sum -c
  MANIFEST.sha256` on the destination against a manifest built at the source.

## Copy one AWS profile to a login node

A job on a cluster that reads or writes S3 itself needs credentials on the
cluster. Copy one profile, not your whole `~/.aws`, and keep the keys out of
chat, scripts, and command lines (a command line is visible to other users in
`ps`):

```bash
P=work                              # the local profile (static, SSO or role)
aws sso login --profile $P          # SSO profiles only; the sign-in may have expired

aws configure export-credentials --profile $P >/dev/null && echo ok   # go on only if this prints ok

# Send the remote script and then the section on stdin of `sh -s`, so the
# login shell (bash, zsh, csh) never parses the script and no key is in a
# command line. The script drops an old [labmill-$P] section, keeps all other
# lines, and writes through a temp file, so a second run replaces the section
# and never duplicates it. A failed step stops before the final mv.
{ cat <<'EOF'
umask 077
d=$HOME/.aws
mkdir -p "$d" && chmod 700 "$d" || exit 1
f=$d/credentials
if [ -L "$f" ]; then f=$(readlink -f "$f") || exit 1; fi
new=$(mktemp "$(dirname "$f")/.labmill-new.XXXXXX") || exit 1
trap 'rm -f "$new"' EXIT
if [ -f "$f" ]; then awk -v n="[$1]" '/^\[/ { s = ($0 == n) } s == 0 { print }' "$f" > "$new" || exit 1; fi
cat >> "$new" <<'SECTION'
EOF
  echo "[labmill-$P]"
  aws configure export-credentials --profile $P --format env-no-export \
  | sed -e 's/^AWS_ACCESS_KEY_ID=/aws_access_key_id = /' \
        -e 's/^AWS_SECRET_ACCESS_KEY=/aws_secret_access_key = /' \
        -e 's/^AWS_SESSION_TOKEN=/aws_session_token = /' \
        -e 's/^AWS_CREDENTIAL_EXPIRATION=/# expires /'
  printf 'SECTION\nchmod 600 "$new" && mv -f "$new" "$f"\n'
} | ssh login sh -s -- "labmill-$P"
```

- Use a new section name (`[labmill-work]`), so you never overwrite another
  section. The command above replaces only that section and keeps all others.
- Set `umask 077` first. The folder is mode 700 and the file mode 600.
- Static keys never expire, so prefer SSO or role profiles. Write a
  `# expires <time>` comment for temporary credentials; a job that runs longer
  than the credentials needs a fresh copy.
- In the job script set `AWS_PROFILE=labmill-work` and the region
  (`AWS_DEFAULT_REGION`); the credentials file holds no region.
- No profile yet: run `aws configure` yourself in your own terminal (in
  Claude Code, or on the login node itself). Never paste keys into a chat.
  In Labmill the `s3_credentials_to_cluster` tool with `enter_keys` opens a
  masked dialog instead; the keys go to the cluster and not to the model.
