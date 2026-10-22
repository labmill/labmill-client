# Getting help from research computing support

Support teams can solve most problems quickly when the first message already
contains what they would otherwise ask for. Where the help channel is (ticket
portal, email address, office hours, chat) and its hours: see your site skill.
If you can't find it, the site's documentation or the message of the day on
login (`cat /etc/motd`) usually names it.

## Try these first (five minutes)

1. **Read the full error message**, from the first line, and search for its
   exact text in the site documentation.
2. **Re-run it** once; transient file system or network hiccups happen.
3. **Check the basics**:

   ```bash
   hostname; whoami; date              # where am I, as whom, when
   df -h ~ /path/to/project            # out of space? (see filesystems-and-storage.md)
   squeue --me                         # Slurm: is my job really in the queue?
   sacct -j <jobid> --format=JobID,State,ExitCode,Elapsed,MaxRSS,NodeList
   ```

4. **Look at the job's logs** (`tail -50 slurm-<jobid>.out`) and, for Slurm,
   the pending reason (`squeue -j <jobid> -o "%i %T %r"`); see slurm-user.
5. **Ask: what changed?** A new module version, a moved folder, an expired
   password, a full quota, a different node type. "It worked last week" plus
   what changed since is the most useful clue.
6. **Isolate it**: a minimal example (`python -c "import x"`, one small input)
   that shows the problem beats a 500-line pipeline.
7. For access problems, try from a different network (campus versus home) and
   check the site's status or announcement page (site skill).

## What a good ticket contains

Put these in the **first** message:

- **Who and where**: your username, the cluster or system name, the login host
  you used, your research group or project/account.
- **What you tried to do**, in one sentence, and **what you expected**.
- **The exact command or script**, copied as text (not a screenshot): for
  Slurm the full `sbatch` script path, for a login problem the exact `ssh -v`
  command.
- **The full error text**, not a paraphrase, and the relevant log lines (the
  *first* error, not only the last).
- **Job IDs** (and the node name if known); a path to the script and logs they
  can read, with permissions that let staff see them.
- **When** it started and **when it last worked**, and what changed.
- **How to reproduce it**, the smallest steps.
- **Environment**: loaded modules (`module list`), Python or conda environment
  (`python --version`, `pip list` snippet), container image and tag.
- **Impact and urgency**: a paper deadline in two days helps staff
  prioritize; "urgent" alone doesn't.
- One problem per ticket. Reply on the same ticket rather than opening a new
  one.

A useful template:

```
Subject: Job 123456 on <cluster> killed with OUT_OF_MEMORY (worked last week)

User: myuser   Group: mylab   Login host: <host>
Goal: run train.py on 4 GPUs. Expected: finishes in ~2 h (last on 2025-03-01).
Command: sbatch /path/to/project/train.sbatch
Error (first lines of /path/to/project/slurm-123456.out):
  <paste text>
sacct: <paste output of sacct -j 123456 --format=JobID,State,ExitCode,MaxRSS,ReqMem,NodeList>
Changed since last success: updated PyTorch module 2.1 -> 2.3.
Tried: same script with 1/10 of the data runs fine.
```

## Don't send

- **Passwords, 2FA codes, SSH private keys, API tokens, AWS or other cloud
  keys.** Support will never ask for them. If you pasted one, treat it as
  leaked and rotate it (site skill for how).
- Screenshots when the text can be copied; entire home folders; patient or
  other sensitive data. Describe the data's location instead.

## Where else to look

- The site's documentation and FAQ (site skill), and announcements for
  maintenance windows and outages.
- Vendor documentation: Slurm <https://slurm.schedmd.com/documentation.html>,
  Apptainer <https://apptainer.org/docs/>, Lmod
  <https://lmod.readthedocs.io/>.
- Your own group: a colleague who runs the same pipeline has often seen it.
- Office hours or consultations (site skill) are best for "how should I
  structure this workflow", which doesn't fit a ticket.

## After it's solved

Reply with what fixed it and note it in your project's README so the next
person (and future you) doesn't need a ticket.
