# Using the saved SSH settings in a terminal

Labmill saves your cluster connection settings in the standard OpenSSH format. If you ever want to work on a cluster by hand, you can use the saved name, called an alias, in any terminal:

```
ssh bigcluster-login
```

The alias for each cluster is in `~/.config/labmill/clusters.yaml` (the `ssh_host` entry) and in the file `~/.ssh/config.d/labmill`. It carries your user name, the host, and any jump host, so you don't have to type them. If you already had your own SSH name for the same cluster, Labmill reused it and you keep using that.

On macOS and Linux, the settings also tell `ssh` to keep the login open for 10 minutes after you disconnect, so a second terminal window doesn't ask for your password and two-factor approval again. (These lines are left out on Windows, where the built-in OpenSSH doesn't support them.) The terminal's login is separate from Labmill's own, so you may be asked once more the first time.

Your password and two-factor approvals work exactly as they do for any cluster login. Labmill doesn't store them.

## A terminal on the cluster from Labmill

While Labmill is signed in to a cluster, you can open a terminal on its login node without a new password or approval:

```
labmill shell bigcluster
```

Use the cluster name from Labmill. To start in a folder on the cluster, add it: `labmill shell bigcluster --dir /data/mygroup/project`. Inside the terminal interface, type `/shell bigcluster` (also with `--dir`); Labmill steps aside, shows a short note, and comes back when you type `exit`. If Labmill is not signed in, it asks for your password and approval first, once. This works in a Windows console, Terminal.app and Linux terminals alike.

The terminal is yours: what you type and see there is not sent to the assistant or saved in the chat, and the assistant never uses it. Labmill only records in its log that a terminal was opened and closed on the cluster. Labmill keeps the sign-in open while the terminal is open.

## If file transfers fail

If uploads or downloads fail with odd errors, check your login files on the cluster (for example `~/.bashrc`, `~/.bash_profile` or `~/.cshrc`). A file that prints text every time you log in, such as a greeting, `echo` lines or a module list, breaks file transfers. Move such lines so they only run for interactive logins, or remove them, and try again.
