# Connect the client to your Labmill account

You can connect Labmill to your account at [app.labmill.org](https://app.labmill.org). Then your jobs show in the web app: their state, times and resources, the job script, the end of the job output, and the problems Labmill saw. You and the research software engineers (RSEs) of your institution can then see what went wrong and help. This is optional. Labmill works the same without it, and while it is not connected it sends nothing to Labmill.

## What is sent

**Labmill sends the job script, the last lines of the job output and error reports to your Labmill account so you and your RSE team can help; mark a job private to send only the scheduler facts.**

For each job, when the scheduler accepts it, when it starts and when it ends, Labmill sends one record. A later record of the same job replaces the earlier one. The record holds:

- the scheduler facts: the cluster (as the HPC system that the web app knows), the Slurm state, the failure category (time-out, out of memory, node failure, preempted, missing software, missing input file, other non-zero exit), the exit code, when the job was submitted, started and ended, the queue wait, the resources it asked for (CPUs, memory, GPUs and GPU type, time limit, nodes), what it got and used (from `sacct`: CPUs, memory, GPUs, nodes, elapsed time, CPU efficiency, largest memory use), the partition and the software modules it loaded (for example `samtools/1.21`);
- the sbatch script, redacted;
- when the job has ended: the last 500 lines of its output and of its error output, redacted; for a failed job also the first 100 lines.

Labmill also sends a short report of each problem in a session: connection and sign-in failures, errors of the assistant (the model, the broker or the relay), failed tool calls (with the tool name), sbatch refusals (the scheduler's message), transfer failures, crashes (with Labmill's own code locations only) and turns that you stopped. Each report is at most 2 KB.

## Redaction

Labmill redacts on your computer before it sends, and app.labmill.org redacts again. Redaction removes passwords, tokens and keys, email addresses, IP addresses, paths, host names, long numbers, your user names and account names, the cluster host names, and the names of the job and of its folders. In the script it also removes the values of `#SBATCH` options that hold names (job name, account, output and error files, folder). Redaction can miss: an output line can still name a gene, a compound or a project. Do not use Labmill for export-controlled or other restricted data.

Labmill never sends data files, program code other than the job script, or environment variable values.

## Private jobs and a fully private account

- **A private job:** tell the assistant "keep this job private" when you ask for the job. Its record then holds only the scheduler facts. Labmill does not read its output and sends no problem report about it.
- **A fully private account:** when the contribution setting of your account is off (app.labmill.org, **Settings**), every record holds only the scheduler facts, and Labmill sends no problem reports. The server also keeps no script, output lines or problem reports for such an account.

Only jobs that change after you connect are sent. Jobs that ended before stay on your computer only.

## Connect

1. Sign in at [app.labmill.org](https://app.labmill.org) and open **Settings > Program tokens**.
2. Make a token. The web app shows it once. It starts with `lmt_` and is valid for one year.
3. Give it to Labmill:
    - **Desktop app:** open **Settings > Labmill account**, paste the token into **Program token** and click **Connect**.
    - **Terminal:** run `labmill login` and paste the token when Labmill asks. The token is not shown while you paste it.

Labmill checks the token with your account before it keeps it. A wrong or revoked token is refused with a message.

The token is kept on this computer only, in `account.json` next to `clusters.yaml` (`~/.config/labmill/account.json`). Only your user account can read the file. Labmill sends the token only to app.labmill.org, over an encrypted connection.

## Check the connection

- **Desktop app:** **Settings > Labmill account > Check connection**.
- **Terminal:** `labmill whoami` shows the account, since when it is connected, and how many job records were sent or are waiting.

## When the computer is offline

Labmill keeps the records and the problem reports in its job database on your computer and sends them when it can reach your account again. It tries again after 1 minute, then after longer pauses, up to 6 hours. Sending never stops your work. The same job is never stored twice in your account, also when two Labmill windows are open. At most 2,000 problem reports wait; newer ones are not kept.

If your account does not know a cluster yet, its records wait and Labmill tries again later. If the account refuses the token (for example, you revoked it), the records wait until you connect with a new token.

## Disconnect

- **Desktop app:** **Settings > Labmill account > Disconnect**.
- **Terminal:** `labmill logout`.

Labmill deletes the token from this computer and sends nothing after that. Records that were not sent stay on your computer. To stop the token from working anywhere, revoke it at **Settings > Program tokens** in the web app.
