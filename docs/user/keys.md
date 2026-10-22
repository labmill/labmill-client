# Keyboard reference

## In the chat

| Key | What it does |
|---|---|
| **Enter** | Send your message (while Labmill is answering: queue it) |
| **Alt+Enter** (or **Ctrl+J**) | New line in your message without sending |
| **Tab** | Show or hide the jobs panel |
| **PgUp / PgDn** | Scroll the conversation |
| **Ctrl+C** | Stop the current answer or action. Press it a second time to quit |
| **Ctrl+D** | Quit Labmill |

While Labmill is working on an answer, you can keep typing. Enter then queues your message, which is shown above the input and sent when the answer ends. Labmill holds one queued message, and a newer one replaces it. Ctrl+C stops the answer but keeps the queued message, which is then sent.

## In an approval box (upload, job script, cancel, download)

| Key | What it does |
|---|---|
| **←/→** (or **Tab**) | Choose **Approve** or **Decline**. Decline is selected at first |
| **Enter** | Confirm the selected button |
| **↑/↓**, **PgUp/PgDn**, **Home/End** | Scroll the details when they don't fit |
| **Esc** | Decline right away |
| **Ctrl+C** | Cancel the whole turn (declines the request) |
| **Ctrl+D** | Quit Labmill |

When you edit a job script, the cursor starts in the text. **Tab** moves to the buttons, and **Tab** again goes back to the text. **Esc** still declines.

## In a choice box

When Labmill offers you choices (for example "use the cluster's Python module" or "make your own environment"), pick one with the arrow keys and Enter, a number key, or a click. You don't have to type the answer.

| Key | What it does |
|---|---|
| **↑/↓** (or **Tab**) | Move between the options. The first option is selected at first |
| **Enter** | Pick the selected option |
| **1** to **8** | Pick that option directly (a box has up to 8 options) |
| **Esc** | Skip the question; Labmill then asks in the chat instead |

If none of the options fits, move to **Other** and type your own answer, then press **Enter**. Picking an option only answers the question: uploads, job submissions and downloads still ask for your approval separately.

## In a sign-in box

Type the answer and press **Enter**. For the "trust this server?" question, use the arrow keys to choose Yes or No (No is the default). **Esc** cancels the sign-in.

## The half-second pause

When an approval, choice or sign-in box opens, Labmill ignores every key except **Esc**, **Ctrl+C** and **Ctrl+D** for half a second. This is a safety measure: if you were typing or pressing Enter just as the box appeared, those keys can't accidentally approve or pick something you hadn't seen yet. If a key seems to do nothing right after a box opens, press it again.

If the box doesn't fit in the window, enlarge the window. Until then only Esc and the quit keys work.
