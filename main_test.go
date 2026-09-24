package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rivo/tview"
)

// Keep ffplay silent: SDL's dummy driver still paces playback in real time.
func TestMain(m *testing.M) {
	os.Setenv("SDL_AUDIODRIVER", "dummy")
	os.Exit(m.Run())
}

// Count only ffplay processes this test binary itself started. Matching
// machine-wide would also catch anything else on the box playing the same file.
func countFFplay(t *testing.T, _ string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid()), "ffplay").Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// Requirement 5: no overlapping audio, ever.
func TestPlayerNeverOverlaps(t *testing.T) {
	p := newPlayer()
	go p.run()
	defer p.Stop()

	var cur, max int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		p.Play(func(ctx context.Context) {
			defer wg.Done()
			n := atomic.AddInt32(&cur, 1)
			for {
				m := atomic.LoadInt32(&max)
				if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond) // deliberately ignores ctx
			atomic.AddInt32(&cur, -1)
		})
		time.Sleep(5 * time.Millisecond)
	}
	// Queued-but-dropped jobs never run, so wg would never reach zero; just settle.
	time.Sleep(400 * time.Millisecond)
	if got := atomic.LoadInt32(&max); got != 1 {
		t.Fatalf("max concurrent jobs = %d, want 1", got)
	}
}

// Requirement 5: a new request cancels the one in flight and then runs.
func TestPlayerInterruptsAndRestarts(t *testing.T) {
	p := newPlayer()
	go p.run()
	defer p.Stop()

	started := make(chan struct{})
	cancelled := make(chan struct{})
	p.Play(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first job never started")
	}

	ran := make(chan struct{})
	p.Play(func(ctx context.Context) { close(ran) })

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("first job was not cancelled by the second Play")
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("second job never ran")
	}
}

// Play is called from the tview event loop; it must never block there.
func TestPlayNeverBlocks(t *testing.T) {
	p := newPlayer()
	go p.run()
	defer p.Stop()

	p.Play(func(ctx context.Context) { <-ctx.Done() }) // occupy the worker
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			p.Play(func(ctx context.Context) { <-ctx.Done() })
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Play blocked the caller")
	}
}

func TestPlayAfterStopIsSafe(t *testing.T) {
	p := newPlayer()
	go p.run()
	p.Stop()
	p.Stop() // idempotent
	p.Play(func(ctx context.Context) { t.Error("job ran after Stop") })
	time.Sleep(100 * time.Millisecond)
}

// Requirement: quitting must not orphan ffplay.
func TestStopKillsRealPlayback(t *testing.T) {
	p := newPlayer()
	go p.run()

	p.Play(func(ctx context.Context) { _ = playFile(ctx, "mda-ebat.ogg") })
	time.Sleep(700 * time.Millisecond)
	if countFFplay(t, "mda-ebat") == 0 {
		t.Fatal("ffplay never started")
	}

	start := time.Now()
	p.Stop()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Stop took %v, want prompt return", d)
	}
	time.Sleep(300 * time.Millisecond)
	if n := countFFplay(t, "mda-ebat"); n != 0 {
		t.Fatalf("%d orphaned ffplay processes after Stop", n)
	}
}

// The headline requirement: mash Enter, hear one stream restart, never two.
func TestMashEnterNeverStacksProcesses(t *testing.T) {
	p := newPlayer()
	go p.run()
	defer p.Stop()

	stop := make(chan struct{})
	var maxSeen int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := int32(countFFplay(t, "mda-ebat")); n > atomic.LoadInt32(&maxSeen) {
				atomic.StoreInt32(&maxSeen, n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	for i := 0; i < 3; i++ { // three Enters inside one second
		p.Play(func(ctx context.Context) { _ = playFile(ctx, "mda-ebat.ogg") })
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(1 * time.Second)
	close(stop)
	wg.Wait()

	if n := atomic.LoadInt32(&maxSeen); n > 1 {
		t.Fatalf("saw %d concurrent ffplay processes, want at most 1", n)
	}
	if atomic.LoadInt32(&maxSeen) == 0 {
		t.Fatal("never observed ffplay running at all")
	}
}

func TestPlayFileSuccess(t *testing.T) {
	if err := playFile(context.Background(), "mda-ebat.ogg"); err != nil {
		t.Fatalf("playFile: %v", err)
	}
}

// An interrupt must be distinguishable from a real failure.
func TestPlayFileInterruptIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- playFile(ctx, "mda-ebat.ogg") }()
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("got %v, want errInterrupted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("playFile did not return promptly after cancel")
	}
	time.Sleep(300 * time.Millisecond)
	if n := countFFplay(t, "mda-ebat"); n != 0 {
		t.Fatalf("%d ffplay processes survived cancellation", n)
	}
}

// A genuine failure must still surface, with stderr attached.
func TestPlayFileRealErrorSurfaces(t *testing.T) {
	err := playFile(context.Background(), "definitely-not-here.ogg")
	if err == nil {
		t.Fatal("want an error for a missing file")
	}
	if errors.Is(err, errInterrupted) {
		t.Fatalf("missing file misreported as an interruption: %v", err)
	}
	if !strings.Contains(err.Error(), "ffplay") {
		t.Fatalf("error lost its context: %v", err)
	}
	t.Logf("surfaced: %v", err)
}

// ffplay exits 0 on an undecodable file too, so stderr must carry the verdict.
func TestPlayFileCorruptFileSurfaces(t *testing.T) {
	f, err := os.CreateTemp("", "bogus-*.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("this is definitely not audio")
	f.Close()

	err = playFile(context.Background(), f.Name())
	if err == nil {
		t.Fatal("want an error for an undecodable file")
	}
	if errors.Is(err, errInterrupted) {
		t.Fatalf("corrupt file misreported as an interruption: %v", err)
	}
	t.Logf("surfaced: %v", err)
}

// A clean play must not be mistaken for a failure by the stderr check.
func TestPlayFileSuccessHasNoFalsePositive(t *testing.T) {
	for i := 0; i < 2; i++ {
		if err := playFile(context.Background(), "mda-ebat.ogg"); err != nil {
			t.Fatalf("run %d: clean playback reported an error: %v", i, err)
		}
	}
}

func TestIsSoundKeyAndFindSound(t *testing.T) {
	for _, s := range []string{"0", "1", "9"} {
		if !isSoundKey(s) {
			t.Errorf("isSoundKey(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "10", "1.", "привет", "5 ", "a"} {
		if isSoundKey(s) {
			t.Errorf("isSoundKey(%q) = true, want false", s)
		}
	}
	if _, ok := findSound("1"); !ok {
		t.Error(`findSound("1") not bound`)
	}
	if _, ok := findSound("7"); ok {
		t.Error(`findSound("7") unexpectedly bound`)
	}
}

// --- Enter consumes the field; replay comes from remembered state ---

func speakReq(text string) request { return request{kind: reqSpeak, text: text} }

const helpMarker = "Enter replays:[-] "

// stripTags removes tview colour tags so assertions measure visible width.
func stripTags(s string) string {
	for {
		i := strings.Index(s, "[")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "]")
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

func TestRouteConsumesAndRemembersText(t *testing.T) {
	d := route("привет", request{})
	if d.play.kind != reqSpeak || d.play.text != "привет" {
		t.Fatalf("play = %+v, want reqSpeak \"привет\"", d.play)
	}
	if d.last != d.play {
		t.Fatalf("last = %+v, want it to equal play", d.last)
	}
	if d.log == "" {
		t.Error("want a log line")
	}
}

func TestRouteEmptyReplaysLast(t *testing.T) {
	prev := speakReq("привет")
	d := route("", prev)
	if d.play != prev {
		t.Fatalf("play = %+v, want %+v", d.play, prev)
	}
	if d.last != prev {
		t.Fatalf("last = %+v, want unchanged %+v", d.last, prev)
	}
	if !strings.Contains(d.log, "Replay") {
		t.Errorf("log = %q, want it to mention Replay", d.log)
	}
}

func TestRouteEmptyWithNothingRemembered(t *testing.T) {
	d := route("", request{})
	if d.play.kind != reqNone {
		t.Fatalf("play = %+v, want nothing dispatched", d.play)
	}
	if d.last.kind != reqNone {
		t.Fatalf("last = %+v, want still empty", d.last)
	}
	if !strings.Contains(d.log, "Nothing to replay") {
		t.Errorf("log = %q", d.log)
	}
}

func TestRouteWhitespaceIsEmpty(t *testing.T) {
	prev := speakReq("привет")
	for _, in := range []string{"", "   ", "\t", " \t "} {
		if got, want := route(in, prev), route("", prev); got != want {
			t.Errorf("route(%q) = %+v, want same as empty %+v", in, got, want)
		}
	}
}

func TestRouteBoundDigit(t *testing.T) {
	d := route("1", request{})
	if d.play.kind != reqSound {
		t.Fatalf("play = %+v, want reqSound", d.play)
	}
	if d.play.sound != sounds[0] {
		t.Fatalf("sound = %+v, want %+v", d.play.sound, sounds[0])
	}
	if d.last != d.play {
		t.Error("a played sound must become the replay target")
	}
}

// The one that matters: a warning must not destroy the replay target.
func TestRouteUnboundDigitDoesNotClobberLast(t *testing.T) {
	prev := speakReq("привет")
	d := route("5", prev)
	if d.play.kind != reqNone {
		t.Fatalf("play = %+v, want nothing dispatched", d.play)
	}
	if d.last != prev {
		t.Fatalf("last = %+v, want the previous target %+v preserved", d.last, prev)
	}
	if !strings.Contains(d.log, "No sound bound") {
		t.Errorf("log = %q", d.log)
	}
	// And the original target must still replay afterwards.
	if again := route("", d.last); again.play != prev {
		t.Fatalf("after the warning, replay gave %+v, want %+v", again.play, prev)
	}
}

func TestRouteReplayOfSoundStaysASound(t *testing.T) {
	prev := route("1", request{}).last
	d := route("", prev)
	if d.play.kind != reqSound {
		t.Fatalf("play.kind = %v, want reqSound (not the TTS path)", d.play.kind)
	}
	if d.play.sound.path != sounds[0].path {
		t.Errorf("path = %q, want %q", d.play.sound.path, sounds[0].path)
	}
}

func TestRouteEscapesUserText(t *testing.T) {
	d := route("[red]hi", request{})
	if !strings.Contains(d.log, "[red[]") {
		t.Errorf("log = %q, want the colour tag escaped", d.log)
	}
}

func TestRouteIsPure(t *testing.T) {
	prev := speakReq("привет")
	a := route("1", prev)
	b := route("1", prev)
	if a != b {
		t.Errorf("route is not deterministic: %+v vs %+v", a, b)
	}
	if prev != speakReq("привет") {
		t.Error("route mutated its argument")
	}
}

func TestHelpTextEmptyTarget(t *testing.T) {
	if got := helpText(request{}); strings.Contains(got, helpMarker) {
		t.Errorf("helpText = %q, want no replay-target fragment", got)
	}
}

// The target must come before the hints: the full line is wider than an
// 80-column terminal and the help view is one row, so the tail is clipped.
func TestHelpTextPutsTargetBeforeHints(t *testing.T) {
	got := helpText(speakReq("привет"))
	target, hints := strings.Index(got, helpMarker), strings.Index(got, "1-9: sound")
	if target < 0 || hints < 0 {
		t.Fatalf("helpText = %q, want both target and hints", got)
	}
	if target > hints {
		t.Errorf("target at %d comes after hints at %d; it would be clipped", target, hints)
	}
	if n := utf8.RuneCountInString(stripTags(got[:hints])); n > 60 {
		t.Errorf("target fragment is %d cols, too wide to survive an 80-col terminal", n)
	}
}

func TestHelpTextEscapesAndTruncates(t *testing.T) {
	if got := helpText(speakReq("[red]x")); !strings.Contains(got, "[red[]") {
		t.Errorf("helpText = %q, want the tag escaped", got)
	}

	long := strings.Repeat("я", 200)
	got := helpText(speakReq(long))
	if !utf8.ValidString(got) {
		t.Fatal("helpText produced invalid UTF-8 — truncated mid-rune")
	}
	if strings.Contains(got, "\uFFFD") {
		t.Fatal("helpText produced a replacement char")
	}
	i := strings.Index(got, helpMarker)
	if i < 0 {
		t.Fatalf("helpText = %q, want a %q fragment", got, helpMarker)
	}
	// The label sits between the marker and the hints, which start with [gray].
	rest := got[i+len(helpMarker):]
	label := strings.TrimSpace(stripTags(strings.Split(rest, "  [gray]")[0]))
	if n := utf8.RuneCountInString(label); n > helpLabelMax+1 {
		t.Errorf("label is %d runes, want <= %d", n, helpLabelMax+1)
	}
}

func TestTTSCacheKeyedByText(t *testing.T) {
	c := &ttsCache{}
	if _, ok := c.get("a"); ok {
		t.Error("empty cache reported a hit")
	}
	c.put("a", []byte("AUDIO"))
	if _, ok := c.get("b"); ok {
		t.Error("cache returned audio for the wrong text")
	}
	data, ok := c.get("a")
	if !ok || string(data) != "AUDIO" {
		t.Errorf("get(\"a\") = %q, %v", data, ok)
	}
	c.put("b", []byte("OTHER"))
	if _, ok := c.get("a"); ok {
		t.Error("stale entry survived replacement")
	}
}

func TestTTSCacheConcurrent(t *testing.T) {
	c := &ttsCache{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.put(strconv.Itoa(n), []byte{byte(n)})
				c.get(strconv.Itoa(n))
			}
		}(i)
	}
	wg.Wait()
}

// playBytes is the factored-out temp-file tail shared by fresh and cached TTS.
func TestPlayBytesPlaysRealAudio(t *testing.T) {
	data, err := os.ReadFile("mda-ebat.ogg")
	if err != nil {
		t.Fatal(err)
	}
	if err := playBytes(context.Background(), data); err != nil {
		t.Fatalf("playBytes: %v", err)
	}
}

// A cache hit must play without touching the network. No credentials exist in
// this environment, so a fall-through to synthesis would fail loudly.
func TestSpeakUsesCacheWithoutSynthesizing(t *testing.T) {
	data, err := os.ReadFile("mda-ebat.ogg")
	if err != nil {
		t.Fatal(err)
	}
	c := &ttsCache{}
	c.put("привет", data)

	status := tview.NewTextView()
	start := time.Now()
	if err := speak(context.Background(), status, c, "привет"); err != nil {
		t.Fatalf("speak on a cache hit: %v", err)
	}
	t.Logf("cached replay took %v", time.Since(start))

	if logged := status.GetText(true); !strings.Contains(logged, "Cached.") {
		t.Errorf("status log = %q, want it to report a cache hit", logged)
	}
}
