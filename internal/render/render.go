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

// Frame is one viewport image plus any text overlay. Text stays empty until the
// overlay path is wired.
type Frame struct {
	Img  *image.RGBA // the viewport bitmap, already at or above cell resolution
	Text []GlyphRun  // empty until the text overlay path is wired
}

// Renderer emits a Frame to a writer. Each backend renders the same Frame in a
// different terminal representation.
type Renderer interface {
	Render(f Frame, w io.Writer) error // emit one frame
	Name() string
}
