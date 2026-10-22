# Voice: "Hey Labmill"

!!! warning "Planned, not yet available"
    Voice activation is a design for the desktop app on Windows and macOS. It isn't built yet. The text chat described in the rest of this guide is what exists today.

## The idea

You say **"Hey Labmill"** and the Labmill window comes to the front with the cursor in the chat box. Labmill then presses your computer's own dictation shortcut for you (Win+H on Windows, or the dictation shortcut you have set in System Settings on a Mac), so you can speak your request instead of typing it.

Dictation often mishears technical words, so Labmill will correct them ("S batch" becomes `sbatch`, folder and cluster names are matched to the real ones). It will show you the corrected text and wait for you to confirm before doing anything. If the request is still unclear, it asks. If dictation doesn't start or nothing is heard, Labmill retries once and then tells you what went wrong.

On a Mac, you would be asked once to allow the microphone, speech recognition and accessibility (needed to press the shortcut).

## Privacy

The listening for the phrase "Hey Labmill" is planned to stay on your device. Windows voice typing may use Microsoft's online speech service, and macOS Dictation runs on your device on Apple silicon for supported languages and may otherwise use Apple's service. The resulting text goes to Claude like any message you type. Every approval still applies.

Voice is not planned for the text-window version.
