package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

// errInterrupted marks playback that was deliberately cut short by a newer
// request. It is never something the user needs to see in the log.
var errInterrupted = errors.New("interrupted")

type sound struct{ key, name, path string }

// Sounds are bound to single digits. Add an entry here to bind another one.
var sounds = []sound{
	{key: "1", name: "mda ebat", path: "mda-ebat.ogg"},
}

func findSound(key string) (sound, bool) {
	for _, s := range sounds {
		if s.key == key {
			return s, true
		}
	}
	return sound{}, false
}

// isSoundKey reserves single digits for the soundboard. Type "5." or "пять" to
// have a digit spoken instead.
func isSoundKey(s string) bool {
	return len(s) == 1 && s[0] >= '0' && s[0] <= '9'
}

func boundKeys() string {
	keys := make([]string, 0, len(sounds))
	for _, s := range sounds {
		keys = append(keys, s.key)
	}
	return strings.Join(keys, ", ")
}

func main() {
	app := tview.NewApplication()
	pl := newPlayer()

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
		SetLabel("Text (ru) or 1-9: ").
		SetFieldWidth(0).
		SetDoneFunc(func(key tcell.Key) {
			if key != tcell.KeyEnter {
				return
			}
			// The text is deliberately left in place: tview fires this on every
			// Enter, so pressing it again replays the same input.
			text := strings.TrimSpace(input.GetText())
			if text == "" {
				appendStatus(status, "[yellow]Enter some text first[-]")
				return
			}

			if isSoundKey(text) {
				s, ok := findSound(text)
				if !ok {
					appendStatus(status, fmt.Sprintf("[yellow]No sound bound to %q[-] (bound: %s)", text, boundKeys()))
					return
				}
				pl.Play(func(ctx context.Context) {
					appendStatus(status, fmt.Sprintf("[cyan]Playing:[-] %s", s.name))
					report(status, playFile(ctx, s.path))
				})
				return
			}

			pl.Play(func(ctx context.Context) {
				report(status, synthAndPlay(ctx, status, text))
			})
		})

	help := tview.NewTextView().
		SetDynamicColors(true).
		SetText("[gray]Enter: speak • 1-9: sound • Enter again: replay (cuts current) • Esc or Ctrl+C to quit[-]")

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
	if path, err := exec.LookPath("ffplay"); err != nil {
		appendStatus(status, "[red]ffplay not found[-] — install it: brew install ffmpeg")
	} else {
		appendStatus(status, "[gray]ffplay: "+path+"[-]")
	}
	listSounds(status)

	go pl.run()

	err := app.SetRoot(layout, true).EnableMouse(false).Run()
	// Without this, quitting would orphan ffplay and keep the audio going.
	pl.Stop()
	if err != nil {
		log.Fatal(err)
	}
}

// listSounds prints the key mapping at startup and flags entries whose file is
// missing, so a wrong working directory is self-diagnosing.
func listSounds(status *tview.TextView) {
	if len(sounds) == 0 {
		appendStatus(status, "[gray]No sounds bound.[-]")
		return
	}
	appendStatus(status, "[white]Sounds:[-]")
	for _, s := range sounds {
		abs, err := filepath.Abs(s.path)
		if err != nil {
			abs = s.path
		}
		if _, err := os.Stat(abs); err != nil {
			appendStatus(status, fmt.Sprintf("  [red]%s  MISSING[-]  %s", s.key, abs))
			continue
		}
		appendStatus(status, fmt.Sprintf("  [green]%s[-]  %s  [gray]%s[-]", s.key, s.name, s.path))
	}
}

// report logs a playback error unless it was an interruption, which is expected
// every time Enter is pressed again and would otherwise bury the log in noise.
func report(status *tview.TextView, err error) {
	if err == nil || errors.Is(err, errInterrupted) {
		return
	}
	// Escape: the message carries ffplay's stderr and file paths, and a stray
	// "[" would otherwise be eaten as a colour tag.
	appendStatus(status, fmt.Sprintf("[red]Error:[-] %s", tview.Escape(err.Error())))
}

// player runs at most one playback job at a time. A new job cancels the one in
// flight; a job still queued is simply replaced, so mashing Enter restarts the
// audio instead of stacking it up.
type player struct {
	mu      sync.Mutex
	next    func(context.Context)
	cancel  context.CancelFunc
	stopped bool
	wake    chan struct{}
	done    chan struct{}
}

func newPlayer() *player {
	return &player{
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

// Play never blocks: it is called from the tview event loop, which is the only
// goroutine that can redraw the screen.
func (p *player) Play(job func(context.Context)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.next = job
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case p.wake <- struct{}{}:
	default:
		// A wake is already pending and will pick up the newer job.
	}
}

// run is the single worker. One worker means the previous job has returned
// before the next starts, so two ffplay processes never overlap.
func (p *player) run() {
	defer close(p.done)
	for range p.wake {
		p.mu.Lock()
		job := p.next
		p.next = nil
		if job == nil {
			p.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		p.mu.Unlock()

		job(ctx)

		cancel()
		p.mu.Lock()
		p.cancel = nil
		p.mu.Unlock()
	}
}

// Stop cancels whatever is playing and waits for the worker to exit.
func (p *player) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.next = nil
	if p.cancel != nil {
		p.cancel()
	}
	close(p.wake)
	p.mu.Unlock()
	<-p.done
}

func synthAndPlay(ctx context.Context, status *tview.TextView, text string) error {
	appendStatus(status, fmt.Sprintf("[cyan]Synthesize:[-] %s", tview.Escape(fmt.Sprintf("%q", text))))

	// The 30s budget covers synthesis only. Playback is bounded by the audio
	// itself, and a long utterance runs well past it.
	playCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := texttospeech.NewClient(ctx)
	if err != nil {
		if playCtx.Err() != nil {
			return errInterrupted
		}
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
		if playCtx.Err() != nil {
			return errInterrupted
		}
		return fmt.Errorf("synthesize: %w", err)
	}
	appendStatus(status, "[green]Synthesis complete.[-] Playing…")

	tmp, err := os.CreateTemp("", "tts-*.mp3")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(resp.AudioContent); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp mp3: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp mp3: %w", err)
	}

	start := time.Now()
	if err := playFile(playCtx, tmp.Name()); err != nil {
		if errors.Is(err, errInterrupted) {
			// Don't litter the repo with a fallback file on every Enter-mash.
			return err
		}
		_ = os.WriteFile("tts.mp3", resp.AudioContent, 0644)
		return fmt.Errorf("%w\n(saved fallback file: tts.mp3)", err)
	}
	appendStatus(status, fmt.Sprintf("[green]Done.[-] (%.1fs)", time.Since(start).Seconds()))
	return nil
}

// playFile plays any format ffmpeg understands.
//
// An exit code is never trusted as proof of playback here, because neither
// available player earns that trust: afplay exits 0 on Ogg Opus while emitting
// pure silence, and ffplay exits 0 when the file is missing or undecodable,
// reporting the reason only on stderr. So stderr is the success signal —
// -nostats keeps it empty on a clean play.
func playFile(ctx context.Context, path string) error {
	// Absolute so a path that happens to start with "-" can't be parsed as an
	// ffplay option, and so errors name the file unambiguously.
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	cmd := exec.CommandContext(ctx, "ffplay", "-nodisp", "-autoexit", "-nostats", "-loglevel", "error", path)
	// SIGINT rather than the default SIGKILL: a cleanly torn-down CoreAudio
	// client is less likely to wedge the BlackHole device on rapid restarts.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = time.Second

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Stdin and Stdout stay nil (/dev/null): the child must never share the
	// TUI's raw-mode stdin, and inherited stdout would scribble over the screen.

	err := cmd.Run()

	// This must come first. ffplay exits 123 on SIGINT, so Run returns an
	// *ExitError and the context.Canceled that os/exec would otherwise report
	// is discarded (os/exec/exec.go: "if err == nil && watch.err != nil").
	if ctx.Err() != nil {
		return errInterrupted
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("ffplay: %s", msg)
	}
	if err != nil {
		return fmt.Errorf("ffplay: %w", err)
	}
	return nil
}

// appendStatus writes straight into the TextView, which is mutex-guarded and
// safe from any goroutine. It must NOT use QueueUpdate: that blocks until the
// event loop runs the callback, and this is called from the event loop itself.
func appendStatus(tv *tview.TextView, line string) {
	fmt.Fprintln(tv, line)
}
