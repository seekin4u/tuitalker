package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

func main() {
	app := tview.NewApplication()

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

	// Input field
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

	help := tview.NewTextView().
		SetDynamicColors(true).
		SetText("[gray]Enter to synthesize & play • Esc or Ctrl+C to quit[-]")

	// Layout
	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(input, 3, 0, true).
		AddItem(status, 0, 1, false).
		AddItem(help, 1, 0, false)

	// Quit keys
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc || ev.Rune() == 3 {
			app.Stop()
			return nil
		}
		return ev
	})

	// Player check
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("afplay"); err != nil {
			appendStatus(app, status, "[yellow]Warning:[-] 'afplay' not found; playback may fail.")
		}
	case "linux":
		if _, err := exec.LookPath("mpg123"); err != nil {
			appendStatus(app, status, "[yellow]Warning:[-] 'mpg123' not found; playback may fail.")
		}
	}

	if err := app.SetRoot(layout, true).EnableMouse(false).Run(); err != nil {
		log.Fatal(err)
	}
}

func synthAndPlay(app *tview.Application, status *tview.TextView, text string) error {
	appendStatus(app, status, fmt.Sprintf("[cyan]Synthesize:[-] %q", text))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := texttospeech.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create TTS client: %w", err)
	}
	defer client.Close()

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
			AudioEncoding: texttospeechpb.AudioEncoding_MP3,
		},
	}

	resp, err := client.SynthesizeSpeech(ctx, req)
	if err != nil {
		return fmt.Errorf("synthesize: %w", err)
	}
	appendStatus(app, status, "[green]Synthesis complete.[-] Playing…")

	start := time.Now()
	if err := playMP3(resp.AudioContent); err != nil {
		_ = os.WriteFile("tts.mp3", resp.AudioContent, 0644)
		return fmt.Errorf("%w\n(saved fallback file: tts.mp3)", err)
	}
	appendStatus(app, status, fmt.Sprintf("[green]Done.[-] (%.1fs)", time.Since(start).Seconds()))
	return nil
}

func playMP3(mp3 []byte) error {
	// Always prefer mpg123 if available
	if _, err := exec.LookPath("mpg123"); err == nil {
		cmd := exec.Command("mpg123", "-")
		cmd.Stdin = bytes.NewReader(mp3)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("mpg123 failed: %v\n%s", err, string(out))
		}
		return nil
	}

	// Fallback: OS-specific players
	switch runtime.GOOS {
	case "darwin":
		// afplay needs a file, not stdin
		tmp, err := os.CreateTemp("", "tts-*.mp3")
		if err != nil {
			return fmt.Errorf("create temp file: %w", err)
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(mp3); err != nil {
			return fmt.Errorf("write temp mp3: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("close temp mp3: %w", err)
		}
		cmd := exec.Command("afplay", tmp.Name())
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("afplay failed: %v\n%s", err, string(out))
		}
		return nil

	case "linux":
		return fmt.Errorf("'mpg123' not found; please install it (e.g. apt install mpg123)")

	default:
		if err := os.WriteFile("tts.mp3", mp3, 0644); err != nil {
			return fmt.Errorf("unsupported OS %q and failed to write tts.mp3: %w", runtime.GOOS, err)
		}
		return fmt.Errorf("unsupported OS %q; wrote tts.mp3 for manual playback", runtime.GOOS)
	}
}

func appendStatus(app *tview.Application, tv *tview.TextView, line string) {
	app.QueueUpdateDraw(func() {
		fmt.Fprintln(tv, line)
		tv.ScrollToEnd()
	})
}
