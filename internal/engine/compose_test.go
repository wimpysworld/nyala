package engine

import (
	"context"
	"image"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
)

// cancelledContext returns a context that is already cancelled, so the snapshot
// inside snapshotRuns fails fast and takes the error-hold branch without
// launching a browser. It avoids passing a nil context, which staticcheck flags.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestComposeFrameCopiesMetaUnderCapture asserts composeFrame copies the
// pendingFrame screencast metadata onto frame.Meta when capture is on, so the
// metadata reaches the bundle. A nil textSync gives a bitmap-only frame; the meta
// copy is independent of the overlay.
func TestComposeFrameCopiesMetaUnderCapture(t *testing.T) {
	e := &ChromiumEngine{captureOn: true, backend: "block"}
	ts := cdp.TimeSinceEpoch(time.Unix(0, int64(1500*float64(time.Millisecond))))
	pending := pendingFrame{
		img: image.NewRGBA(image.Rect(0, 0, 2, 2)),
		meta: &page.ScreencastFrameMetadata{
			OffsetTop:       5,
			PageScaleFactor: 2,
			DeviceWidth:     1024,
			DeviceHeight:    768,
			ScrollOffsetX:   33,
			ScrollOffsetY:   240,
			Timestamp:       &ts,
		},
	}
	frame := e.composeFrame(pending, nil)
	if frame.Meta == nil {
		t.Fatal("frame.Meta is nil under capture, want the screencast metadata copied")
	}
	if frame.Meta.ScrollOffsetY != 240 || frame.Meta.ScrollOffsetX != 33 {
		t.Errorf("frame.Meta scroll = (%v,%v), want (33,240)", frame.Meta.ScrollOffsetX, frame.Meta.ScrollOffsetY)
	}
	if frame.Meta.PageScaleFactor != 2 {
		t.Errorf("frame.Meta page scale = %v, want 2", frame.Meta.PageScaleFactor)
	}
	if frame.Meta.DeviceWidth != 1024 || frame.Meta.DeviceHeight != 768 {
		t.Errorf("frame.Meta device = (%v,%v), want (1024,768)", frame.Meta.DeviceWidth, frame.Meta.DeviceHeight)
	}
	if frame.Meta.OffsetTop != 5 || frame.Meta.Timestamp != 1.5 {
		t.Errorf("frame.Meta offsetTop=%v timestamp=%v, want 5 and 1.5", frame.Meta.OffsetTop, frame.Meta.Timestamp)
	}
}

// TestComposeFrameFoldsPublishedText asserts composeFrame folds the latest runs
// the snapshot goroutine published into Frame.Text, without running any snapshot
// itself. The snapshot is now decoupled onto its own goroutine; composeFrame only
// reads the published set.
func TestComposeFrameFoldsPublishedText(t *testing.T) {
	e := &ChromiumEngine{backend: "block"}
	ts := &textSync{}
	want := decodedRuns
	ts.store(want, 1.5, 0, 0)

	frame := e.composeFrame(pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 2, 2))}, ts)
	if len(frame.Text) != len(want) || (len(want) > 0 && frame.Text[0].Text != want[0].Text) {
		t.Errorf("frame.Text = %v, want the published runs %v", frame.Text, want)
	}
}

// TestSnapshotRunsRecordsForBlockOnly asserts snapshotRuns records a dom_snapshot
// event when it runs (the block path), and that the graphics backends report
// textOverlayBackend false so runTextSync skips the snapshot for them. The
// graphics backends draw the bitmap alone and ignore the overlay, so the snapshot
// over the full graphics viewport is wasted work that runTextSync skips. A
// cancelled ctx fails the snapshot fast, so no browser is launched.
func TestSnapshotRunsRecordsForBlockOnly(t *testing.T) {
	// Block: snapshotRuns runs and records dom_snapshot even though the cancelled
	// ctx fails it fast.
	rec := &fakeRecorder{}
	e := &ChromiumEngine{captureOn: true, backend: "block", recorder: rec}
	e.snapshotRuns(cancelledContext(), overlayState{})
	if !rec.has(captureEventDOMSnapshot) {
		t.Error("block snapshotRuns did not record dom_snapshot")
	}

	// Graphics: runTextSync gates on textOverlayBackend, which must be false, so the
	// snapshot is skipped for sixel and kitty.
	for _, backend := range []string{"sixel", "kitty"} {
		ge := &ChromiumEngine{captureOn: true, backend: backend}
		if ge.textOverlayBackend() {
			t.Errorf("backend %q reports textOverlayBackend true, want false (snapshot skipped)", backend)
		}
	}
}

// TestComposeFrameLeavesMetaNilOffCapture asserts composeFrame leaves frame.Meta
// nil when capture is off, so production stays zero-cost.
func TestComposeFrameLeavesMetaNilOffCapture(t *testing.T) {
	e := &ChromiumEngine{captureOn: false, backend: "block"}
	pending := pendingFrame{
		img:  image.NewRGBA(image.Rect(0, 0, 2, 2)),
		meta: &page.ScreencastFrameMetadata{ScrollOffsetY: 240},
	}
	frame := e.composeFrame(pending, nil)
	if frame.Meta != nil {
		t.Errorf("frame.Meta = %+v off capture, want nil", frame.Meta)
	}
}
