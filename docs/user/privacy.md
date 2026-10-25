# Privacy

The full privacy policy is at <https://labmill.org/privacy/>. This page tells what the client does.

## What stays on your computer

- Your files, until you approve an upload.
- Your cluster password, two-factor codes and key passphrases. They are never written to disk.
- Your Anthropic API key. Labmill reads it from the environment and never writes it to disk.
- The job list, the causes of failed jobs and the log. They are kept in `labmill.db` and `labmill.log` on your computer.
- Labmill has no telemetry. [CHECK: confirm with the dev team that no usage data is sent to Labmill, and what the optional broker sees.]

## What goes to Claude

Labmill's assistant is Claude. Labmill sends to the Anthropic API (or to the broker, if you set one):

- your messages;
- the results of Labmill's actions: file names, folder sizes, short previews, job status, the end of a job log, and the output of commands on the cluster.

Labmill shows the assistant file names and metadata, and small samples only. It does not send whole data sets to Claude. Values that look like passwords, tokens or API keys are replaced by a placeholder before the assistant sees them.

## What goes to the cluster

Only what you approved: the files in the approved list, the job script, and commands that you approved (or that only read). Reads of files that hold keys or tokens ask first, because the content would go to Claude.

## What Labmill does not read on your computer

Labmill refuses these on your own computer, even with approval: `.ssh`, `.gnupg`, `.aws/credentials`, `.netrc`, private keys (`id_rsa`, `*.pem`, `*.key` and similar), password files and shell histories. It does not choose hidden folders, system folders, `~/Library` (macOS) or `AppData` (Windows).

## What Labmill does not do

- It has no accounts and no web service of its own in the default setup.
- It has no shell on your computer. Claude uses fixed tools only.
- It installs nothing on the cluster and leaves nothing running there.

## Questions

Write to <help@labmill.org>.
