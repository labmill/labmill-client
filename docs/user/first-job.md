# Your first job

A typical conversation looks like this. Type in plain words. You can ask follow-up questions at any time.

## Tell Labmill where your data (and code) are (optional)

You can point Labmill at the folder with your data, and the folder with your code if you have any, before you start, so you don't have to describe where they are. **You don't need code:** if you only add a data folder, Labmill offers to write the analysis code for you, and you see that code before anything runs.

- **Desktop app:** click the **+** at the upper left of the chat box and choose **Add data folder…** (or **Add code folder…**), or drop a folder from Explorer or Finder onto the chat box and pick "Add as data folder" or "Add as code folder" (see [the desktop app](desktop-app.md)).
- **Terminal:** type `/data ~/data/mri-2026` and, if you have code, `/code ~/projects/mri`. Tab completes folder names. `/data` alone shows the current choice, and `/data -` clears it (the same for `/code`). The status bar shows `Data: …` and `Code: …`.

Choosing a folder sends nothing. Your next message tells Labmill about it, and Labmill then searches there first, can preview a file by its name in the folder, makes upload lists from the data folder, and looks at the code folder to choose the software. Hidden folders, system folders and folders that look like they hold passwords or keys can't be chosen. Labmill remembers the folders between runs. The desktop app and the terminal share them; a change made in one shows in the other the next time it starts.

## 1. Describe the job

> Train the segmentation model in my Experiments folder on the scans in ~/data/mri-2026, 1 GPU, overnight.

Labmill may ask a few questions if something is unclear, such as which script to run or how long it should run. Not every job needs your data: you can also ask for something like a quick GPU check, and Labmill skips the upload steps.

As soon as you send a message, Labmill shows that it's working and what it's waiting for, such as "Connecting to bigcluster…" or "Waiting for Claude…". The desktop app shows a small hourglass under the conversation (see [the hourglass](desktop-app.md#the-hourglass-labmill-is-working)); the terminal shows a spinner with the same text in the status bar. After 5 seconds both also show how long it has been working.

## 2. Labmill finds your data

Labmill looks through your chosen data and code folders (or, if you chose none, your home folder) for folders that match your description and shows what it found: the folder, how many files, how big it is, and a few example file names. It sees names and sizes, and it can preview a file if that helps: a short sample from the start of a data file, or a page at a time of a document or script (see the [overview](index.md)). Hidden folders and key files are skipped (see the [overview](index.md)).

You confirm the right folder, or tell Labmill which one you meant. Nothing has left your computer yet. Labmill only makes a list of the files, with their sizes and checksums, so it can later verify that everything arrived intact.

## 3. Labmill picks where to run

Labmill looks at what each cluster offers (partitions, GPU types, how busy it is) and suggests one. You can say "use the other cluster" or name a partition.

## 4. Approve the upload

A box covers the screen with the title of what Labmill wants to do, for example *"Upload 14,212 files (86 GB) from /home/you/data/mri-2026 to bigcluster:~/labmill/segmentation"*. A destination written `cluster:~/folder` is inside your home folder on the cluster. You can also ask for an absolute group folder such as `/data/mygroup/data`; hidden folders (starting with a dot) and system folders are refused.

- Below the title is a breakdown by top-level folder, then the file list (the first 200 lines, then "and K more") and the totals. Scroll with **PgUp/PgDn** or the arrow keys. The hint line at the bottom says when scrolling is possible.
- Choose with **←/→**: **Approve** or **Decline**. **Decline is selected by default.** Press **Enter** to confirm your choice. **Esc** declines right away.
- If you decline, nothing is sent, and Labmill is told you declined. You can also tell Labmill why (it asks in one line; press Enter to send or Esc to skip), and it will adjust its plan.
- If the window is too small to show the whole approval, Labmill asks you to enlarge it. Only Esc (decline) works until then.

While the data moves, Labmill shows live progress in the style of rclone's stats, for example `Transferred: 1.2 GiB / 4.0 GiB, 30%, 45 MiB/s, ETA 1m02s · Files: 3 / 10 · now: data/part-003.csv`. In the desktop app it is a bar above the input; in the terminal app it takes the status line. When it ends, the result shows a summary such as `Transferred 4.0 GiB in 1m31s (45 MiB/s), 10 files`, plus a line starting with `rclone-lib:`. That line holds the transfer's parameters in rclone's own syntax. Labmill has rclone built in as a library and starts no separate rclone process, so `rclone-lib:` marks it as Labmill's. To repeat the same transfer with a stand-alone rclone, replace `rclone-lib:` with `rclone`. The line never contains passwords or keys, and it is not part of what you approve. On Windows, PowerShell 5.1 mangles the quotes in the `--sftp-ssh` value; use PowerShell 7.3 or newer for that command.

Keys pressed in the first half second after the box opens are ignored, on purpose. See the [keyboard reference](keys.md).

Files go through the cluster's transfer computer (the approval names it), which has its own sign-in like the first one, with its own two-factor approval. After that you aren't asked again while the login stays open. A progress line shows how it is going.

**Long transfers.** While a transfer runs, your computer does not go to sleep. If a transfer breaks (the network drops or the sign-in is lost), Labmill starts it again by itself, up to three times. It skips the files that are already there. If it still fails, the chat tells you.

## 5. Labmill drafts the job script

Labmill writes the job script (what the cluster runs) from your request, with the right number of GPUs, time limit and working folder. It runs in the folder you just uploaded.

### Software for Slurm jobs

The assistant uses the software that the cluster already offers (its modules) first. If the job needs something else, it can use a Python virtual environment, a conda environment or an Apptainer image. Labmill builds it once, in the software folder of your institution or in `.labmill/envs` in your home folder. Later jobs use it again. Labmill never removes it. The job script shows the steps, and you approve them like the rest of the script.

### When the job needs a named program

If your job needs a program or library by name (for example a compiler or a particular analysis tool) and runs on Slurm, Labmill first looks at what the cluster already provides. It searches the cluster's software list (the "modules"), then any other software your institution installs centrally, and only after that considers setting up your own environment or using a container. Most of these searches only look and run without asking; a check that needs a full login on the cluster is shown to you first. If a ready-made version exists, the job script uses that exact version, so the same job runs the same way next time.

### When the code has no run script

Sample code often comes with a README and a build file (`CMakeLists.txt`, `Makefile`, `setup.py`, `requirements.txt`) but no script that builds and runs it. Labmill does not guess script names and does not ask you how to run it. It lists the folder, reads the README and the build files page by page, and works out the steps. It uploads your folder as it is, and writes the missing scripts (for example `labmill-build.sh` and `labmill-run.sh`) into the job's folder on the cluster, never on your computer. They are shown to you below the job script when you approve the job, and you can edit them there. Labmill creates them just before the job is submitted. If a file with that name already exists on the cluster with other content, Labmill does not overwrite it and uses another name. Folder names with spaces, such as `OneDrive - Example University`, are fine.

### If a file does not answer

Files kept in a cloud folder (such as OneDrive, with "files on demand") or on a network drive can stop answering when the drive is offline or the file is only a placeholder. Labmill does not wait for such a file: after a few seconds it skips that file, tells you which one, and carries on with the rest. Make the file available offline (or reconnect the drive) and ask again. If a drive stays stuck, quit Labmill from the tray (or the terminal) and start it again after reconnecting.

## 6. Approve the job

Before anything is submitted, Labmill shows the **whole script** in an approval box. Here you can **edit the script directly**: the cursor starts inside the text. Press **Tab** to move to the buttons, **←/→** to choose Approve or Decline, **Enter** to confirm, and **Tab** again to return to editing. If you change the text, what you edited is what gets submitted. Decline is again the default.

If the text contains tabs or hidden characters, Labmill warns you that the editor shows them differently, and that an edit submits the text as shown.

### The check before you submit

The approval box also shows a check that Labmill ran with Slurm. It says that Slurm accepts the script and when it expects the job to start. If Slurm rejects the script, the box shows Slurm's error, so you can fix the script before you submit. If the check cannot run, the box says so. You can still approve.

## 7. Check progress

Once the job is submitted, Labmill tells you its number (for example "job #12") and the job **runs in the background**. The chat is free right away: you can ask questions, start another job, or just wait.

While Labmill is open, it checks your running jobs about every 45 seconds. When one finishes, fails or is cancelled, a short line appears in the chat, for example "Job #12 (train) on bigcluster finished. Ask me to fetch the results." These checks never ask you to sign in. If you are not signed in to the cluster at that moment, Labmill skips the check and tries again later.

Ask "how is my job doing?" or "show me the log of job 12", and Labmill reads the job status and the last lines of the log. If you really want Labmill to sit and wait, say "wait for job 12 to finish". It then checks for up to two minutes at a time, and the chat waits with it. Ask again to keep waiting.

**Job end notice.** When a job ends, the desktop app shows a notification (Settings: **Notify me when a job ends**, on by default). If jobs ended while Labmill was closed, Labmill lists them in a **While you were away** notice when you start it. Slurm can also send you an email when a job ends: turn on **Email me when a Slurm job ends** (off by default). Labmill then adds the email request to the job script, which you see in the review. The email goes to the default address of your institution.

**If a job fails.** Ask "why did job 12 fail?". The assistant collects the facts: the state, the exit code, the memory and time the job used against what you asked for, and the end of the log. Then it names the likely cause. Labmill remembers the cause on your computer only. The next time you draft a job from the same folder on the same cluster, Labmill tells you that an earlier run failed and why.

Press **Tab** to open the **jobs panel** on the right. It lists your recent jobs and refreshes every few seconds. You can close Labmill and come back later: jobs are remembered on your computer, and the cluster keeps the real status.

To stop a job, say "cancel job 12". You approve that too.

**In the desktop app**, each job in the job list has small buttons: **Status**, **Logs**, **Fetch** (when a job has finished or failed and has a folder on the cluster) and **Cancel** (while it is waiting or running). A button simply sends the matching message for you, such as "Cancel job #12", so you get the same answers and the same approval boxes as when you type it.

## Long tasks

A whole job, from finding the data to fetching the results, takes many steps. After a large number of steps in one go (80), Labmill stops with a note such as "I stopped after 80 steps so you can check in. Say 'continue' and I'll go on." This is not an error: type **continue** and Labmill picks up where it left off.

## While Labmill is answering

You don't have to wait for an answer to finish before typing the next message. Type it and press Enter (in the desktop app the Send button says **Queue** while Labmill is busy). The message is held, shown below the chat as "queued", and sent as soon as the current answer ends. Labmill holds one message: if you send another, it replaces the first. Cancelling the current answer doesn't drop the queued message. It is sent right after. In the desktop app, **Remove** drops it.

## 8. Get your results

When the job is finished, say "get the results". Labmill proposes which files to download and where to put them. By default, results go into `~/labmill-results/<job name>`. You approve the download, and the files appear in that folder.

After the download, Labmill compares each file's SHA-256 checksum with the one computed on the cluster, so you know nothing was damaged on the way. If a file doesn't match, Labmill keeps your copy under a name ending in `.mismatch` (nothing is deleted or overwritten), tells you which file, and doesn't mark the job as fetched, so you can fetch it again. If the cluster can't compute checksums, Labmill falls back to comparing file sizes and says so. In the desktop app, click **Open folder** (in the chat or next to the job in the job list) to open it in Explorer, Finder or your file manager.
