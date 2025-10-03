Set default MacOs soung system to "Input" and "Output" devices.
Spawn BlackHole16ch device.
Open "Audio Midi setup" and make sure your input device includes your actual input microfone, same as BackHole 16ch,
Same for Multi-Output device - BlackHole16ch and your output (Ugreen headphones in my case)
Set discord to use "default system input" and output devices. Then run this app - it will choose Blackhole's channel to output the TTS to.

Build for Macos: go build .
Build for Windows: GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o tuitalker.exe

---

Windows usage:
First of all, set env vars for google auth in PowerShell:
`$env:GOOGLE_APPLICATION_CREDENTIALS = "C:\Users\Anchous\Documents\google-auth.json"`
Then run the talker: `.\tuitalker.exe`

Regarding the sound setup:
There are two screenshots.
Install VB-CABLE, find virtual Output Cable in Input Devices in sound manager, toggle "listen from this device", and in propdown choose one that you use as output (Focusrite has my headphones connected so it is my output).
Then set this input called "Output Cable" as a default one.

In Output section just make one virtual Input Cable as a default.

How this probably works: this talker gets a wav or mp3 file from GCP, and outputs it to available OUTPUT device, by default its just your headphones.

Windows (and not only windows') audio separates render and capture, and OS doesn’t allow an arbitrary process to present it's render stream, another words i cannot output a sound "as if it was coming from a microphone", and here i need kernel virtual audio device, which VB-Cable (free) or Virtual Audio Cable (non-free) is.

VB-Cable on windows does not allow to make "2-in-1 output device" that will combine my microphone and output device, so i will need to eigher switch to my microphone OR to this playback virtual cable. It is possible on MacOs tho with BlackHole16p.