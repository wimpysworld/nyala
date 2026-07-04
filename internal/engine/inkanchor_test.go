package engine

import (
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// amber is the brand foreground the phantom-badge case paints, matching the
// linuxmatters episode-number overlay hue the anchoring pass targets.
var amber = color.RGBA{R: 248, G: 179, B: 29, A: 0xff}

// newBand returns a bitmap of the given device size filled with bg, so a test
// starts from a blank page and paints only the ink it wants the scan to find.
func newBand(w, h int, bg color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, bg)
		}
	}
	return img
}

// paintInk fills the device-pixel rectangle [x0,x1) x [y0,y1) with fg, standing in
// for a painted glyph run so the scan finds ink at a known column.
func paintInk(img *image.RGBA, x0, y0, x1, y1 int, fg color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetRGBA(x, y, fg)
		}
	}
}

// TestAnchorRunsFoundAtBoxUnchanged asserts a run whose ink sits at its box column
// publishes unchanged, the common fast path. Cell 10x20, scale 1: a run at column 5
// (box X 50) with ink painted at x 50 keeps column 5.
func TestAnchorRunsFoundAtBoxUnchanged(t *testing.T) {
	img := newBand(200, 40, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	paintInk(img, 50, 4, 58, 20, color.RGBA{R: 16, G: 16, B: 16, A: 0xff})
	run := render.GlyphRun{Col: 5, Row: 0, Text: "hi", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 50, Y: 0, W: 16, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 {
		t.Fatalf("got %d runs, want 1 kept", len(got))
	}
	if got[0].Col != 5 {
		t.Errorf("Col = %d, want 5 unchanged", got[0].Col)
	}
}

// TestAnchorRunsFoundOffsetSnaps asserts a run whose ink is offset within the window
// snaps its start column onto the ink. The box says column 5 but the paint sits at
// column 8, so the run snaps to column 8 and its X follows.
func TestAnchorRunsFoundOffsetSnaps(t *testing.T) {
	img := newBand(200, 40, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	paintInk(img, 80, 4, 88, 20, color.RGBA{R: 16, G: 16, B: 16, A: 0xff})
	run := render.GlyphRun{Col: 5, Row: 0, Text: "hi", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 50, Y: 0, W: 16, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 {
		t.Fatalf("got %d runs, want 1 kept", len(got))
	}
	if got[0].Col != 8 {
		t.Errorf("Col = %d, want 8 snapped onto ink", got[0].Col)
	}
	if got[0].X != 80 {
		t.Errorf("X = %v, want 80 following the snap", got[0].X)
	}
}

// TestAnchorRunsNoInkKeptAtBox asserts a run with no matching ink anywhere in the
// window is kept at its box, not dropped. The paired bitmap can lag the DOMSnapshot,
// so an ink-absent run is not a proven phantom; the pass never removes real text.
func TestAnchorRunsNoInkKeptAtBox(t *testing.T) {
	img := newBand(400, 40, color.RGBA{R: 20, G: 40, B: 60, A: 0xff})
	run := render.GlyphRun{Col: 20, Row: 0, Text: "84", FG: amber, X: 200, Y: 0, W: 20, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 || got[0].Col != 20 {
		t.Fatalf("got %v, want the run kept unchanged at column 20", got)
	}
}

// TestAnchorRunsKeptWhenInkAtBox asserts the amber badge run is kept unchanged when
// its ink is painted at the box, the agree fast path.
func TestAnchorRunsKeptWhenInkPresent(t *testing.T) {
	img := newBand(400, 40, color.RGBA{R: 20, G: 40, B: 60, A: 0xff})
	paintInk(img, 200, 4, 220, 20, amber)
	run := render.GlyphRun{Col: 20, Row: 0, Text: "84", FG: amber, X: 200, Y: 0, W: 20, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 || got[0].Col != 20 {
		t.Fatalf("got %v, want the run kept at column 20", got)
	}
}

// TestAnchorRunsNoContrastKeepsAtBox asserts a run in a band with no contrast at all
// is never dropped and never snapped. A near-uniform band (glyph indistinguishable
// from paper, or a band the paint has not reached) shows no contrast onset and no
// foreground ink away from the box, so the run publishes unchanged rather than
// dropping or chasing a phantom edge.
func TestAnchorRunsNoContrastKeepsAtBox(t *testing.T) {
	img := newBand(200, 40, color.RGBA{R: 128, G: 128, B: 128, A: 0xff})
	run := render.GlyphRun{Col: 5, Row: 0, Text: "hi", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 50, Y: 0, W: 16, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 || got[0].Col != 5 {
		t.Fatalf("got %v, want the no-contrast run kept unchanged", got)
	}
}

// TestAnchorRunsDivergentColourSnapsByContrast asserts a run whose paint colour
// diverges from its computed foreground still snaps onto the ink by shape. The box
// says column 5, the run's FG is dark, but the paint at column 8 is a bright amber
// the computed style never reported. The colour match finds nothing, so the contrast
// fallback matches the glyph against the white band and snaps the run to column 8.
func TestAnchorRunsDivergentColourSnapsByContrast(t *testing.T) {
	img := newBand(200, 40, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	paintInk(img, 80, 4, 96, 20, color.RGBA{R: 253, G: 124, B: 8, A: 0xff})
	run := render.GlyphRun{Col: 5, Row: 0, Text: "hi", FG: color.RGBA{R: 20, G: 20, B: 20, A: 0xff}, X: 50, Y: 0, W: 16, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 {
		t.Fatalf("got %d runs, want 1 kept", len(got))
	}
	if got[0].Col != 8 {
		t.Errorf("Col = %d, want 8 snapped onto divergent-colour ink by contrast", got[0].Col)
	}
}

// TestAnchorRunsNeighbourInkDoesNotCapture asserts a thin stray edge in the window
// does not capture the run through the contrast fallback. The box says column 5; the
// run's FG is dark, and a one-column bright speckle (a divergent-colour compression
// artefact the colour match ignores) sits at column 8, below the run-length and width
// floors. The contrast onset rejects it, so the run keeps its box column rather than
// snapping onto the neighbour's stray ink.
func TestAnchorRunsNeighbourInkDoesNotCapture(t *testing.T) {
	img := newBand(200, 40, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	// One device column of contrast at column 8, painted in a colour the run's dark FG
	// does not match, so only the contrast path can see it: below inkRunLength and
	// narrower than the run's first grapheme cell.
	paintInk(img, 80, 4, 81, 20, color.RGBA{R: 253, G: 124, B: 8, A: 0xff})
	run := render.GlyphRun{Col: 5, Row: 0, Text: "hi", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 50, Y: 0, W: 16, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 || got[0].Col != 5 {
		t.Fatalf("got %v, want the run kept at column 5 (stray speckle rejected)", got)
	}
}

// TestAnchorRunsWindowBounds asserts ink beyond the search window does not snap a
// run: with the ink 6 cells right of the box (past the 4-cell window) and below the
// drop region, the run finds no ink in range, its own region reads as an unpainted
// placeholder, and it stays at its box rather than snapping onto the far ink or
// dropping. The band is tall enough that the far ink sits past the drop region's
// vertical reach, so the region around the run stays flat.
func TestAnchorRunsWindowBounds(t *testing.T) {
	img := newBand(400, 220, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	paintInk(img, 160, 200, 168, 216, color.RGBA{R: 16, G: 16, B: 16, A: 0xff})
	run := render.GlyphRun{Col: 10, Row: 0, Text: "hi", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 100, Y: 0, W: 16, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 || got[0].Col != 10 {
		t.Fatalf("got %v, want the run kept at column 10 (ink outside the window)", got)
	}
}

// TestAnchorRunsWideGraphemeSkipped asserts a run of a width-2 grapheme is published
// unchanged: the ink scan's leftmost-cell paint origin does not hold for a wide CJK
// cluster whose advance is not one cell, so anchoring skips it and never snaps or
// drops it. The box says column 3; the run keeps column 3 even with ink painted
// elsewhere.
func TestAnchorRunsWideGraphemeSkipped(t *testing.T) {
	img := newBand(300, 40, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	paintInk(img, 60, 4, 80, 20, color.RGBA{R: 16, G: 16, B: 16, A: 0xff})
	run := render.GlyphRun{Col: 3, Row: 0, Text: "世", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 30, Y: 0, W: 20, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 || got[0].Col != 3 || got[0].Text != "世" {
		t.Errorf("got %v, want the wide run kept unchanged at column 3", got)
	}
}

// TestAnchorRunsRTLSkipped asserts a run carrying a right-to-left character is
// published unchanged: RTL glyphs paint from the right, so the leftmost-cell paint
// origin does not hold and anchoring skips the run rather than snapping onto a
// neighbour's ink.
func TestAnchorRunsRTLSkipped(t *testing.T) {
	img := newBand(300, 40, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	// Ink painted 3 cells right: an anchorable run would snap, but the RTL run stays.
	paintInk(img, 60, 4, 100, 20, color.RGBA{R: 16, G: 16, B: 16, A: 0xff})
	run := render.GlyphRun{Col: 3, Row: 0, Text: "مرحبا", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 30, Y: 0, W: 40, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(got) != 1 || got[0].Col != 3 {
		t.Errorf("got %v, want the RTL run kept unchanged at column 3", got)
	}
}

// TestAnchorRunsOccupancyGuardBlocksSnap asserts a run whose ink sits under a run
// already kept on the same row is not snapped onto it: the first run claims its
// cells, so the second keeps its box column rather than colliding. The first run at
// column 8 (ink at 80) claims columns 8..10; the second run's box is column 12 but
// its only nearby ink is at column 8, already claimed, so it stays at column 12.
func TestAnchorRunsOccupancyGuardBlocksSnap(t *testing.T) {
	img := newBand(300, 40, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	dark := color.RGBA{R: 16, G: 16, B: 16, A: 0xff}
	paintInk(img, 80, 4, 100, 20, dark)
	first := render.GlyphRun{Col: 8, Row: 0, Text: "ab", FG: dark, X: 80, Y: 0, W: 20, H: 20, CellW: 10}
	second := render.GlyphRun{Col: 12, Row: 0, Text: "cd", FG: dark, X: 120, Y: 0, W: 20, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{first, second}, 10, 20, 1, nil)
	if len(got) != 2 {
		t.Fatalf("got %d runs, want 2 kept", len(got))
	}
	if got[0].Col != 8 {
		t.Errorf("first Col = %d, want 8", got[0].Col)
	}
	if got[1].Col != 12 {
		t.Errorf("second Col = %d, want 12 (snap blocked by occupancy)", got[1].Col)
	}
}

// TestAnchorRunsNilBitmapPasses asserts a nil bitmap, non-positive cell, or empty
// run set returns the runs untouched, so a snapshot with no paired bitmap publishes
// as before.
func TestAnchorRunsNilBitmapPasses(t *testing.T) {
	runs := []render.GlyphRun{{Col: 1, Text: "x", X: 10, H: 20, CellW: 10}}
	if got, dropped, _ := anchorRuns(nil, runs, 10, 20, 1, nil); len(got) != 1 || dropped != 0 {
		t.Errorf("nil bitmap dropped runs, want passthrough")
	}
	img := newBand(50, 40, color.RGBA{A: 0xff})
	if got, dropped, _ := anchorRuns(img, runs, 0, 20, 1, nil); len(got) != 1 || dropped != 0 {
		t.Errorf("zero cellW dropped runs, want passthrough")
	}
	if got, dropped, _ := anchorRuns(img, nil, 10, 20, 1, nil); got != nil || dropped != 0 {
		t.Errorf("empty runs returned %v, want nil passthrough", got)
	}
}

// TestAnchorRunsScaleMapsToDevice asserts the layout scale maps a run's CSS
// coordinates to device pixels: at scale 2 a box at CSS X 25 lands at device x 50,
// where the ink is painted, so the run is verified in place.
func TestAnchorRunsScaleMapsToDevice(t *testing.T) {
	img := newBand(200, 80, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	// CSS cell 10, scale 2 => device cell 20. CSS X 25 => device x 50, CSS Y 0..20 =>
	// device y 0..40.
	paintInk(img, 50, 8, 66, 40, color.RGBA{R: 16, G: 16, B: 16, A: 0xff})
	run := render.GlyphRun{Col: 3, Row: 0, Text: "hi", FG: color.RGBA{R: 16, G: 16, B: 16, A: 0xff}, X: 25, Y: 0, W: 16, H: 20, CellW: 10}
	got, _, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 2, nil)
	if len(got) != 1 {
		t.Fatalf("got %d runs, want 1 kept", len(got))
	}
	// Device cell 20, box device X 50 => box column round(50/20)=3, ink column
	// round(50/20)=3, so unchanged.
	if got[0].Col != 3 {
		t.Errorf("Col = %d, want 3 (ink at box under scale)", got[0].Col)
	}
}

// paintNoise fills the drop-guard region of a 400x200 test band with a per-pixel
// varying colour, standing in for a painted region (an avatar photo, a card edge) so
// the drop guard's variance floor reads it as painted rather than a flat placeholder.
// It paints rows 40..200 across the full 400-column width, leaving the run's band
// (rows 0..20, device y) flat, the shape every caller needs. The extent is fixed
// because every drop-guard test builds the same band size.
func paintNoise(img *image.RGBA) {
	for y := 40; y < 200; y++ {
		for x := range 400 {
			v := byte((x*37 + y*53) & 0xff)
			img.SetRGBA(x, y, color.RGBA{R: v, G: 255 - v, B: byte((x * 91) & 0xff), A: 0xff})
		}
	}
}

// TestAnchorRunsPhantomKeptFirstGenerationThenDropped asserts the two-snapshot
// phantom-drop agreement: a run with no ink of its own but a painted region around it
// (the DOMSnapshot reported the text, the page painted an avatar band but drew no glyph)
// is kept on the first generation as a candidate, then dropped only when the second
// generation confirms it ink-free. The first pass must never drop, so a real run whose
// glyph merely has not painted yet in this verification bitmap survives one generation.
func TestAnchorRunsPhantomKeptFirstGenerationThenDropped(t *testing.T) {
	img := newBand(400, 200, color.RGBA{R: 235, G: 235, B: 235, A: 0xff})
	// Paint the region above and below the run's band (band is y 0..20), leaving the
	// band itself flat so no ink matches the run.
	paintNoise(img)
	run := render.GlyphRun{Col: 20, Row: 0, Text: "84", FG: amber, X: 200, Y: 0, W: 20, H: 20, CellW: 10}
	// Generation 1: no prior candidates, so the phantom is kept and flagged.
	got, dropped, phantoms := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if dropped != 0 {
		t.Fatalf("generation 1 dropped = %d, want 0 (kept as a candidate)", dropped)
	}
	if len(got) != 1 || got[0].Col != 20 {
		t.Fatalf("generation 1 got %v, want the run kept at column 20", got)
	}
	if len(phantoms) != 1 {
		t.Fatalf("generation 1 phantom set = %v, want one candidate", phantoms)
	}
	// Generation 2: the same ink-free run, now confirmed by the carried candidate set,
	// is dropped.
	got, dropped, _ = anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, phantoms)
	if dropped != 1 {
		t.Fatalf("generation 2 dropped = %d, want 1 phantom dropped", dropped)
	}
	if len(got) != 0 {
		t.Fatalf("generation 2 got %v, want the phantom dropped", got)
	}
}

// TestAnchorRunsPhantomReinkedSurvives asserts a run flagged a phantom candidate on
// generation 1 but whose glyph paints by generation 2 is kept, not dropped: the second
// pass finds ink at the box (the agree fast path), so the run never reaches the drop
// guard and the stale candidacy cannot remove it. This is the seven-linuxmatters-runs
// case, where late-painting text survives.
func TestAnchorRunsPhantomReinkedSurvives(t *testing.T) {
	blank := newBand(400, 200, color.RGBA{R: 235, G: 235, B: 235, A: 0xff})
	paintNoise(blank)
	run := render.GlyphRun{Col: 20, Row: 0, Text: "84", FG: amber, X: 200, Y: 0, W: 20, H: 20, CellW: 10}
	_, _, phantoms := anchorRuns(blank, []render.GlyphRun{run}, 10, 20, 1, nil)
	if len(phantoms) != 1 {
		t.Fatalf("generation 1 phantom set = %v, want one candidate", phantoms)
	}
	// Generation 2: the glyph has now painted at the box, so the run is kept despite the
	// carried candidacy.
	inked := newBand(400, 200, color.RGBA{R: 235, G: 235, B: 235, A: 0xff})
	paintNoise(inked)
	paintInk(inked, 200, 4, 220, 20, amber)
	got, dropped, _ := anchorRuns(inked, []render.GlyphRun{run}, 10, 20, 1, phantoms)
	if dropped != 0 {
		t.Fatalf("generation 2 dropped = %d, want 0 (glyph painted)", dropped)
	}
	if len(got) != 1 || got[0].Col != 20 {
		t.Fatalf("generation 2 got %v, want the re-inked run kept at column 20", got)
	}
}

// TestAnchorRunsFlatPlaceholderKeeps asserts a run with no ink over a flat placeholder
// is kept, not dropped: the paint has not reached the region, so an ink-absent run is
// not a proven phantom. A uniform fill carries near-zero variance, below the drop
// guard's floor, so the run survives until its paint lands.
func TestAnchorRunsFlatPlaceholderKeeps(t *testing.T) {
	img := newBand(400, 200, color.RGBA{R: 30, G: 30, B: 30, A: 0xff})
	run := render.GlyphRun{Col: 20, Row: 0, Text: "84", FG: amber, X: 200, Y: 0, W: 20, H: 20, CellW: 10}
	got, dropped, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 over a flat placeholder", dropped)
	}
	if len(got) != 1 || got[0].Col != 20 {
		t.Fatalf("got %v, want the run kept at column 20 over a placeholder", got)
	}
}

// TestAnchorRunsMatchingInkKeptOverPaintedRegion asserts a run whose own ink is
// painted at its box is kept even when the surrounding region is heavily painted: the
// agree fast path fires before the drop guard, so real text over a busy region is never
// dropped.
func TestAnchorRunsMatchingInkKeptOverPaintedRegion(t *testing.T) {
	img := newBand(400, 200, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	paintNoise(img)
	paintInk(img, 200, 4, 220, 20, amber)
	run := render.GlyphRun{Col: 20, Row: 0, Text: "84", FG: amber, X: 200, Y: 0, W: 20, H: 20, CellW: 10}
	got, dropped, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (ink at box)", dropped)
	}
	if len(got) != 1 || got[0].Col != 20 {
		t.Fatalf("got %v, want the inked run kept at column 20", got)
	}
}

// TestAnchorRunsSnappedInkKeptOverPaintedRegion asserts a run whose ink is offset
// within the window snaps and is kept even over a painted region: finding ink anywhere
// in the window means the run is not a phantom, so it snaps rather than drops.
func TestAnchorRunsSnappedInkKeptOverPaintedRegion(t *testing.T) {
	img := newBand(400, 200, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	paintNoise(img)
	// Ink at column 23 (device x 230), 3 cells right of the box column 20.
	paintInk(img, 230, 4, 250, 20, amber)
	run := render.GlyphRun{Col: 20, Row: 0, Text: "84", FG: amber, X: 200, Y: 0, W: 20, H: 20, CellW: 10}
	got, dropped, _ := anchorRuns(img, []render.GlyphRun{run}, 10, 20, 1, nil)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (ink snapped in window)", dropped)
	}
	if len(got) != 1 || got[0].Col != 23 {
		t.Fatalf("got %v, want the run snapped to column 23", got)
	}
}
