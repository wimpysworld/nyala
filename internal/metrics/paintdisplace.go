package metrics

import (
	"image"
	"math"
	"unicode"

	"github.com/mattn/go-runewidth"
)

// PaintRun is one decoded text run the paint-displacement metric measures: the
// run's Text, its snapped Col and Row on the cell grid, its foreground colour FG,
// its box top-left X, Y and size W, H in CSS pixels, and CellW, the CSS-pixel cell
// width the run was snapped with. It mirrors the per-frame text-map.json entry; the
// metric reads only these fields.
type PaintRun struct {
	// Text is the run's text; its script decides whether the run is measurable.
	Text string
	// Col is the snapped start column the frame placed the run at, round(X/CellW).
	Col int
	// Row is the snapped start row the frame placed the run at.
	Row int
	// FG is the run's foreground colour, the ink hue the paint scan matches.
	FG PaintColour
	// X is the box left edge in CSS pixels, on-screen (scroll subtracted).
	X float64
	// Y is the box top edge in CSS pixels, on-screen (scroll subtracted).
	Y float64
	// W is the box width in CSS pixels.
	W float64
	// H is the box height in CSS pixels.
	H float64
	// CellW is the CSS-pixel cell width the run was snapped with. The device scale
	// is cellPxW/CellW, so a zero or negative CellW makes the run unmeasurable.
	CellW float64
}

// PaintColour is the red, green, blue foreground triple the paint scan matches
// against, dropping the alpha the RGBA foreground carries. It mirrors the R, G, B
// fields of a text-map.json run's FG object.
type PaintColour struct {
	// R, G, B are the 0..255 channels of the run's foreground ink.
	R, G, B uint8
}

// PaintDisplaceStats aggregates paint-referenced glyph displacement over one
// frame's runs. For each measurable run the metric finds the painted ink's leftmost
// inked column and top inked row in the SOURCE page paint (screenshot.png, not the
// overlay render), and reports the signal displacement of the DOM-snapped cell from
// that painted origin. Because Col and Row come from the DOM box while the ink comes
// from the page's own paint, a wrong cell width, a wrong scale, a bad text-to-bitmap
// pairing, or a scroll skew moves one and not the other, so the error cannot cancel
// the way a box-against-itself score does.
type PaintDisplaceStats struct {
	// Measured is the number of runs whose painted ink the scan could read and score.
	Measured int
	// Skipped is the number of runs the scan could not score: RTL, wide-script, or
	// low-contrast runs whose paint origin is unreadable, plus runs whose ink the
	// bounded window held none of. A high SkippedFraction cannot hide a fault, so it
	// is reported as its own signal.
	Skipped int
	// SkippedFraction is Skipped over the total run count (Measured plus Skipped), or
	// zero when there are no runs.
	SkippedFraction float64
	// MeanAbsH is the mean absolute horizontal displacement in cells over the measured
	// runs: painted ink column minus Col.
	MeanAbsH float64
	// MaxAbsH is the max absolute horizontal displacement in cells over the measured
	// runs.
	MaxAbsH float64
	// MeanAbsV is the mean absolute vertical displacement in cells over the measured
	// runs: painted ink row minus Row.
	MeanAbsV float64
	// MaxAbsV is the max absolute vertical displacement in cells over the measured
	// runs.
	MaxAbsV float64
	// PerRun holds the per-run displacement of each measured run, in input order,
	// skipped runs excluded.
	PerRun []PaintRunDisplace
}

// PaintRunDisplace holds one measured run's displacement, both axes in cells.
type PaintRunDisplace struct {
	// Text is the measured run's text, so a caller can name a displaced run.
	Text string
	// H is the horizontal displacement in cells: painted ink column minus Col.
	H float64
	// V is the vertical displacement in cells: painted ink row minus Row.
	V float64
}

// paintScanWindowCells is the horizontal half-width, in cells, of the search window
// the ink scan sweeps around a run's box start column. The painted glyph and the DOM
// box can disagree by a few cells under a scale or scroll skew, so the scan looks a
// few cells either side for the run's ink before it gives up. The scan is a symmetric
// nearest-first sweep, so a run whose ink sits about a cell left of the box is found
// (the nearest matching column wins) without a naive window snapping onto a far
// neighbour's same-colour ink.
const paintScanWindowCells = 4

// paintScanRowWindowCells is the vertical half-height, in cells, of the search window
// the ink scan sweeps around a run's box band. It mirrors paintScanWindowCells on the
// vertical axis: the painted glyph and the DOM box can disagree by a few cells under a
// scroll or reflow skew, so the scan looks a few rows either side of the box band,
// nearest first, for the run's ink before it gives up. A tight half-cell band would
// return no-ink and silently discard a real vertical displacement; this window catches
// it, and nearest-first keeps a drifted run from matching a neighbouring row's
// same-colour ink at distance.
const paintScanRowWindowCells = 4

// paintColourTolerance is the squared-distance threshold in RGB space (each channel
// 0..255) below which a screenshot pixel is treated as the run's foreground ink.
// Antialiasing dims and blends a glyph edge against the background, so the match is
// loose enough to catch the blended edge pixels while still separating a brand-hue
// glyph from an unrelated background. It is a squared Euclidean radius, so the
// per-channel radius is its square root.
const paintColourTolerance = 90 * 90

// paintInkCoverageFloor is the minimum fraction of a column's band pixels that must
// match the run's foreground colour for the column to count as inked. A thin glyph
// stroke covers only a few per cent of a cell band, so the floor is low; it is above
// zero so a single stray antialiased pixel does not read as ink.
const paintInkCoverageFloor = 0.02

// paintContrastFloor is the minimum luma contrast, in 0..255 channel units, between a
// run's foreground and its box-band background for the run to be measurable. A run
// whose ink barely differs from its background paints too faintly for the colour scan
// to separate from noise, so it is skipped and counted, not scored on unreadable
// paint: a low-contrast run is not scored rather than mis-scored.
const paintContrastFloor = 24

// PaintDisplacement measures paint-referenced glyph displacement for one frame's
// runs against the source page paint. img is the decoded screenshot.png (the page's
// own painted text, forced to monospace, with no overlay); cellPxW and cellPxH are
// the terminal cell size in device pixels. The device scale from CSS to the
// screenshot is cellPxW/run.CellW, isotropic, so it also converts the box row band and
// the ink row. For each run:
//
//   - It skips a run whose script the scan cannot place by its leftmost inked cell:
//     any right-to-left character or any wide grapheme. It also skips a run with a
//     non-positive CellW (the scale is unknown) and a run whose foreground barely
//     contrasts with its box-band background (paintContrastFloor), whose paint the scan
//     cannot read.
//   - It places the box origin in device pixels (X*scale, box band Y*scale..(Y+H)*scale)
//     and searches a bounded window on both axes: paintScanRowWindowCells rows and
//     paintScanWindowCells columns either side of the origin, nearest first on each
//     axis, for a cell whose band carries the run's foreground colour above
//     paintInkCoverageFloor. Nearest first means the closest matching cell wins, so a
//     drifted run cannot snap onto a neighbouring row or column's same-colour ink.
//   - It reports horizontal displacement inkCol-Col and vertical displacement
//     inkRow-Row in cells, where inkCol is the matched device column over cellPxW and
//     inkRow is the topmost inked device row in that column over cellPxH. A run whose
//     paint drifted rows above or below the box is measured on the vertical axis, not
//     silently skipped.
//
// A run whose window holds no matching ink is skipped and counted, so an unreadable
// run inflates SkippedFraction rather than scoring zero. A nil image, a non-positive
// cell size, or no runs returns the zero value. The image is read in RGBA byte order.
func PaintDisplacement(img *image.RGBA, runs []PaintRun, cellPxW, cellPxH int) PaintDisplaceStats {
	var stats PaintDisplaceStats
	if img == nil || cellPxW <= 0 || cellPxH <= 0 || len(runs) == 0 {
		return stats
	}
	bounds := img.Bounds()
	var sumAbsH, sumAbsV float64
	for _, run := range runs {
		if !paintMeasurable(run.Text) || run.CellW <= 0 {
			stats.Skipped++
			continue
		}
		scale := float64(cellPxW) / run.CellW
		if scale <= 0 {
			stats.Skipped++
			continue
		}
		d, ok := scanRunDisplacement(img, bounds, run, scale, cellPxW, cellPxH)
		if !ok {
			stats.Skipped++
			continue
		}
		stats.Measured++
		stats.PerRun = append(stats.PerRun, d)
		absH := math.Abs(d.H)
		absV := math.Abs(d.V)
		sumAbsH += absH
		sumAbsV += absV
		if absH > stats.MaxAbsH {
			stats.MaxAbsH = absH
		}
		if absV > stats.MaxAbsV {
			stats.MaxAbsV = absV
		}
	}
	total := stats.Measured + stats.Skipped
	if total > 0 {
		stats.SkippedFraction = float64(stats.Skipped) / float64(total)
	}
	if stats.Measured > 0 {
		n := float64(stats.Measured)
		stats.MeanAbsH = sumAbsH / n
		stats.MeanAbsV = sumAbsV / n
	}
	return stats
}

// scanRunDisplacement finds a run's painted ink in img and returns its displacement
// from the run's DOM-snapped cell, both axes in cells. scale is the device scale
// cellPxW/CellW. It estimates the box-band background, skips a run whose foreground
// barely contrasts with it, then searches for the run's ink over a bounded window on
// both axes: paintScanRowWindowCells rows and paintScanWindowCells columns either side
// of the box origin, nearest first on each axis. The row band is the box's own height
// shifted by the candidate row offset; the first candidate column whose band carries
// the run's foreground colour above paintInkCoverageFloor at the nearest candidate row
// wins. It reports the horizontal displacement inkCol-Col and the vertical
// displacement inkRow-Row in cells, so a run whose paint drifted rows above or below
// the box is measured, not skipped. It returns ok false when the box band is
// off-image, the run is too low contrast, or no cell in the window holds matching ink.
func scanRunDisplacement(img *image.RGBA, bounds image.Rectangle, run PaintRun, scale float64, cellPxW, cellPxH int) (PaintRunDisplace, bool) {
	boxX := run.X * scale
	boxTop := run.Y * scale
	boxHeight := run.H * scale
	yMin := int(math.Floor(boxTop))
	yMax := int(math.Ceil(boxTop + boxHeight))
	if yMin < bounds.Min.Y {
		yMin = bounds.Min.Y
	}
	if yMax > bounds.Max.Y {
		yMax = bounds.Max.Y
	}
	if yMin >= yMax {
		return PaintRunDisplace{}, false
	}
	bg := paintBandBackground(img, bounds, yMin, yMax)
	fg := run.FG
	if paintLumaContrast(fg, bg) < paintContrastFloor {
		return PaintRunDisplace{}, false
	}
	boxXi := int(math.Round(boxX))
	rowWindow := paintScanRowWindowCells * cellPxH
	colWindow := paintScanWindowCells * cellPxW
	// Sweep rows nearest first around the box band, and columns nearest first around
	// the box origin, so the closest matching cell wins on both axes and a run cannot
	// snap onto a neighbouring row or column's same-colour ink at distance.
	for dy := 0; dy <= rowWindow; dy++ {
		for _, ry := range nearestOffsets(dy) {
			bandMin := yMin + ry
			bandMax := yMax + ry
			if bandMin < bounds.Min.Y {
				bandMin = bounds.Min.Y
			}
			if bandMax > bounds.Max.Y {
				bandMax = bounds.Max.Y
			}
			if bandMin >= bandMax {
				continue
			}
			if inkX, ok := scanBandColumns(img, bounds, fg, boxXi, colWindow, bandMin, bandMax); ok {
				inkCol := inkX / cellPxW
				inkY := topInkedRow(img, fg, inkX, bandMin, bandMax)
				inkRow := inkY / cellPxH
				return PaintRunDisplace{
					Text: run.Text,
					H:    float64(inkCol - run.Col),
					V:    float64(inkRow - run.Row),
				}, true
			}
		}
	}
	return PaintRunDisplace{}, false
}

// nearestOffsets returns the signed row offsets to try at distance dy, nearest first:
// the single 0 at distance 0, then -dy and +dy above and below the box band. Trying
// the offset above before the one below is arbitrary but stable, so a symmetric drift
// resolves deterministically.
func nearestOffsets(dy int) []int {
	if dy == 0 {
		return []int{0}
	}
	return []int{-dy, dy}
}

// scanBandColumns sweeps columns nearest first around boxXi, up to colWindow device
// pixels either side, clipped to bounds, and returns the first column whose band in
// [bandMin, bandMax) carries the run's foreground ink above paintInkCoverageFloor. It
// returns the device column and true on a match, or ok false when no column in the
// window is inked. Nearest first means a run whose ink sits about a cell left of the
// box origin is found without a far same-colour neighbour winning.
func scanBandColumns(img *image.RGBA, bounds image.Rectangle, fg PaintColour, boxXi, colWindow, bandMin, bandMax int) (int, bool) {
	for dx := 0; dx <= colWindow; dx++ {
		for _, rx := range nearestOffsets(dx) {
			x := boxXi + rx
			if x < bounds.Min.X || x >= bounds.Max.X {
				continue
			}
			if columnInked(img, fg, x, bandMin, bandMax) {
				return x, true
			}
		}
	}
	return 0, false
}

// columnInked reports whether device column x carries the run's foreground ink: its
// share of pixels in [yMin, yMax) within paintColourTolerance of fg reaches
// paintInkCoverageFloor. The band is read in RGBA byte order.
func columnInked(img *image.RGBA, fg PaintColour, x, yMin, yMax int) bool {
	total := yMax - yMin
	if total <= 0 {
		return false
	}
	hits := 0
	for y := yMin; y < yMax; y++ {
		off := img.PixOffset(x, y)
		if paintColourDistance(img.Pix[off], img.Pix[off+1], img.Pix[off+2], fg.R, fg.G, fg.B) < paintColourTolerance {
			hits++
		}
	}
	return float64(hits)/float64(total) >= paintInkCoverageFloor
}

// topInkedRow returns the topmost device row in [yMin, yMax) at column x whose pixel
// is within paintColourTolerance of fg, the run's painted vertical origin. It returns
// yMin when no pixel matches, which cannot happen for a column columnInked accepted.
// The band is read in RGBA byte order.
func topInkedRow(img *image.RGBA, fg PaintColour, x, yMin, yMax int) int {
	for y := yMin; y < yMax; y++ {
		off := img.PixOffset(x, y)
		if paintColourDistance(img.Pix[off], img.Pix[off+1], img.Pix[off+2], fg.R, fg.G, fg.B) < paintColourTolerance {
			return y
		}
	}
	return yMin
}

// paintBandBackground estimates the dominant background colour of a device-pixel band
// by averaging its pixels. A text band is mostly background with a thin glyph stroke,
// so the mean sits near the background colour; the contrast skip compares the run's
// foreground against it. It samples every pixel in the band, a few cells tall and a
// full row wide. The band is read in RGBA byte order.
func paintBandBackground(img *image.RGBA, bounds image.Rectangle, yMin, yMax int) PaintColour {
	var rSum, gSum, bSum, n uint64
	for y := yMin; y < yMax; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			off := img.PixOffset(x, y)
			rSum += uint64(img.Pix[off])
			gSum += uint64(img.Pix[off+1])
			bSum += uint64(img.Pix[off+2])
			n++
		}
	}
	if n == 0 {
		return PaintColour{}
	}
	return PaintColour{
		R: uint8((rSum / n) & 0xff),
		G: uint8((gSum / n) & 0xff),
		B: uint8((bSum / n) & 0xff),
	}
}

// paintLumaContrast returns the absolute luma difference between two colours, a
// 0..255 measure of how far a run's foreground stands out from its band background.
// A run below paintContrastFloor is skipped rather than scored on paint the colour
// scan cannot separate from noise.
func paintLumaContrast(a, b PaintColour) float64 {
	la := 0.299*float64(a.R) + 0.587*float64(a.G) + 0.114*float64(a.B)
	lb := 0.299*float64(b.R) + 0.587*float64(b.G) + 0.114*float64(b.B)
	return math.Abs(la - lb)
}

// paintMeasurable reports whether a run's text is a left-anchored, one-cell-per-glyph
// run the paint scan can place by its leftmost inked cell. It returns false for a run
// carrying any right-to-left character or any wide grapheme, since the scan's
// paint-origin assumption does not hold for those scripts. Latin, digits, and
// punctuation return true.
func paintMeasurable(text string) bool {
	for _, r := range text {
		if paintRTL(r) || runewidth.RuneWidth(r) >= 2 {
			return false
		}
	}
	return true
}

// paintRTL reports whether a rune belongs to a right-to-left script. Arabic, Hebrew,
// Syriac, Thaana, and N'Ko cover the RTL scripts the corpus exercises plus the other
// common ones.
func paintRTL(r rune) bool {
	return unicode.In(r, unicode.Arabic, unicode.Hebrew, unicode.Syriac, unicode.Thaana, unicode.Nko)
}

// paintColourDistance returns the squared Euclidean distance between two RGB colours
// in 0..255 channel space. Squared distance avoids a square root and compares directly
// against the squared paintColourTolerance.
func paintColourDistance(r1, g1, b1, r2, g2, b2 uint8) int {
	dr := int(r1) - int(r2)
	dg := int(g1) - int(g2)
	db := int(b1) - int(b2)
	return dr*dr + dg*dg + db*db
}
