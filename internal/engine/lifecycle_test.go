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

func TestCloseGracefullyClosesLiveSession(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	gracefulCalled := false
	rawCancelled := false
	engineUnlocked := false
	screencastUnlocked := false
	e.ctx = ctx
	e.closeBrowserOp = func(context.Context) error {
		engineUnlocked = e.mu.TryLock()
		if engineUnlocked {
			e.mu.Unlock()
		}
		screencastUnlocked = e.screencastCtl.TryLock()
		if screencastUnlocked {
			e.screencastCtl.Unlock()
		}
		gracefulCalled = true
		cancel()
		return nil
	}
	e.cancel = func() {
		rawCancelled = true
		cancel()
	}
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	e.supervised = true
	go e.supervise(make(chan struct{}))
	if err := e.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if !gracefulCalled {
		t.Fatal("Close did not request graceful browser shutdown")
	}
	if rawCancelled {
		t.Fatal("Close selected raw cancellation after a successful graceful close")
	}
	if !engineUnlocked || !screencastUnlocked {
		t.Fatal("graceful close ran while an engine lock was held")
	}
}

func TestTeardownSessionDeadlineFallsBackToRawCancellation(t *testing.T) {
	if gracefulCloseTimeout != 500*time.Millisecond {
		t.Fatalf("gracefulCloseTimeout = %v, want 500ms", gracefulCloseTimeout)
	}
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	closeStarted := make(chan struct{})
	rawCancelled := make(chan struct{})
	releaseClose := make(chan struct{})
	e.ctx = ctx
	e.closeTimeout = 20 * time.Millisecond
	e.closeBrowserOp = func(context.Context) error {
		close(closeStarted)
		<-releaseClose
		return errors.New("close remained blocked")
	}
	e.cancel = func() {
		cancel()
		close(rawCancelled)
		close(releaseClose)
	}
	done := make(chan struct{})
	go func() {
		e.teardownSession()
		close(done)
	}()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("graceful close did not start")
	}
	select {
	case <-rawCancelled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("deadline did not trigger raw cancellation")
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("teardown did not return after raw cancellation")
	}
}

func TestTeardownCancelledSessionSkipsGracefulClose(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gracefulCalled := false
	rawCancelled := false
	e.ctx = ctx
	e.closeBrowserOp = func(context.Context) error {
		gracefulCalled = true
		return nil
	}
	e.cancel = func() { rawCancelled = true }
	e.teardownSession()
	if gracefulCalled {
		t.Fatal("cancelled session attempted graceful browser shutdown")
	}
	if !rawCancelled {
		t.Fatal("cancelled session did not use raw cancellation")
	}
}

func TestRebuildContinuesAfterBoundedTeardown(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gracefulClose func(context.Context, context.CancelFunc) error
		wantFallback  bool
	}{
		{
			name: "graceful close",
			gracefulClose: func(_ context.Context, cancel context.CancelFunc) error {
				cancel()
				return nil
			},
		},
		{
			name: "deadline fallback",
			gracefulClose: func(ctx context.Context, _ context.CancelFunc) error {
				<-ctx.Done()
				return ctx.Err()
			},
			wantFallback: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New("")
			oldCtx, cancelOld := context.WithCancel(context.Background())
			replacementCtx, cancelReplacement := context.WithCancel(context.Background())
			t.Cleanup(cancelReplacement)
			fallback := make(chan struct{})
			e.ctx = oldCtx
			e.closeTimeout = 20 * time.Millisecond
			e.closeBrowserOp = func(ctx context.Context) error {
				return tc.gracefulClose(ctx, cancelOld)
			}
			e.cancel = func() {
				cancelOld()
				close(fallback)
			}

			replacementStarted := make(chan struct{})
			replacementCrashed := make(chan struct{})
			var replacementErr error
			e.startSessionOp = func() (chan struct{}, error) {
				defer close(replacementStarted)
				select {
				case <-oldCtx.Done():
				default:
					replacementErr = errors.New("replacement started before the old session was released")
					return nil, replacementErr
				}
				e.mu.Lock()
				oldSessionCleared := e.ctx == nil && e.closeBrowserOp == nil && e.cancel == nil
				e.mu.Unlock()
				if !oldSessionCleared {
					replacementErr = errors.New("replacement started while old session fields remained")
					return nil, replacementErr
				}
				if tc.wantFallback {
					select {
					case <-fallback:
					default:
						replacementErr = errors.New("replacement started before bounded fallback")
						return nil, replacementErr
					}
				} else {
					select {
					case <-fallback:
						replacementErr = errors.New("successful graceful close selected raw fallback")
						return nil, replacementErr
					default:
					}
				}
				e.mu.Lock()
				e.ctx = replacementCtx
				e.cancel = cancelReplacement
				e.mu.Unlock()
				return replacementCrashed, nil
			}

			rebuildDone := make(chan bool, 1)
			go func() {
				crashed, ok := e.rebuild()
				rebuildDone <- ok && crashed == replacementCrashed
			}()
			select {
			case <-replacementStarted:
			case <-time.After(time.Second):
				t.Fatal("rebuild did not continue to the replacement session")
			}
			if replacementErr != nil {
				t.Fatal(replacementErr)
			}
			select {
			case ok := <-rebuildDone:
				if !ok {
					t.Fatal("rebuild did not return the replacement session")
				}
			case <-time.After(time.Second):
				t.Fatal("rebuild did not return")
			}
			e.mu.Lock()
			gotCtx := e.ctx
			e.mu.Unlock()
			if gotCtx != replacementCtx {
				t.Fatal("rebuild did not install the replacement session")
			}
		})
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
