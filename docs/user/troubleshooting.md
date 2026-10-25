# Troubleshooting

## Labmill says it has no assistant

`ANTHROPIC_API_KEY` is not set in the program's environment. Set it, then start Labmill again. On Windows, set a user environment variable and open a new window. A desktop app started from the Start menu needs the variable set before you log in. [CHECK: how the desktop app on macOS finds the key when started from Finder.]

## macOS or Windows blocks the program

The programs are not signed at first. See [Install](install.md).

## No two-factor push arrives

- You may not be enrolled in two-factor sign-in. Labmill shows your institution's setup page when it knows it. Or ask your help desk.
- Your phone may be offline.
- The push may have been denied or have expired.

Fix the cause, then ask Labmill to try again.

## The password is rejected

Labmill tries once and stops, because many clusters lock an account after repeated failures. Check or reset your password, then ask Labmill to try again.

## "Still refusing connections"

The cluster shut the door for a short time after several sign-in tries. Wait a few minutes and start again.

## The server identity changed

Labmill refuses to connect and tells you not to continue. This can mean that someone intercepts the connection. Contact your cluster support.

## Uploads or downloads fail with odd errors

Check your login files on the cluster (`~/.bashrc`, `~/.bash_profile`, `~/.cshrc`). A file that prints text at every login (a greeting, `echo` lines, a module list) breaks file transfers. Make such lines run only for interactive logins.

## A file does not answer

A file in a cloud folder (for example OneDrive "files on demand") or on a network drive can stop answering. Labmill skips it after a few seconds and tells you which file. Make the file available offline and ask again.

## A key press does nothing right after a box opens

Labmill ignores keys for half a second when an approval box opens. Press the key again. See the [keyboard reference](keys.md).

## The window is too small for an approval

Enlarge the window. Until then only Esc (decline) works.

## The tray icon is missing (Linux)

Your desktop may have no system tray. GNOME needs the AppIndicator extension. Without a tray, closing the window quits Labmill.

## Labmill stops after many steps

After 80 steps in one go, Labmill stops so you can check in. Type **continue**.

## My settings changes have no effect

Labmill reads `clusters.yaml` and the SSH settings when it starts. Quit Labmill from the tray menu and start it again.

## Logs

The log is `labmill.log` in the folder of the job database (`~/.local/state/labmill/` by default). In the desktop app, **Settings > About and diagnostics > Open logs folder** opens it. **Copy diagnostics** makes a short report without passwords, keys, codes or user names. Send it with your question to <help@labmill.org>.
