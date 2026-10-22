# Connecting to a Slurm cluster and getting a compute node

Generic SSH and login concepts. The actual host names, account type, two-factor
method, VPN requirements and documentation links are in your site skill
(`site-<institution domain>`).

## Hosts you may meet

| Role | What it is | Example placeholder |
|---|---|---|
| **Login host** (submit host) | Where you run `sbatch`, `squeue`, edit files. Shared by everyone; no heavy work. | `<login-host>` |
| **Bastion / jump host** | A gateway you pass through to reach the login host when it isn't reachable directly. Sometimes the bastion *is* the login host. | `<bastion>` |
| **Transfer host** (data transfer node) | Built for moving data in and out; sees the same home and project folders as the login host. | `<transfer-host>` |
| **Compute nodes** | Where jobs run. Reached through Slurm, not by SSH from your laptop. | `<node>` |

Some sites offer one login host for campus/VPN users and another, public one
for off-campus users without VPN. Your site skill says which is which.

## Logging in

```bash
ssh <user>@<login-host>
```

- `<user>@` is only needed if your local user name differs and `~/.ssh/config`
  doesn't set `User`.
- Port 22 is the default; a `Port` line is needed only when the site says so.
- Most sites ask for a password and then a **second factor**: a push
  notification to a phone app, a passcode, a phone call or SMS. A typical prompt
  is a numbered menu such as `Passcode or option (1-3):`, where one number sends
  a push. Some servers push without showing a menu, so a pause after the password
  usually means "approve it on your phone". If no push ever arrives, the account
  probably isn't enrolled for two-factor yet; the enrollment page is in your
  site skill.

### Keys, `~/.ssh/config`, jump hosts, connection sharing

The general SSH know-how is in the **research-computing** skill; this file keeps
only what is specific to Slurm clusters. How: see research-computing →
`references/ssh.md` for keys and `ssh-copy-id` ("Keys"), one short name per
host ("`~/.ssh/config`"), a separate bastion with `ProxyJump` ("Jump hosts and
connection sharing"; each hop can ask for its own password and second factor),
and signing in once with `ControlMaster`. Some sites still require the second
factor with a key; that's expected. Typical cluster entry:

```
Host mycluster
    HostName <login-host>
    User <user>
    ServerAliveInterval 60
```

Then `ssh mycluster`, `scp file mycluster:` and `sftp mycluster` all work.

### VPN

Some login hosts are reachable only on campus or over the institution's VPN.
Under WSL on Windows, VPN clients often break name resolution or routing;
enabling split tunnel / local LAN access, or `dnsTunneling=true` in
`.wslconfig` on Windows 11, can help.

## Transferring files

Use the transfer host when the site has one, especially for anything large:

```bash
scp bigfile <user>@<transfer-host>:<dest-dir>/
sftp <user>@<transfer-host>
rsync -av --progress ./data/ <user>@<transfer-host>:<dest-dir>/data/
```

- Some login hosts **disable the SFTP subsystem**: `sftp` (and tools built on
  it) fail there with `subsystem request failed` while plain `ssh` works. Use
  the transfer host instead.
- The transfer host usually has its own login (same account, same password and
  second factor).
- To fetch big files from the web *onto* the cluster, SSH in and use `wget` or
  `curl` there instead of downloading to your laptop first (mind the proxy on
  compute nodes; see [proxy-and-github.md](proxy-and-github.md)).
- Where to put data (home, project or group folders, scratch) and the quotas:
  see your site skill.

## Login node versus compute node

**Do not run compute-heavy work on the login node.** Get a compute node first:

```bash
srun --pty bash                                    # interactive shell on a compute node
salloc --time=2:00:00 --cpus-per-task=4            # allocate, then run commands with srun
srun --pty --partition=<gpu-partition> --gres=gpu:1 bash    # interactive GPU node
```

Some sites kill particular programs on login nodes (long-running processes, IDE
servers, AI coding agents). If a tool is "killed" right after starting on the
login node, start it on a compute node from `srun --pty bash`. Your site skill
lists any such rules.

`srun --pty bash` starts a *non-login* shell that reads `~/.bashrc`, not
`~/.bash_profile`; put environment fixes for compute nodes in `~/.bashrc`.

See [slurm.md](slurm.md) for partitions, GPUs and batch jobs.
