# Settings

The desktop app has a Settings page where you can see and change everything Labmill keeps, without editing files. Open it with the **gear button** at the top right of the window, or choose **Settings…** in the tray menu (on a Mac: **File → Settings…**, Cmd+,). **← Back to chat** (or Esc) takes you back to the chat.

The sections are listed on the left. With the keyboard, use the arrow keys (and Home or End) to move through the list; each section opens as you reach it, and Tab moves into it.

Settings never shows or stores a password, a sign-in code, a key or a token.

## Safety switches at a glance

| Switch | Default | What it does |
|---|---|---|
| Run commands on the cluster without asking | off | Labmill runs commands on the cluster without showing them first. |
| Approve everything automatically | off | Labmill approves every step. Secret-file reads, questions and logins still ask. |
| Download results automatically | off | Labmill brings results of finished jobs to your computer. |
| Notify me when a job ends | on | The desktop app shows a notification. |
| Email me when a Slurm job ends | off | Labmill adds an email request to the job script. |

Details for each switch are below. See also [Privacy](privacy.md).

## Institution

Shows the institution Labmill is set up for, and its documentation address (as text: copy it into your browser). Choose another institution from the list and click **Save**. Your clusters and connections stay as they are; the institution decides which help and skills Labmill uses.

### A cluster at another institution

Labmill works with your own institution by default. If you name a cluster at another institution that Labmill has information for (for example "run this on the cluster at example.edu"), it asks you to approve adding that cluster. The approval is always shown, even with **Approve everything automatically** on. After you approve, the cluster is added to your list and can be used right away. The connection settings go into the same SSH settings file as your other clusters. You sign in to it with your own account there.

## Clusters

The clusters in `clusters.yaml`. **Edit** changes one, **Add cluster** adds one, **Remove** removes one (Labmill asks first).

- **Slurm clusters** have a name, an SSH host (one of your SSH connections), an optional files host for transfers, an optional time zone (rarely needed) and the transfer method.
- **S3 staging** (optional): bucket, prefix, region, AWS profile, endpoint and encryption. Keys never go here: Labmill uses the AWS profile you name. Leave all S3 fields empty for no S3 settings.

### More clusters at your institution

If your institution has gained a cluster since you first set up Labmill, the Clusters page lists it under **More clusters at your institution** (the section is hidden when nothing is missing). **Add** first shows what will be written: the SSH connection for the cluster's login host, the files that change, and your account name there (taken from your existing setup; change it if the cluster uses another account). If you already have an SSH entry for the same host, Labmill reuses it as it is and says so, with the account that entry names; Labmill signs in with that account, so if the account you type is a different one, Labmill refuses with a message instead of ignoring it (use that account, or change the existing entry first). **Add cluster** then adds it at the end of your list. A cluster you already have under another name is not offered again. Nothing is sent to or run on any cluster, and your existing clusters and connections stay as they are. You can also ask Labmill in the chat ("set up the cluster …"); it shows the same details for your approval, even with **Approve everything automatically** on. Labmill always tries the account name you already use at that institution (or at another cluster of it) first, and only asks for a different name if that sign-in is rejected; you can then tell it the other name in the chat. The terminal version lists these clusters in `/settings` and adds one when you ask in the chat.

Labmill checks your changes with the same rules it uses when it starts. If a value isn't valid, the message appears under that field and nothing is saved.

Before the first change, Labmill saves a copy of `clusters.yaml` next to it (`clusters.yaml.labmill-backup-<date>-<time>`). It never overwrites an older copy. Your comments in the file are kept.

## SSH connections

The connections Labmill uses: name, host name, user name, port and jump host.

- Labmill's own connections (in `~/.ssh/config.d/labmill`) can be edited, added and removed here. The name of an existing connection can't change, because clusters refer to it. A connection that a cluster uses can't be removed.
- Your own entries in `~/.ssh/config` are shown read-only. Labmill never changes them, and won't add a connection with the same name.
- Each connection shows whether Labmill is logged in to it right now. Logins end by themselves 10 minutes after Labmill quits.

## Folders

Your data folder (first) and code folder, the same ones as in the chat box's **+** menu: **Choose…**, **Remove**, or **Use** a recent folder. Only the folder names are kept; nothing is read or sent until you ask in the chat and approve.

## App

- **Start Labmill in the tray when I log in**: the same switch as in the tray menu. Off by default.
- **Show the one-time tips again**: for example the note that Labmill keeps running in the tray when you close its window.
- **Notify me when a job ends**: the desktop app shows a notification when one of your jobs ends. On by default.
- **Email me when a Slurm job ends**: Labmill adds a request for an email when the job ends or fails to the Slurm script. The email goes to the default address of your institution. Off by default.

## About and diagnostics

Read-only: the version, the assistant's address (a broker address, or none when Labmill calls Anthropic directly), and where Labmill keeps its files (clusters, SSH settings, desktop app settings, job database, log).

- **Open logs folder** opens the folder with Labmill's log and job database.
- **Copy diagnostics** copies a short report for your site's support. The page shows exactly what is copied. It holds no passwords, keys, sign-in codes or user names.

## Run commands on the cluster without asking

In the **App** section, **Run commands on the cluster without asking** is off by default. While it is off, Labmill runs plain reads on its own (for example listing your jobs or files) and shows you every other command, exactly as it will run, and waits for your OK. When you turn it on, Labmill runs those commands on your cluster account without showing them first. Reading a file that holds sign-in secrets, such as a private key or a token file, still asks first, because the content would go to Claude. Your cluster account's own permissions still decide what a command can do. The setting applies to the next command, and it is kept for the desktop app and the terminal alike.

## Approve everything automatically

In the **App** section, under **Approvals**, **Approve everything automatically** is off by default. The same checkbox is in the main chat window, next to **Send**. When it is on, Labmill approves every step for you, without showing it first and without letting you edit it: uploads, downloads, job submissions, cancellations and commands on the cluster. A banner above the chat says **Auto-approve is on** and has a **Turn off** button, and every step appears in the chat as **Auto-approved: ...**, so you can still see what was done.

Three things always ask, even when it is on:

- Reading a file that holds sign-in secrets, such as a private key or a token file, because the content would go to Claude.
- Questions from Labmill with a few answers to choose from.
- Logins: your password and the two-factor push or code.

**How it differs from "Run commands on the cluster without asking".** That setting only covers commands on the cluster. Uploads, downloads and job submissions still show their approval screen. **Approve everything automatically** includes everything that setting does, and the other approval screens too. You can keep the narrower one on and the wider one off. Turn the wider one on only when you trust what you are asking for, because nobody checks each step. The setting applies to the next step.

## Download results automatically

In the **App** section, under **Approvals**, **Download results automatically** is off by default. The same checkbox is in the main chat window, next to **Approve everything automatically**. When it is on and a job you started completes, fails or runs out of time (not when you cancel it), Labmill downloads its results to your results folder, in the folder named after the job, and checks the files against the cluster. It asks nothing, because the files only come to your computer. The chat says what was downloaded and where, with an **Open folder** button.

It brings only what the job produced: files you uploaded, hidden or private-looking files (such as `.netrc`, `.env` or token files), environments and caches, and symbolic links are left out, and the chat says how many. It leaves results above 2 GB or 2000 files, or larger than your free disk space minus 10 %, for you to ask for, and says why. When you are not signed in, it tells you once and downloads at its next check after you sign in. When the download fails, it tells you and you can ask Labmill to fetch the results. A failed job stays marked as failed. Each job is downloaded automatically at most once, and only if it finishes while the setting is on. The setting applies to the next job that finishes, and it is kept for the desktop app and the terminal alike.

## When changes take effect

Labmill reads `clusters.yaml` and the SSH settings when it starts. After you save a change, the page says so: **quit labmill** (tray menu → Quit Labmill) and open it again to use the new settings. Folders and the App settings, including **Run commands on the cluster without asking** and **Approve everything automatically**, take effect at once.

## In the terminal

In the terminal interface, type `/settings` to see the same settings (without secrets) and where the files are. `/settings` also shows whether commands on the cluster run without asking. To change it from the terminal, add the line `"runWithoutAsking": true` (or `false`) to the desktop app settings file that `/settings` lists under Files (`gui.json`, in the same folder as `clusters.yaml`; create it as `{ "runWithoutAsking": true }` if it is missing). A running desktop app may overwrite a hand edit with its own copy, so edit the file while the app is closed, or use the Settings page. Type `/autoapprove on` or `/autoapprove off` to change **Approve everything automatically** (`/autoapprove` alone shows it). Type `/autodownload on` or `/autodownload off` to change **Download results automatically** (`/autodownload` alone shows it; `/settings` shows both). The terminal's status line reads **AUTO-APPROVE ON** while it is on. The choice is saved in the same file as the desktop app's, so both use it. Editing is done in the desktop app, or in the files themselves.
