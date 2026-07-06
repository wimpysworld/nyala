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

// init pins go-runewidth's global condition so ambiguous-width characters count
// as one cell, matching the wezterm and kitty defaults. StringWidth reads
// DefaultCondition, so this makes CellWidth deterministic and independent of the
// process locale, keeping the overlay's cell count aligned with the terminal.
func init() {
	runewidth.DefaultCondition.EastAsianWidth = false
}

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
// single foreground colour. The engine builds these runs from the DOM text
// snapshot and the block backend folds them onto the grid as the crisp text
// overlay.
type GlyphRun struct {
	Col, Row int        // cell position
	Text     string     // one or more graphemes
	FG       color.RGBA // foreground colour
	// BG is the DOM effective background colour resolved for the run's box, in
	// RGBA order. A zero-alpha BG means "no opaque background resolved". The block
	// overlay no longer reads it (the bitmap carries the real background with no
	// painted text ink), but the walk resolves it and the capture text-map records
	// it for inspection.
	BG color.RGBA
	X  float64 // box left, CSS pixels, on-screen (scroll subtracted)
	Y  float64 // box top, CSS pixels, on-screen (scroll subtracted)
	W  float64 // box width, CSS pixels
	H  float64 // box height, CSS pixels
	// CellW is the CSS-pixel cell width the run was snapped with. It is zero when
	// unknown, which disables the sub-cell recompose for the run.
	CellW float64
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
	// LayoutScale is the block-only layout scale Z for this frame, capture path
	// only. It is max(1, cols*cellW / layoutWidth) on a block grid wider than the
	// target layout width, and 1 otherwise.
	LayoutScale float64
}

// LoadTimings holds the four load-path milestone elapsed times, in milliseconds
// since launch, that the engine records for the capture bundle. The engine records
// them only when capture (or the NYALA_DEBUG_TIMING switch) is on; the production
// live path records nothing. A field is zero when its milestone did not fire, for
// example a partial capture whose page never published text. Like FrameStats, it is
// a plain value the capture package maps to its own JSON shape.
type LoadTimings struct {
	// ProcessSpawnedMS is the elapsed time to the Chromium process spawn.
	ProcessSpawnedMS float64
	// ScreencastStartedMS is the elapsed time to the screencast start.
	ScreencastStartedMS float64
	// FirstFrameMS is the elapsed time to the first emitted frame.
	FirstFrameMS float64
	// FirstTextMS is the elapsed time to the first published text overlay.
	FirstTextMS float64
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

// Frame is one viewport image plus its text overlay. The engine fills Text from
// the DOM text snapshot; it is empty when the page carries no text runs.
type Frame struct {
	Img   *image.RGBA     // the viewport bitmap, already at or above cell resolution
	Text  []GlyphRun      // text overlay runs, empty when the page has none
	Stats *FrameStats     // engine-phase timings, nil off the capture path
	Meta  *ScreencastMeta // screencast frame metadata, nil off the capture path
}

// Renderer emits a Frame to a writer. Each backend renders the same Frame in a
// different terminal representation.
type Renderer interface {
	Render(f Frame, w io.Writer) error // emit one frame
	Name() string
}
