# Data transfer and checks

Labmill never sends a file until you approve the list and the destination.

## Before the upload

1. Labmill looks in the folders you chose (or your home folder) and shows what it found: folder, number of files, size, example file names.
2. It makes a manifest: the paths, sizes and checksums of the files.
3. You see an approval box with the file list, the totals and the destination, for example `bigcluster:~/labmill/segmentation`. The default answer is **Decline**.

Hidden folders, system folders and files that look like keys or passwords are not chosen. See [Privacy](privacy.md).

## How files move

- Labmill sends files one by one with a built-in copy of rclone. It does not start a separate rclone program.
- It never syncs and never deletes files.
- The default way is SFTP, through the cluster's transfer host (`files_host`) when you have one.
- S3 is optional. See [Starting Labmill and signing in](first-run.md#s3).
- A progress line shows the amount, the speed and the time left.

## Long transfers

- Your computer does not go to sleep while a transfer runs.
- If a transfer breaks, Labmill starts it again by itself, up to three times. It skips files that are already there.
- If it still fails, the chat tells you.

## Checks on results

When you download results:

1. You approve which files come down and where. The default folder is `~/labmill-results/<job name>`.
2. Labmill compares the SHA-256 checksum of each file with the checksum computed on the cluster.
3. A file that does not match is kept with the name ending `.mismatch`. Labmill tells you which file. Nothing is overwritten or deleted. The job is not marked as fetched, so you can fetch it again.
4. If the cluster cannot compute checksums, Labmill compares file sizes and says so.

You can turn on **Download results automatically** (off by default). See [Settings](settings.md).

## Limits

[CHECK: the largest upload Labmill accepts is not a fixed number in the user docs. Ask the dev team if a limit should be written here.]
