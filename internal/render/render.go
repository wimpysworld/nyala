// Package render defines the frame and renderer seam for Nyala.
//
// One Frame and one Renderer sit at the boundary between the frame source and
// the terminal backends. Everything downstream swaps behind this seam.
package render

import (
	"image"
	"image/color"
	"io"
)

// GlyphRun is a run of one or more graphemes at a cell position, drawn in a
// single foreground colour. The text overlay path is not wired yet, so no
// renderer reads it.
type GlyphRun struct {
	Col, Row int        // cell position
	Text     string     // one or more graphemes
	FG       color.RGBA // foreground colour
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

// Frame is one viewport image plus any text overlay. Text stays empty until the
// overlay path is wired.
type Frame struct {
	Img   *image.RGBA // the viewport bitmap, already at or above cell resolution
	Text  []GlyphRun  // empty until the text overlay path is wired
	Stats *FrameStats // engine-phase timings, nil off the capture path
}

// Renderer emits a Frame to a writer. Each backend renders the same Frame in a
// different terminal representation.
type Renderer interface {
	Render(f Frame, w io.Writer) error // emit one frame
	Name() string
}
