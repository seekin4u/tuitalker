package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

func main() {
	app := tview.NewApplication()

	// ---- Status/log pane
	var status *tview.TextView
	status = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true).
		SetChangedFunc(func() {
			status.ScrollToEnd()
			app.Draw()
		})
	status.SetMaxLines(2000)

	// ---- Input field
	var input *tview.InputField
	input = tview.NewInputField().
		SetLabel("Text (ru): ").
		SetFieldWidth(0).
		SetDoneFunc(func(key tcell.Key) {
			if key == tcell.KeyEnter {
				text := input.GetText()
				if text == "" {
					appendStatus(app, status, "[yellow]Enter some text first[-]")
					return
				}
				go func(t string) {
					if err := synthAndPlay(app, status, t); err != nil {
						appendStatus(app, status, fmt.Sprintf("[red]Error:[-] %v", err))
					}
				}(text)
			}
		})

	// ---- Help/footer
	help := tview.NewTextView().
		SetDynamicColors(true).
		SetText("[gray]Enter to synthesize & play • Esc or Ctrl+C to quit[-]")

	// ---- Layout
	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(input, 3, 0, true).
		AddItem(status, 0, 1, false).
		AddItem(help, 1, 0, false)

	// ---- Global quit keys
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc || ev.Rune() == 3 { // Ctrl+C
			app.Stop()
			return nil
		}
		return ev
	})

	// ---- Run
	if err := app.SetRoot(layout, true).EnableMouse(false).Run(); err != nil {
		log.Fatal(err)
	}
}

func synthAndPlay(app *tview.Application, status *tview.TextView, text string) error {
	appendStatus(app, status, fmt.Sprintf("[cyan]Synthesize:[-] %q", text))

	// Keep these in sync with the WAV header we’ll write.
	const sampleRate = 24000
	const channels = 1
	const bitsPerSample = 16

	// Timeout so RPCs don't hang forever
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := texttospeech.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create TTS client: %w", err)
	}
	defer client.Close()

	// Request raw PCM (LINEAR16) so we can wrap a WAV and use native players.
	req := &texttospeechpb.SynthesizeSpeechRequest{
		Input: &texttospeechpb.SynthesisInput{
			InputSource: &texttospeechpb.SynthesisInput_Text{Text: text},
		},
		Voice: &texttospeechpb.VoiceSelectionParams{
			LanguageCode: "ru",
			SsmlGender:   texttospeechpb.SsmlVoiceGender_NEUTRAL,
			Name:         "ru-RU-Standard-A",
		},
		AudioConfig: &texttospeechpb.AudioConfig{
			AudioEncoding:   texttospeechpb.AudioEncoding_LINEAR16,
			SampleRateHertz: int32(sampleRate),
		},
	}

	resp, err := client.SynthesizeSpeech(ctx, req)
	if err != nil {
		return fmt.Errorf("synthesize: %w", err)
	}
	appendStatus(app, status, "[green]Synthesis complete.[-] Preparing WAV…")

	wavBytes, err := pcmToWAV(resp.AudioContent, sampleRate, channels, bitsPerSample)
	if err != nil {
		return fmt.Errorf("build wav: %w", err)
	}

	appendStatus(app, status, "[green]WAV ready.[-] Playing…")
	start := time.Now()
	if err := playWAV(wavBytes); err != nil {
		// Save fallback for manual playback / debugging
		_ = os.WriteFile("tts.wav", wavBytes, 0644)
		return fmt.Errorf("%w\n(saved fallback file: %s)", err, filepath.Join(".", "tts.wav"))
	}
	appendStatus(app, status, fmt.Sprintf("[green]Done.[-] (%.1fs)", time.Since(start).Seconds()))
	return nil
}

// pcmToWAV wraps LINEAR16 PCM into a RIFF/WAVE container.
// bitsPerSample must be 16 for this function.
func pcmToWAV(pcm []byte, sampleRate, channels, bitsPerSample int) ([]byte, error) {
	if bitsPerSample != 16 {
		return nil, fmt.Errorf("only 16-bit PCM supported, got %d", bitsPerSample)
	}
	dataLen := uint32(len(pcm))
	byteRate := uint32(sampleRate * channels * (bitsPerSample / 8))
	blockAlign := uint16(channels * (bitsPerSample / 8))
	riffSize := uint32(36) + dataLen

	var b bytes.Buffer
	// RIFF header
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, riffSize)
	b.WriteString("WAVE")
	// fmt chunk
	b.WriteString("fmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))            // PCM fmt chunk size
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))             // AudioFormat = 1 (PCM)
	_ = binary.Write(&b, binary.LittleEndian, uint16(channels))      // NumChannels
	_ = binary.Write(&b, binary.LittleEndian, uint32(sampleRate))    // SampleRate
	_ = binary.Write(&b, binary.LittleEndian, byteRate)              // ByteRate
	_ = binary.Write(&b, binary.LittleEndian, blockAlign)            // BlockAlign
	_ = binary.Write(&b, binary.LittleEndian, uint16(bitsPerSample)) // BitsPerSample
	// data chunk
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, dataLen)
	b.Write(pcm)

	return b.Bytes(), nil
}

// playWAV writes a temp .wav and uses a native player per OS.
// - macOS: afplay
// - Windows: PowerShell SoundPlayer (no GUI, synchronous)
// - Linux: aplay | paplay | ffplay (first found)
func playWAV(wav []byte) error {
	tmp, err := os.CreateTemp("", "tuitalker-*.wav")
	if err != nil {
		return fmt.Errorf("create temp wav: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(wav); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp wav: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp wav: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("afplay"); err != nil {
			return fmt.Errorf("'afplay' not found (cannot play WAV). Saved at: %s", tmpPath)
		}
		cmd := exec.Command("afplay", tmpPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("afplay failed: %v\n%s", err, string(out))
		}
		return nil

	case "windows":
		// Use SoundPlayer.PlaySync from System.Media (no window, blocks until finished)
		ps := fmt.Sprintf(`$p=New-Object System.Media.SoundPlayer -Arg '%s'; $p.PlaySync()`, tmpPath)
		cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("PowerShell SoundPlayer failed: %v\n%s", err, string(out))
		}
		return nil

	default: // linux, etc.
		player, args := linuxPlayer(tmpPath)
		if player == "" {
			return fmt.Errorf("no suitable player found (aplay/paplay/ffplay). Saved at: %s", tmpPath)
		}
		cmd := exec.Command(player, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s failed: %v\n%s", player, err, string(out))
		}
		return nil
	}
}

func linuxPlayer(path string) (string, []string) {
	if _, err := exec.LookPath("aplay"); err == nil {
		return "aplay", []string{path}
	}
	if _, err := exec.LookPath("paplay"); err == nil {
		return "paplay", []string{path}
	}
	if _, err := exec.LookPath("ffplay"); err == nil {
		return "ffplay", []string{"-nodisp", "-autoexit", path}
	}
	return "", nil
}

func appendStatus(app *tview.Application, tv *tview.TextView, line string) {
	app.QueueUpdateDraw(func() {
		fmt.Fprintln(tv, line)
		tv.ScrollToEnd()
	})
}
