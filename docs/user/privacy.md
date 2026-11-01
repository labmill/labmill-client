# Privacy

The full privacy policy is at <https://labmill.org/privacy/>. This page tells what the client does.

## What stays on your computer

- Your files, until you approve an upload.
- Your cluster password, two-factor codes and key passphrases. They are never written to disk.
- Your Anthropic API key. Labmill reads it from the environment and never writes it to disk.
- The job list, the causes of failed jobs and the log. They are kept in `labmill.db` and `labmill.log` on your computer.
- Labmill has no telemetry. [CHECK: what the optional broker sees.]
- Your Labmill program token, if you connect an account. It is kept in `account.json`, which only your user account can read.

## What goes to Claude

Labmill's assistant is Claude. Labmill sends to the Anthropic API with your own key (or to the broker, if you set one, or to the Labmill test relay, see below):

- your messages;
- the results of Labmill's actions: file names, folder sizes, short previews, job status, the end of a job log, and the output of commands on the cluster.

Labmill shows the assistant file names and metadata, and small samples only. It does not send whole data sets to Claude. Values that look like passwords, tokens or API keys are replaced by a placeholder before the assistant sees them.

Without your own `ANTHROPIC_API_KEY`, a client connected to your Labmill account uses the Labmill test relay (for invited testers): the same requests go through app.labmill.org to Anthropic, with Labmill's key. app.labmill.org does not store or log them; it keeps only token counts per day, for the limits. With your own key, the requests go to Anthropic directly and nothing goes through Labmill. That is the private choice.

## What goes to your Labmill account

Nothing, unless you connect the client to your account at app.labmill.org. Then:

**Labmill sends the job script, the last lines of the job output and error reports to your Labmill account so you and your RSE team can help; mark a job private to send only the scheduler facts.**

In detail: the scheduler facts of each job (cluster, state, times, resources asked for, got and used, partition, software module names), the sbatch script, the last 500 lines of the job output and of its error output (for a failed job also the first 100 lines), and a short report of each problem in a session (connection, sign-in, assistant, tool, sbatch and transfer errors, crashes, stopped turns). Labmill redacts all text on your computer before it sends it (passwords, tokens, keys, emails, IP addresses, paths, host names, user and job names) and the server redacts it again. A private job sends only the scheduler facts. A fully private account (contribution off) sends only the scheduler facts and no problem reports. It never sends data files or program code other than the job script. See [Connect the client to your Labmill account](account.md).

## What goes to the cluster

Only what you approved: the files in the approved list, the job script, and commands that you approved (or that only read). Reads of files that hold keys or tokens ask first, because the content would go to Claude.

## What Labmill does not read on your computer

Labmill refuses these on your own computer, even with approval: `.ssh`, `.gnupg`, `.aws/credentials`, `.netrc`, private keys (`id_rsa`, `*.pem`, `*.key` and similar), password files and shell histories. It does not choose hidden folders, system folders, `~/Library` (macOS) or `AppData` (Windows).

## What Labmill does not do

- It needs no Labmill account. Connecting one is optional, and until you do, nothing goes to Labmill. Labmill has no telemetry: problem reports go only to your own connected account.
- It has no shell on your computer. Claude uses fixed tools only.
- It installs nothing on the cluster and leaves nothing running there.

## Questions

Write to <help@labmill.org>.
