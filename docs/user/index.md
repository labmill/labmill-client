# User guide

Labmill lets you describe a compute job in plain language and then does the rest: it finds your data, sends it to a cluster, starts the job, watches it, and brings the results back. You don't write job scripts and you don't need to know the terminal.

!!! note "Where Labmill stands today"
    The text-based chat window (the "TUI") works on macOS and Linux. The desktop app (GUI) is built and being tested on Windows and Linux; this guide describes the text window, and the desktop app has the same approvals. The "Hey Labmill" voice feature and the headless Agent mode are planned, not available yet. See [Voice](voice.md).

## What you need

- A computer (a laptop is fine).
- An account on a Slurm cluster at your institution, with its two-factor sign-in set up if your institution uses one (for example a push to your phone). Labmill doesn't create accounts. It uses the one you already have.
- Your own Anthropic API key in the `ANTHROPIC_API_KEY` environment variable (see [Starting Labmill and signing in](first-run.md)).
- Your cluster user name and password.

## What Labmill does without asking, and what it never does without asking

**Needs your approval every time.** Labmill shows you exactly what it wants to do and waits for you to say yes:

| Action | What you are shown |
|---|---|
| Uploading files | The list of files, their sizes, and the destination on the cluster (for example `bigcluster:~/labmill/segmentation`, where `~` is your home folder there) |
| Submitting a job | The full job script, which you can edit before approving |
| Cancelling a job | Which job will be cancelled and where |
| Downloading results | Which files, from where, to which folder on your computer |

The default answer on every approval screen is **Decline**, so pressing Enter by mistake doesn't send anything.

**If you don't want to confirm each step.** The checkbox **Approve everything automatically** (next to **Send** in the desktop app, or `/autoapprove on` in the terminal) is off by default. When it is on, Labmill approves every step above for you, a banner stays visible, and each step shows in the chat as "Auto-approved: ...". Reading key or token files, questions from Labmill and logins still ask. See [Settings](settings.md#approve-everything-automatically). The checkbox **Download results automatically** next to it (`/autodownload on` in the terminal) brings the results of a finished job to your results folder by itself; see [Settings](settings.md#download-results-automatically).

**Done without asking, because it only looks.** Labmill can search the folders you chose (or your home folder) for folders that match what you describe. It sees file *names*, folder sizes and short previews, never whole files:

- For a **document or code file** (a README, a script, a build file) it can read a page at a time, up to 200 lines or 16 KiB per page, in files up to 1 MiB. Values that look like passwords, tokens or API keys are replaced by a placeholder before anything is shown.
- For a **data file** (a CSV, a table) it reads only a short sample from the start, so it can see the column names.

It can also check the cluster's queues, your job status and your job logs. Ask it things like "What's in my queue on bigcluster?" or "What did I run last week?" and it lists your jobs there, including ones you started yourself outside Labmill.

**Commands on the cluster.** To answer questions and diagnose problems, Labmill can run commands on the cluster's login computer on your behalf.

- **Commands that only look** run at once, on any file or folder your account is allowed to read. These are things like listing queues, listing or reading a file, checking disk space, listing software modules, or reading a job log.
- **Commands that could change something** show you the exact line that will run and wait for your approval.
- **Reading a file that holds keys or tokens** (for example an SSH private key or a cloud credential) asks you first, because what it prints would be sent to Claude. The approval box says so.
- Labmill doesn't add its own limits on what you may run on the cluster. What your account may do there is decided by your institution's administrators, as always.

**Never touched on your own computer.** Labmill ignores hidden folders (names starting with a dot, such as `.ssh`), key and password files (for example `id_*`, `*.pem`, `*.key`, `.env`, `.netrc`, or names containing "secret" or "credential"), and on macOS and Windows the system folders (`Library`, `AppData`). These are never listed, previewed, uploaded or written to.

Your password and two-factor code are typed into Labmill's sign-in box, used for that sign-in only, and never saved to disk.

## Next steps

1. [Starting Labmill and signing in](first-run.md)
2. [Your first job](first-job.md)
3. [The desktop app and the tray](desktop-app.md): closing the window keeps Labmill running; quit from the tray icon
4. [Settings](settings.md)
5. [Keyboard reference](keys.md)
6. [Using the cluster from a terminal later](terminal.md)
7. [Using the same know-how in Claude Code](claude-code.md)
8. [Know-how updates](updates.md)

## Where to go next

- [Install](install.md): download Labmill for macOS, Windows or Linux.
- [Your first job](first-job.md): a full walkthrough.
- [Clusters and sign-in](clusters.md), [Data transfer and checks](data-transfer.md), [Failed jobs](failed-jobs.md).
- [Connect the client to your Labmill account](account.md) (optional).
- [Privacy](privacy.md), [FAQ](faq.md), [Troubleshooting](troubleshooting.md), [Contact](contact.md).
