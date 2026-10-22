# Using the same know-how in Claude Code

Labmill's knowledge about clusters is not locked inside the app. It is written as **skills**, plain text folders that Claude Code can read too. If you already use Claude Code in a terminal, or prefer it to a chat window, you can install the skills and ask Claude to do the same kind of work. In that case Claude runs the cluster commands for you in your own terminal, and Labmill is not involved.

## The skills

There are three folders to install:

| Skill | What it knows |
|---|---|
| `slurm-user` | Slurm: writing and submitting batch scripts, choosing partitions, reading job status and failures. |
| `research-computing` | Everything that isn't specific to one scheduler: environments, containers, file systems and storage, moving data, getting help and writing a good support ticket. |
| your institution's **site skill** (for example `site-example-edu`) | The facts that differ from place to place: login addresses, sign-in and two-factor steps, cluster names, storage folders, how to get an account. |

The first two are the same for everyone and contain no institution-specific facts. The site skill carries those. If your institution has no site skill, use the generic one (`site-generic`); it tells Claude to ask you for the missing facts and to look them up on the cluster.

## Where they come from

The generic skills are in the `skills/` folder of the Labmill client project, and the generic site skill is in `sites/generic/`. The Labmill app includes the same files, so the app and Claude Code give the same advice. Site skills for a particular institution are not part of the Labmill client. Ask whoever set up Labmill for your institution for your site skill, or use the generic one.

## Installing them

Claude Code looks for skills in a `.claude/skills/` folder, either in a project or in your home folder (`~/.claude/skills/` on macOS and Linux, `%USERPROFILE%\.claude\skills\` on Windows). Copy the three folders there, one folder per skill, and keep the folder names. Copy the **whole** site folder: it points to files inside its cluster folders.

Then start a new Claude Code session and ask in plain words, for example "why is my job pending?" or "run my training script on a GPU". Claude loads the right skill when it needs it.

## What differs from the Labmill app

- The app has built-in approval boxes for uploads, job submissions and downloads. In Claude Code, Claude asks for permission to run each command the way Claude Code always does, so read what it wants to run before you allow it.
- The app keeps your cluster login open for you. In Claude Code you sign in (with your two-factor step) in your terminal first, as you would for any cluster work. See [using the cluster from a terminal](terminal.md).
- The skills hold the same checks and lessons as the app: how to choose a partition and the software, how to check that a download arrived intact, how to diagnose a failed job.

## Keeping them current

The skill files in your `.claude/skills/` folder are yours; nothing updates them behind your back. To get newer ones, copy the folders again from the Labmill project. The Labmill app can update its own copy automatically, see [Know-how updates](updates.md).
