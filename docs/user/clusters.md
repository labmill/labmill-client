# Clusters and sign-in

Labmill uses your own cluster account. It has no accounts of its own. It supports Slurm clusters only.

## What Labmill needs from you

- Your home institution (a list in the first-run form, or enter the login host yourself).
- Your user name on the cluster.
- Your password and your two-factor step, when the cluster asks for them.

The first-run form is in [Starting Labmill and signing in](first-run.md).

## SSH

Labmill signs in over SSH. It does its own SSH. It does not start the `ssh` program.

- Labmill writes its connection settings in OpenSSH format to `~/.ssh/config.d/labmill`.
- It adds one `Include` line at the top of `~/.ssh/config`. It makes a backup of your file first.
- It never changes entries that you wrote yourself.
- Because the format is standard, `ssh <alias>` works in a terminal too. See [Using the saved SSH settings in a terminal](terminal.md).
- Labmill keeps one login open while you work. A small helper program holds it and exits about 10 minutes after you stop.
- Host keys go to your normal `~/.ssh/known_hosts` file. The first time, Labmill asks "Do you trust this server?". The default answer is **No**.

## Two-factor sign-in

1. Labmill asks for your password in its own box. The characters are hidden.
2. If your institution uses a push to your phone, Labmill picks that option. Approve the push on your phone. Labmill waits up to about a minute.
3. Labmill keeps the login open. It does not ask again for each upload or job.

Passwords, two-factor codes and key passphrases are never written to disk.

If your password is wrong, Labmill tries once and stops, so that your account is not locked. If no push arrives, see [Troubleshooting](troubleshooting.md).

## Clusters that accept only SSH keys

Labmill guides you through a one-time key setup. It looks for keys you already have, can create a new key, and shows you the public key to register. Labmill never overwrites an existing key. The full steps are in [Starting Labmill and signing in](first-run.md#if-your-cluster-accepts-only-ssh-keys).

## Add or change a cluster

- In the desktop app: **Settings > Clusters**. See [Settings](settings.md).
- In the terminal app: type `/settings` to see the settings. Ask Labmill in the chat to set up another cluster. It shows the details for your approval.
- By hand: edit `~/.config/labmill/clusters.yaml`. Labmill reads it when it starts.

Before the first change, Labmill saves a copy of `clusters.yaml` next to it.

## Open a terminal on the cluster

`labmill shell <cluster>` opens a terminal on the login node while Labmill is signed in. The desktop app has a **Terminal** button. What you type there is not sent to the assistant.
