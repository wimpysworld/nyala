package block

import (
	"image"
	"image/color"

	"github.com/wimpysworld/nyala/internal/render"
)

// shadedCellPxW and shadedCellPxH are the source-pixel footprint of one cell in
// the Shaded scheme: the same 2x4 block the quadrant scheme captures. Each of
// the eight source pixels is one coverage bin, read individually rather than
// averaged into four quadrant colours.
const (
	shadedCellPxW = 2
	shadedCellPxH = 4
)

// fullBlock is the U+2588 full-block glyph. A flat Shaded cell emits it with
// foreground equal to background, matching the quadrant flat-cell convention.
const fullBlock = 0x2588

// lightShade, mediumShade, and darkShade are the Block Elements full-cell
// dither glyphs U+2591..U+2593, drawn by terminals as 25, 50, and 75 per cent
// foreground dot patterns since CP437. The Shaded scheme uses them to blend
// two palette entries across a whole cell, emulating tones between palette
// steps.
const (
	lightShade  = 0x2591
	mediumShade = 0x2592
	darkShade   = 0x2593
)

// ShadedPalette selects the output palette the Shaded scheme quantises
// candidate colours to before scoring, so the scored cell is the cell the
// terminal displays. Only the Shaded scheme reads it; truecolour is absent by
// design, because the Shaded scheme is a low-colour mode.
type ShadedPalette int

const (
	// ShadedXTerm256 quantises to the canonical xterm-256 palette, indices
	// 16..255, through XTerm256. It is the zero value.
	ShadedXTerm256 ShadedPalette = iota
	// ShadedXTermGrey quantises to the fixed xterm greyscale ramp, indices
	// 232..255, through XTermGrey.
	ShadedXTermGrey
)

// shadedLatticeW and shadedLatticeH are the unit-cell sample lattice the
// coverage model is defined on: 8 columns by 16 rows, so each 2x4 coverage bin
// holds a 4x4 sub-lattice and the Route A 8x16 cell raster maps one pixel to
// one lattice point. Lattice point (cx, cy) sits at the continuous cell
// coordinate ((cx+0.5)/8, (cy+0.5)/16).
const (
	shadedLatticeW = 8
	shadedLatticeH = 16
)

// shadedCandidates is the Shaded scheme's candidate glyph set in scoring
// order. Solid candidates come first, so an equal-error tie prefers a solid
// glyph over shade texture. The shade set is the Symbols for Legacy Computing
// rectangular medium-shade glyphs from Unicode 13.0; U+1FB93, the missing
// left-block-and-right-shade mirror, is unassigned in Unicode 17.0, and the
// triangular medium shades U+1FB9C..U+1FB9F are excluded because their
// half-solid texture reads as visual noise. The trailing full-cell dither
// candidates score through bracketing palette pairs (shadedDithers), so
// shadedCell iterates only the first shadedSpatialCount entries spatially.
var shadedCandidates = [...]rune{
	fullBlock,   // U+2588 full block
	0x0020,      // space
	0x2580,      // upper half block
	0x2584,      // lower half block
	0x258C,      // left half block
	0x2590,      // right half block
	0x1FB8C,     // left half medium shade
	0x1FB8D,     // right half medium shade
	0x1FB8E,     // upper half medium shade
	0x1FB8F,     // lower half medium shade
	0x1FB91,     // upper half block and lower half inverse medium shade
	0x1FB92,     // upper half inverse medium shade and lower half block
	0x1FB94,     // left half inverse medium shade and right half block
	lightShade,  // U+2591 light shade, 25 per cent full-cell dither
	mediumShade, // U+2592 medium shade, 50 per cent full-cell dither
	darkShade,   // U+2593 dark shade, 75 per cent full-cell dither
}

// shadedDithers lists the full-cell dither candidates in scoring order with
// their uniform per-bin coverage weight in 32nds. Their coverage is the same
// in every bin, so the covered and uncovered spatial means both collapse to
// the cell mean; shadedCell scores them through bracketing palette pairs
// instead.
var shadedDithers = [...]struct {
	glyph rune
	w     int // uniform per-bin coverage weight, 0..shadedBinMax
}{
	{lightShade, shadedBinMax / 4},
	{mediumShade, shadedBinMax / 2},
	{darkShade, 3 * shadedBinMax / 4},
}

// shadedSpatialCount is the number of leading shadedCandidates entries scored
// through the covered/uncovered spatial means. The remaining entries are the
// dither candidates.
const shadedSpatialCount = len(shadedCandidates) - len(shadedDithers)

// ShadedCandidates returns the Shaded scheme's candidate glyph set in scoring
// order. The test card tool and the raster tests iterate it, so the displayed
// set and the composed set stay one list.
func ShadedCandidates() []rune {
	return append([]rune(nil), shadedCandidates[:]...)
}

// ShadedOpacity returns twice the ink opacity (0 background, 1 medium shade,
// 2 solid) of a Shaded candidate glyph at lattice point (cx, cy) on the 8x16
// unit-cell lattice, and whether glyph is a Shaded candidate. The coverage
// tables and the Route A procedural raster both read this one geometry, so the
// scored model and the rasterised proof agree pixel for pixel. Unicode names
// the geometry and the shade type but not a numeric opacity; the 0.5 medium
// shade is Nyala's model, not a Unicode promise.
func ShadedOpacity(glyph rune, cx, cy int) (int, bool) {
	if cx < 0 || cx >= shadedLatticeW || cy < 0 || cy >= shadedLatticeH {
		return 0, false
	}
	switch glyph {
	case fullBlock:
		return 2, true
	case 0x0020:
		return 0, true
	case 0x2580:
		return ifInk(cy < 8, 2), true
	case 0x2584:
		return ifInk(cy >= 8, 2), true
	case 0x258C:
		return ifInk(cx < 4, 2), true
	case 0x2590:
		return ifInk(cx >= 4, 2), true
	case 0x1FB8C:
		return ifInk(cx < 4, 1), true
	case 0x1FB8D:
		return ifInk(cx >= 4, 1), true
	case 0x1FB8E:
		return ifInk(cy < 8, 1), true
	case 0x1FB8F:
		return ifInk(cy >= 8, 1), true
	case 0x1FB91:
		return 2 - ifInk(cy >= 8, 1), true
	case 0x1FB92:
		return 2 - ifInk(cy < 8, 1), true
	case 0x1FB94:
		return 2 - ifInk(cx < 4, 1), true
	case lightShade:
		return ifInk(cx%2 == 0 && cy%2 == 0, 2), true
	case mediumShade:
		return ifInk((cx+cy)%2 == 0, 2), true
	case darkShade:
		return ifInk(cx%2 == 1 || cy%2 == 1, 2), true
	}
	return 0, false
}

// ifInk maps a geometry predicate to an opacity contribution.
func ifInk(inside bool, opacity int) int {
	if inside {
		return opacity
	}
	return 0
}

// shadedBinMax is the maximum coverage weight of one 2x4 bin: 16 lattice
// points at solid opacity 2. Weights are integer 32nds, so all scoring stays
// in integer arithmetic and is byte-identical across architectures.
const shadedBinMax = 32

// shadedCoverage holds one 8-entry coverage table per candidate, bin i =
// dy*2 + dx in the cell's 2x4 source block, each weight in 0..shadedBinMax.
// buildShadedCoverage folds the 4x4 sub-lattice of each bin, so the table is
// the supersampled form of ShadedOpacity and diagonal bins carry fractional
// coverage rather than aliasing on bin centres.
var shadedCoverage = buildShadedCoverage()

func buildShadedCoverage() [len(shadedCandidates)][8]uint8 {
	var t [len(shadedCandidates)][8]uint8
	for ci, glyph := range shadedCandidates {
		for cy := range shadedLatticeH {
			for cx := range shadedLatticeW {
				op, _ := ShadedOpacity(glyph, cx, cy)
				bin := (cy/4)*2 + cx/4
				t[ci][bin] += uint8(op) //nolint:gosec // sixteen points at opacity two sum to 32
			}
		}
	}
	return t
}

// xterm256Stable maps each canonical xterm-256 index 16..255 to the stable
// index whose canonical colour the perceptual LUT maps back to itself. A few
// canonical colours fall in a 5-bit LUT bucket owned by a perceptual
// neighbour, so a single quantise would score one colour and display another;
// iterating to the fixed point keeps the scored cell and the emitted SGR on
// one colour.
var xterm256Stable = buildXTerm256Stable()

func buildXTerm256Stable() [256]uint8 {
	var t [256]uint8
	for i := 16; i < 256; i++ {
		idx := uint8(i) //nolint:gosec // loop bound keeps i in byte range
		for range 4 {
			next := XTerm256(XTerm256RGBA(idx))
			if next == idx {
				break
			}
			idx = next
		}
		t[i] = idx
	}
	return t
}

// shadedQuantise maps an estimated cell colour onto the active output palette
// and returns that palette entry's canonical RGBA, the palette-before-score
// rule: the scored displayed result is built from colours the writer can emit.
func shadedQuantise(c color.RGBA, pal ShadedPalette) color.RGBA {
	if pal == ShadedXTermGrey {
		return XTermGreyRGBA(XTermGrey(c))
	}
	return XTerm256RGBA(xterm256Stable[XTerm256(c)])
}

// shadedNeutralSpread is the channel spread at or below which a cell mean
// counts as near-neutral, adding the grey-ramp bracket as an alternative
// dither pair under the xterm-256 palette. The gate is a cost filter, not a
// selector: the scoring contest still arbitrates between the cube and ramp
// pairs, so the threshold only skips ramp pairs a chromatic mean makes
// clearly unwinnable. 32 sits under the cube's smallest step (40), so any
// mean the ramp could plausibly serve passes.
const shadedNeutralSpread = 32

// cubeBracketStep returns the 0..5 xterm colour-cube steps whose levels
// bracket channel value v: cubeLevel(lo) <= v <= cubeLevel(hi). A value at or
// above the top level returns the top step twice.
func cubeBracketStep(v uint8) (lo, hi int) {
	if v < 95 {
		return 0, 1
	}
	i := int(v-95)/40 + 1
	if i >= 5 {
		return 5, 5
	}
	return i, i + 1
}

// shadedGreyBracket returns the adjacent xterm greyscale ramp entries
// bracketing the mean's Rec.601 grey level, brighter entry first, keeping the
// rule that grey output uses only ramp entries. Levels outside the ramp clamp
// to its nearest pair.
func shadedGreyBracket(mean color.RGBA) (hi, lo color.RGBA) {
	const (
		base = 8 * 256
		step = 10 * 256
	)
	li := 0
	if l := luma(mean); l > base {
		li = min((l-base)/step, 23)
	}
	return XTermGreyRGBA(uint8(232 + min(li+1, 23))), XTermGreyRGBA(uint8(232 + li)) //nolint:gosec // li is clamped to 0..23
}

// shadedDitherPairs fills pairs with up to two bracketing palette pairs for
// the full-cell dither candidates, brighter entry first, and returns how many
// it wrote. The derivation is O(1) from the palette lattice: under the
// greyscale palette the single pair is the adjacent 24-step ramp entries
// around the mean's grey level; under xterm-256 the first pair brackets each
// channel on the 6x6x6 cube lattice, and a near-neutral mean adds the grey
// ramp bracket, whose 10-step pitch is far finer than the cube near the
// neutral axis. Every entry passes through shadedQuantise, so the scored pair
// is writer-emittable, the palette-before-score rule.
func shadedDitherPairs(mean color.RGBA, pal ShadedPalette, pairs *[2][2]color.RGBA) int {
	if pal == ShadedXTermGrey {
		hi, lo := shadedGreyBracket(mean)
		pairs[0] = [2]color.RGBA{shadedQuantise(hi, pal), shadedQuantise(lo, pal)}
		return 1
	}
	rLo, rHi := cubeBracketStep(mean.R)
	gLo, gHi := cubeBracketStep(mean.G)
	bLo, bHi := cubeBracketStep(mean.B)
	pairs[0] = [2]color.RGBA{
		shadedQuantise(color.RGBA{R: cubeLevel(rHi), G: cubeLevel(gHi), B: cubeLevel(bHi), A: 0xff}, pal),
		shadedQuantise(color.RGBA{R: cubeLevel(rLo), G: cubeLevel(gLo), B: cubeLevel(bLo), A: 0xff}, pal),
	}
	spread := int(max(mean.R, mean.G, mean.B)) - int(min(mean.R, mean.G, mean.B))
	if spread > shadedNeutralSpread {
		return 1
	}
	hi, lo := shadedGreyBracket(mean)
	pairs[1] = [2]color.RGBA{shadedQuantise(hi, pal), shadedQuantise(lo, pal)}
	return 2
}

// shadedFlat reports whether every source sample of the cell is one colour.
func shadedFlat(s *[8]color.RGBA) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

// shadedCell scores every candidate glyph against the cell's eight source
// samples and returns the lowest-error cell. Per spatial candidate it
// estimates the foreground from the covered samples and the background from
// the uncovered samples (weighted by the coverage table), quantises both to
// the active palette, then scores the displayed result coverage*FG +
// (1-coverage)*BG against the samples. This derives two colours per candidate
// rather than searching palette pairs, so the work stays proportional to
// candidate count times eight samples. The full-cell dither candidates score
// after the spatial set: their pair comes from the palette lattice brackets
// around the cell mean (shadedDitherPairs), blended at the glyph's uniform
// coverage ratio, so a tone between two palette steps can win as a dithered
// blend. An exactly flat cell short-circuits to a full block with foreground
// equal to background, so flat regions never gain shade texture. Equal-error
// ties keep the earliest candidate, so the result is deterministic and solid
// glyphs win ties.
func shadedCell(s *[8]color.RGBA, pal ShadedPalette) Cell {
	if shadedFlat(s) {
		q := shadedQuantise(s[0], pal)
		return Cell{Glyph: fullBlock, FG: q, BG: q}
	}

	// The background sums derive from the whole-cell totals, so each candidate
	// accumulates only its weighted foreground side.
	var trs, tgs, tbs int
	for i := range s {
		trs += int(s[i].R)
		tgs += int(s[i].G)
		tbs += int(s[i].B)
	}

	best := Cell{}
	bestErr := int64(-1)
	for ci := range shadedCandidates[:shadedSpatialCount] {
		cov := &shadedCoverage[ci]
		var frs, fgs, fbs, fw int
		for i := range s {
			w := int(cov[i])
			frs += w * int(s[i].R)
			fgs += w * int(s[i].G)
			fbs += w * int(s[i].B)
			fw += w
		}
		brs := trs*shadedBinMax - frs
		bgs := tgs*shadedBinMax - fgs
		bbs := tbs*shadedBinMax - fbs
		bw := len(s)*shadedBinMax - fw
		var fg, bg color.RGBA
		switch {
		case fw == 0:
			bg = avgChannels(brs/shadedBinMax, bgs/shadedBinMax, bbs/shadedBinMax, len(s))
			fg = bg
		case bw == 0:
			fg = avgChannels(frs/shadedBinMax, fgs/shadedBinMax, fbs/shadedBinMax, len(s))
			bg = fg
		default:
			fg = color.RGBA{R: clampByte(frs / fw), G: clampByte(fgs / fw), B: clampByte(fbs / fw), A: 0xff}
			bg = color.RGBA{R: clampByte(brs / bw), G: clampByte(bgs / bw), B: clampByte(bbs / bw), A: 0xff}
		}
		fq := shadedQuantise(fg, pal)
		bq := shadedQuantise(bg, pal)

		var errSum int64
		for i := range s {
			w := int(cov[i])
			dr := (w*int(fq.R) + (shadedBinMax-w)*int(bq.R) + shadedBinMax/2) / shadedBinMax
			dg := (w*int(fq.G) + (shadedBinMax-w)*int(bq.G) + shadedBinMax/2) / shadedBinMax
			db := (w*int(fq.B) + (shadedBinMax-w)*int(bq.B) + shadedBinMax/2) / shadedBinMax
			dr -= int(s[i].R)
			dg -= int(s[i].G)
			db -= int(s[i].B)
			errSum += int64(dr*dr + dg*dg + db*db)
		}
		if bestErr < 0 || errSum < bestErr {
			bestErr = errSum
			best = Cell{Glyph: shadedCandidates[ci], FG: fq, BG: bq}
		}
	}

	// Full-cell dither candidates: bracket the cell mean on the palette
	// lattice, then score each glyph's uniform blend ratio*FG + (1-ratio)*BG.
	// The displayed colour is constant across the cell, so it computes once
	// per candidate and pair. The strict comparison keeps the spatial
	// candidates, which come earlier in scoring order, ahead on ties.
	var pairs [2][2]color.RGBA
	n := shadedDitherPairs(avgChannels(trs, tgs, tbs, len(s)), pal, &pairs)
	for _, d := range shadedDithers {
		for _, p := range pairs[:n] {
			fq, bq := p[0], p[1]
			dr := (d.w*int(fq.R) + (shadedBinMax-d.w)*int(bq.R) + shadedBinMax/2) / shadedBinMax
			dg := (d.w*int(fq.G) + (shadedBinMax-d.w)*int(bq.G) + shadedBinMax/2) / shadedBinMax
			db := (d.w*int(fq.B) + (shadedBinMax-d.w)*int(bq.B) + shadedBinMax/2) / shadedBinMax
			var errSum int64
			for i := range s {
				er := dr - int(s[i].R)
				eg := dg - int(s[i].G)
				eb := db - int(s[i].B)
				errSum += int64(er*er + eg*eg + eb*eb)
			}
			if errSum < bestErr {
				bestErr = errSum
				best = Cell{Glyph: d.glyph, FG: fq, BG: bq}
			}
		}
	}
	return best
}

// composeShaded scores the frame image onto a fresh Cols x Rows Shaded grid.
// An empty or nil image yields an all-black grid: every sample reads opaque
// black, a flat cell, so the full block with black foreground and background.
func (br *Renderer) composeShaded(f render.Frame) Grid {
	g := Grid{
		Cols:  br.Cols,
		Rows:  br.Rows,
		Cells: make([]Cell, br.Cols*br.Rows),
	}
	br.composeShadedInto(&g, f)
	return g
}

// composeShadedInto scores the frame image onto g, which the caller has
// already sized to Cols x Rows. It is the shared body of composeShaded (fresh
// grid) and ComposeInto (reused grid), so both fill cells the same way.
func (br *Renderer) composeShadedInto(g *Grid, f render.Frame) {
	br.composeShadedRows(g, f, 0, g.Rows)
}

// composeShadedRows scores grid rows [row0, row1) of g from the frame image.
// It is the per-band body of composeShadedInto: each cell reads its own 2x4
// source block and writes only its own cell, so a call over a disjoint row
// range writes byte-for-byte the same cells the whole-grid pass writes for
// those rows and the parallel band-join stays byte-identical to the serial
// pass. The palette rides the renderer value, so every band scores with one
// palette.
func (br *Renderer) composeShadedRows(g *Grid, f render.Frame, row0, row1 int) {
	img, _ := f.Img.(*image.RGBA)
	for row := row0; row < row1; row++ {
		for col := range g.Cols {
			var s [8]color.RGBA
			x0 := col * shadedCellPxW
			y0 := row * shadedCellPxH
			for dy := range shadedCellPxH {
				for dx := range shadedCellPxW {
					r, gc, b := pixel(img, x0+dx, y0+dy)
					s[dy*shadedCellPxW+dx] = color.RGBA{R: r, G: gc, B: b, A: 0xff}
				}
			}
			*g.At(col, row) = shadedCell(&s, br.Palette)
		}
	}
}
