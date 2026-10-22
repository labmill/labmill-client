# Know-how updates

Labmill's knowledge about clusters (how to choose the software, how to fix common failures, what your institution's storage folders are) improves over time. When Labmill uses a relay service (a broker, see [Starting Labmill](first-run.md#before-you-start-your-anthropic-api-key)), you don't have to install a new version of the app to get it. Without a broker, Labmill uses the know-how that came with the app.

## How it works

- While Labmill is open, it quietly asks the broker whether newer cluster know-how exists for your institution. This runs in the background and never delays your chat.
- A newer set is downloaded and **checked** before it is kept: every file must match its published checksum. A set that fails the check is thrown away and Labmill keeps using what it has.
- A downloaded set is used **from the next start of labmill**, never in the middle of a conversation, so the advice in a chat stays consistent.
- When Labmill notices that newer know-how exists, it shows a one-line notice in the chat: "Updated cluster know-how is available; Labmill uses it from the next start."
- If you are offline, or the service has nothing newer, Labmill uses the know-how that came with the app. Nothing breaks.
- After you install a newer version of the app itself, the know-how that came with it is used until the next download.
- You only ever receive the know-how for your own institution, plus the general parts.

## Your own changes always win

If you keep your own modified copies of the skills in Labmill's skills folder (`~/.config/labmill/skills`), those files are used instead of the downloaded or built-in ones. Labmill tells you once per session: "Using your own modified skills from <folder>." Updates never overwrite your files.

If you also use the same skills in Claude Code, see [the Claude Code page](claude-code.md): those copies are separate and are updated by you.
