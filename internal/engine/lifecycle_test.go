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
	go func() { _ = e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked after failed Navigate")
	}
}

func TestCloseBeforeNavigatePreventsLateStartup(t *testing.T) {
	e := New("missing-browser")
	if err := e.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := e.Navigate("https://example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Navigate error = %v, want context.Canceled", err)
	}
}

func TestCloseDuringStartupReturns(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx, e.cancel = ctx, cancel
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { _ = e.Close(); close(done) }()
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

func TestFinishStartupAfterCloseCleansLifecycle(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx, e.cancel = ctx, cancel
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
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

func TestSuperviseWithNilContextReturns(t *testing.T) {
	e := New("")
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	go e.supervise(make(chan struct{}))
	select {
	case <-e.supDone:
	case <-time.After(time.Second):
		t.Fatal("supervise blocked with nil context")
	}
}

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
	e.ctx, e.cancel = ctx, cancel
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
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
		t.Fatal("supervise rebuilt a session during shutdown")
	}
}

func TestComposeFrameNilTextKeepsBitmap(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	frame := New("").composeFrame(pendingFrame{img: img}, nil, nil)
	if frame.Img != img {
		t.Fatal("composeFrame dropped the bitmap")
	}
	if len(frame.Text) != 0 {
		t.Fatalf("composeFrame text = %v, want bitmap-only frame", frame.Text)
	}
}
