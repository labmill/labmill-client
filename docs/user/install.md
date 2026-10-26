# Install

Labmill has two programs. Both do the same work and share the same settings and job list.

| Program | What it is |
|---|---|
| `labmill-gui` | The desktop app: one chat box and a job list. |
| `labmill` | The terminal app (TUI): a chat pane and a job dashboard. |

## Download

Go to the [Labmill releases page](https://github.com/labmill/labmill-client/releases). Open the newest release. Download the files for your computer from the **Assets** list.

| Your computer | Desktop app | Terminal app |
|---|---|---|
| macOS (Apple silicon) | `labmill-gui-darwin-arm64` | `labmill-darwin-arm64` |
| macOS (Intel) | not built | `labmill-darwin-amd64` |
| Windows (64 bit, Intel or AMD) | `labmill-gui-windows-amd64.exe` | `labmill-windows-amd64.exe` |
| Windows on Arm | not built | `labmill-windows-arm64.exe` |
| Linux (64 bit, Intel or AMD) | `labmill-gui-linux-amd64` | `labmill-linux-amd64` |
| Linux on Arm | `labmill-gui-linux-arm64` | `labmill-linux-arm64` |

The release also has a file `SHA256SUMS`. It is the only checksum file: there is no `.sha256` file for each download. To check a download, compute its SHA-256 checksum and compare it with the line for your file.

```
sha256sum labmill-linux-amd64        # Linux
shasum -a 256 labmill-darwin-arm64   # macOS
certutil -hashfile labmill-windows-amd64.exe SHA256   # Windows
```

Labmill is one file. There is no installer yet. Put the file in a folder you keep, for example `~/bin` or `C:\Tools`.

## Before you start

You need:

- an account on a Slurm cluster, with its two-factor sign-in if your institution uses one;
- your own Anthropic API key in the `ANTHROPIC_API_KEY` environment variable, or, for invited testers, a connected Labmill account (see [Starting Labmill and signing in](first-run.md)).

## macOS

The Labmill programs are not signed by Apple at first. macOS blocks them the first time. This is expected.

1. Open the Terminal app. Go to the folder with the download.
2. Make the file runnable and remove the download mark:
   ```
   chmod +x labmill-gui-darwin-arm64
   xattr -d com.apple.quarantine labmill-gui-darwin-arm64
   ```
3. Start it: `./labmill-gui-darwin-arm64`.

Instead of steps 2 and 3, you can Control-click the file in Finder, choose **Open**, and confirm.

If macOS still says that it cannot check the program, open **System Settings > Privacy & Security**, scroll down, and click **Open Anyway** next to the message about Labmill. Then start the program again and confirm.

Do the same steps for `labmill-darwin-arm64` (or `-amd64`) if you want the terminal app. Run it as `./labmill-darwin-arm64`. Rename it to `labmill` and put it in a folder on your PATH, and you can start it by typing `labmill`.

The macOS desktop app is a plain program file, not a `.app` bundle or a `.dmg`.

## Windows

The Windows programs are not signed at first. Windows SmartScreen may show "Windows protected your PC".

1. Double-click `labmill-gui-windows-amd64.exe`.
2. In the SmartScreen window click **More info**, then **Run anyway**.

For the terminal app, open PowerShell in the folder and run `.\labmill-windows-amd64.exe`. Rename the file to `labmill.exe` and put it in a folder on your PATH to start it with `labmill`.

Labmill uses the OpenSSH settings of your user profile (`~/.ssh`). You do not need to install anything else.

## Linux

1. Make the file runnable: `chmod +x labmill-gui-linux-amd64 labmill-linux-amd64`.
2. Start it: `./labmill-gui-linux-amd64` or `./labmill-linux-amd64`.

The desktop app needs the WebKitGTK 4.1 library. On Debian and Ubuntu the package is `libwebkit2gtk-4.1-0`. [CHECK: confirm the run-time packages on a clean system.]

For the tray icon, your desktop needs a system tray. KDE and most panels have one. GNOME needs the AppIndicator extension. Without a tray, closing the window quits Labmill. See [The desktop app and the tray](desktop-app.md).

## Check that it works

Run `labmill version`. It prints `Labmill` and the version number.

## Updates

Labmill does not update itself. To update, download the newest files from the releases page and replace the old ones. Your settings and job list stay.

## Remove Labmill

Delete the program file. Labmill keeps these files, and you can keep or delete them:

| File | What it holds |
|---|---|
| `~/.config/labmill/clusters.yaml` | your cluster list |
| `~/.config/labmill/gui.json` | desktop app settings |
| `~/.ssh/config.d/labmill` | the SSH settings Labmill wrote (and one `Include` line at the top of `~/.ssh/config`) |
| `~/.local/state/labmill/` | the job list (`labmill.db`) and the log (`labmill.log`) |
| `~/labmill-results/` | results you downloaded |

If you turned on **Start in the tray when I log in**, turn it off first (tray menu). See [The desktop app and the tray](desktop-app.md#starting-with-your-computer).

[CHECK: the paths of the state and config folders on Windows and macOS. The code uses `~/.config/labmill` unless `XDG_CONFIG_HOME` is set, and `~/.local/state/labmill` unless `XDG_STATE_HOME` is set.]
