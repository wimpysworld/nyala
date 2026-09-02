package block

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/rowband"
)

var (
	shadedWhite = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	shadedBlack = color.RGBA{A: 0xff}
)

// shadedSamples builds one cell's eight samples from a per-position function
// over the 2x4 source block, in the compose sample order dy*2 + dx.
func shadedSamples(at func(dx, dy int) color.RGBA) [8]color.RGBA {
	var s [8]color.RGBA
	for dy := range 4 {
		for dx := range 2 {
			s[dy*2+dx] = at(dx, dy)
		}
	}
	return s
}

// TestShadedCoverageTables pins the coverage model: every bin weight is within
// 0..32, the flat candidates hit the extremes, the shade halves carry exactly
// half opacity on their covered side, and each triangle covers exactly half
// the cell at medium shade.
func TestShadedCoverageTables(t *testing.T) {
	totals := map[rune]int{}
	for ci, glyph := range shadedCandidates {
		total := 0
		for bin, w := range shadedCoverage[ci] {
			if w > shadedBinMax {
				t.Fatalf("glyph %U bin %d weight %d exceeds %d", glyph, bin, w, shadedBinMax)
			}
			total += int(w)
		}
		totals[glyph] = total
	}
	if totals[fullBlock] != 8*shadedBinMax || totals[0x0020] != 0 {
		t.Fatalf("flat candidates: full %d, space %d", totals[fullBlock], totals[0x0020])
	}
	for _, glyph := range []rune{0x1FB8C, 0x1FB8D, 0x1FB8E, 0x1FB8F} {
		if totals[glyph] != 8*shadedBinMax/4 {
			t.Fatalf("shade half %U total %d, want %d", glyph, totals[glyph], 8*shadedBinMax/4)
		}
	}
	for _, glyph := range []rune{0x1FB9C, 0x1FB9D, 0x1FB9E, 0x1FB9F} {
		if totals[glyph] != 8*shadedBinMax/4 {
			t.Fatalf("triangle %U total %d, want half-cell medium shade %d", glyph, totals[glyph], 8*shadedBinMax/4)
		}
	}
	for _, glyph := range []rune{0x1FB91, 0x1FB92, 0x1FB94} {
		if totals[glyph] != 8*shadedBinMax*3/4 {
			t.Fatalf("combined glyph %U total %d, want %d", glyph, totals[glyph], 8*shadedBinMax*3/4)
		}
	}
}

// TestShadedOpacityRejectsNonCandidates keeps the geometry seam honest: a rune
// outside the candidate set and a lattice point outside the cell both answer
// not-a-candidate.
func TestShadedOpacityRejectsNonCandidates(t *testing.T) {
	if _, ok := ShadedOpacity(0x1FB93, 0, 0); ok {
		t.Fatal("U+1FB93 is unassigned and must not be a candidate")
	}
	if _, ok := ShadedOpacity(fullBlock, shadedLatticeW, 0); ok {
		t.Fatal("out-of-lattice point must not report a candidate")
	}
}

// TestShadedCellRectangularPolarity pins the solid-split selections: a
// vertical split picks the upper-half block in both polarities (it precedes
// the lower half in scoring order and both model the cell exactly), and a
// horizontal split picks the left-half block, each with the exact canonical
// palette colours on the right sides.
func TestShadedCellRectangularPolarity(t *testing.T) {
	cases := []struct {
		name   string
		at     func(dx, dy int) color.RGBA
		glyph  rune
		fg, bg color.RGBA
	}{
		{"top white", func(_, dy int) color.RGBA {
			if dy < 2 {
				return shadedWhite
			}
			return shadedBlack
		}, 0x2580, shadedWhite, shadedBlack},
		{"bottom white", func(_, dy int) color.RGBA {
			if dy >= 2 {
				return shadedWhite
			}
			return shadedBlack
		}, 0x2580, shadedBlack, shadedWhite},
		{"left white", func(dx, _ int) color.RGBA {
			if dx == 0 {
				return shadedWhite
			}
			return shadedBlack
		}, 0x258C, shadedWhite, shadedBlack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := shadedSamples(tc.at)
			cell := shadedCell(&s, ShadedXTerm256)
			if cell.Glyph != tc.glyph || cell.FG != tc.fg || cell.BG != tc.bg {
				t.Fatalf("cell = %U fg %v bg %v, want %U fg %v bg %v", cell.Glyph, cell.FG, cell.BG, tc.glyph, tc.fg, tc.bg)
			}
		})
	}
}

// TestShadedCellFlatFallback pins the flat-cell rule under a colour palette: a
// one-colour cell emits the full block with foreground equal to background on
// the canonical palette entry, never shade texture.
func TestShadedCellFlatFallback(t *testing.T) {
	flat := color.RGBA{R: 0x84, G: 0x84, B: 0x84, A: 0xff}
	s := shadedSamples(func(_, _ int) color.RGBA { return flat })
	for _, pal := range []ShadedPalette{ShadedXTerm256, ShadedXTermGrey, ShadedANSITonal} {
		cell := shadedCell(&s, pal)
		if cell.Glyph != fullBlock || cell.FG != cell.BG {
			t.Fatalf("palette %d flat cell = %U fg %v bg %v, want full block with fg == bg", pal, cell.Glyph, cell.FG, cell.BG)
		}
		if q := shadedQuantise(cell.FG, pal); q != cell.FG {
			t.Fatalf("palette %d flat colour %v is not a canonical palette entry", pal, cell.FG)
		}
	}
}

// TestShadedCellMonoTone pins the monochrome coverage model: flat black reads
// as the space glyph, flat white as the full block, and flat mid grey as a
// medium-shade candidate rather than either flat extreme, because coverage is
// the only tone monochrome has.
func TestShadedCellMonoTone(t *testing.T) {
	flat := func(c color.RGBA) [8]color.RGBA {
		return shadedSamples(func(_, _ int) color.RGBA { return c })
	}
	black := flat(shadedBlack)
	if cell := shadedCell(&black, ShadedMono); cell.Glyph != 0x0020 {
		t.Fatalf("mono flat black = %U, want space", cell.Glyph)
	}
	white := flat(shadedWhite)
	if cell := shadedCell(&white, ShadedMono); cell.Glyph != fullBlock {
		t.Fatalf("mono flat white = %U, want full block", cell.Glyph)
	}
	grey := flat(color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff})
	cell := shadedCell(&grey, ShadedMono)
	if cell.Glyph == 0x0020 || cell.Glyph == fullBlock {
		t.Fatalf("mono flat mid grey = %U, want a shade candidate carrying tone", cell.Glyph)
	}
	if _, ok := ShadedOpacity(cell.Glyph, 0, 0); !ok {
		t.Fatalf("mono mid grey picked %U, which is not a candidate", cell.Glyph)
	}
}

// TestShadedCellDiagonalPolarity pins the triangular selections in monochrome:
// a soft diagonal tone field oriented at each corner picks that corner's
// triangular medium shade, which the axis-aligned candidates cannot model.
func TestShadedCellDiagonalPolarity(t *testing.T) {
	// mix returns the displayed tone of a triangle's own coverage, so each
	// fixture is the exact field its triangle models: covered samples at half
	// tone, uncovered at black.
	tone := func(covered bool) color.RGBA {
		if covered {
			return color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff}
		}
		return shadedBlack
	}
	cases := []struct {
		name  string
		at    func(dx, dy int) color.RGBA
		glyph rune
	}{
		{"upper left", func(dx, dy int) color.RGBA { return tone(2*dx+dy <= 2) }, 0x1FB9C},
		{"upper right", func(dx, dy int) color.RGBA { return tone(dy <= 2*dx) }, 0x1FB9D},
		{"lower right", func(dx, dy int) color.RGBA { return tone(2*dx+dy >= 3) }, 0x1FB9E},
		{"lower left", func(dx, dy int) color.RGBA { return tone(dy >= 2*dx+1) }, 0x1FB9F},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := shadedSamples(tc.at)
			cell := shadedCell(&s, ShadedMono)
			if cell.Glyph != tc.glyph {
				t.Fatalf("cell = %U, want %U", cell.Glyph, tc.glyph)
			}
		})
	}
}

// TestShadedPaletteRoundTrips proves the palette-before-score coherence: every
// colour the compose stores maps back to the same palette entry at the writer,
// so the scored cell and the emitted SGR are one colour. It also pins the
// canonical ANSI and greyscale tables against their index converters.
func TestShadedPaletteRoundTrips(t *testing.T) {
	for i := range 16 {
		idx := uint8(i)
		if got := ANSI16Tonal(ANSI16RGBA(idx)); got != idx {
			t.Fatalf("ANSI16Tonal(ANSI16RGBA(%d)) = %d", idx, got)
		}
	}
	for i := 232; i < 256; i++ {
		idx := uint8(i)
		if got := XTermGrey(XTermGreyRGBA(idx)); got != idx {
			t.Fatalf("XTermGrey(XTermGreyRGBA(%d)) = %d", idx, got)
		}
	}
	for i := 16; i < 256; i++ {
		stable := xterm256Stable[i]
		if got := XTerm256(XTerm256RGBA(stable)); got != stable {
			t.Fatalf("stable index %d for %d is not a fixed point: maps to %d", stable, i, got)
		}
	}
	rng := rand.New(rand.NewSource(51))
	for range 4096 {
		c := color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 0xff}
		for _, pal := range []ShadedPalette{ShadedXTerm256, ShadedXTermGrey, ShadedANSITonal} {
			q := shadedQuantise(c, pal)
			if again := shadedQuantise(q, pal); again != q {
				t.Fatalf("palette %d quantise of %v is not idempotent: %v then %v", pal, c, q, again)
			}
		}
	}
}

// TestShadedComposeParallelByteIdentity proves the parallel Shaded compose is
// byte-identical to the serial compose for every palette, on a noisy source
// large enough to split into several bands.
func TestShadedComposeParallelByteIdentity(t *testing.T) {
	const cols, rows = 48, 24
	img := image.NewRGBA(image.Rect(0, 0, cols*2, rows*4))
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = uint8(rng.Intn(256))
		img.Pix[i+1] = uint8(rng.Intn(256))
		img.Pix[i+2] = uint8(rng.Intn(256))
		img.Pix[i+3] = 0xff
	}
	frame := render.Frame{Img: img}
	pool := rowband.New()
	pc := NewParallelCompose()
	for _, pal := range []ShadedPalette{ShadedXTerm256, ShadedXTermGrey, ShadedANSITonal, ShadedMono} {
		br := Renderer{Cols: cols, Rows: rows, Scheme: SchemeShaded, Palette: pal}
		serial := br.Compose(frame)
		var parallel Grid
		pc.ComposeInto(&br, &parallel, frame, pool)
		if serial.Cols != parallel.Cols || serial.Rows != parallel.Rows {
			t.Fatalf("palette %d grid shape mismatch", pal)
		}
		for i := range serial.Cells {
			if serial.Cells[i] != parallel.Cells[i] {
				t.Fatalf("palette %d cell %d: serial %+v parallel %+v", pal, i, serial.Cells[i], parallel.Cells[i])
			}
		}
	}
}

// TestShadedYCbCrMatchesRGBA proves the direct native-YCbCr Shaded compose
// equals the RGBA compose fed the converted image, so the production JPEG path
// and the diagnostics PNG path draw one scheme. A full-size 4:4:4 source makes
// the sampler one-to-one, so every sample converts identically and the grids
// must match cell for cell.
func TestShadedYCbCrMatchesRGBA(t *testing.T) {
	const cols, rows = 16, 8
	ycbcr := image.NewYCbCr(image.Rect(0, 0, cols*2, rows*4), image.YCbCrSubsampleRatio444)
	rng := rand.New(rand.NewSource(11))
	for y := 0; y < rows*4; y++ {
		for x := 0; x < cols*2; x++ {
			yi := ycbcr.YOffset(x, y)
			ci := ycbcr.COffset(x, y)
			ycbcr.Y[yi] = uint8(rng.Intn(256))
			ycbcr.Cb[ci] = uint8(rng.Intn(256))
			ycbcr.Cr[ci] = uint8(rng.Intn(256))
		}
	}
	rgba := image.NewRGBA(ycbcr.Bounds())
	for y := 0; y < rows*4; y++ {
		for x := 0; x < cols*2; x++ {
			r, g, b := color.YCbCrToRGB(ycbcr.Y[ycbcr.YOffset(x, y)], ycbcr.Cb[ycbcr.COffset(x, y)], ycbcr.Cr[ycbcr.COffset(x, y)])
			rgba.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 0xff})
		}
	}
	br := Renderer{Cols: cols, Rows: rows, Scheme: SchemeShaded, Palette: ShadedXTerm256}
	fromYCbCr := br.ComposeYCbCr(render.Frame{Img: ycbcr})
	fromRGBA := br.Compose(render.Frame{Img: rgba})
	for i := range fromRGBA.Cells {
		if fromYCbCr.Cells[i] != fromRGBA.Cells[i] {
			t.Fatalf("cell %d: ycbcr %+v rgba %+v", i, fromYCbCr.Cells[i], fromRGBA.Cells[i])
		}
	}
}

// TestShadedDiffEmitsSMPRunes proves the cell differ writes the four-byte
// UTF-8 form of the Supplementary Multilingual Plane shade glyphs and re-emits
// a cell whose glyph alone changes, so shade texture never sticks.
func TestShadedDiffEmitsSMPRunes(t *testing.T) {
	grid := Grid{Cols: 1, Rows: 1, Cells: []Cell{{Glyph: 0x1FB9C, FG: shadedWhite, BG: shadedBlack}}}
	var d GridDiff
	var out bytes.Buffer
	if err := d.EmitTrueColour(&out, grid); err != nil {
		t.Fatalf("EmitTrueColour: %v", err)
	}
	if !strings.Contains(out.String(), "\U0001FB9C") {
		t.Fatalf("payload %q does not carry the four-byte shade glyph", out.String())
	}
	grid.Cells[0].Glyph = 0x1FB9D
	out.Reset()
	if err := d.EmitTrueColour(&out, grid); err != nil {
		t.Fatalf("EmitTrueColour second frame: %v", err)
	}
	if !strings.Contains(out.String(), "\U0001FB9D") {
		t.Fatalf("glyph-only change was not re-emitted: %q", out.String())
	}
}
