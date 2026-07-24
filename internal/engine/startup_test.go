package engine

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
)

func TestNavigateAndStartCaptureOrder(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lifecycle := newSessionLifecycle()
	e := &ChromiumEngine{
		ctx:       ctx,
		cancel:    cancel,
		backend:   backendBlock,
		cols:      80,
		rows:      24,
		cellW:     8,
		cellH:     16,
		rawQ:      newRawFrameQueue(1),
		acks:      newAckQueue(),
		text:      &textSync{},
		lifecycle: lifecycle,
	}
	e.sizedSurfaceW, e.sizedSurfaceH = e.surfaceSize(e.cols, e.rows)
	e.sizedScale = e.layoutScale()
	var order []string
	e.navigateOp = func(context.Context, string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		if e.rawQ == nil || e.acks == nil || e.text == nil || e.lifecycle != lifecycle {
			t.Fatal("navigation ran before frame plumbing was ready")
		}
		order = append(order, "navigate", "commit")
		lifecycle.recordCommit(&cdp.Frame{ID: "main", LoaderID: "loader"})
		return "main", "loader", false, nil
	}
	e.startScreencastOp = func(context.Context, int, int, screencastFmt) error {
		order = append(order, "capture")
		return nil
	}

	// The waits run under a navGuard-derived context, as startSession wires
	// them. Capture must still start: it takes the session ctx, because the
	// screencast transition validates session identity by Done-channel
	// equality and would silently skip a start on a derived context.
	waitCtx, stop := navGuard(ctx, make(chan struct{}), make(chan struct{}))
	defer stop()
	if err := e.navigateAndStartCapture(ctx, waitCtx, lifecycle, "https://example.test"); err != nil {
		t.Fatalf("navigate and start capture: %v", err)
	}
	if want := []string{"navigate", "commit", "capture"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("startup order = %v, want %v", order, want)
	}
}

func TestNavigateToCommitReturnsNavigationError(t *testing.T) {
	t.Parallel()
	want := errors.New("navigation failed")
	e := &ChromiumEngine{navigateOp: func(context.Context, string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		return "", "", false, want
	}}
	if err := e.navigateToCommit(context.Background(), newSessionLifecycle(), "https://example.test"); !errors.Is(err, want) {
		t.Fatalf("navigate error = %v, want %v", err, want)
	}
}

func TestNavigateToCommitCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := newSessionLifecycle()
	e := &ChromiumEngine{navigateOp: func(context.Context, string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		return "main", "loader", false, nil
	}}
	done := make(chan error, 1)
	go func() { done <- e.navigateToCommit(ctx, lifecycle, "https://example.test") }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("navigate cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("navigation commit wait ignored cancellation")
	}
}

// TestNavGuardAbortsCommitWaitOnCrash pins the rebuild-path deadlock fix: a
// navigation whose commit never arrives is released when the session crashes,
// so the supervisor goroutine (which runs startSession on the rebuild path)
// can never park forever and deadlock Close on supDone.
func TestNavGuardAbortsCommitWaitOnCrash(t *testing.T) {
	t.Parallel()
	crashed := make(chan struct{})
	closing := make(chan struct{})
	navCtx, stop := navGuard(context.Background(), crashed, closing)
	defer stop()
	lifecycle := newSessionLifecycle()
	e := &ChromiumEngine{navigateOp: func(context.Context, string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		return "main", "loader", false, nil // the matching commit never arrives
	}}
	done := make(chan error, 1)
	go func() { done <- e.navigateToCommit(navCtx, lifecycle, "https://hung.test") }()
	close(crashed)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("commit wait after crash = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("commit wait ignored the crash signal")
	}
}

// TestNavGuardAbortsCommitWaitOnClose pins the Close half of the guard: a
// Close during a non-committing navigation releases the wait instead of
// blocking behind it.
func TestNavGuardAbortsCommitWaitOnClose(t *testing.T) {
	t.Parallel()
	crashed := make(chan struct{})
	closing := make(chan struct{})
	navCtx, stop := navGuard(context.Background(), crashed, closing)
	defer stop()
	lifecycle := newSessionLifecycle()
	e := &ChromiumEngine{navigateOp: func(context.Context, string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		return "main", "loader", false, nil
	}}
	done := make(chan error, 1)
	go func() { done <- e.navigateToCommit(navCtx, lifecycle, "https://hung.test") }()
	close(closing)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("commit wait after close = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("commit wait ignored the closing signal")
	}
}

// TestNavGuardStopReleasesWatcher pins the guard release: stop is idempotent
// and ends the watcher goroutine, which cancels the derived context on exit
// so no context resources leak.
func TestNavGuardStopReleasesWatcher(t *testing.T) {
	t.Parallel()
	crashed := make(chan struct{})
	closing := make(chan struct{})
	navCtx, stop := navGuard(context.Background(), crashed, closing)
	stop()
	stop() // idempotent
	select {
	case <-navCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("watcher did not release the derived context after stop")
	}
}

func TestNavigateErrorDocumentStillStartsCapture(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lifecycle := newSessionLifecycle()
	e := &ChromiumEngine{
		ctx: ctx, cancel: cancel, backend: backendBlock, rawQ: newRawFrameQueue(1), lifecycle: lifecycle,
		applySizingOp: func(context.Context, browser.WindowID, int, int, float64) error { return nil },
	}
	e.navigateOp = func(context.Context, string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		lifecycle.recordCommit(&cdp.Frame{ID: "main", LoaderID: "error-loader", UnreachableURL: "https://bad.test"})
		return "main", "error-loader", false, nil
	}
	var starts atomic.Int64
	e.startScreencastOp = func(context.Context, int, int, screencastFmt) error {
		starts.Add(1)
		return nil
	}
	if err := e.navigateAndStartCapture(ctx, ctx, lifecycle, "https://bad.test"); err != nil {
		t.Fatalf("error document startup: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("capture starts = %d, want 1", got)
	}
}

func TestMainFrameFCPStampsOnlyLaterFrames(t *testing.T) {
	t.Parallel()
	lifecycle := newSessionLifecycle()
	e := &ChromiumEngine{lifecycle: lifecycle, acks: newAckQueue(), ingest: &ingestCounter{on: true, window: ingestWindow}}
	q := newRawFrameQueue(2)
	e.handleScreencastFrameLive(q, 1, &page.EventScreencastFrame{SessionID: 1})
	lifecycle.recordCommit(&cdp.Frame{ID: "main", LoaderID: "loader"})
	if lifecycle.recordFCP("subframe") {
		t.Fatal("subframe FCP opened the main-frame gate")
	}
	if !lifecycle.recordFCP("main") {
		t.Fatal("main-frame FCP did not open the gate")
	}
	e.handleScreencastFrameLive(q, 2, &page.EventScreencastFrame{SessionID: 2})
	first, second := <-q.items, <-q.items
	if first.afterFCP || !second.afterFCP {
		t.Fatalf("after-FCP stamps = [%v %v], want [false true]", first.afterFCP, second.afterFCP)
	}
	if got := e.ingest.summary().Frames; got != 1 {
		t.Fatalf("FCP-gated ingest frames = %d, want 1", got)
	}
}

// TestIsMainFrame asserts the within-document URL gate: no frame matches
// before the first commit, then only the committed main frame matches, so a
// subframe navigatedWithinDocument event never updates the display URL.
func TestIsMainFrame(t *testing.T) {
	t.Parallel()
	var nilLifecycle *sessionLifecycle
	if nilLifecycle.isMainFrame("main") {
		t.Fatal("nil lifecycle matched a frame")
	}
	lifecycle := newSessionLifecycle()
	if lifecycle.isMainFrame("main") {
		t.Fatal("pre-commit lifecycle matched a frame")
	}
	lifecycle.recordCommit(&cdp.Frame{ID: "main", LoaderID: "loader"})
	if !lifecycle.isMainFrame("main") {
		t.Fatal("committed main frame did not match")
	}
	if lifecycle.isMainFrame("subframe") {
		t.Fatal("subframe matched the main frame")
	}
}
