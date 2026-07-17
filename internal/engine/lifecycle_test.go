package engine

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
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

// TestSuperviseCloseWinsOverRebuild checks the shutdown race where Close fires
// together with a crash. The select picks a ready case at random, so it can wake
// on crashed while closing is also set; supervise must re-check closing and tear
// down rather than relaunch Chromium. The browser flag points at a real
// executable that no rebuild may launch: a spurious rebuild would call launch and
// set a fresh e.ctx, so a nil e.ctx after supervise returns proves close won.
func TestSuperviseCloseWinsOverRebuild(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")

	browserPath := filepath.Join(t.TempDir(), "fake-browser")
	if err := os.WriteFile(browserPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake browser: %v", err)
	}

	e := New(browserPath)
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	e.cancel = cancel
	e.slot = newFrameSlot()
	e.frames = make(chan render.Frame)
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})

	// Both wake conditions are ready before supervise runs: closing is set (Close
	// in flight) and crashed is closed (a concurrent crash). Close must win.
	crashed := make(chan struct{})
	close(crashed)
	e.closeOnce.Do(func() { close(e.closing) })

	go e.supervise(crashed)

	select {
	case <-e.supDone:
	case <-time.After(time.Second):
		t.Fatal("supervise blocked instead of tearing down on close")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("supervise did not tear down the session on close")
	}
	if e.ctx != nil {
		t.Fatal("supervise rebuilt a session during shutdown instead of closing")
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
		e.runFrameLoop(ctx, slot, frames, &textSync{}, nil, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runFrameLoop blocked after session teardown")
	}
}

// TestCaptureTeardownDropsPublishedSessionFrames proves recovery removes capture
// frames already buffered by the old session before a replacement starts publishing.
func TestCaptureTeardownDropsPublishedSessionFrames(t *testing.T) {
	e := New("")
	e.captureOn = true
	ctx, cancel := context.WithCancel(context.Background())
	slot := newFrameSlot()
	frames := make(chan render.Frame, frameChannelBuffer)
	oldImg := image.NewRGBA(image.Rect(0, 0, 1, 1))
	newImg := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e.ctx, e.cancel, e.slot, e.frames = ctx, cancel, slot, frames
	e.decode = func(string) (*image.RGBA, error) { return oldImg, nil }
	publisherDone := make(chan struct{})
	e.framePublisherDone = publisherDone
	go func() {
		defer close(publisherDone)
		e.runFrameLoop(ctx, slot, frames, &textSync{}, nil, nil)
	}()

	slot.put(rawFrame{data: "old"})
	select {
	case old := <-frames:
		if old.Img != oldImg {
			t.Fatal("capture publisher emitted the wrong old-session bitmap")
		}
		frames <- old
	case <-time.After(time.Second):
		t.Fatal("capture publisher did not emit the old-session frame")
	}

	teardownDone := make(chan struct{})
	go func() {
		e.teardownSession()
		close(teardownDone)
	}()
	select {
	case <-teardownDone:
	case <-time.After(time.Second):
		t.Fatal("capture teardown did not join its frame publisher")
	}

	frames <- render.Frame{Img: newImg}
	if got := <-frames; got.Img != newImg {
		t.Fatal("replacement received a buffered frame from the old capture session")
	}
}

// TestCaptureShutdownJoinsPublisherBeforeClosingFrames proves shutdown cannot close
// the engine-lifetime channel while a capture frame loop is still inside decode and
// may send. The frame channel must remain open until that publisher exits.
func TestCaptureShutdownJoinsPublisherBeforeClosingFrames(t *testing.T) {
	e := New("")
	e.captureOn = true
	ctx, cancel := context.WithCancel(context.Background())
	slot := newFrameSlot()
	frames := make(chan render.Frame, frameChannelBuffer)
	decodeStarted := make(chan struct{})
	releaseDecode := make(chan struct{})
	e.ctx, e.cancel, e.slot, e.frames = ctx, cancel, slot, frames
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})
	e.decode = func(string) (*image.RGBA, error) {
		close(decodeStarted)
		<-releaseDecode
		return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
	}
	publisherDone := make(chan struct{})
	e.framePublisherDone = publisherDone
	go func() {
		defer close(publisherDone)
		e.runFrameLoop(ctx, slot, frames, &textSync{}, nil, nil)
	}()

	slot.put(rawFrame{data: "blocked"})
	select {
	case <-decodeStarted:
	case <-time.After(time.Second):
		t.Fatal("capture frame loop did not enter decode")
	}
	close(e.closing)
	go e.supervise(make(chan struct{}))
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("capture shutdown did not cancel the session")
	}
	select {
	case _, ok := <-frames:
		if !ok {
			t.Fatal("frames closed before the capture publisher exited")
		}
		t.Fatal("capture publisher emitted while decode was blocked")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseDecode)
	select {
	case <-e.supDone:
	case <-time.After(time.Second):
		t.Fatal("capture shutdown did not finish after the publisher exited")
	}
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("frames remained open after capture shutdown")
		}
	default:
		t.Fatal("frames did not close after capture shutdown")
	}
}

// TestComposeFrameNilTextKeepsBitmap checks a frame composed with no published
// overlay (a nil textSync, as before the first snapshot lands) keeps the bitmap
// and emits a bitmap-only frame, without panicking.
func TestComposeFrameNilTextKeepsBitmap(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e := New("")

	frame := e.composeFrame(pendingFrame{img: img}, nil, nil)

	if frame.Img != img {
		t.Fatal("composeFrame dropped the bitmap")
	}
	if len(frame.Text) != 0 {
		t.Fatalf("composeFrame text = %v, want bitmap-only frame", frame.Text)
	}
}
