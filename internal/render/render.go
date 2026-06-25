// Package render defines the frame and renderer seam for Nyala.
//
// One Frame and one Renderer sit at the boundary between the frame source and
// the terminal backends. Everything downstream swaps behind this seam.
package render

import (
	"image"
	"image/color"
	"io"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

// CellWidth measures the display width of one grapheme cluster, clamped to 1 or
// 2. This is the shared Stage 2 width rule: a cluster of measured width 2 or
// more claims two cells, everything else (including a width-0 standalone
// combining mark) claims one. The clip budget and the cell binning both use it
// so a width-2 cluster is never half-emitted.
func CellWidth(grapheme string) int {
	if runewidth.StringWidth(grapheme) >= 2 {
		return 2
	}
	return 1
}

// ClipToCells trims text to at most maxCells display cells, measuring each
// grapheme cluster with CellWidth. A width-2 cluster that would exceed the
// budget is dropped whole, never half-emitted. A non-positive maxCells yields
// the empty string.
func ClipToCells(text string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		cluster := gr.Str()
		w := CellWidth(cluster)
		if used+w > maxCells {
			break
		}
		b.WriteString(cluster)
		used += w
	}
	return b.String()
}

// GlyphRun is a run of one or more graphemes at a cell position, drawn in a
// single foreground colour. The text overlay path is not wired yet, so no
// renderer reads it.
type GlyphRun struct {
	Col, Row int        // cell position
	Text     string     // one or more graphemes
	FG       color.RGBA // foreground colour
	X        float64    // box left, CSS pixels, on-screen (scroll subtracted)
	Y        float64    // box top, CSS pixels, on-screen (scroll subtracted)
	W        float64    // box width, CSS pixels
	H        float64    // box height, CSS pixels
}

// FrameStats holds the engine-phase timings and sizes captured while building
// one Frame. The engine fills it only when capture is enabled; it is nil on the
// production path. Timings are wall-clock durations in milliseconds and are
// never negative. Viewer draw-phase timing lives on a separate viewer-owned
// struct, not here.
type FrameStats struct {
	// ScreencastPayloadBytes is the byte size of the screencast frame payload
	// the engine decoded for this Frame.
	ScreencastPayloadBytes int64
	// ScreencastDecodeMS is the time spent decoding the screencast frame image,
	// in milliseconds.
	ScreencastDecodeMS float64
	// DOMSnapshotMS is the time spent capturing the DOM text snapshot, in
	// milliseconds.
	DOMSnapshotMS float64
}

// ScreencastMeta is the neutral screencast frame metadata the capture bundle
// records: the scroll offset, page scale, and device size of the frame, with no
// image bytes. It mirrors the fields of the CDP page.ScreencastFrameMetadata
// without importing the chromedp type, so the render seam stays free of any
// chromedp dependency. The engine fills it only under capture; it is nil on the
// production path.
type ScreencastMeta struct {
	// OffsetTop is the top offset in device-independent pixels.
	OffsetTop float64
	// PageScaleFactor is the page zoom scale factor.
	PageScaleFactor float64
	// DeviceWidth is the device screen width in device-independent pixels.
	DeviceWidth float64
	// DeviceHeight is the device screen height in device-independent pixels.
	DeviceHeight float64
	// ScrollOffsetX is the horizontal scroll offset in CSS pixels.
	ScrollOffsetX float64
	// ScrollOffsetY is the vertical scroll offset in CSS pixels.
	ScrollOffsetY float64
	// Timestamp is the frame timestamp.
	Timestamp float64
}

// Frame is one viewport image plus any text overlay. Text stays empty until the
// overlay path is wired.
type Frame struct {
	Img   *image.RGBA     // the viewport bitmap, already at or above cell resolution
	Text  []GlyphRun      // empty until the text overlay path is wired
	Stats *FrameStats     // engine-phase timings, nil off the capture path
	Meta  *ScreencastMeta // screencast frame metadata, nil off the capture path
}

// Renderer emits a Frame to a writer. Each backend renders the same Frame in a
// different terminal representation.
type Renderer interface {
	Render(f Frame, w io.Writer) error // emit one frame
	Name() string
}
