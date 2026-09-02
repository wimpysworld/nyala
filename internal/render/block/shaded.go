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
	// ShadedANSITonal quantises to the 16 canonical ANSI colours by Rec.601
	// luma, through ANSI16Tonal.
	ShadedANSITonal
	// ShadedMono scores glyph coverage against sample luma with no colour
	// quantisation; the monochrome writer emits no colour SGR. The model
	// assumes a light-on-dark terminal, matching the binarise schemes, whose
	// bright sub-cells also map to glyph ink.
	ShadedMono
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
// half-solid texture reads as visual noise.
var shadedCandidates = [...]rune{
	fullBlock, // U+2588 full block
	0x0020,    // space
	0x2580,    // upper half block
	0x2584,    // lower half block
	0x258C,    // left half block
	0x2590,    // right half block
	0x1FB8C,   // left half medium shade
	0x1FB8D,   // right half medium shade
	0x1FB8E,   // upper half medium shade
	0x1FB8F,   // lower half medium shade
	0x1FB91,   // upper half block and lower half inverse medium shade
	0x1FB92,   // upper half inverse medium shade and lower half block
	0x1FB94,   // left half inverse medium shade and right half block
}

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

// shadedMonoInk is the modelled luma of full glyph ink for monochrome scoring:
// the integer Rec.601 luma of opaque white, matching luma's fixed-point scale.
const shadedMonoInk = (lumaWeightR + lumaWeightG + lumaWeightB) * 255

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
// Monochrome returns the colour unchanged, because its writer emits no SGR and
// its scoring runs on luma coverage instead.
func shadedQuantise(c color.RGBA, pal ShadedPalette) color.RGBA {
	switch pal {
	case ShadedXTermGrey:
		return XTermGreyRGBA(XTermGrey(c))
	case ShadedANSITonal:
		return ANSI16RGBA(ANSI16Tonal(c))
	case ShadedMono:
		return c
	default:
		return XTerm256RGBA(xterm256Stable[XTerm256(c)])
	}
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
// samples and returns the lowest-error cell. Per candidate it estimates the
// foreground from the covered samples and the background from the uncovered
// samples (weighted by the coverage table), quantises both to the active
// palette, then scores the displayed result coverage*FG + (1-coverage)*BG
// against the samples. This derives two colours per candidate rather than
// searching palette pairs, so the work stays proportional to candidate count
// times eight samples. Monochrome scores modelled ink luma against sample
// luma. An exactly flat cell under a colour palette short-circuits to a full
// block with foreground equal to background, so flat regions never gain shade
// texture; monochrome keeps full scoring, because rendering flat tone as
// coverage is that mode's purpose. Equal-error ties keep the earliest
// candidate, so the result is deterministic and solid glyphs win ties.
func shadedCell(s *[8]color.RGBA, pal ShadedPalette) Cell {
	if pal != ShadedMono && shadedFlat(s) {
		q := shadedQuantise(s[0], pal)
		return Cell{Glyph: fullBlock, FG: q, BG: q}
	}

	var lumas [8]int
	if pal == ShadedMono {
		for i := range s {
			lumas[i] = luma(s[i])
		}
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
	for ci := range shadedCandidates {
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
		if pal == ShadedMono {
			for i := range s {
				d := int(cov[i])*shadedMonoInk/shadedBinMax - lumas[i]
				errSum += int64(d) * int64(d)
			}
		} else {
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
		}
		if bestErr < 0 || errSum < bestErr {
			bestErr = errSum
			best = Cell{Glyph: shadedCandidates[ci], FG: fq, BG: bq}
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
