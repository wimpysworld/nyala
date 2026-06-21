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
// single foreground colour. It is unused in Stage 0.
type GlyphRun struct {
	Col, Row int        // cell position
	Text     string     // one or more graphemes
	FG       color.RGBA // foreground colour
}

// Frame is one viewport image plus any text overlay. In Stage 0 Text is empty.
type Frame struct {
	Img  *image.RGBA // the viewport bitmap, already at or above cell resolution
	Text []GlyphRun  // empty in Stage 0
}

// Renderer emits a Frame to a writer. Each backend renders the same Frame in a
// different terminal representation.
type Renderer interface {
	Render(f Frame, w io.Writer) error // emit one frame
	Name() string
}
