<p align="center">
  <img src="assets/icon/labmill-256.png" alt="Labmill logo" width="128" height="128">
</p>

<h1 align="center">Labmill client</h1>

**Labmill client** is a conversational front end to your Slurm clusters. You tell it in plain language what you want to compute. It finds the data on your computer, stages it, sends it to the cluster, writes and submits the job script, watches the job and brings the results back.

Labmill client is a fresh copy of the yeetl client and supports **Slurm only**. Documentation and downloads: <https://client.labmill.org/>.

Labmill is for researchers who already run their code on their own Mac, Windows or Linux computer, often on a single GPU, and now need more. You don't need to know the cluster or the terminal. You say what you want to run in a chat.

## What it does

1. **Understands the request.** For example: "train this model on the images in my Experiments folder, 1 GPU, 12 hours".
2. **Finds the data.** It searches the folders you chose for the files you mean and shows you what it found before anything moves.
3. **Stages it.** It makes a manifest of the selected data (paths, sizes, checksums).
4. **Transfers it.** It uploads the data to the cluster file system over SSH, or to an S3 bucket.
5. **Picks the cluster.** It chooses among your configured Slurm clusters by resources, queue state and your rules.
6. **Runs the job.** It writes the `sbatch` script and shows it to you before it submits. Sample code without run scripts is fine: Labmill reads the README and build files and writes the missing build and run scripts into the job's folder on the cluster, shown to you first.
7. **Monitors and returns.** Status, logs and resource use, then it brings the results back to your computer and checks them.

## Modes

| Mode | Platforms | Purpose |
|------|-----------|---------|
| **TUI** (`labmill`) | macOS, Linux, Windows | Interactive terminal interface: chat pane and job dashboard. |
| **GUI** (`labmill-gui`) | macOS, Windows, Linux | A simple desktop app: one chat box, one job list, one progress view. On Windows and Linux, closing the window keeps Labmill in the system tray, so you stay signed in; quit from the tray icon. On a Mac, Labmill stays in the Dock and quits with Cmd+Q. |

Both modes share the same core, so a job started in one can be watched from the other. A headless Agent mode and "Hey Labmill" voice input are planned, not built.

## Design principles

- **Confirm before moving data.** Nothing leaves your computer until you approve the file list and the destination.
- **Checks on the cluster without asking, changes only with your OK.** The assistant can look things up on your cluster account by itself (your queue, job details, logs, any file your account can read). Labmill doesn't limit what you may run on the cluster; your cluster's administrators decide that. Any command that could change something is shown to you first, exactly as it will run. Reading a file that holds keys or tokens also asks first, because its content would be sent to Claude. The Settings page has the switches "Run commands on the cluster without asking" and "Approve everything automatically" (both off by default); key or token file reads, questions and logins still ask.
- **Transparent.** Every generated script can be inspected and edited before it is submitted.
- **Sensible use of the cluster.** Labmill prefers node-local scratch for single-node jobs and a preemptible partition where the cluster has one, shows the fallback plan and the estimated start before you approve, and resubmits a preempted job automatically (at most twice, then once on a regular partition).
- **A check before you submit.** The review of a job says whether Slurm accepts the script and when it expects the job to start, or shows Slurm's error.
- **Why did it fail?** For a failed job the assistant collects the facts (state, exit code, memory and time used, the end of the log) and names the cause. Labmill remembers the cause on your computer only.
- **Long transfers survive.** Your computer doesn't go to sleep during a transfer, and a broken transfer starts again by itself, up to three times.
- **Results are checked.** Downloads are compared with checksums computed on the cluster. A file that doesn't match is kept under a `.mismatch` name, never overwritten.
- **Your accounts, your login.** Labmill needs no account of its own. You sign in with your existing cluster account, including any two-factor step. Passwords and two-factor codes are never written to disk.
- **Optional Labmill account.** `labmill login` (or Settings > Labmill account) connects the client to app.labmill.org with a program token. Then a record of each finished job (state, times, resources, partition, modules) goes to your account; never scripts, data, code, names or logs.
- **One binary.** Nothing to install on the cluster and nothing left running when you're done, except a small SSH helper that holds your login and exits about 10 minutes after you stop.

## Build from source

You need Go 1.26 or newer (`GOTOOLCHAIN=auto`, the default, downloads it for you).

```
make build        # bin/labmill (TUI)
make gui-linux    # bin/labmill-gui (desktop app on Linux; needs cgo and webkit2gtk-4.1)
make gui-windows  # bin/cross/labmill-gui.exe (cross-built from Linux)
make check        # the test gate: gofmt, vet, tests, cross builds
```

## Model access: your Anthropic API key

Labmill's assistant is Claude. By default Labmill calls the Anthropic Messages API directly with your own API key. Set it in your environment before you start Labmill:

```
export ANTHROPIC_API_KEY=sk-ant-...
labmill
```

Labmill reads the key from the environment only and never writes it to disk. Without a key, Labmill starts but tells you that it has no assistant.

- Planning turns use `claude-opus-5-5`, the other turns `claude-sonnet-5-5`. Set `LABMILL_MODEL` to use one model for all turns.
- **Optional broker.** If your institution runs a Labmill relay service, set `LABMILL_BROKER_URL` to its address (or use a site profile with `broker_url`) and `LABMILL_BROKER_TOKEN` to your token. Labmill then sends model calls there instead of to Anthropic.

## First run

1. **Choose your home institution.** Labmill always asks. If your institution isn't in the list, choose **My institution isn't listed** and enter the cluster login host (the name you would type after `ssh`, for example `login.example.edu`).
2. **Enter your account name** on the cluster.
3. **Save.** Labmill writes the connection settings in the standard OpenSSH format to `~/.ssh/config.d/labmill`, adds one `Include` line at the top of `~/.ssh/config` (after a backup), and writes your cluster list to `~/.config/labmill/clusters.yaml`.

The first time Labmill connects, it asks for your password in its own sign-in box and starts the two-factor step (for example a push to your phone). It then keeps the login open while you work. Clusters that accept only SSH keys get a guided key setup. Because the settings are in OpenSSH format, `ssh <alias>` works in a terminal too.

Results go to `~/labmill-results/<job name>` by default.

## Configuration

Clusters are defined in `~/.config/labmill/clusters.yaml`:

```yaml
clusters:
  - name: bigcluster
    type: slurm
    ssh_host: bigcluster-login   # SSH settings live in ~/.ssh/config.d/labmill
    files_host: bigcluster-files # a dedicated transfer host (recommended)
    transfer: sftp
    # timezone: America/Los_Angeles   # rarely needed: only if job times look off
  - name: gpucluster
    type: slurm
    ssh_host: gpucluster-login
    transfer: s3                 # stage data through S3 instead of SFTP
    s3:
      bucket: my-bucket
      prefix: labmill
      region: us-west-2
      profile: myprofile         # credentials come from this AWS profile, never from this file
site: example.edu
```

`site` is your home institution (`generic` when you chose "My institution isn't listed"). Data is uploaded through `files_host` when it is set, because a login host may not allow file transfers. You can send files to your home folder or to an absolute group folder such as `/data/mygroup/...`; hidden and system folders are refused.

## Skills and Claude Code

Labmill's know-how is written as Claude Code skills: the generic `slurm-user` and `research-computing` skills in `skills/` and the generic site skill in `sites/generic/`. The skills come from yeetl (see `skills/SOURCE.md`). You can use the same skills in Claude Code on their own; see `sites/README.md`.

## Documentation

The user guide and the developer pages are in `docs/` (build them with `make docs`) and online at <https://client.labmill.org/>.

## License

MIT, see [LICENSE](LICENSE).
