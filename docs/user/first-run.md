# Starting Labmill and signing in

## Before you start: your Anthropic API key

Labmill's assistant is Claude. Labmill talks to Claude with your own Anthropic API key. Set it in your environment before you start Labmill:

```
export ANTHROPIC_API_KEY=sk-ant-...
```

On Windows, set it as a user environment variable (Settings, search for "environment variables"). Labmill reads the key from the environment only. It never writes the key to disk. Without a key, Labmill starts but tells you that it has no assistant yet.

Planning turns use a larger Claude model and the other turns a faster one. To use one model for all turns, set `LABMILL_MODEL` (for example `LABMILL_MODEL=claude-sonnet-5-5`).

If your institution runs a Labmill relay service (a "broker"), set `LABMILL_BROKER_URL` to its address and `LABMILL_BROKER_TOKEN` to the token it gave you. Labmill then sends model calls there instead of to Anthropic.

## Start Labmill

Open a terminal window, type `labmill` and press Enter. (If your IT team installed Labmill for you, they will tell you if it starts differently.)

To leave Labmill, press **Ctrl+D**. If a job is being submitted at that moment, Labmill waits a few seconds so the job is recorded properly.

## The first-run setup

The very first time, Labmill shows a short welcome form:

1. **Your home institution.** Pick where your cluster account is from the list. You always choose one. Your data mostly lives on your home institution's systems, so Labmill sends work there first. When your institution has a site profile, Labmill knows its login and file transfer computers and how its two-factor sign-in works, and its assistant knows the local details (cluster names, storage folders, software notes). If your institution isn't in the list, choose **My institution isn't listed**: Labmill then asks for the cluster login computer (the name you would type after `ssh`, for example `login.example.edu`). This tells Labmill which institution is your home.
2. **Your account name.** Type the user name you use to sign in to the cluster (your institution's username). Use lowercase letters, digits, dots, dashes and underscores.
3. **Save these settings?** Choose **Yes**. Labmill saves the cluster addresses and your user name in the standard SSH settings format, in a separate file (`~/.ssh/config.d/labmill`), and adds one line at the top of your `~/.ssh/config` so that file is used. Before changing your own file it makes a backup copy next to it. If you already had a name for the same cluster, Labmill reuses your name and doesn't write a second one. Labmill saves two computers for each Slurm cluster: the login computer and the file transfer computer. Uploads and downloads go through the transfer computer, which you sign in to the same way. The upload and download approval questions name the computer the files travel through. In `clusters.yaml` the transfer computer is the `files_host` line. Keep it: if you remove it, Labmill falls back to the login computer, which may not allow file transfers.

You only do this once. Your cluster list is kept in `~/.config/labmill/clusters.yaml`, and the institution you chose is its `site:` line (for example `site: example.edu`). If `clusters.yaml` has no `site:` line (for example settings saved by an older Labmill) or names an institution this Labmill doesn't know, Labmill shows the institution form again with a short explanation and adds only the `site:` line; your clusters stay as they are. To switch institution later, change the `site:` line (and the cluster entries), or move `clusters.yaml` aside and start Labmill again to get the welcome form.

Labmill reads job times in UTC, so you normally don't need to set anything. Only if a Slurm cluster's job start and end times still look wrong (its Slurm ignores the time zone request), add a `timezone:` line to its entry, for example `timezone: Europe/Berlin`.

## The first login

Labmill signs in to the cluster the first time it needs it, for example when you ask about queues or when you approve an upload. A small box pops up in the window.

1. **Password.** Type your cluster password. The characters are hidden and the password is not saved anywhere.
2. **Two-factor sign-in.** If your institution uses a push to your phone, Labmill chooses that option for you. Look at your phone and **approve the push notification**. Labmill waits up to about a minute.
3. That's it. Labmill keeps the login open while you work, so you aren't asked again for every upload or job. A small background helper holds the login and closes it by itself about 10 minutes after you stop using Labmill.

### If no push arrives

After the wait, Labmill tells you that two-factor sign-in did not complete and shows how to set it up. The usual reasons are that you are not enrolled in two-factor sign-in yet (the message shows your institution's setup page, or ask your help desk), your phone is offline, or the push was denied or expired. Fix that and just ask Labmill to try again.

### If the password is rejected

Labmill tries your password once and stops. It says that the sign-in was denied, repeats your institution's help when there is any, and reminds you to check your password before trying again: many institutions lock the account after repeated failed sign-ins. Labmill will not start another sign-in by itself in that conversation turn. Check or reset your password, then tell Labmill to try again.

### If your cluster accepts only SSH keys

Some clusters don't accept passwords at all, only SSH keys. Labmill knows which ones from your institution's profile, and instead of the password box it guides you through the one-time key setup:

1. **Your existing keys.** Labmill first looks for a key it can use: the default key files in your `.ssh` folder (`id_ed25519`, `id_ecdsa`, `id_rsa`), a key your SSH agent holds, or one named in your SSH settings. If a key is protected by a passphrase, Labmill asks for it (the characters are hidden and the passphrase is not saved anywhere). If the cluster already knows the key, you are signed in and nothing else happens.
2. **Reusing a key you already have.** Labmill asks the cluster which of your existing keys it already accepts. It sends only the **public** keys for this and never signs in again just to ask. It checks the keys on this computer and, if you are already signed in to your home institution's cluster, the keys stored in the `.ssh` folder there. If an accepted key sits on that cluster and not on this computer, Labmill offers to **copy it here**. The box shows the key file, where it comes from and where it goes, and warns that this puts a copy of a private key on your computer. The choice is **No** unless you pick **Yes**. The copy goes into a new file that only you can read (an existing file is never overwritten), and the key's contents are never shown to the assistant or written to the chat. Labmill also tells you whether the key has a passphrase, which it then asks for at sign-in. If the path to the cluster goes through another host that needs a password, Labmill skips this check and says so.
3. **Creating a key.** If no existing key is accepted, or you decline the copy, Labmill asks whether to create a new one. The private key stays on this computer in your `.ssh` folder and Labmill never overwrites an existing key (a new key gets its own name if `id_ed25519` is taken). You may protect it with a passphrase, which Labmill then asks for at each sign-in.
4. **Registering the public key.** Labmill shows the **public** key with a **Copy** button (**Ctrl+Y** in the terminal app) and your institution's instructions for where to paste it, usually a page on the cluster's website. Do that, then choose **I've added it, try again**. The public key is meant to be shared; the private key never leaves your computer.
5. **Signing in.** Labmill signs in with the key and remembers it in the SSH settings it keeps for you (the same file the terminal's `ssh` reads), so `ssh` and `sftp` work with it too.

If the cluster does not accept the key yet, Labmill asks you again to finish the upload; some registries take a few minutes. Choose **Cancel** to stop; nothing is lost, and the key stays in your `.ssh` folder for the next try. On Windows the new key files get the permissions that your user profile folder gives them, which is what the built-in OpenSSH expects.

**If the cluster briefly refuses connections.** While Labmill checks which of your existing keys the cluster accepts (step 2), the cluster may shut the door for a short time. Clusters often shut the door for a short time after several sign-in tries in a row. Labmill guards against this by asking about a few keys at a time and pausing between tries. If the cluster still refuses, Labmill says so, waits a minute and checks the remaining keys once more. If it is still refused after that, Labmill stops with a message like "is still refusing connections; wait a few minutes and try again". It does not ask you to create a new key in that case, because the problem is the waiting time, not your keys. Wait a few minutes and start again.

### If your password is rejected

Labmill tells you to check the user name and password. It tries your password once per login, so repeated mistakes don't lock your account.

## The first connection to a cluster: "Do you trust this server?"

The first time Labmill connects to a cluster computer, it shows a question like this: *"This is the first time you connect to login.example.edu, so Labmill cannot yet check that it is the real server. Its identity fingerprint is SHA256:... Continue and remember this server?"*

This is the same check your terminal's `ssh` does. Every server has a unique fingerprint. By saying **Yes**, you tell Labmill "this is the right server, remember it". From then on Labmill checks automatically. If your HPC center publishes the fingerprint, you can compare it. Otherwise it is normally safe to continue the first time, from a trusted network.

The default answer is **No**. If, later, the server's identity changes, Labmill refuses to connect and tells you not to continue, because it can mean someone is intercepting the connection. Contact your HPC support in that case.

Labmill saves the remembered servers in your normal `~/.ssh/known_hosts` file.

## S3

A cluster entry can name an S3 bucket instead of sending files over SFTP. This suits large data sets. Set `transfer: s3` and add an `s3` block:

```yaml
- name: bigcluster
  type: slurm
  ssh_host: bigcluster-login
  transfer: s3
  s3:
    bucket: my-bucket
    prefix: Labmill        # optional: everything Labmill writes goes below this
    region: us-east-1    # optional: default is the region of your AWS profile
    profile: myprofile   # optional: name of your AWS profile
    # endpoint: https://s3.example.edu   # optional: S3-compatible storage
    # sse: aws:kms                       # optional: for buckets that require KMS encryption
```

If your bucket requires KMS encryption, add `sse: aws:kms`; to use a specific key, also add `kms_key_id` (a key ID, ARN or alias). `sse: AES256` asks for S3-managed encryption.

Your S3 credentials come from your AWS profile (`aws configure`, or `aws sso login` for single sign-on). They never go in this file, and Labmill refuses a file that has them. Labmill asks you to approve the file list, the full `s3://bucket/prefix/folder` destination and the AWS profile before anything is sent. It only writes below that folder, never creates buckets, and checks each uploaded object (size and checksum) against your approved list. Results are downloaded the same way, and never replace files you already have.

Labmill does not set up the bucket or give your jobs access to it. Your job must be able to read the bucket itself, for example with credentials on the cluster (see below). Ask your cluster support if you're not sure.

### Use your AWS profile in Slurm jobs

You can ask Labmill to copy one AWS profile from your computer to the login node of a Slurm cluster, so your jobs can read and write S3. Labmill stores it as a separate section named `[labmill-<name>]` in `~/.aws/credentials` there and leaves your other sections alone. This always asks first, also when **Approve everything automatically** is on. If the profile uses single sign-on (SSO), Labmill copies temporary keys that expire. Claude never sees the key values. If you have no AWS profile on your computer, run `aws configure` first. Your institution can turn this off.
