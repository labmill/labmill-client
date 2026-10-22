# SSH: keys, agent, config, jump hosts

How to log in to your site's login host (name and 2FA: see your site skill) and
make it comfortable and safe. These commands run on **your own computer**.

## Keys

```bash
ssh-keygen -t ed25519 -C "me@laptop"        # accept the default file; SET A PASSPHRASE
ls -l ~/.ssh                                # id_ed25519 (private), id_ed25519.pub (public)
chmod 700 ~/.ssh && chmod 600 ~/.ssh/id_ed25519
ssh-copy-id -i ~/.ssh/id_ed25519.pub <alias-or-user@host>   # installs the public key
```

- Only the **public** key (`.pub`) is ever shared or uploaded. Never send,
  paste or commit the private key.
- Use a **passphrase**: an unprotected key stolen from a laptop is full access
  to the cluster, and many sites forbid passphrase-less keys.
- Some sites don't allow keys (password plus 2FA only) or want you to register
  the public key in a portal; check your site skill. Keys don't replace 2FA
  where the site enforces it.
- Windows: the built-in OpenSSH client works in PowerShell or Windows Terminal
  (`ssh`, `ssh-keygen`; the optional "OpenSSH Client" feature may need to be
  enabled). The files live in `%USERPROFILE%\.ssh`.
  Windows has no `ssh-copy-id`: append the contents of
  `%USERPROFILE%\.ssh\id_ed25519.pub` to `~/.ssh/authorized_keys` on the
  cluster instead.

## Clusters that accept only keys

Some clusters turn password login off: `ssh` then ends with
`Permission denied (publickey)` before it ever asks for a password, and
`ssh-copy-id` can't work (it needs a login). The public key is registered
somewhere else, usually a web portal (your site skill says where and which
menu). The steps, all on **your own computer**:

1. **See whether you already have a key and an agent.**

   ```bash
   ls -l ~/.ssh/id_ed25519 ~/.ssh/id_ecdsa ~/.ssh/id_rsa 2>/dev/null
   ssh-add -l                                   # keys the agent holds
   ```

   `ssh` tries the default key names above and the agent's keys on its own. If
   one is already registered with the site, skip to step 5.
2. **Create a key** if you have none (never overwrite an existing one: pick
   another file name with `-f`, for example `~/.ssh/id_ed25519_mycluster`):

   ```bash
   mkdir -p ~/.ssh && chmod 700 ~/.ssh
   ssh-keygen -t ed25519 -C "me@laptop" -f ~/.ssh/id_ed25519_mycluster   # SET A PASSPHRASE
   ```

   The private key (no `.pub`) must be readable by you alone (`chmod 600`);
   `ssh` refuses a key that others can read.
3. **Show the public key and upload it** where your site skill says (portal,
   menu path, or an administrator's e-mail address). Upload the whole line
   from the `.pub` file, and nothing else:

   ```bash
   cat ~/.ssh/id_ed25519_mycluster.pub          # macOS: ... | pbcopy   Windows: ... | clip
   ```

   Never upload or paste the private key. Some registries need a few minutes
   (or an administrator's approval) before the key works.
4. **Tell `ssh` which key goes with the host**, in `~/.ssh/config` (a key that
   isn't one of the default names is not tried otherwise):

   ```
   Host mycluster
       HostName login.example.edu
       User myuser
       IdentityFile ~/.ssh/id_ed25519_mycluster
       IdentitiesOnly yes
   ```

5. **Test it**, with `-v` to see which keys are offered and which is accepted:

   ```bash
   ssh -v mycluster
   ```

   `Permission denied (publickey)` after "Offering public key" means the
   server does not know that key yet (not uploaded, wrong line, still being
   approved, or the wrong user name); `no such identity` or `UNPROTECTED
   PRIVATE KEY FILE` is a path or permission problem on your side.
6. Load the key into the agent (next section) so the passphrase is typed once
   per session. If you lose the private key, create a new one and replace the
   uploaded public key; the old one stays valid until you remove it.

A key identifies you without a second step, so protect it with a passphrase,
keep it on one computer you control, and remove old public keys from the
registry when a computer is retired.

## The agent: type the passphrase once

```bash
eval "$(ssh-agent -s)"                       # Linux, if no agent is running
ssh-add ~/.ssh/id_ed25519                    # asks the passphrase once
ssh-add -l                                   # list loaded keys
```

- **macOS:** `ssh-add --apple-use-keychain ~/.ssh/id_ed25519` (older macOS:
  `ssh-add -K`) stores the passphrase in the Keychain, and in `~/.ssh/config`:

  ```
  Host *
    AddKeysToAgent yes
    UseKeychain yes
  ```

  (`UseKeychain` is macOS only; on other systems an unknown option makes ssh
  error unless you add `IgnoreUnknown UseKeychain` above it.)
- **Linux:** the desktop usually runs an agent; otherwise use `keychain` so one agent survives across terminals. Its
  `eval "$(keychain --eval --quiet id_ed25519)"` line belongs below the
  interactive guard in `~/.bashrc` (see shell.md), or it can hang scp and rsync.
- **Windows:** `Get-Service ssh-agent | Set-Service -StartupType Automatic;
  Start-Service ssh-agent` (administrator once), then `ssh-add`.
- **Agent forwarding** (`ssh -A`, `ForwardAgent yes`) lets the remote host use
  your keys, for example to `git clone` from a login host. Use it only to hosts
  you trust, and prefer `ProxyJump` for hops.

## `~/.ssh/config`

```
Host mycluster
    HostName login.example.edu
    User myusername
    ServerAliveInterval 60
    # IdentityFile ~/.ssh/id_ed25519
```

Then `ssh mycluster`, `scp file mycluster:path/`, `sftp mycluster`,
`rsync -av dir/ mycluster:path/` all work with that short name. Keep the file
private: `chmod 600 ~/.ssh/config`.

**How `Host` matching works:**

- The patterns match **the name you type on the command line**, before any DNS
  lookup. `ssh login` doesn't match `Host login.example.edu` or `Host
  *.example.edu`, even if DNS resolves the short name to the same machine. List
  every spelling you use: `Host mycluster mycluster.example.edu login`.
- Wildcards: `*` any run of characters, `?` one character, `!` negation. For
  each setting the **first value found wins**, so put specific `Host` blocks
  before `Host *` and any `Include` lines at the top of the file (an `Include`
  after a `Host` block only applies inside that block).
- See what ssh will really use: `ssh -G mycluster | head -20`. Debug a
  connection with `ssh -v mycluster`.

## Jump hosts and connection sharing

```
Host login-behind-gateway
    HostName login.internal.example.edu
    User myusername
    ProxyJump gateway.example.edu            # or: ssh -J gateway.example.edu …

Host mycluster
    ControlMaster auto
    ControlPath ~/.ssh/controlmasters/%r@%h:%p
    ControlPersist 10m
```

- `ProxyJump` chains hops (`ProxyJump a,b`), each with its own authentication.
- **ControlMaster** keeps one authenticated connection open (`mkdir -p
  ~/.ssh/controlmasters` first); later `ssh`, `scp`, `sftp`, `rsync` and
  tunnels reuse it with no new password or 2FA prompt for `ControlPersist` time.
  `ssh -O check mycluster` tests it, `ssh -O exit mycluster` closes it.
  **Open the connection once** in a real terminal, where you can type the
  password and answer the second factor: `ssh -fN mycluster` signs in and
  keeps the connection in the background (check it with `ssh -O check`).
  Scripts and agents then use `ssh -o BatchMode=yes mycluster '<command>'`: a
  connection that has expired fails at once instead of waiting for a password
  nobody can type.
  Windows' built-in OpenSSH doesn't support these three lines; leave them out
  there.
- **A terminal on the login node without signing in again.** While that shared
  connection is open, `ssh mycluster` (a second terminal window, a new tab)
  gives an interactive shell with no password or 2FA prompt. To start in a
  project folder: `ssh -t mycluster 'cd -- /path/to/folder && exec $SHELL -l'`
  (`-t` asks for a terminal; quote the path if it has spaces). In Labmill the
  same thing is `labmill shell mycluster` (add `--dir <folder>`), or `/shell
  mycluster` in the terminal interface, or the Terminal button of the desktop
  app (tabs, also "Terminal here" for a job's folder, and "Open in system
  terminal" for the computer's own terminal program); they use the login
  Labmill already holds, and what you type there is never sent to the assistant. If the shared
  connection has expired, sign in once first (one attempt: a wrong password
  repeated can lock the account, see below).

## The account name on a new cluster

One account usually serves every cluster of an institution, so do not ask the
user for a user name first. For a new cluster, reuse the `User` of the entry
of another cluster there: `ssh -G <other-alias> | grep '^user '` prints the
name ssh would use (or read it from `~/.ssh/config` and `~/.ssh/config.d/`).
Put that name in the new `Host` block (`User <name>`) and try the sign-in once.
Ask the user for a different account name only when that sign-in is rejected
(or the site skill says the cluster uses a separate account), then change the
`User` line of the new block and try again. Never retry the same password
repeatedly: see "Failed password logins can lock you out". In Labmill,
`add_cluster` does the reuse (the proposal says which cluster the name came
from) and `set_cluster_account` changes the name in Labmill's own SSH settings;
it never rewrites the user's own `~/.ssh/config`.

## Another cluster at your own institution

Institutions add clusters over time, so a setup made earlier may lack one that
now exists. Add it only when the user asks for it by name or clearly needs it,
never as a default. It usually shares the account and sign-in of the clusters
you already use (the site skill says when it does not). As a Claude Code user:
look in `~/.ssh/config` and `~/.ssh/config.d/` for a `Host` block that already
points at the new login host (`grep -ri <hostname> ~/.ssh/config ~/.ssh/config.d`)
and reuse its alias (that block's own `User` decides the account ssh signs in
with, so if it names a different account than the one you mean, stop and sort
that out first: repeated wrong-account logins can lock an account); otherwise add a `Host` block for it (and its jump or
transfer host) with the user name you already use there, leave your existing
blocks alone, and test with `ssh -o BatchMode=yes <host> true` after the first
interactive sign-in. In Labmill the `add_cluster` tool does this for a cluster of
the user's own institution that is not set up yet (the prompt lists them): the
same approval as for another institution, an existing alias for the host is
reused, the new cluster is appended to `clusters.yaml` and nothing else changes;
the desktop app's Settings page lists these clusters under "More clusters at
your institution" with an Add button.

## A cluster at another institution

Work goes to the user's own institution by default. Use another institution's
cluster only when the user explicitly names it (or one of its clusters), never
because it looks faster or emptier, and never move work there on your own.

An account at another institution is separate: its own user name, its own
sign-in method (password plus a second factor, or SSH keys only) and its own
sign-in help, which the site skill of **that** institution gives. Load that
site skill for its host names and rules, and do not apply the home
institution's facts to it. As a Claude Code user, ask for the user name, add a
`Host` block for each of its hosts to `~/.ssh/config` (see above), and test with
`ssh -o BatchMode=yes <host> true` after the first interactive sign-in. In
Labmill the `add_cluster` tool does the same from the institution's profile: it
asks the user to approve the cluster, the hosts and the files it changes, then
writes `clusters.yaml` and `~/.ssh/config.d/labmill`.

### A key-only cluster there: reuse a key you already have

A cluster that accepts only SSH keys knows only the public keys registered at
**its** institution. Before creating a new key, check whether a key you already
have is registered there. Listing the keys on your home cluster uses your
usual home sign-in (with its second factor, if any). Asking the other host about
a key needs no password, but if the way to it goes through a jump host that
takes a password and a second factor, that sign-in comes first: skip the check
in that case and create a new key (step 4).

1. **Keys on your home cluster's login node** (the account you already use):

   ```
   ssh <home> "sh -c 'for f in ~/.ssh/*.pub; do ssh-keygen -lf \"\$f\"; done'"
   ```

   (The `sh -c` keeps it working when your login shell there is csh or tcsh.)

   Each line is a size, a fingerprint (`SHA256:...`), a comment and a type.
2. **Does the other host accept a key?** With the private key on your computer,
   this is a real, key-only login attempt (it opens a session and closes it at
   once; no password or prompt, but it shows up in the host's login log):

   ```
   ssh -o BatchMode=yes -o IdentitiesOnly=yes -o PreferredAuthentications=publickey \
       -i ~/.ssh/<key> <user>@<host> true
   ```

   Exit 0 means the key works; `Permission denied (publickey)` means this
   institution does not know it yet. With only the `.pub` file there is no
   way to ask with plain `ssh`; Labmill asks the server directly (the public-key
   query of the SSH protocol, which never signs anything and opens no session),
   at most 16 keys at a time and only when it needs no jump-host login.
   Many wrong keys in a row can make a server refuse you for a minute or
   more (OpenSSH per-source penalties close the connection before it even
   starts: `Connection closed by <host>` or `kex_exchange_identification`).
   Each failed sign-in adds a few seconds of penalty, so wait several seconds
   between tries. Offer one key at a time with
   `ssh -o IdentitiesOnly=yes -i <key> ...`, as above, never a loop over every
   key. Plain `ssh` without `IdentitiesOnly` offers every key in the agent
   and can stop with `Too many authentication failures` (the server's
   MaxAuthTries). After a "Connection closed" wait a minute before trying
   again. Labmill keeps each connection under the server's limit, pauses
   between connections, and if the host still refuses it waits a minute and
   asks the remaining keys once more, then tells you to wait a few minutes
   instead of suggesting a new key.
3. **Use the home key:** copy the private key without overwriting anything,
   and keep it readable by you alone:

   ```
   test -e ~/.ssh/id_ed25519_<home> && echo "exists: pick another name"
   scp <home>:~/.ssh/<name> ~/.ssh/id_ed25519_<home>
   scp <home>:~/.ssh/<name>.pub ~/.ssh/id_ed25519_<home>.pub
   chmod 600 ~/.ssh/id_ed25519_<home>
   ```

   Then add `IdentityFile ~/.ssh/id_ed25519_<home>` (and `IdentitiesOnly yes`)
   to the `Host` block of the other cluster. A copy of a private key is a
   second place to lose it. Check whether the key has a passphrase
   (`ssh-keygen -y -P "" -f ~/.ssh/id_ed25519_<home>` prints a public key only
   when it has none); if not, add one with `ssh-keygen -p -f <file>` or prefer
   step 4. Never paste a private key into a chat or ticket.
4. **Otherwise create a new key and register it** where the other
   institution's site skill says (`ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519_<name>`,
   then upload the `.pub` file as in "Clusters that accept only keys").

In Labmill this happens when a key-only host rejects the sign-in: it first tries
key files on the computer that were not offered, then (if the home login node
is already connected) the keys there; it asks before copying any private key
(file, fingerprint, source, destination and the risk are shown), checks that
the copy matches the public key, writes it to a new file with mode 600, adds
the `IdentityFile` line to `~/.ssh/config.d/labmill` and signs in. It tells the
user whether the copied key has a passphrase (it asks for it at each sign-in and
never stores it); with none, advise adding one (`ssh-keygen -p`). If you decline
or nothing is accepted, it offers to create a new key and shows the site's
upload page and instruction.

## Long sessions: tmux or screen

Anything that must survive a dropped connection runs inside `tmux`
(`tmux new -s work`, detach with `Ctrl-b d`, `tmux attach -t work`) or
`screen` (`screen -S work`, `Ctrl-a d`, `screen -r work`). Jobs submitted with
`sbatch` don't need it. **If the login name is a pool of several hosts** (DNS
round-robin), a tmux session lives on one of them: log in to that specific host
to find it again (the individual names are in your site skill), and note
`hostname` when you start it.

## Host key warnings

First connection: ssh shows a fingerprint and asks to trust it. Compare it
with the fingerprint your site publishes (site skill) if it gives one.

```
WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!
```

- Often harmless: the host was reinstalled or a login host was replaced. Check
  your site's announcements or ask support **before** continuing. If it is
  confirmed, remove the old entry and reconnect:

  ```bash
  ssh-keygen -R login.example.edu        # use the exact name in the warning
  ssh mycluster
  ```

- If you are on an unfamiliar network (a hotel, a café) or support doesn't
  know about a change, **stop and ask support**. That warning is how a
  man-in-the-middle shows up. Never edit `known_hosts` blindly, and never
  disable checking with `StrictHostKeyChecking no`.

## Failed password logins can lock you out

Stop after **one** failed password login. Check the password (type it in a
text field you can see, or sign in to the institution's web portal with it) or
reset it first, then try again. Many institutions lock the account, sometimes
the e-mail account too, after a few failed sign-ins, and a script or an
assistant that retries by itself can trigger that within seconds. Never loop
over `ssh` with a password, and never try the same password on several hosts
in a row. A denied or timed-out 2FA push is not a wrong password: the password
was accepted, so fix the second factor (phone online, enrolled, approve the
push) rather than changing the password.

## Troubleshooting

| Symptom | Try |
|---|---|
| `Permission denied (publickey)` | `ssh -v`; the wrong user name, key not offered (`ssh-add -l`), or key not installed |
| `Too many authentication failures` | the agent offers many keys; `IdentitiesOnly yes` plus `IdentityFile` in the Host block |
| `Connection timed out` | not on the campus network or VPN the site requires (site skill), or a firewall |
| `UNPROTECTED PRIVATE KEY FILE` | `chmod 600` the key |
| Hangs after login, scp/sftp fails | something in `~/.bashrc` prints text or waits for input; see shell.md |
| Broken pipe after idle | add `ServerAliveInterval 60` |
