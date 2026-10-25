# FAQ

**Which clusters does Labmill work with?**
Slurm clusters only.

**Do I need an account with Labmill?**
No. You use your own cluster account and your own Anthropic API key.

**Does Labmill cost money?**
[CHECK: price of the client and of the Anthropic API use. Write this when the owner decides.]

**Does it send my data to Claude?**
No. Claude sees file names, sizes, short previews and small samples. Files go to the cluster only after you approve. See [Privacy](privacy.md).

**Can I check the job script before it runs?**
Yes. Every script is shown in full. You can edit it before you approve it.

**Can I run a job without writing code?**
Yes. Add a data folder only. Labmill can write the analysis code, and you see it before anything runs.

**Can I close the window while a job runs?**
Yes. The job runs on the cluster. On Windows and Linux the desktop app keeps running in the tray, so your login stays open. See [The desktop app and the tray](desktop-app.md).

**Is anything left running on my computer?**
Only the desktop app in the tray (you quit it from the tray menu) and a small SSH helper that exits about 10 minutes after you stop.

**Where are my results?**
In `~/labmill-results/<job name>`.

**Is a file I downloaded damaged?**
Labmill checks each file with a SHA-256 checksum. A file that does not match gets the name ending `.mismatch`. See [Data transfer and checks](data-transfer.md).

**What are the "approve" switches?**
**Run commands on the cluster without asking** and **Approve everything automatically**. Both are off by default. See [Settings](settings.md).

**Does it work with Claude Code?**
The skills are in Claude Code format, so you can use them there too. See [Claude Code](claude-code.md).

**Is voice input available?**
No. It is planned. See [Voice](voice.md).

**How do I update?**
Download the newest files from the [releases page](https://github.com/labmill/labmill-client/releases). See [Install](install.md).
