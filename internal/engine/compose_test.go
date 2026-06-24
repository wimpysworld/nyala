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
// inside composeFrame fails fast and takes the error-hold branch without
// launching a browser. It avoids passing a nil context, which staticcheck flags.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestComposeFrameCopiesMetaUnderCapture asserts composeFrame copies the
// pendingFrame screencast metadata onto frame.Meta when capture is on, so the
// metadata reaches the bundle. A cancelled ctx makes the snapshot fail fast, so
// no browser is launched.
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
	frame := e.composeFrame(cancelledContext(), pending)
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

// TestComposeFrameGraphicsBackendSkipsSnapshot asserts composeFrame skips the DOM
// snapshot for the graphics backends (sixel, kitty) and runs it for block. The
// graphics backends draw the bitmap alone and ignore Frame.Text, so the snapshot
// over the full graphics viewport is wasted work that starves the first draw; the
// skip lets the frame flow. The recorder's dom_snapshot event is the seam: block
// records it (the snapshot ran, even though the cancelled ctx fails it fast),
// while sixel and kitty record none and emit a frame with nil Text. No browser is
// launched.
func TestComposeFrameGraphicsBackendSkipsSnapshot(t *testing.T) {
	cases := []struct {
		backend      string
		wantSnapshot bool
	}{
		{"block", true},
		{"sixel", false},
		{"kitty", false},
	}
	for _, c := range cases {
		t.Run(c.backend, func(t *testing.T) {
			rec := &fakeRecorder{}
			e := &ChromiumEngine{captureOn: true, backend: c.backend, recorder: rec}
			pending := pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 2, 2))}

			frame := e.composeFrame(cancelledContext(), pending)

			if got := rec.has(captureEventDOMSnapshot); got != c.wantSnapshot {
				t.Errorf("dom_snapshot recorded = %v, want %v for backend %q", got, c.wantSnapshot, c.backend)
			}
			if !c.wantSnapshot && frame.Text != nil {
				t.Errorf("graphics backend %q emitted Text = %v, want nil", c.backend, frame.Text)
			}
			if frame.Img == nil {
				t.Errorf("backend %q emitted a nil-image frame, want the bitmap", c.backend)
			}
		})
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
	frame := e.composeFrame(cancelledContext(), pending)
	if frame.Meta != nil {
		t.Errorf("frame.Meta = %+v off capture, want nil", frame.Meta)
	}
}
