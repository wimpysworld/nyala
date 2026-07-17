package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/page"
)

func newStreamControlTestEngine(t *testing.T) (*ChromiumEngine, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	e := &ChromiumEngine{
		ctx:               ctx,
		cancel:            cancel,
		rawQ:              newRawFrameQueue(1),
		backend:           "sixel",
		cols:              80,
		rows:              24,
		cellW:             8,
		cellH:             16,
		screencastFmt:     screencastFmt{format: page.ScreencastFormatPng},
		screencastStarted: true,
	}
	t.Cleanup(cancel)
	return e, ctx
}

// TestSixelFrameBurstAndIdleKeepPNG proves frame arrival rate no longer controls
// Sixel's format. A rapid burst and an idle period issue no stop or start and leave
// the active stream at PNG.
func TestSixelFrameBurstAndIdleKeepPNG(t *testing.T) {
	e, _ := newStreamControlTestEngine(t)
	stops, starts := 0, 0
	e.stopScreencastOp = func(context.Context) error {
		stops++
		return nil
	}
	e.startScreencastOp = func(context.Context, int, int, screencastFmt) error {
		starts++
		return nil
	}
	for sessionID := int64(1); sessionID <= 8; sessionID++ {
		e.receiveScreencastFrame(&page.EventScreencastFrame{SessionID: sessionID})
	}
	time.Sleep(25 * time.Millisecond)
	e.mu.Lock()
	format := e.screencastFmt.format
	e.mu.Unlock()
	if stops != 0 || starts != 0 || format != page.ScreencastFormatPng {
		t.Fatalf("burst and idle issued stops=%d starts=%d format=%q, want no controls and PNG", stops, starts, format)
	}
}

// TestBackendRestartsUseFixedFormats proves live backend switches restart with the
// destination policy: Block JPEG q90, then Kitty PNG and Sixel PNG.
func TestBackendRestartsUseFixedFormats(t *testing.T) {
	e, _ := newStreamControlTestEngine(t)
	e.stopScreencastOp = func(context.Context) error { return nil }
	e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error { return nil }
	var starts []screencastFmt
	e.startScreencastOp = func(_ context.Context, _, _ int, format screencastFmt) error {
		starts = append(starts, format)
		return nil
	}
	for _, backend := range []string{"block", "kitty", "sixel"} {
		e.SetBackend(backend)
	}
	want := []screencastFmt{
		{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
		{format: page.ScreencastFormatPng},
		{format: page.ScreencastFormatPng},
	}
	if !slices.Equal(starts, want) {
		t.Fatalf("backend restart formats = %v, want %v", starts, want)
	}
}

// TestInitialStartSerialisesWithBackendRestart holds the initial StartScreencast
// operation open while a backend switch begins. The switch cannot stop or restart
// until the initial start releases the shared control lock.
func TestInitialStartSerialisesWithBackendRestart(t *testing.T) {
	e, ctx := newStreamControlTestEngine(t)
	e.screencastFmt = screencastFmt{}
	e.screencastStarted = false
	var mu sync.Mutex
	var events []string
	startCount := 0
	firstStart := make(chan struct{})
	release := make(chan struct{})
	e.stopScreencastOp = func(context.Context) error {
		mu.Lock()
		events = append(events, "stop")
		mu.Unlock()
		return nil
	}
	e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error {
		mu.Lock()
		events = append(events, "size")
		mu.Unlock()
		return nil
	}
	e.startScreencastOp = func(_ context.Context, _, _ int, format screencastFmt) error {
		mu.Lock()
		events = append(events, "start:"+string(format.format))
		startCount++
		first := startCount == 1
		mu.Unlock()
		if first {
			close(firstStart)
			<-release
		}
		return nil
	}

	initialDone := make(chan error, 1)
	go func() { initialDone <- e.startInitialScreencast(ctx) }()
	select {
	case <-firstStart:
	case <-time.After(time.Second):
		t.Fatal("initial start did not reach the barrier")
	}
	backendAttempted := make(chan struct{})
	backendDone := make(chan struct{})
	go func() {
		close(backendAttempted)
		e.SetBackend("kitty")
		close(backendDone)
	}()
	<-backendAttempted
	select {
	case <-backendDone:
		t.Fatal("backend restart passed the blocked initial start")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-initialDone; err != nil {
		t.Fatalf("initial start: %v", err)
	}
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("backend restart did not finish after initial start")
	}

	mu.Lock()
	got := slices.Clone(events)
	mu.Unlock()
	want := []string{"size", "start:png", "stop", "size", "start:png"}
	if !slices.Equal(got, want) {
		t.Fatalf("stream control order = %v, want %v", got, want)
	}
}

// TestResizeBeforeInitialStartDefersRestart pauses the initial transition before it
// takes screencastCtl, then lets Resize win the lock after the frame sink is
// published. Resize must update the held geometry without a pre-start stop or start;
// the released initial transition applies the new size with one StartScreencast.
func TestResizeBeforeInitialStartDefersRestart(t *testing.T) {
	e, ctx := newStreamControlTestEngine(t)
	e.screencastStarted = false
	paused := make(chan struct{})
	release := make(chan struct{})
	releaseStart := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseStart)
	e.beforeInitialLock = func() {
		close(paused)
		<-release
	}
	stops, sizes, starts := 0, 0, 0
	var startW, startH int
	e.stopScreencastOp = func(context.Context) error {
		stops++
		return nil
	}
	e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error {
		sizes++
		return nil
	}
	e.startScreencastOp = func(_ context.Context, capW, capH int, _ screencastFmt) error {
		starts++
		startW, startH = capW, capH
		return nil
	}

	initialDone := make(chan error, 1)
	go func() { initialDone <- e.startInitialScreencast(ctx) }()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("initial start did not reach the pre-lock barrier")
	}
	e.Resize(96, 30)
	if stops != 0 || sizes != 0 || starts != 0 {
		t.Fatalf("pre-start resize issued stops=%d sizes=%d starts=%d", stops, sizes, starts)
	}
	releaseStart()
	if err := <-initialDone; err != nil {
		t.Fatalf("initial start: %v", err)
	}
	wantW, wantH := e.screencastCap(96, 30)
	if stops != 0 || sizes != 1 || starts != 1 {
		t.Fatalf("initial start issued stops=%d sizes=%d starts=%d", stops, sizes, starts)
	}
	if startW != wantW || startH != wantH {
		t.Fatalf("initial start cap = %dx%d, want %dx%d", startW, startH, wantW, wantH)
	}
}

// TestBackendBeforeRebuildInitialStartDefersRestart reproduces the same reverse
// ordering for a replacement session carrying the old session's format. A backend
// change before the replacement's first start must not stop that unstarted stream,
// and the single initial start must select the new backend's PNG policy.
func TestBackendBeforeRebuildInitialStartDefersRestart(t *testing.T) {
	e, ctx := newStreamControlTestEngine(t)
	e.backend = "block"
	e.screencastFmt = screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality}
	e.screencastStarted = false
	paused := make(chan struct{})
	release := make(chan struct{})
	releaseStart := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseStart)
	e.beforeInitialLock = func() {
		close(paused)
		<-release
	}
	stops, sizes, starts := 0, 0, 0
	var startedFormat screencastFmt
	e.stopScreencastOp = func(context.Context) error {
		stops++
		return nil
	}
	e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error {
		sizes++
		return nil
	}
	e.startScreencastOp = func(_ context.Context, _, _ int, format screencastFmt) error {
		starts++
		startedFormat = format
		return nil
	}

	initialDone := make(chan error, 1)
	go func() { initialDone <- e.startInitialScreencast(ctx) }()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("rebuild initial start did not reach the pre-lock barrier")
	}
	e.SetBackend("kitty")
	if stops != 0 || sizes != 0 || starts != 0 {
		t.Fatalf("pre-start backend change issued stops=%d sizes=%d starts=%d", stops, sizes, starts)
	}
	releaseStart()
	if err := <-initialDone; err != nil {
		t.Fatalf("rebuild initial start: %v", err)
	}
	if stops != 0 || sizes != 1 || starts != 1 {
		t.Fatalf("rebuild initial start issued stops=%d sizes=%d starts=%d", stops, sizes, starts)
	}
	if startedFormat.format != page.ScreencastFormatPng {
		t.Fatalf("rebuild initial format = %q, want PNG", startedFormat.format)
	}
	e.mu.Lock()
	backend, started := e.backend, e.screencastStarted
	e.mu.Unlock()
	if backend != "kitty" || !started {
		t.Fatalf("replacement stream = backend %q started=%v, want Kitty started", backend, started)
	}
}

// TestRestartOperationFailureCancelsSession checks every operation that can fail
// after a healthy stream enters a restart. The desired PNG format is never committed,
// and cancellation hands recovery to the existing supervisor lifecycle.
func TestRestartOperationFailureCancelsSession(t *testing.T) {
	failure := errors.New("injected stream-control failure")
	for _, tc := range []struct {
		name   string
		fail   string
		resize bool
	}{
		{name: "stop through resize", fail: "stop", resize: true},
		{name: "sizing through backend", fail: "size"},
		{name: "start through resize", fail: "start", resize: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ctx := newStreamControlTestEngine(t)
			e.screencastFmt = screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality}
			e.stopScreencastOp = func(context.Context) error {
				if tc.fail == "stop" {
					return failure
				}
				return nil
			}
			e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error {
				if tc.fail == "size" {
					return failure
				}
				return nil
			}
			e.startScreencastOp = func(context.Context, int, int, screencastFmt) error {
				if tc.fail == "start" {
					return failure
				}
				return nil
			}

			if tc.resize {
				e.Resize(81, 24)
			} else {
				e.SetBackend("kitty")
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("restart failure did not cancel the active session")
			}
			e.mu.Lock()
			got := e.screencastFmt.format
			e.mu.Unlock()
			if got == page.ScreencastFormatPng {
				t.Fatal("failed restart committed the target PNG format")
			}
		})
	}
}

// TestCloseDuringRestartFailureReturns blocks StartScreencast until Close cancels
// the session. Shutdown and the restart caller must both return.
func TestCloseDuringRestartFailureReturns(t *testing.T) {
	e, ctx := newStreamControlTestEngine(t)
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})
	e.stopScreencastOp = func(context.Context) error { return nil }
	e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error { return nil }
	startEntered := make(chan struct{})
	e.startScreencastOp = func(ctx context.Context, _, _ int, _ screencastFmt) error {
		close(startEntered)
		<-ctx.Done()
		return ctx.Err()
	}

	restartDone := make(chan struct{})
	go func() {
		e.Resize(81, 24)
		close(restartDone)
	}()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("restart did not reach the start barrier")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- e.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked during restart")
	}
	select {
	case <-restartDone:
	case <-time.After(time.Second):
		t.Fatal("restart stayed blocked after Close")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("session error = %v, want context.Canceled", ctx.Err())
	}
}
