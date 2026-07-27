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
// 2. A cluster of measured width 2 or more claims two cells, while everything
// else (including a width-0 standalone
// combining mark) claims one. The clip budget and the cell binning both use it
// so a width-2 cluster is never half-emitted.
func CellWidth(grapheme string) int {
	if runewidth.StringWidth(grapheme) >= 2 {
		return 2
	}
	return 1
}

// DisplayWidth returns the total display-cell width of text, summing each
// grapheme cluster's CellWidth. It is the whole-run counterpart of CellWidth and
// lets a run cache its display width at decode, so the compose path reads the
// cached value instead of re-segmenting the run every frame.
func DisplayWidth(text string) int {
	cells := 0
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		cells += CellWidth(gr.Str())
	}
	return cells
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
	// painted text ink), but diagnostics retain it for inspection.
	BG color.RGBA
	X  float64 // box left, CSS pixels, on-screen (scroll subtracted)
	Y  float64 // box top, CSS pixels, on-screen (scroll subtracted)
	W  float64 // box width, CSS pixels
	H  float64 // box height, CSS pixels
	// CellW is the CSS-pixel cell width the run was snapped with. It is zero when
	// unknown, which disables the sub-cell recompose for the run.
	CellW float64
	// DisplayCells is the run's total display-cell width, cached at decode via
	// DisplayWidth so the compose boundary recompose reads it instead of
	// re-segmenting the run every frame. It is zero when unset (a hand-built run),
	// in which case the compose path falls back to measuring Text. It is excluded
	// from serialised diagnostic runs because it is derived data.
	DisplayCells int `json:"-"`
	// StackRank is the run's front-to-back stacking rank, 0 being front-most. The
	// page derives it from the CSS stacking order of the run's element and the
	// client only carries it, so it is page data rather than derived data. No
	// consumer reads it yet, so it changes no placement today.
	StackRank int
}

// IngestStats is the session ingest summary the NYALA_FPS_TIMING quit line
// reports: the screencast ingest frame count and rate statistics, plus the worker
// decode budget. The engine computes it from the true per-frame arrival times
// (recorded before the live queue drops any) and the per-frame worker decode
// times, so under the decoupled receiver/worker model the gap-based extremes are
// meaningful. The viewer reads it over the ingestReporter seam without importing the
// engine, so it lives in the render seam. Every field is zero when the switch
// is off or the session recorded no frame.
type IngestStats struct {
	// Frames is the count of delivered screencast frames over the session.
	Frames int
	// MeanFPS is the mean ingest rate over the first-to-last-frame span.
	MeanFPS float64
	// MinFPS is the slowest instantaneous rate, 1 over the largest inter-arrival gap.
	MinFPS float64
	// MaxFPS is the fastest instantaneous rate, 1 over the smallest inter-arrival gap.
	MaxFPS float64
	// MedianFPS is the median instantaneous rate, the median inter-arrival gap inverted.
	MedianFPS float64
	// WorstGapMS is the largest inter-arrival gap in milliseconds, the worst ingest stall.
	WorstGapMS float64
	// DecodeMeanMS is the mean worker decode wall time per frame in milliseconds.
	DecodeMeanMS float64
	// DecodeMaxMS is the slowest single worker decode in milliseconds.
	DecodeMaxMS float64
	// AckRTTMeanMS is the mean screencast acknowledgement command round trip in
	// milliseconds, measured on the ack pump.
	AckRTTMeanMS float64
	// AckRTTMaxMS is the slowest single acknowledgement round trip in milliseconds.
	AckRTTMaxMS float64
}

// ScreencastMeta is neutral screencast frame metadata: the scroll offset, page
// scale, and device size of the frame, with no image bytes. It mirrors the
// fields of the CDP page.ScreencastFrameMetadata
// without importing the chromedp type, so the render seam stays free of any
// chromedp dependency. The engine attaches it to decoded frames so diagnostics
// can check scroll synchronisation.
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

// FrameDiagnostics holds data requested by diagnostics but unused by normal
// rendering. Frame.Diagnostics stays nil on the production path.
type FrameDiagnostics struct {
	// Meta is the CDP screencast metadata paired with the frame.
	Meta ScreencastMeta
	// CalibrationSerial counts completed diagnostic font-calibration attempts.
	CalibrationSerial int64
	// CalibrationSucceeded reports whether the latest attempt measured and
	// applied a corrected font size.
	CalibrationSucceeded bool
	// CalibratedFontSize is the corrected CSS-pixel font size, or the seed size
	// when the latest calibration attempt failed.
	CalibratedFontSize float64
}

// Frame is one viewport image plus its text overlay. Img keeps the decoder's
// native image layout, so a JPEG can cross the engine/render seam as YCbCr and a
// PNG can keep the layout its decoder produced. EncodedPNG holds an owned CDP
// base64 PNG payload on the live Kitty path; that path leaves Img nil and
// sends the payload to Kitty unchanged. Other paths leave EncodedPNG empty.
type Frame struct {
	Img         image.Image       // decoder-native viewport pixels, nil for direct Kitty PNG
	EncodedPNG  string            // owned base64 PNG payload for direct live Kitty transmission
	Text        []GlyphRun        // text overlay runs, empty when the page has none
	Diagnostics *FrameDiagnostics // diagnostic data, nil on the production path

	// Release returns the frame's pooled pixel plane to the engine's plane
	// pool. The engine sets it only on kitty-shm decoded frames, where Img is
	// a pooled RGBA plane; it is nil on every other path. The frame's final
	// owner calls it exactly once, when the frame is dropped or its pixels
	// have been consumed (painted or copied out). A nil Release needs no
	// call, so consumers guard with a nil check and existing paths are
	// unaffected.
	Release func()

	// AfterFCP reports that this frame arrived after the main frame's first
	// contentful paint lifecycle event. The viewer uses it as the backend-neutral
	// FPS warm-up gate, including for text-free pages and direct Kitty payloads.
	AfterFCP bool
}

// HasImage reports whether the frame carries decoded pixels or a direct encoded
// PNG payload. The latter is a usable live Kitty frame even though Img is nil.
func (f Frame) HasImage() bool {
	return f.Img != nil || f.EncodedPNG != ""
}

// Renderer emits a Frame to a writer. Each backend renders the same Frame in a
// different terminal representation.
type Renderer interface {
	Render(f Frame, w io.Writer) error // emit one frame
	Name() string
}
