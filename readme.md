# tuitalker

A TUI that speaks typed Russian text through Google Cloud TTS, or fires off
pre-recorded sound clips, into a virtual audio device so other apps (Discord)
pick it up as a microphone.

## Prerequisites

- `brew install ffmpeg` — playback uses `ffplay`. macOS has no Ogg Opus decoder,
  so `afplay` silently plays nothing for `.ogg` files.
- Google Cloud credentials in the environment (`GOOGLE_APPLICATION_CREDENTIALS`
  or `gcloud auth application-default login`).

## Audio routing

Set default MacOs soung system to "Input" and "Output" devices.
Spawn BlackHole16ch device.
Open "Audio Midi setup" and make sure your input device includes your actual input microfone, same as BackHole 16ch,
Same for Multi-Output device - BlackHole16ch and your output (Ugreen headphones in my case)
Set discord to use "default system input" and output devices. Then run this app.

Playback goes to the **system default output device** — the app does not select a
channel itself, which is why the Multi-Output device above has to be the default.

## Usage

| Input | What happens |
|---|---|
| Any text | Synthesized via Google Cloud TTS (`ru-RU-Standard-A`) and played |
| A single digit | Plays the bound local sound instead — no API call |
| Enter again | Replays the current input, cutting off whatever is still playing |
| Esc / Ctrl+C | Quit (stops playback too) |

Digits `0`-`9` are reserved for sounds, so they are never spoken. To have a digit
read aloud, type `5.` or `пять`.

The startup screen lists the current key mapping and flags any sound file it
cannot find.

## Adding a sound

Drop the file in the repo root and add one line to `sounds` in `main.go`:

```go
var sounds = []sound{
	{key: "1", name: "mda ebat", path: "mda-ebat.ogg"},
	{key: "2", name: "airhorn", path: "airhorn.ogg"},
}
```

Any format ffmpeg can decode works.
