package engine

import (
	"context"
	"errors"
	"image"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestCloseAfterNavigateFailureReturns checks that Close does not wait for a
// supervisor that never started. This is the shutdown path when startup fails
// after Navigate has prepared its lifecycle channels.
func TestCloseAfterNavigateFailureReturns(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")

	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(80, 24)
	if err := e.Navigate("https://example.invalid"); err == nil {
		t.Fatal("Navigate succeeded with no browser on PATH")
	}

	done := make(chan struct{})
	go func() {
		_ = e.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked after failed Navigate")
	}
}

// TestCloseBeforeNavigatePreventsLateStartup checks the viewer race where a quit
// key can close the engine before the Navigate goroutine has entered Navigate.
// The later Navigate call must observe the close signal and avoid launching a
// browser.
func TestCloseBeforeNavigatePreventsLateStartup(t *testing.T) {
	e := New("missing-browser")

	if err := e.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := e.Navigate("https://example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Navigate error = %v, want context.Canceled", err)
	}
}

// TestCloseDuringStartupReturns checks the Ctrl-C path while startup is in
// progress. Close must cancel the partial session and return even though the
// supervisor has not started.
func TestCloseDuringStartupReturns(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	e.cancel = cancel
	e.slot = newFrameSlot()
	e.frames = make(chan render.Frame)
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})

	done := make(chan struct{})
	go func() {
		_ = e.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked before the supervisor started")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Close did not cancel the startup context")
	}
}

// TestFinishStartupAfterCloseCleansLifecycle checks the race where Close wins
// while Navigate is still inside startSession. Since no supervisor will run, the
// startup finisher owns the final channel closes.
func TestFinishStartupAfterCloseCleansLifecycle(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	e.cancel = cancel
	e.frames = make(chan render.Frame)
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})
	e.closeOnce.Do(func() { close(e.closing) })

	if err := e.finishStartup(); !errors.Is(err, context.Canceled) {
		t.Fatalf("finishStartup error = %v, want context.Canceled", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("finishStartup did not cancel the startup context")
	}
	select {
	case _, ok := <-e.frames:
		if ok {
			t.Fatal("frames channel is open")
		}
	default:
		t.Fatal("frames channel was not closed")
	}
	select {
	case <-e.supDone:
	default:
		t.Fatal("supDone channel was not closed")
	}
}

// TestSuperviseWithNilContextReturns checks the defensive nil-context path used
// during failed or partial startup. It must exit, close supDone, and avoid a nil
// Context.Done panic.
func TestSuperviseWithNilContextReturns(t *testing.T) {
	e := New("")
	e.frames = make(chan render.Frame)
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})

	go e.supervise(make(chan struct{}))

	select {
	case <-e.supDone:
	case <-time.After(time.Second):
		t.Fatal("supervise blocked with nil context")
	}
}

// TestRunFrameLoopReturnsWhenSessionClearedBeforeStart checks the startup race
// where teardown clears ctx and slot before the just-started frame-loop goroutine
// snapshots them.
func TestRunFrameLoopReturnsWhenSessionClearedBeforeStart(t *testing.T) {
	e := New("")
	frames := make(chan render.Frame)
	ctx, cancel := context.WithCancel(context.Background())
	slot := newFrameSlot()
	e.ctx = ctx
	e.cancel = cancel
	e.slot = slot

	e.teardownSession()

	done := make(chan struct{})
	go func() {
		e.runFrameLoop(ctx, slot, frames, &textSync{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runFrameLoop blocked after session teardown")
	}
}

// TestComposeFrameNilTextKeepsBitmap checks a frame composed with no published
// overlay (a nil textSync, as before the first snapshot lands) keeps the bitmap
// and emits a bitmap-only frame, without panicking.
func TestComposeFrameNilTextKeepsBitmap(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e := New("")

	frame := e.composeFrame(pendingFrame{img: img}, nil)

	if frame.Img != img {
		t.Fatal("composeFrame dropped the bitmap")
	}
	if len(frame.Text) != 0 {
		t.Fatalf("composeFrame text = %v, want bitmap-only frame", frame.Text)
	}
}
