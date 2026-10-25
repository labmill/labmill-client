# Connect the client to your Labmill account

You can connect Labmill to your account at [app.labmill.org](https://app.labmill.org). Then your finished jobs show in the web app: their state, times and resources, and the cause category of a failed job. This is optional. Labmill works the same without it, and while it is not connected it sends nothing to Labmill.

## What is sent, and what is not

When a job ends (completed, failed, timed out, out of memory or cancelled), Labmill sends one record of it to your account:

- the cluster (as the HPC system that the web app knows);
- the Slurm state and, for a time-out or out-of-memory failure, that category;
- when the job was submitted, started and ended, and how long it waited in the queue;
- the resources the job asked for: CPUs, memory, GPUs and GPU type, time limit, nodes;
- the partition;
- the names and versions of the software modules it loaded (for example `samtools/1.21`).

Labmill does **not** send:

- job scripts or commands;
- data files, program code or file names;
- job names, folder names or paths;
- environment variables, passwords, keys or tokens;
- log lines or the failure causes it remembers on your computer.

Only jobs that end after you connect are sent. Jobs from before stay on your computer only.

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

Labmill keeps the records in its job database on your computer and sends them when it can reach your account again. It tries again after 1 minute, then after longer pauses, up to 6 hours. The same job is never stored twice in your account, also when two Labmill windows are open.

If your account does not know a cluster yet, its records wait and Labmill tries again later. If the account refuses the token (for example, you revoked it), the records wait until you connect with a new token.

## Disconnect

- **Desktop app:** **Settings > Labmill account > Disconnect**.
- **Terminal:** `labmill logout`.

Labmill deletes the token from this computer and sends nothing after that. Records that were not sent stay on your computer. To stop the token from working anywhere, revoke it at **Settings > Program tokens** in the web app.
