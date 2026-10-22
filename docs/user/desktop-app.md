# The desktop app and the tray

The Labmill desktop app has the chat on the left and your jobs on the right. You add your data folder, and your code folder if you have one, with the **+** button in the chat box.

## The hourglass: Labmill is working

Labmill always shows when it is busy, so you never have to guess whether it heard you.

**While it starts.** The window opens with an hourglass and **Starting labmill…**. The text below it says what Labmill is doing:

| Text | What Labmill is doing |
|---|---|
| Loading your settings… | Reading your cluster settings (the first time, it then asks for your institution and username). |
| Opening the job database… | Opening its list of your jobs on this computer. |
| Loading skills… | Loading what it knows about your clusters. |

If starting takes longer than about 10 seconds, a second line says which step it is still waiting for, for example "Still waiting for: loading skills (14 s)". The chat replaces the hourglass as soon as the window is up; the line under it shows the step still running, and a message you type meanwhile is sent when Labmill has finished starting. On Windows a small **Starting labmill…** window with a moving bar appears at once, even before the main window, and closes when the main window is ready; the tray icon appears early too; until Labmill is ready its tooltip reads "Labmill is starting…".

**After you send a message.** A small hourglass with **Working…** appears under the conversation right away, and changes to the step Labmill is on:

| Text | What Labmill is doing |
|---|---|
| Connecting to … | Logging in to a cluster (a login window or two-factor approval may follow). |
| Waiting for Claude… | Waiting for the assistant's answer. |
| Running … | Running one of its actions, for example `cluster_info` or `upload_data`. Uploads and downloads also show their own progress bar. |

After 5 seconds the row also shows how many seconds Labmill has been working, so you can tell it hasn't frozen. The row disappears when the answer starts to appear, and **Cancel** stays available the whole time.

If your computer is set to reduce motion (Windows: Settings → Accessibility → Visual effects → Animation effects off; Mac: System Settings → Accessibility → Display → Reduce motion), the hourglass stands still. Screen readers announce the status text when it changes.

## Approve everything automatically

Next to **Send** is a checkbox, **Approve everything automatically**. It is off by default. When you tick it, a banner appears above the chat: "Auto-approve is on, Labmill approves every step for you", with a **Turn off** button. From then on Labmill does not show approval screens. Each step appears in the chat as "Auto-approved: ...". Reading key or token files, questions from Labmill and logins still ask. The same switch is on the Settings page, which also explains how it differs from "Run commands on the cluster without asking".

## Download results automatically

Next to **Approve everything automatically** is a second checkbox, **Download results automatically**. It is off by default, and Labmill remembers your choice. When it is on and a job you started finishes (completed, failed or ran out of time, but not when you cancel it), Labmill brings the job's results to your results folder by itself, in the folder named after the job, and checks every file against the cluster. You do not have to ask and you do not get an approval screen, because a download only brings files to your computer; nothing leaves it.

The chat tells you what came down and where, with an **Open folder** button, and the job shows as downloaded in the job list. It also works while the window is closed and Labmill runs in the tray. Only what the job produced comes down: files you uploaded, hidden or private-looking files (such as `.netrc`, `.env` or token files), environments and caches, and symbolic links are left out, and the chat says how many. Results above 2 GB or 2000 files, or more than your disk can hold (Labmill keeps 10 % free), are not downloaded automatically: the chat says why and you can ask Labmill to fetch them. If you are not signed in to the cluster, Labmill tells you once and downloads at its next check after you sign in; it never opens a sign-in window by itself. If the download fails, the chat says so in plain words and you can ask Labmill to fetch the results. A failed job stays marked as failed. A job is downloaded automatically only once, and only if it finishes while the checkbox is on. The same switch is on the Settings page.

## Your data and code folders

At the upper left of the box where you type is a **+** button. Click it (or press Enter or Space on it) to open a small menu:

- **Add data folder…** comes first. Many people only have data and want it analyzed: you don't need any code. Labmill can write the analysis code for you, and shows it to you before anything runs.
- **Add code folder…** is for when you already have scripts or a program.
- Below a line, up to five folders you used recently, marked **Data:** or **Code:**, so you can switch back quickly.
- **Remove data folder** and **Remove code folder** appear once those folders are set.

The two "Add" items open the usual folder window (Explorer on Windows, Finder on a Mac). Use the arrow keys to move in the menu and **Esc** to close it.

The chosen folders show as small labels just above your text, data first: `Data: ~/…/mri-2026` and `Code: ~/…/mri/code`. Point at one to see the full path; its **×** removes it.

You can also drag a folder from Explorer or Finder and drop it onto the chat box. Labmill then asks **Add as data folder** or **Add as code folder** (press Enter for data, or Esc to cancel). Drops anywhere else are ignored.

Only the folder's location is used; nothing is copied or uploaded when you choose or drop a folder. Labmill remembers your choice and shows it again the next time you start it. The terminal version of Labmill uses the same folders (see [your first job](first-job.md)).

Your next message tells Labmill about the chosen folders, and Labmill looks there first when it searches for your data, previews a file, makes the file list for an upload or chooses the software for your code. You still approve every upload as usual.

Some folders can't be chosen: a whole drive, a system folder (such as `C:\Windows`, `Program Files` or `/usr`), a folder that contains your home folder, hidden folders, and folders whose names suggest passwords or keys. Labmill says why just above the chat box.

## Opening your results

After Labmill has downloaded the results of a job, an **Open folder** button appears in the chat, and the job gets an **Open folder** button in the job list. It opens the results folder in Explorer, Finder or your Linux file manager.

## A terminal on the cluster

The **Terminal** button at the top right opens a terminal on a cluster's login node, inside Labmill, with no new password or approval while Labmill is signed in. Pick the cluster and press **New terminal** (with only one cluster, the button opens it at once). Each terminal is a tab; open as many as you like and close a tab with its **×**, which ends that session. **Back to chat** only hides the terminals: they keep running until you close their tabs. In **Settings > Clusters** every cluster also has a **Terminal** button.

In the jobs panel, **Terminal here** opens a terminal in the job's folder on the cluster. **Open in system terminal** starts your computer's own terminal program (Windows Terminal or the command prompt, Terminal.app, or your Linux terminal) on the cluster instead; it needs the Labmill command-line program (`labmill`) next to the desktop app or on your PATH, and is hidden when Labmill can't find it.

If Labmill isn't signed in to the cluster, the usual sign-in dialog appears, once. While the assistant is busy with a reply, a terminal can't ask you to sign in: it works only if Labmill is already signed in, and otherwise says to try again when the assistant has finished. Only one terminal connects at a time. Copy and paste work as in any terminal (Ctrl+Shift+C and Ctrl+Shift+V, or Cmd+C and Cmd+V on a Mac).

The terminal is yours: what you type and see there is not sent to the assistant, shown in the chat or saved by Labmill, and the assistant never uses it. Labmill only records in its log that a terminal was opened and closed on the cluster.

## Making the jobs panel wider or narrower

Between the chat and the jobs panel on the right there is a thin divider. Drag it with the mouse to make the panel wider or narrower, or click it and use the Left and Right arrow keys (Home and End jump to the narrowest and widest). Double-click it to go back to the normal width, 40% of the window. Neither side can become too small to read. Labmill remembers the width together with the window position and opens the window the same way next time. In a very narrow window the jobs panel sits below the chat and there is no divider.

## Closing the window

Labmill remembers the size and position of its window, and whether it was maximized, and opens it the same way next time. If that spot no longer fits your screens (a monitor was unplugged, for example), the window opens centered at its normal size.

The Labmill desktop app works like OneDrive or Teams: **closing its window doesn't quit it.** Labmill keeps running in the system tray, so your cluster login stays open and you don't have to type your password and approve the two-factor sign-in again when you come back. Jobs keep being watched while the window is hidden.

Click the window's close button (or press Alt+F4 on Windows, Cmd+W on a Mac). The window disappears, and Labmill keeps running:

- **Windows:** the Labmill icon is in the notification area at the right end of the taskbar. If you don't see it, click the small arrow (^) there; you can drag the icon onto the taskbar to keep it visible.
- **Linux:** the icon is in your panel's tray, if your desktop has one (KDE and most panels do; GNOME needs the AppIndicator extension). Without a tray, closing the window quits Labmill as before, because there would be no way to bring it back.
- **Mac:** there is no tray icon. Labmill behaves like any Mac app: it stays in the Dock when you close the window, a click on the Dock icon shows the window again, and Cmd+Q quits it.

On Windows and Linux, the first time you close the window, Labmill tells you once: "Labmill is still running in the tray to keep your cluster login. Right-click the tray icon and choose Quit to exit."

## Bringing the window back

- Click the Labmill tray icon (Windows), or right-click it and choose **Open labmill**.
- Or simply start Labmill again: instead of opening a second copy, the running Labmill shows its window.

## The tray menu

Right-click the tray icon:

| Item | What it does |
|---|---|
| **Open labmill** | Shows the window. |
| **Settings…** | Shows the window on the [Settings](settings.md) page. |
| Status line | For example "2 jobs running · logged in to bigcluster". It is for reading only. |
| **Start in the tray when I log in** | When ticked, Labmill starts hidden in the tray each time you log in to your computer. Off by default. |
| **Quit labmill** | Really quits Labmill. |

## Quitting

Right-click the tray icon and choose **Quit labmill** (on a Mac: the Labmill menu → Quit, or Cmd+Q). A reply that is still being written is stopped, and any question Labmill was waiting for is answered with "no". Your cluster login stays open for about 10 more minutes, in case you start Labmill again, and then closes by itself.

## Starting with your computer

**Start in the tray when I log in** (tray menu) is off unless you turn it on. When you turn it on, Labmill adds itself to your own startup list, for your account only and without administrator rights:

- **Windows:** an entry named "labmill" in Settings → Apps → Startup (it lives under `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run`).
- **Mac:** a login item file in `~/Library/LaunchAgents`.
- **Linux:** a `labmill.desktop` file in `~/.config/autostart`.

Turning the option off removes the Windows entry and switches the Mac and Linux files off.

With the option on, Labmill starts hidden in the tray when you log in; click the tray icon when you want the window. If the tray isn't available at that moment, Labmill shows its window instead. While the window is hidden, an approval, a login question or a choice from Labmill brings the window to the front, because nobody would see it otherwise; a job notice does not (see the tray's status line).

There is no start-in-tray option on a Mac, because a Mac has no tray icon.

## The to-do list

For a request with several steps (for example scan your code, choose the software, upload your data, submit, monitor, fetch the results), Labmill shows a short to-do list above the message box and keeps it up to date as it works. ☐ is a step still to do, ◐ is the step in progress (with what Labmill is doing right now), and ☑ is done. You can collapse or expand the list with its title button (keyboard: Tab to it, then Space or Enter). When every step is done, the list stays through your next message and then disappears; it also disappears when a request is canceled or fails. One-step questions don't get a list.
