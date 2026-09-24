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

// Sounds are bound to one- or two-digit numbers. Add an entry here to bind another one.
var sounds = []sound{
	{key: "1", name: "mda ebat mp3", path: "mda-ebat.mp3"},
	{key: "2", name: "come on motherfuckers", path: "come-on-motherfuckers.ogg"},
	{key: "3", name: "da ty cho", path: "da-ty-cho.ogg"},
	{key: "4", name: "davaite dumat", path: "davaite-dumat.ogg"},
	{key: "5", name: "ebany rot etava kazino", path: "ebany-rot-etava-kazino.ogg"},
	{key: "6", name: "ei pidar", path: "ei_pidar.ogg"},
	{key: "7", name: "eto pizdec", path: "eto-pizdec.ogg"},
	{key: "8", name: "gnome wooooooh", path: "gnome-wooooooh.ogg"},
	{key: "9", name: "hey grandpa", path: "hey-grandpa.ogg"},
	{key: "10", name: "ignoriruju", path: "ignoriruju.ogg"},
	{key: "11", name: "ktota vstretil stipzbergena", path: "ktota-vstretil-stipzbergena.ogg"},
	{key: "12", name: "kusochek demona", path: "kusochek-demona.ogg"},
	{key: "13", name: "nahui poslan", path: "nahui-poslan.ogg"},
	{key: "14", name: "null otvetov", path: "null-otvetov.ogg"},
	{key: "15", name: "pasha hrr tfu", path: "pasha-hrr-tfu.ogg"},
	{key: "16", name: "pashol nahui", path: "pashol-nahui.ogg"},
	{key: "17", name: "razduplis", path: "razduplis.ogg"},
	{key: "18", name: "starina sjebi", path: "starina-sjebi.ogg"},
	{key: "19", name: "teper o samom hlavnom", path: "teper-o-samom-hlavnom.ogg"},
	{key: "20", name: "tyazhelo", path: "tyazhelo.ogg"},
	{key: "21", name: "vas zdes ne zhdut", path: "vas-zdes-ne-zhdut.ogg"},
	{key: "22", name: "warcraft rabota", path: "warcraft-rabota.ogg"},
	{key: "23", name: "zhyl i umer", path: "zhyl-i-umer.ogg"},
}

func findSound(key string) (sound, bool) {
	for _, s := range sounds {
		if s.key == key {
			return s, true
		}
	}
	return sound{}, false
}

// isSoundKey reserves one- and two-digit numbers for the soundboard. Type "5."
// or "пять" to have a number spoken instead.
func isSoundKey(s string) bool {
	if len(s) == 0 || len(s) > 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func boundKeys() string {
	keys := make([]string, 0, len(sounds))
	for _, s := range sounds {
		keys = append(keys, s.key)
	}
	return strings.Join(keys, ", ")
}

type reqKind int

const (
	reqNone reqKind = iota
	reqSound
	reqSpeak
)

// request is what Enter on an empty field replays. The zero value means nothing
// has been asked for yet. It holds no pointers or slices, so copying it is a
// full copy and callers never alias each other's state.
type request struct {
	kind  reqKind
	text  string // the typed text; the digit itself when kind == reqSound
	sound sound  // valid only when kind == reqSound
}

func (r request) label() string {
	if r.kind == reqSound {
		return r.sound.name
	}
	return r.text
}

// decision is the outcome of one Enter press.
type decision struct {
	play request // kind == reqNone means nothing is dispatched
	last request // the new replay target; the caller assigns it unconditionally
	log  string  // colour-tagged status line, or "" to log nothing
}

// route decides what a single Enter press does, given the raw field contents and
// the current replay target. It is pure: no I/O, no tview, nothing mutated.
//
// Two rules carry the design. Enter ALWAYS consumes the field, in every branch,
// so the field is never a leftover. And a request that does not dispatch never
// becomes the replay target — otherwise an unbound digit would both replay its
// own warning forever and destroy the previous good target.
func route(in string, last request) decision {
	text := strings.TrimSpace(in)

	if text == "" {
		if last.kind == reqNone {
			return decision{last: last, log: "[yellow]Nothing to replay yet — type something first[-]"}
		}
		return decision{
			play: last,
			last: last,
			log:  fmt.Sprintf("[gray]Replay:[-] %s", tview.Escape(last.label())),
		}
	}

	if isSoundKey(text) {
		s, ok := findSound(text)
		if !ok {
			return decision{
				last: last, // unchanged: a warning must not become the replay target
				log:  fmt.Sprintf("[yellow]No sound bound to %q[-] (bound: %s)", text, boundKeys()),
			}
		}
		r := request{kind: reqSound, text: text, sound: s}
		return decision{play: r, last: r, log: fmt.Sprintf("[cyan]Play:[-] %s", tview.Escape(s.name))}
	}

	r := request{kind: reqSpeak, text: text}
	return decision{
		play: r,
		last: r,
		// Imperative wording on purpose: this echoes the request, and says
		// nothing about whether the audio was actually reached.
		log: fmt.Sprintf("[cyan]Speak:[-] %s", tview.Escape(fmt.Sprintf("%q", text))),
	}
}

const helpBase = "[gray]number: sound • type + Enter: speak • Enter alone: replay • Esc or Ctrl+C: quit[-]"

// helpLabelMax is how much of the replay target the help bar shows, in runes.
const helpLabelMax = 40

// helpText renders what Enter would currently replay, plus the key hints. The
// emptied input field no longer shows the target, and the newest log line is
// often a warning rather than the target, so this is the only honest indicator.
func helpText(last request) string {
	if last.kind == reqNone {
		return helpBase
	}
	label := last.label()
	// Truncate by runes: the text is usually Cyrillic, and byte-slicing would
	// split a 2-byte rune into replacement characters.
	if r := []rune(label); len(r) > helpLabelMax {
		label = string(r[:helpLabelMax]) + "…"
	}
	// Target first, hints second. The full line is wider than an 80-column
	// terminal and the help view is one row tall, so whatever sits at the end
	// is silently clipped. The static hints can afford to vanish; the replay
	// target is the only thing on screen that says what Enter will do.
	// Escape AFTER truncating, or the cut can land inside an inserted escape.
	return fmt.Sprintf("[white]Enter replays:[-] %s  %s", tview.Escape(label), helpBase)
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

	help := tview.NewTextView().SetDynamicColors(true)
	cache := &ttsCache{}

	// last is what Enter on an empty field replays. It is read and written ONLY
	// by the done func below, which tview runs on the event-loop goroutine, so
	// it needs no mutex. Nothing inside a pl.Play job may touch it — that would
	// be a silent data race with no compiler help.
	var last request

	// Input field
	var input *tview.InputField
	input = tview.NewInputField().
		SetLabel("Text (ru) or number: ").
		SetFieldWidth(0).
		SetDoneFunc(func(key tcell.Key) {
			// tview fires this for Tab and Backtab too; only Enter consumes.
			if key != tcell.KeyEnter {
				return
			}

			d := route(input.GetText(), last)

			// Consume the field: it was the request, and the request is spent.
			// Safe from in here only because this InputField has no changed or
			// autocomplete handler -- adding one would make SetText re-enter
			// user code while tview holds autocompleteListMutex.
			input.SetText("")
			last = d.last
			help.SetText(helpText(last))

			// Log before dispatching, so the request is recorded by the event
			// loop and survives even when the next Enter replaces the job
			// before it ever runs.
			if d.log != "" {
				appendStatus(status, d.log)
			}

			// Capture by value: a job must never read tview state.
			switch d.play.kind {
			case reqSound:
				s := d.play.sound
				pl.Play(func(ctx context.Context) {
					report(status, playFile(ctx, s.path))
				})
			case reqSpeak:
				text := d.play.text
				pl.Play(func(ctx context.Context) {
					report(status, speak(ctx, status, cache, text))
				})
			}
		})

	help.SetText(helpText(last))

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

// ttsCache holds the audio of the most recently synthesized utterance so that
// replaying it costs neither an API call nor a second of latency.
//
// Today both put and get run on the single player worker, so the mutex is
// belt-and-braces. It stays because that invariant lives nowhere the compiler
// can see it, and a second worker would turn this into a real race.
type ttsCache struct {
	mu   sync.Mutex
	text string
	data []byte // immutable once stored; readers share the backing array
}

func (c *ttsCache) put(text string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text, c.data = text, data
}

// get is keyed by text on purpose. "Replay whatever is cached" would happily
// play the previous utterance when synthesis of the current one was cancelled
// before it ever stored anything.
func (c *ttsCache) get(text string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.text != text || c.data == nil {
		return nil, false
	}
	return c.data, true
}

// speak plays an utterance, synthesizing it only if it is not already cached.
func speak(ctx context.Context, status *tview.TextView, cache *ttsCache, text string) error {
	if data, ok := cache.get(text); ok {
		appendStatus(status, "[gray]Cached.[-] Playing…")
		return playBytes(ctx, data)
	}
	return synthAndPlay(ctx, status, cache, text)
}

func synthAndPlay(ctx context.Context, status *tview.TextView, cache *ttsCache, text string) error {
	// The request itself was already logged by route, on the event loop.
	appendStatus(status, "[gray]Synthesizing…[-]")

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

	// Cache before playing, not after: playback is routinely cut short by the
	// next Enter, and that is exactly the case the cache exists to make free.
	cache.put(text, resp.AudioContent)

	start := time.Now()
	if err := playBytes(playCtx, resp.AudioContent); err != nil {
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

// playBytes plays audio held in memory by handing it to playFile through a temp
// file. Caching the path instead would mean unlinking on replacement, cleaning
// up after Stop, and leaking on SIGKILL; rewriting a few hundred KB is free next
// to spawning ffplay.
func playBytes(ctx context.Context, data []byte) error {
	tmp, err := os.CreateTemp("", "tts-*.mp3")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp mp3: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp mp3: %w", err)
	}
	return playFile(ctx, tmp.Name())
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
