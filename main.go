package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

func main() {
	app := tview.NewApplication()

	// A small status/log pane
	status := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetChangedFunc(func() {
			// ensure UI redraws when we append to the status view
			app.Draw()
		})

	// Single input field
	var input *tview.InputField

	input = tview.NewInputField().
		SetLabel("Text (ru): ").
		SetFieldWidth(0).
		SetDoneFunc(func(key tcell.Key) {
			if key == tcell.KeyEnter {
				text := input.GetText() // now accessible
				if text == "" {
					appendStatus(status, "[yellow]Enter some text first[-]")
					return
				}
				// Kick off TTS in background so UI stays responsive
				go func(t string) {
					if err := synthAndPlay(app, status, t); err != nil {
						appendStatus(status, fmt.Sprintf("[red]Error:[-] %v", err))
					}
				}(text)
			}
		})

	// Basic key help + quit hint
	help := tview.NewTextView().
		SetDynamicColors(true).
		SetText("[gray]Press Enter to synthesize & play. Press Esc or Ctrl+C to quit.[-]")

	// Layout: Input at top, status log fills the rest, help at bottom
	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(input, 3, 0, true).
		AddItem(status, 0, 1, false).
		AddItem(help, 1, 0, false)

	// Quit on Esc
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			app.Stop()
			return nil
		}
		// Also allow Ctrl+C
		if ev.Rune() == 3 {
			app.Stop()
			return nil
		}
		return ev
	})

	// Quick preflight for mpg123 to give a clearer message if missing
	if _, err := exec.LookPath("mpg123"); err != nil {
		appendStatus(status, "[yellow]Warning:[-] mpg123 not found in PATH; audio playback will fail.")
	}

	// Start the UI
	if err := app.SetRoot(layout, true).EnableMouse(false).Run(); err != nil {
		log.Fatal(err)
	}
}

// synthAndPlay creates a short-lived TTS client, synthesizes, and plays via mpg123.
func synthAndPlay(app *tview.Application, status *tview.TextView, text string) error {
	appendStatus(status, fmt.Sprintf("[cyan]Synthesize:[-] %q", text))

	// Context with timeout to avoid hanging calls
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
	appendStatus(status, "[green]Synthesis complete.[-] Playing…")

	// Play audio via mpg123, streaming bytes over stdin
	cmd := exec.Command("mpg123", "-") // "-" tells mpg123 to read from stdin
	cmd.Stdin = bytes.NewReader(resp.AudioContent)

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("playback (mpg123): %w", err)
	}
	appendStatus(status, fmt.Sprintf("[green]Done.[-] (%.1fs)", time.Since(start).Seconds()))
	return nil
}

func appendStatus(tv *tview.TextView, line string) {
	fmt.Fprintf(tv, "%s\n", line)
}
