package engine

import (
	"image"
	"math"
	"strconv"
	"unicode"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/wimpysworld/nyala/internal/render"
)

// anchorWindowCells is the horizontal half-width, in cells, of the search window
// the ink scan sweeps around a run's box start column. The DOMSnapshot box and the
// painted glyph can disagree by a few cells (a measured advance skew grows with x,
// and the painted colour can diverge from the computed style so only the shape
// remains to match), so the scan looks anchorWindowCells cells either side of the
// box start for the run's ink before it snaps. A wider window risks snapping onto an
// unrelated neighbour's ink; a few cells covers the measured drift.
const anchorWindowCells = 4

// anchorRowToleranceCells is the vertical half-height, in cells, added above and
// below a run's box band when the scan reads the bitmap. The painted glyph can sit a
// fraction of a cell off the box top, so the band is padded a little each way to catch
// a glyph that straddles the box edge. It is a small fraction of a cell, not a whole
// one: a full-cell pad reaches into the row above or below, and an avatar, banner, or
// neighbouring line there paints heavy contrast at the box column that would fire the
// agree fast path and defeat the snap. A quarter cell covers the antialias overhang
// while keeping the band inside the run's own text row.
const anchorRowToleranceCells = 0.25

// inkCoverageFloor is the minimum fraction of a column's band pixels that must
// register as ink (foreground-matching or contrast) for the column to count as
// inked. A thin glyph stroke covers only a few per cent of a cell band, so the floor
// is low; it is above zero so a single stray antialiased pixel does not read as ink.
const inkCoverageFloor = 0.02

// inkColourTolerance is the squared-distance threshold in RGB space (each channel
// 0..255) below which a bitmap pixel is treated as the run's foreground ink.
// Antialiasing dims and blends a glyph edge against the background, so the match is
// loose: a generous radius catches the blended edge pixels while still separating a
// brand-hue glyph from an unrelated background. It is the squared Euclidean radius,
// so the effective per-channel radius is its square root.
const inkColourTolerance = 90 * 90

// inkContrastThreshold is the squared RGB distance from the band's dominant
// background above which a bitmap pixel counts as contrast ink. It is the shape
// signal the scan falls back to when the painted colour diverges from the run's
// computed foreground (a page that renders a title in a colour the computed style
// does not report). It sits well above antialiasing noise against a flat background
// yet below a full glyph-to-paper step, so a glyph stroke fires and a near-uniform
// band does not. It is a squared Euclidean radius, so the per-channel radius is its
// square root.
const inkContrastThreshold = 48 * 48

// inkRunLength is the minimum count of consecutive contrast columns a candidate ink
// onset must span before it counts. A single noisy contrast column (a compression
// speckle, a lone antialiased edge) does not start a run; a real glyph paints
// contrast across several adjacent columns, so the run-length gate rejects the
// speckle and keeps the glyph.
const inkRunLength = 2

// dropRegionRowsCells is the vertical half-height, in cells, of the region the
// paint-stability guard reads around a run's box band to decide whether the page
// painted there. A run is dropped as a phantom only when its own band is ink-free
// and this surrounding region carries real paint variance, so a flat placeholder
// (a pre-paint band with nothing painted around it) keeps every run. A few rows
// each way reaches the sibling content that paints beside the phantom (an avatar,
// a card, a heading) without stretching into an unrelated section of the page.
const dropRegionRowsCells = 3

// dropRegionVarianceFloor is the minimum mean per-channel variance a region's
// pixels must carry for the region to count as painted. A flat placeholder (a
// uniform dark or light fill before content lands) sits near zero variance, so the
// floor keeps every run over it; a painted region (an avatar photo, a card edge, a
// heading) carries real variance well above it. It is a variance in 0..255 channel
// space, so the floor is a small squared-deviation the antialiasing noise of a flat
// fill never reaches.
const dropRegionVarianceFloor = 64

// anchorOutcome is the per-run decision anchorRun returns: keep the run (anchored
// or at its box), or mark it a phantom candidate this generation (ink-free with the
// region painted around it). The drop is deferred one generation, so a phantom
// candidate is kept this pass and dropped only when the next generation confirms it.
type anchorOutcome int

const (
	// anchorKeep keeps the run: ink sits at or near its box, or the band is a flat
	// placeholder the paint has not reached, so the run is not a proven phantom.
	anchorKeep anchorOutcome = iota
	// anchorPhantomCandidate marks the run ink-free with real paint around it: a drop
	// candidate this generation, kept unless the previous generation flagged it too.
	anchorPhantomCandidate
)

// anchorRuns anchors each decoded run to the verification bitmap and returns the
// anchored set, the count of runs dropped as phantoms, and the phantom-candidate key
// set this generation produced. It is the ink-anchoring pass: for each run it scans a
// bounded window of the bitmap for ink at the run's row band, keeps the run unchanged
// when ink sits at its box, and snaps the run's start column onto the ink when the ink
// is offset within the window. Ink is matched first by the run's foreground colour,
// which disambiguates two nearby texts; when the paint colour diverges from the
// computed foreground the scan falls back to a local-contrast test, matching the glyph
// by its shape against the band's dominant background.
//
// A run whose window holds no ink of either kind is a phantom candidate only when the
// drop guard says the page painted around it: its own band is ink-free and the
// enclosing region carries real paint variance (dropRegionPainted). This is the
// DOMSnapshot-reported text the forced page never paints as glyphs (a presenter name
// over an avatar photo, a badge the layout draws as an image). The drop is deferred
// one generation for two-snapshot agreement: a run is dropped only when it is a
// phantom candidate this generation and prevPhantoms (the previous generation's
// candidate keys) holds its key too. A first-time ink-free run is kept and returned in
// the new candidate set, so a run whose glyph merely has not painted yet in this
// verification bitmap survives until the next generation reads it; only a run ink-free
// across two consecutive generations drops. When the region is a flat placeholder (the
// paint has not reached it yet) the run is never even a candidate, so late-painting
// text survives regardless. cssCellW and cssCellH are the CSS-pixel cell dimensions
// the runs were snapped with; scale is the block layout scale Z that converts a run's
// on-screen CSS coordinates to device pixels on the bitmap. It runs once per published
// snapshot, never per composed frame, and never mutates the input slice. A nil bitmap,
// a non-positive cell, or an empty run set returns the runs untouched with a zero drop
// count and a nil candidate set, so a snapshot with no verification bitmap yet
// publishes as before.
func anchorRuns(img *image.RGBA, runs []render.GlyphRun, cssCellW, cssCellH, scale float64, prevPhantoms map[string]bool) ([]render.GlyphRun, int, map[string]bool) {
	if img == nil || len(runs) == 0 || cssCellW <= 0 || cssCellH <= 0 || scale <= 0 {
		return runs, 0, nil
	}
	bounds := img.Bounds()
	out := make([]render.GlyphRun, 0, len(runs))
	// claimed tracks the cell ranges already kept per row, so a snap never moves a run
	// onto a cell another run already holds. A bidi-embedded LTR island can find its
	// ink a few cells off its box, but snapping it there would collide with the run
	// beside it; the occupancy guard blocks that snap and keeps the run at its box.
	claimed := make(map[int][]cellSpan)
	phantoms := make(map[string]bool)
	dropped := 0
	for _, run := range runs {
		kept, outcome := anchorRun(img, bounds, run, cssCellW, cssCellH, scale, claimed)
		if outcome == anchorPhantomCandidate {
			key := phantomKey(run)
			// Two-snapshot agreement: drop only when the previous generation flagged this
			// run a phantom candidate too. Otherwise keep it and carry the candidacy
			// forward, so a run whose glyph has not painted yet survives one generation.
			if prevPhantoms[key] {
				dropped++
				continue
			}
			phantoms[key] = true
		}
		out = append(out, kept)
		claimed[kept.Row] = append(claimed[kept.Row], cellSpan{start: kept.Col, end: kept.Col + runCellWidth(kept.Text)})
	}
	return out, dropped, phantoms
}

// phantomKey identifies a run across generations for the two-snapshot phantom-drop
// agreement: its text plus its snapped cell position. A run keeps this key while it
// stays ink-free at the same cell, so a genuine phantom (present across two
// generations) drops, while a run that shifts a cell as the page settles reads as a
// fresh candidate rather than a confirmed phantom.
func phantomKey(run render.GlyphRun) string {
	return run.Text + "\x00" + strconv.Itoa(run.Col) + "\x00" + strconv.Itoa(run.Row)
}

// cellSpan is a half-open cell range [start, end) a kept run occupies on its row,
// used by the occupancy guard to reject a snap onto an already-claimed cell.
type cellSpan struct {
	start, end int
}

// spansOverlap reports whether a candidate half-open cell range [start, end)
// overlaps any span already claimed on the row.
func spansOverlap(spans []cellSpan, start, end int) bool {
	for _, s := range spans {
		if start < s.end && s.start < end {
			return true
		}
	}
	return false
}

// runCellWidth returns the display cell width of a run's text, summing each
// grapheme's Stage 2 cell width. It bounds the run's occupied cell range for the
// occupancy guard.
func runCellWidth(text string) int {
	w := 0
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		w += render.CellWidth(gr.Str())
	}
	return w
}

// firstGraphemeCells returns the display cell width of a run's first grapheme, the
// minimum plausible ink extent for an onset to belong to the run. An onset whose
// contrast run is narrower than the first glyph's cell is too small to be the run's
// paint origin (a stray edge, a neighbour's descender), so the contrast fallback
// rejects it. An empty run returns one cell.
func firstGraphemeCells(text string) int {
	gr := uniseg.NewGraphemes(text)
	if gr.Next() {
		return render.CellWidth(gr.Str())
	}
	return 1
}

// anchorRun anchors one run to the bitmap. It reads the vertical band the run's box
// occupies in device pixels (padded by anchorRowToleranceCells each way), checks
// whether the run's own box cell already holds ink and keeps the run unchanged when
// it does, the common fast path where the DOMSnapshot box and the paint agree. When
// the box cell has no ink it scans outward from the box column, nearest first, within
// anchorWindowCells cells either side, and snaps the run onto the nearest inked cell.
// Ink is matched first by the run's foreground colour, then, when that finds nothing,
// by local contrast against the band's dominant background, so a title painted in a
// colour the computed style does not report still anchors by its shape. When no cell
// in the window holds ink of either kind it consults the drop guard: it flags the run
// a phantom candidate (anchorPhantomCandidate) when the enclosing region shows real
// paint (the page rendered content there but no glyph for this run), and keeps it at
// its box otherwise (a flat placeholder the paint has not reached, so the run is not a
// proven phantom). The caller defers the drop one generation, so a flagged run still
// composes this pass. Keeping an already-inked box cell fixed means a run that agrees
// with the paint never moves, so anchoring cannot introduce a collision on agreeing
// text. It never snaps a run onto a cell already claimed by a kept run on the same row
// (claimed), so anchoring cannot create a same-row collision. It returns the
// possibly-snapped run and its anchor outcome.
func anchorRun(img *image.RGBA, bounds image.Rectangle, run render.GlyphRun, cssCellW, cssCellH, scale float64, claimed map[int][]cellSpan) (render.GlyphRun, anchorOutcome) {
	if !anchorable(run.Text) {
		// The ink scan assumes a left-anchored, one-cell-per-glyph run: it reads the
		// paint origin as the leftmost inked cell. Right-to-left and wide-script runs
		// break that assumption (glyphs paint from the right, shaped clusters spread
		// their ink across cells), so anchoring them would snap onto a neighbour's ink.
		// Publish them where the DOMSnapshot box placed them.
		return run, anchorKeep
	}
	deviceCellW := cssCellW * scale
	if deviceCellW <= 0 {
		return run, anchorKeep
	}
	// The run's box coordinates are on-screen CSS pixels; multiply by the layout
	// scale to reach the bitmap's device pixels.
	boxX := run.X * scale
	boxTop := run.Y * scale
	boxBottom := (run.Y + run.H) * scale
	pad := anchorRowToleranceCells * cssCellH * scale
	yMin := int(math.Floor(boxTop - pad))
	yMax := int(math.Ceil(boxBottom + pad))
	if yMin < bounds.Min.Y {
		yMin = bounds.Min.Y
	}
	if yMax > bounds.Max.Y {
		yMax = bounds.Max.Y
	}
	if yMin >= yMax {
		return run, anchorKeep
	}
	bg := bandBackground(img, bounds, yMin, yMax)
	fg := colourRGB{run.FG.R, run.FG.G, run.FG.B}
	boxCol := int(math.Round(boxX / deviceCellW))
	minCells := firstGraphemeCells(run.Text)
	delta, found := nearestInkedDelta(img, fg, bg, boxCol, minCells, deviceCellW, yMin, yMax, bounds)
	if !found {
		// No ink in the box cell or anywhere in the window, by colour or contrast. Either
		// the band is featureless (the paint has not reached it, so keep the run and let a
		// later snapshot re-decode) or the page painted around the run but drew no glyph
		// for it (a phantom the forced page renders as an image or not at all). The drop
		// guard tells the two apart by the paint variance in the enclosing region: flag a
		// phantom candidate only when the region is painted, keep over a flat placeholder.
		if dropRegionPainted(img, bounds, cssCellH, scale, yMin, yMax) {
			return run, anchorPhantomCandidate
		}
		return run, anchorKeep
	}
	if delta == 0 {
		// Ink sits at the box cell: the box and the paint agree, so keep the run where the
		// DOMSnapshot placed it. This is the common fast path.
		return run, anchorKeep
	}
	// The nearest ink sits a few cells off the box: snap the run's start column onto it
	// so the overlay glues to the paint, keeping the paint origin as the truth. Reject
	// the snap when the target cells are already claimed by a kept run on this row (a
	// bidi-embedded LTR island finding its ink beside a neighbour); keep the run at its
	// box instead, so the snap never introduces a collision.
	target := run.Col + delta
	if spansOverlap(claimed[run.Row], target, target+runCellWidth(run.Text)) {
		return run, anchorKeep
	}
	run.Col = target
	run.X += float64(delta) * deviceCellW / scale
	return run, anchorKeep
}

// dropRegionPainted reports whether the page painted real content in the region
// around a run's box band, the paint-stability guard the phantom drop gates on. It
// reads a region dropRegionRowsCells cells tall centred on the run's band, spanning
// the full bitmap width, and returns true when the region's mean per-channel colour
// variance reaches dropRegionVarianceFloor. A painted region (an avatar, a card, a
// heading beside the phantom) carries variance above the floor; a flat placeholder
// (a uniform fill the paint has not resolved) sits below it. So a run over a painted
// region with no ink of its own is a phantom, while a run over a flat placeholder is
// kept until its paint lands. The band's own vertical extent is [yMin, yMax) in
// device pixels; the region pads that by dropRegionRowsCells cells each way. It reads
// the bitmap in RGBA channel order and never allocates.
func dropRegionPainted(img *image.RGBA, bounds image.Rectangle, cssCellH, scale float64, yMin, yMax int) bool {
	pad := int(math.Ceil(dropRegionRowsCells * cssCellH * scale))
	ry0 := yMin - pad
	ry1 := yMax + pad
	if ry0 < bounds.Min.Y {
		ry0 = bounds.Min.Y
	}
	if ry1 > bounds.Max.Y {
		ry1 = bounds.Max.Y
	}
	if ry0 >= ry1 || bounds.Min.X >= bounds.Max.X {
		return false
	}
	return regionVariance(img, bounds.Min.X, ry0, bounds.Max.X, ry1) >= dropRegionVarianceFloor
}

// regionVariance returns the mean per-channel variance of the pixels in the
// rectangle [x0,x1) x [y0,y1). It is a single streaming pass: it sums each channel
// and its square, then averages the three per-channel variances (E[c^2] - E[c]^2).
// A flat fill returns near zero; a region carrying painted edges, photos, or text
// returns a variance well above the placeholder floor. The band is read in RGBA
// channel order, dropping alpha.
func regionVariance(img *image.RGBA, x0, y0, x1, y1 int) float64 {
	total := (x1 - x0) * (y1 - y0)
	if total == 0 {
		return 0
	}
	var rSum, gSum, bSum, rSq, gSq, bSq float64
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			off := img.PixOffset(x, y)
			r := float64(img.Pix[off])
			g := float64(img.Pix[off+1])
			b := float64(img.Pix[off+2])
			rSum += r
			gSum += g
			bSum += b
			rSq += r * r
			gSq += g * g
			bSq += b * b
		}
	}
	n := float64(total)
	rVar := rSq/n - (rSum/n)*(rSum/n)
	gVar := gSq/n - (gSum/n)*(gSum/n)
	bVar := bSq/n - (bSum/n)*(bSum/n)
	return (rVar + gVar + bVar) / 3
}

// nearestInkedDelta scans outward from the box column, nearest first, up to
// anchorWindowCells cells either side, and returns the signed cell delta to the
// nearest column where the run's ink onset sits. It matches the box cell first (delta
// zero, the agree fast path), then each offset in turn. A cell holds ink when its band
// carries foreground-matching pixels or, failing that, an onset of at least inkRunLength
// contrast columns whose contrast extent spans at least the run's first grapheme cell
// (minCells). Requiring the contrast onset to be the nearest edge to the box and at
// least a glyph wide keeps a neighbour's word from capturing the run. A tie between an
// equal left and right offset resolves to the left (the paint origin), matching the
// run's left-to-right reading order. It returns found false when no cell in the window
// is inked by either signal.
func nearestInkedDelta(img *image.RGBA, fg, bg colourRGB, boxCol, minCells int, deviceCellW float64, yMin, yMax int, bounds image.Rectangle) (int, bool) {
	if cellInked(img, fg, bg, boxCol, minCells, deviceCellW, yMin, yMax, bounds) {
		return 0, true
	}
	for d := 1; d <= anchorWindowCells; d++ {
		if cellInked(img, fg, bg, boxCol-d, minCells, deviceCellW, yMin, yMax, bounds) {
			return -d, true
		}
		if cellInked(img, fg, bg, boxCol+d, minCells, deviceCellW, yMin, yMax, bounds) {
			return d, true
		}
	}
	return 0, false
}

// cellInked reports whether the device-pixel cell at column col holds ink for the run.
// It fires when the cell's band carries foreground-matching pixels above
// inkCoverageFloor (the colour preference, which disambiguates two nearby texts), or,
// when it does not, when a contrast ink onset begins at the cell: a run of at least
// inkRunLength consecutive contrast columns whose total contrast extent from the onset
// spans at least minCells cells (the run's first grapheme). The colour test wins when
// it fires so a run keeps snapping to its own hue; the contrast test is the fallback
// for a paint colour the computed style does not report. The cell spans
// [col*deviceCellW, (col+1)*deviceCellW) in x and [yMin, yMax) in y, clipped to bounds.
// A cell fully outside bounds reads as not inked. The band is read in RGBA channel
// order.
func cellInked(img *image.RGBA, fg, bg colourRGB, col, minCells int, deviceCellW float64, yMin, yMax int, bounds image.Rectangle) bool {
	x0, x1 := cellXRange(col, deviceCellW, bounds)
	if x0 >= x1 || yMin >= yMax {
		return false
	}
	if colourCoverage(img, fg, x0, x1, yMin, yMax) >= inkCoverageFloor {
		return true
	}
	return contrastOnset(img, bg, col, minCells, deviceCellW, yMin, yMax, bounds)
}

// cellXRange returns the device-pixel x span [x0, x1) of cell col at deviceCellW,
// clipped to bounds. x0 >= x1 means the cell lies wholly outside the bitmap.
func cellXRange(col int, deviceCellW float64, bounds image.Rectangle) (int, int) {
	x0 := int(math.Floor(float64(col) * deviceCellW))
	x1 := int(math.Ceil(float64(col+1) * deviceCellW))
	if x0 < bounds.Min.X {
		x0 = bounds.Min.X
	}
	if x1 > bounds.Max.X {
		x1 = bounds.Max.X
	}
	return x0, x1
}

// colourCoverage returns the fraction of pixels in the rectangle [x0,x1) x [yMin,yMax)
// within inkColourTolerance of fg. It is the run's foreground-colour ink share, the
// colour preference the scan tries before the contrast fallback. The band is read in
// RGBA channel order.
func colourCoverage(img *image.RGBA, fg colourRGB, x0, x1, yMin, yMax int) float64 {
	total := (x1 - x0) * (yMax - yMin)
	if total == 0 {
		return 0
	}
	hits := 0
	for y := yMin; y < yMax; y++ {
		for x := x0; x < x1; x++ {
			off := img.PixOffset(x, y)
			if colourDistance(img.Pix[off], img.Pix[off+1], img.Pix[off+2], fg.R, fg.G, fg.B) < inkColourTolerance {
				hits++
			}
		}
	}
	return float64(hits) / float64(total)
}

// contrastOnset reports whether a contrast ink onset begins at cell col: the cell's
// leftmost band column carries contrast, the contrast run from there spans at least
// inkRunLength consecutive columns, and the contrast extent reaches at least minCells
// cells wide. Requiring the onset at the cell's own left edge makes it the nearest
// contrast edge the scan can claim for the run (a later edge belongs to a column
// further out), and the width floor rejects a neighbour's stray stroke too thin to be
// the run's first glyph. A column counts as contrast when its share of pixels beyond
// inkContrastThreshold from bg reaches inkCoverageFloor. The band is read in RGBA
// channel order.
func contrastOnset(img *image.RGBA, bg colourRGB, col, minCells int, deviceCellW float64, yMin, yMax int, bounds image.Rectangle) bool {
	x0, _ := cellXRange(col, deviceCellW, bounds)
	if !columnHasContrast(img, bg, x0, yMin, yMax, bounds) {
		return false
	}
	// Measure the unbroken contrast run starting at the onset column, so a single
	// contrast speckle (run below inkRunLength) does not fire and the extent floor
	// measures the true painted width.
	extentEnd := int(math.Ceil(float64(col+minCells) * deviceCellW))
	if extentEnd > bounds.Max.X {
		extentEnd = bounds.Max.X
	}
	runLen := 0
	extent := 0
	for x := x0; x < bounds.Max.X; x++ {
		if !columnHasContrast(img, bg, x, yMin, yMax, bounds) {
			break
		}
		runLen++
		if x < extentEnd {
			extent++
		}
	}
	if runLen < inkRunLength {
		return false
	}
	// The onset's contrast must reach across the run's first grapheme cell. A run
	// narrower than that is a stray edge or a neighbour's descender, not the run's
	// paint origin.
	need := extentEnd - x0
	return extent >= need
}

// columnHasContrast reports whether device-pixel column x carries contrast ink: its
// share of pixels in [yMin,yMax) beyond inkContrastThreshold from bg reaches
// inkCoverageFloor. A column outside bounds reads as no contrast. The band is read in
// RGBA channel order.
func columnHasContrast(img *image.RGBA, bg colourRGB, x, yMin, yMax int, bounds image.Rectangle) bool {
	if x < bounds.Min.X || x >= bounds.Max.X || yMin >= yMax {
		return false
	}
	total := yMax - yMin
	hits := 0
	for y := yMin; y < yMax; y++ {
		off := img.PixOffset(x, y)
		if colourDistance(img.Pix[off], img.Pix[off+1], img.Pix[off+2], bg.R, bg.G, bg.B) >= inkContrastThreshold {
			hits++
		}
	}
	return float64(hits)/float64(total) >= inkCoverageFloor
}

// colourRGB is the red, green, blue triple the ink scan matches against, dropping
// the alpha the RGBA foreground carries. render.GlyphRun.FG is color.RGBA, whose R,
// G, B fields feed this directly.
type colourRGB struct {
	R, G, B uint8
}

// bandBackground estimates the dominant background colour of a device-pixel band by
// averaging its pixels. A text band is mostly background with a thin glyph stroke,
// so the mean sits near the background colour; the contrast fallback compares each
// band pixel against it. It samples every pixel in the band, which is a few cells
// tall and a full row wide, once per run at publish. The band is read in RGBA channel
// order.
func bandBackground(img *image.RGBA, bounds image.Rectangle, yMin, yMax int) colourRGB {
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
		return colourRGB{}
	}
	return colourRGB{
		R: meanByte(rSum, n),
		G: meanByte(gSum, n),
		B: meanByte(bSum, n),
	}
}

// meanByte returns sum/count as a byte. Each summed pixel channel is 0..255, so the
// mean is 0..255 and fits a uint8; the explicit 0xff mask makes that bound visible
// to the overflow check.
func meanByte(sum, count uint64) uint8 {
	return uint8((sum / count) & 0xff)
}

// anchorable reports whether a run's text is a left-anchored, one-cell-per-glyph
// run the ink scan can place by its leftmost inked cell. It returns false for a run
// carrying any right-to-left character (Arabic, Hebrew, and the other RTL scripts)
// or any wide grapheme (CJK, whose cluster advances are not one cell), since the
// scan's paint-origin assumption does not hold for them. Latin, digits, and
// punctuation return true, so the pass targets the LTR overlay text where a
// DOMSnapshot box and the paint disagree.
func anchorable(text string) bool {
	for _, r := range text {
		if isRTL(r) || runewidth.RuneWidth(r) >= 2 {
			return false
		}
	}
	return true
}

// isRTL reports whether a rune belongs to a right-to-left script. It covers the
// scripts the corpus exercises (Arabic and Hebrew) plus the other common RTL
// scripts, so a mixed run with any RTL character skips anchoring.
func isRTL(r rune) bool {
	return unicode.In(r, unicode.Arabic, unicode.Hebrew, unicode.Syriac, unicode.Thaana, unicode.Nko)
}

// colourDistance returns the squared Euclidean distance between two RGB colours in
// 0..255 channel space. Squared distance avoids a square root and compares directly
// against the squared thresholds inkColourTolerance and inkContrastThreshold.
func colourDistance(r1, g1, b1, r2, g2, b2 uint8) int {
	dr := int(r1) - int(r2)
	dg := int(g1) - int(g2)
	db := int(b1) - int(b2)
	return dr*dr + dg*dg + db*db
}
