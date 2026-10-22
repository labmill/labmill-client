# Voice: "Hey Labmill"

Status: planned, no code yet. It needs real macOS and Windows hardware. It is GUI-only (Windows and macOS); the TUI is not part of it. CLAUDE.md keeps a short summary.


Saying **"Hey Labmill"** brings the Wails GUI chat box to the front, puts the cursor in it, and presses the OS dictation keystroke so the user can speak the request. The TUI is not part of this. The sequence is the same on both platforms:

1. A local wake word fires.
2. The listener pauses so it doesn't re-trigger or fight dictation for the mic.
3. Labmill shows the window and focuses the chat input. **Focus comes first**, because dictation types into whatever has focus.
4. Labmill presses the dictation keystroke.
5. Dictated text lands in the chat box.
6. The text is corrected (below), the user confirms, and the listener resumes.

## Windows

- **Wake word:** Windows speech recognition (`Windows.Media.SpeechRecognition`) with a one-phrase list constraint. It works offline, with no license and nothing to install. Go reaches it through a small WinRT/COM shim. Don't build on the legacy Windows Speech Recognition desktop app, which is deprecated in Windows 11.
- **Keystroke:** **Win+H**, injected with `SendInput`: `VK_LWIN` down, `H` down and up, `VK_LWIN` up.
- **Focus:** Windows restricts `SetForegroundWindow` for background processes and may only flash the taskbar. Use a known workaround (`AttachThreadInput` to the foreground thread, or a harmless injected key first) and verify it on Windows 11.
- **Did it open?** Check for the voice-typing window with UI Automation, or wait for text in the chat input.

## macOS

- **Wake word:** Apple's Speech framework (`SFSpeechRecognizer`) with `requiresOnDeviceRecognition = true`, streaming from `AVAudioEngine` and matching "hey labmill" in the partial results. Use `contextualStrings` to bias it toward "labmill". Wails on macOS already needs cgo, so add an Objective-C shim.
- **Permissions:** the app needs Microphone and Speech Recognition (TCC usage strings in the `.app` bundle's `Info.plist`) plus **Accessibility**, which posting keystrokes requires (`CGPreflightPostEventAccess` / `CGRequestPostEventAccess`). This is one more reason voice is GUI-only: a bare TUI binary can't get these prompts cleanly.
- **Keystroke:** press the **user's configured dictation shortcut** (System Settings → Keyboard → Dictation → Shortcut). Read it from user defaults; which defaults domain and key hold it still needs checking. Then post it with `CGEventPost`:
  - **Press Control twice:** two `kCGEventFlagsChanged` down/up pairs for `kVK_Control` (0x3B), within about 300 ms.
  - **Press Right Command twice:** the same with `kVK_RightCommand` (0x36). For **Left** or **Either** Command, use `kVK_Command` (0x37).
  - **Press Fn/🌐 twice** or **Microphone key:** these usually can't be synthesized, because Fn and the mic key are handled below the CGEvent layer. Either suggest the user switch the shortcut to Control or Right Command twice (ask; never change system settings silently), or use the fallback.
  - **Fallback when no keystroke works:** send `startDictation:` to the first responder inside Labmill's own process (`[NSApp sendAction:@selector(startDictation:) to:nil from:nil]`, the action behind Edit → Start Dictation…). It needs no Accessibility permission, but the Wails app must keep the standard Edit menu.
- **Did it open?** If Dictation is turned off in System Settings, say so and offer to open that settings pane. Otherwise wait for text in the chat input.

## Self-correction (both platforms)

- **Misheard words.** Dictation mangles domain terms ("S batch", "lab mill", folder names). Messages from voice are tagged, and Claude corrects them using a vocabulary of cluster names from `clusters.yaml`, local folder names and Slurm terms. The corrected text appears in the chat box for the user to confirm or edit before anything runs.
- **Dictation failures.** Dictation may not open, the mic may be muted or busy, or no text may arrive within a few seconds. Retry once, using the fallback path on macOS, and if that fails, tell the user exactly what went wrong.
- **Ambiguity.** If the corrected request is still unclear, ask a clarifying question instead of guessing.

**Privacy:** wake-word detection stays on the device. Windows voice typing may use Microsoft's online speech service. macOS Dictation runs on-device on Apple silicon for supported languages and otherwise may go to Apple. The resulting text goes to Claude like any typed message.

