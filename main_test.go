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

func TestSoundRouting(t *testing.T) {
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
