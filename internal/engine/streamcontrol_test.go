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
	"github.com/wimpysworld/nyala/internal/render"
)

func newStreamControlTestEngine(t *testing.T) (*ChromiumEngine, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	e := &ChromiumEngine{
		ctx:               ctx,
		cancel:            cancel,
		rawQ:              newRawFrameQueue(1),
		backend:           backendSixel,
		cols:              80,
		rows:              24,
		cellW:             8,
		cellH:             16,
		screencastFmt:     screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
		screencastStarted: true,
	}
	t.Cleanup(cancel)
	return e, ctx
}

// TestSixelFrameBurstAndIdleKeepJPEG proves frame arrival rate no longer controls
// Sixel's format. A rapid burst and an idle period issue no stop or start and leave
// the active stream at JPEG.
func TestSixelFrameBurstAndIdleKeepJPEG(t *testing.T) {
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
	if stops != 0 || starts != 0 || format != page.ScreencastFormatJpeg {
		t.Fatalf("burst and idle issued stops=%d starts=%d format=%q, want no controls and JPEG", stops, starts, format)
	}
}

// TestBackendRestartsUseFixedFormats proves live backend switches restart with the
// destination policy: Block JPEG q90, then Kitty PNG and Sixel JPEG q90.
func TestBackendRestartsUseFixedFormats(t *testing.T) {
	e, _ := newStreamControlTestEngine(t)
	e.stopScreencastOp = func(context.Context) error { return nil }
	e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error { return nil }
	var starts []screencastFmt
	e.startScreencastOp = func(_ context.Context, _, _ int, format screencastFmt) error {
		starts = append(starts, format)
		return nil
	}
	for _, backend := range []backend{backendBlock, backendKitty, backendSixel} {
		e.SetBackend(backend)
	}
	want := []screencastFmt{
		{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
		{format: page.ScreencastFormatPng},
		{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
	}
	if !slices.Equal(starts, want) {
		t.Fatalf("backend restart formats = %v, want %v", starts, want)
	}
}

// TestKittyDecodeBackendCycleUsesFixedDestinationFormat proves the transport
// mode update issues no stream control. The paired backend change performs one
// restart with its fixed destination format, without an intermediate restart.
func TestKittyDecodeBackendCycleUsesFixedDestinationFormat(t *testing.T) {
	for _, tc := range []struct {
		name        string
		backend     backend
		format      screencastFmt
		kittyDecode bool
		nextDecode  bool
		nextBackend backend
		want        screencastFmt
	}{
		{
			name:        "enter shm Kitty",
			backend:     backendSixel,
			format:      screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
			nextDecode:  true,
			nextBackend: backendKitty,
			want:        screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
		},
		{
			name:        "leave shm Kitty",
			backend:     backendKitty,
			format:      screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
			kittyDecode: true,
			nextBackend: backendSixel,
			want:        screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
		},
		{
			name:        "enter direct Kitty",
			backend:     backendBlock,
			format:      screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality},
			nextBackend: backendKitty,
			want:        screencastFmt{format: page.ScreencastFormatPng},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newStreamControlTestEngine(t)
			e.backend = tc.backend
			e.screencastFmt = tc.format
			e.kittyDecode = tc.kittyDecode
			stops, starts := 0, 0
			e.stopScreencastOp = func(context.Context) error {
				stops++
				return nil
			}
			e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error { return nil }
			var startedFormats []screencastFmt
			e.startScreencastOp = func(_ context.Context, _, _ int, format screencastFmt) error {
				starts++
				startedFormats = append(startedFormats, format)
				return nil
			}
			e.SetKittyDecode(tc.nextDecode)
			if stops != 0 || starts != 0 {
				t.Fatalf("mode update issued stops=%d starts=%d, want none", stops, starts)
			}
			e.SetBackend(tc.nextBackend)
			wantFormats := []screencastFmt{tc.want}
			if stops != 1 || starts != 1 || !slices.Equal(startedFormats, wantFormats) {
				t.Fatalf("backend change issued stops=%d starts=%v, want one stop and %v", stops, startedFormats, wantFormats)
			}
		})
	}
}

// TestKittyTransportPreStartStartsOnce pins startup format selection: resolved
// shm starts once at JPEG q90, while direct transport starts once at PNG.
func TestKittyTransportPreStartStartsOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kittyDecode bool
		want        screencastFmt
	}{
		{"shm", true, screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality}},
		{"direct", false, screencastFmt{format: page.ScreencastFormatPng}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ctx := newStreamControlTestEngine(t)
			e.screencastFmt = screencastFmt{}
			e.screencastStarted = false
			sink := e.rawQ
			e.rawQ = nil // Navigate has not published the frame sink yet
			stops := 0
			e.stopScreencastOp = func(context.Context) error {
				stops++
				return nil
			}
			e.applySizingOp = func(context.Context, browser.WindowID, int, int, float64) error { return nil }
			var starts []screencastFmt
			e.startScreencastOp = func(_ context.Context, _, _ int, format screencastFmt) error {
				starts = append(starts, format)
				return nil
			}

			if err := e.ConfigureCapture(backendKitty, 80, 24, 8, 16, tc.kittyDecode); err != nil {
				t.Fatal(err)
			}
			if stops != 0 || len(starts) != 0 {
				t.Fatalf("pre-start capture update issued stops=%d starts=%d, want none", stops, len(starts))
			}

			e.rawQ = sink
			if err := e.startInitialScreencast(ctx); err != nil {
				t.Fatalf("initial start: %v", err)
			}
			want := []screencastFmt{tc.want}
			if stops != 0 || !slices.Equal(starts, want) {
				t.Fatalf("initial start issued stops=%d starts=%v, want no stop and %v", stops, starts, want)
			}
		})
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
		e.SetBackend(backendKitty)
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
	want := []string{"size", "start:jpeg", "stop", "size", "start:png"}
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
	e.backend = backendBlock
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
	e.SetBackend(backendKitty)
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
	if backend != backendKitty || !started {
		t.Fatalf("replacement stream = backend %q started=%v, want Kitty started", backend, started)
	}
}

// TestRestartOperationFailureCancelsSession checks every operation that can fail
// after a healthy stream enters a restart. The restart's target format is never
// committed, and cancellation hands recovery to the existing supervisor lifecycle.
// Each case seeds a committed format that differs from the restart's target: the
// resize cases carry a stale PNG on the Sixel stream, whose target is JPEG q90,
// and the backend case carries Sixel's JPEG q90 into a switch to Kitty PNG.
func TestRestartOperationFailureCancelsSession(t *testing.T) {
	failure := errors.New("injected stream-control failure")
	for _, tc := range []struct {
		name   string
		fail   string
		resize bool
		seed   screencastFmt
		target page.ScreencastFormat
	}{
		{name: "stop through resize", fail: "stop", resize: true, seed: screencastFmt{format: page.ScreencastFormatPng}, target: page.ScreencastFormatJpeg},
		{name: "sizing through backend", fail: "size", seed: screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality}, target: page.ScreencastFormatPng},
		{name: "start through resize", fail: "start", resize: true, seed: screencastFmt{format: page.ScreencastFormatPng}, target: page.ScreencastFormatJpeg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ctx := newStreamControlTestEngine(t)
			e.screencastFmt = tc.seed
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
				e.SetBackend(backendKitty)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("restart failure did not cancel the active session")
			}
			e.mu.Lock()
			got := e.screencastFmt.format
			e.mu.Unlock()
			if got == tc.target {
				t.Fatalf("failed restart committed the target %s format", tc.target)
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

func TestConfigureCaptureRestartsWithCompleteSettings(t *testing.T) {
	e, _ := newStreamControlTestEngine(t)
	var steps []string
	e.stopScreencastOp = func(context.Context) error {
		steps = append(steps, "stop")
		return nil
	}
	e.applySizingOp = func(_ context.Context, _ browser.WindowID, w, h int, _ float64) error {
		steps = append(steps, "size")
		if w != 960 || h != 560 {
			t.Errorf("surface = %dx%d, want 960x560", w, h)
		}
		return nil
	}
	e.startScreencastOp = func(_ context.Context, w, h int, format screencastFmt) error {
		steps = append(steps, "start")
		if w != 960 || h != 560 {
			t.Errorf("capture = %dx%d, want 960x560", w, h)
		}
		want := screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality}
		if format != want || !e.kittyPlaneMode() {
			t.Errorf("start format = %v, Kitty plane mode = %v, want shm JPEG", format, e.kittyPlaneMode())
		}
		return nil
	}
	for range 2 {
		if err := e.ConfigureCapture(backendKitty, 96, 28, 10, 20, true); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"stop", "size", "start", "stop", "size", "start"}; !slices.Equal(steps, want) {
		t.Errorf("transition calls = %v, want %v", steps, want)
	}
}

func TestConfigureCaptureReportsOnlyFailedRecovery(t *testing.T) {
	restartErr := errors.New("injected stop error")
	rebuildErr := errors.New("injected rebuild error")
	for _, name := range []string{"successful recovery", "failed recovery"} {
		t.Run(name, func(t *testing.T) {
			e, _ := newStreamControlTestEngine(t)
			e.frames = make(chan render.Frame)
			e.closing, e.supDone = make(chan struct{}), make(chan struct{})
			e.navigated = true
			e.supervised = true
			e.stopScreencastOp = func(context.Context) error { return restartErr }
			rebuilt := make(chan struct{})
			e.startSessionOp = func() (chan struct{}, error) {
				defer close(rebuilt)
				if name == "failed recovery" {
					return nil, rebuildErr
				}
				e.mu.Lock()
				e.ctx, e.cancel = context.WithCancel(t.Context())
				e.mu.Unlock()
				return make(chan struct{}), nil
			}
			go e.supervise(make(chan struct{}))
			t.Cleanup(func() {
				if err := e.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := e.ConfigureCapture(backendKitty, 96, 28, 10, 20, true); err != nil {
				t.Fatalf("ConfigureCapture reported a recoverable error: %v", err)
			}
			select {
			case <-rebuilt:
			case <-time.After(time.Second):
				t.Fatal("supervisor did not attempt recovery")
			}
			if name == "successful recovery" {
				select {
				case <-e.Frames():
					t.Fatal("successful recovery closed Frames")
				default:
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				if err := e.StreamError(); err != nil {
					t.Errorf("StreamError after successful recovery and Close = %v, want nil", err)
				}
				return
			}
			select {
			case _, ok := <-e.Frames():
				if ok {
					t.Fatal("Frames sent a frame, want closed channel")
				}
			case <-time.After(time.Second):
				t.Fatal("failed recovery did not close Frames")
			}
			if err := e.StreamError(); !errors.Is(err, restartErr) || !errors.Is(err, rebuildErr) {
				t.Errorf("StreamError = %v, want both %v and %v", err, restartErr, rebuildErr)
			}
		})
	}
}

func TestConfigureCaptureDefersFailedRestartBeforeSupervisor(t *testing.T) {
	e, ctx := newStreamControlTestEngine(t)
	e.navigated = true // The initial screencast started, but Navigate has not called finishStartup.
	restartErr := errors.New("injected stop error")
	e.stopScreencastOp = func(context.Context) error { return restartErr }
	if err := e.ConfigureCapture(backendKitty, 96, 28, 10, 20, true); err != nil {
		t.Fatalf("ConfigureCapture reported a recoverable error: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("failed restart did not cancel the session for the supervisor to recover")
	}
	if !errors.Is(e.sessionStreamErr, restartErr) {
		t.Errorf("session stream error = %v, want %v", e.sessionStreamErr, restartErr)
	}
}
