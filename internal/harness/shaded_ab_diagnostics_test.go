//go:build diagnostics

package harness

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// The Shaded A/B evidence: A is the quadrant scheme (the current best
// compatible Block scheme and the mode-matrix control), B is the Shaded
// scheme. Both compose through viewer.State.ComposeBlockFrame, the one
// production compose, and rasterise through Route A, so every number
// here reproduces with:
//
//	go test -count=1 -tags diagnostics ./internal/harness/ -run 'TestShadedAB' -v

// shadedABCols and shadedABRows are the synthetic fixture grid: page cells,
// with one extra terminal row reserved for the bar by the viewer state.
const (
	shadedABCols = 24
	shadedABRows = 12
)

// shadedABModes lists the compatible colour modes the A/B runs under.
var shadedABModes = []struct {
	name string
	mode viewer.ColourMode
}{
	{"256", viewer.ColourXTerm256},
	{"ansi-grey", viewer.ColourXTermGrey},
	{"ansi-tonal", viewer.ColourANSITonal},
}

// shadedABFixture is one synthetic A/B input with its assertion class.
type shadedABFixture struct {
	name string
	img  *image.RGBA
	// shadeSignal marks the shade and diagonal fixtures where the Shaded
	// scheme must produce a genuinely different grid, so an identical A/B
	// output fails the harness rather than passing as a silent no-signal run.
	shadeSignal bool
	// flat marks the exactly flat fixtures where the Shaded scheme must not
	// introduce shade texture under a colour palette.
	flat bool
}

// shadedABImage builds a fixture at the Route A raster extent, so the render
// compares against the source at equal resolution and the compose exercises
// the production downscale.
func shadedABImage(at func(x, y float64) color.RGBA) *image.RGBA {
	w := shadedABCols * cellraster.CellPxW
	h := shadedABRows * cellraster.CellPxH
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, at((float64(x)+0.5)/float64(w), (float64(y)+0.5)/float64(h)))
		}
	}
	return img
}

func greyAB(v float64) color.RGBA {
	b := uint8(math.Round(math.Min(math.Max(v, 0), 1) * 255))
	return color.RGBA{R: b, G: b, B: b, A: 0xff}
}

func shadedABFixtures() []shadedABFixture {
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	black := color.RGBA{A: 0xff}
	blue := color.RGBA{R: 0x20, G: 0x40, B: 0xc0, A: 0xff}
	orange := color.RGBA{R: 0xe0, G: 0x80, B: 0x10, A: 0xff}
	return []shadedABFixture{
		{name: "diag45", shadeSignal: true, img: shadedABImage(func(x, y float64) color.RGBA {
			if x+y <= 1 {
				return white
			}
			return black
		})},
		{name: "corner-triangles", shadeSignal: true, img: shadedABImage(func(x, y float64) color.RGBA {
			if (x < 0.5) == (y < 0.5) {
				if x+y <= 0.5 || x+y >= 1.5 {
					return white
				}
				return black
			}
			if math.Abs(x-y) >= 0.5 {
				return white
			}
			return black
		})},
		{name: "aa-diagonal-ramp", shadeSignal: true, img: shadedABImage(func(x, y float64) color.RGBA {
			return greyAB((x + y) / 2)
		})},
		{name: "shade-levels", shadeSignal: true, img: shadedABImage(func(x, _ float64) color.RGBA {
			levels := []float64{0, 0.25, 0.5, 0.75, 1}
			return greyAB(levels[int(x*float64(len(levels)))%len(levels)])
		})},
		{name: "hi-contrast-diag-border", shadeSignal: true, img: shadedABImage(func(x, y float64) color.RGBA {
			edge := x + y - 1
			const aa = 0.02
			switch {
			case edge < -aa:
				return blue
			case edge > aa:
				return orange
			default:
				t := (edge + aa) / (2 * aa)
				return color.RGBA{
					R: uint8(float64(blue.R) + t*float64(int(orange.R)-int(blue.R))),
					G: uint8(float64(blue.G) + t*float64(int(orange.G)-int(blue.G))),
					B: uint8(float64(blue.B) + t*float64(int(orange.B)-int(blue.B))),
					A: 0xff,
				}
			}
		})},
		{name: "topbottom-split", img: shadedABImage(func(_, y float64) color.RGBA {
			if y < 0.5 {
				return white
			}
			return black
		})},
		{name: "leftright-split", img: shadedABImage(func(x, _ float64) color.RGBA {
			if x < 0.5 {
				return orange
			}
			return blue
		})},
		{name: "flat-grey", flat: true, img: shadedABImage(func(_, _ float64) color.RGBA {
			return greyAB(0.5)
		})},
		{name: "flat-colour", flat: true, img: shadedABImage(func(_, _ float64) color.RGBA {
			return blue
		})},
	}
}

// shadedABRender is one composed and rasterised A/B leg.
type shadedABRender struct {
	grid    block.Grid
	img     *image.RGBA
	payload int
}

// composeShadedAB drives one (scheme, mode) leg through the production seams:
// viewer.State.ComposeBlockFrame for the grid, RenderStreamFrame with a fresh
// differ for the full-frame payload bytes, and Route A for the render image.
func composeShadedAB(t *testing.T, rast *cellraster.Rasteriser, scheme block.Scheme, mode viewer.ColourMode, frame render.Frame, cols, rows int) shadedABRender {
	t.Helper()
	st := viewer.NewState(cols, rows+1)
	st.Backend = viewer.BackendBlock
	st.Scheme = scheme
	st.Colour = mode
	st.SetDiff(&block.GridDiff{})
	composed := st.ComposeBlockFrame(frame)
	if composed == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	grid := block.Grid{Cols: composed.Cols, Rows: composed.Rows, Cells: append([]block.Cell(nil), composed.Cells...)}
	var payload bytes.Buffer
	if err := st.RenderStreamFrame(frame, &payload); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	return shadedABRender{grid: grid, img: rast.Rasterise(displayGrid(grid, mode)), payload: payload.Len()}
}

// displayGrid maps a composed grid onto the colours the terminal displays for
// the mode, mirroring the writer's palette conversion, so both A/B legs score
// the displayed result rather than the compose-internal colours. The Shaded
// compose already stores canonical palette colours, so the conversion is a
// no-op there; the quadrant compose stores truecolour averages, which the
// writer quantises at emit.
func displayGrid(g block.Grid, mode viewer.ColourMode) block.Grid {
	quantise := func(c color.RGBA) color.RGBA {
		switch mode {
		case viewer.ColourXTermGrey:
			return block.XTermGreyRGBA(block.XTermGrey(c))
		case viewer.ColourANSITonal:
			return block.ANSI16RGBA(block.ANSI16Tonal(c))
		default:
			return block.XTerm256RGBA(block.XTerm256(c))
		}
	}
	out := block.Grid{Cols: g.Cols, Rows: g.Rows, Cells: append([]block.Cell(nil), g.Cells...)}
	for i := range out.Cells {
		out.Cells[i].FG = quantise(out.Cells[i].FG)
		out.Cells[i].BG = quantise(out.Cells[i].BG)
	}
	return out
}

// countShadeCells counts grid cells drawing a Symbols for Legacy Computing
// medium-shade glyph.
func countShadeCells(g block.Grid) int {
	n := 0
	for _, c := range g.Cells {
		if c.Text == "" && c.Glyph >= 0x1FB8C && c.Glyph <= 0x1FB94 {
			n++
		}
	}
	return n
}

func gridsEqual(a, b block.Grid) bool {
	if a.Cols != b.Cols || a.Rows != b.Rows {
		return false
	}
	for i := range a.Cells {
		if a.Cells[i] != b.Cells[i] {
			return false
		}
	}
	return true
}

// logShadedABRow writes one reproducible metric row for a leg.
func logShadedABRow(t *testing.T, fixture, mode, scheme string, r shadedABRender, ref, idealImg *image.RGBA) {
	t.Helper()
	abs := metrics.ScorePair(r.img, ref)
	rel := metrics.ScorePair(r.img, idealImg)
	wcag := metrics.WCAGGrid(r.grid)
	for name, v := range map[string]float64{
		"ssim": abs.SSIM, "msssim": abs.MSSSIM, "dEmean": abs.DeltaEMean, "dEp95": abs.DeltaEP95,
		"relSSIM": rel.SSIM, "relMSSSIM": rel.MSSSIM, "relMSE": rel.MSE,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("fixture %s mode %s scheme %s metric %s is not finite", fixture, mode, scheme, name)
		}
	}
	t.Logf("shadedAB fixture=%s mode=%s scheme=%s ssim=%.6f msssim=%.6f psnr=%.3f dE2000mean=%.4f dE2000p95=%.4f histDiv=%.6f lapVar=%.2f wcagMin=%.3f wcagBelow=%d relSSIM=%.6f relMSSSIM=%.6f relPSNR=%.3f relMSE=%.3f payloadBytes=%d shadeCells=%d",
		fixture, mode, scheme,
		abs.SSIM, abs.MSSSIM, abs.PSNR, abs.DeltaEMean, abs.DeltaEP95, 1-abs.HistIntersection,
		metrics.LaplacianVariance(r.img), wcag.Min, wcag.BelowThreshold,
		rel.SSIM, rel.MSSSIM, rel.PSNR, rel.MSE, r.payload, countShadeCells(r.grid))
}

// TestShadedABSynthetic runs the quadrant-versus-Shaded comparison over the
// synthetic diagonal, shade, split, and flat fixtures under every compatible
// colour mode, logs the full metric table, and enforces the harness-signal
// facts: the shade and diagonal fixtures produce genuinely different grids
// with shade glyphs in play under ANSI tonal, and the exactly flat fixtures
// never gain shade texture.
func TestShadedABSynthetic(t *testing.T) {
	rast := newRasteriser(t)
	for _, fx := range shadedABFixtures() {
		composeSrc := metrics.Downscale(fx.img, shadedABCols*2, shadedABRows*4)
		ideal := metrics.IdealGrid(composeSrc, shadedABCols, shadedABRows)
		idealImg := rast.Rasterise(ideal)
		for _, m := range shadedABModes {
			frame := render.Frame{Img: fx.img}
			a := composeShadedAB(t, rast, block.SchemeQuadrant, m.mode, frame, shadedABCols, shadedABRows)
			b := composeShadedAB(t, rast, block.SchemeShaded, m.mode, frame, shadedABCols, shadedABRows)
			logShadedABRow(t, fx.name, m.name, "quadrant", a, fx.img, idealImg)
			logShadedABRow(t, fx.name, m.name, "shaded", b, fx.img, idealImg)

			if fx.shadeSignal && m.mode == viewer.ColourANSITonal {
				if gridsEqual(a.grid, b.grid) {
					t.Fatalf("fixture %s mode %s: Shaded grid is identical to quadrant, so the fixture carries no A/B signal", fx.name, m.name)
				}
				if countShadeCells(b.grid) == 0 {
					t.Fatalf("fixture %s mode %s: Shaded composed no shade glyph", fx.name, m.name)
				}
			}
			if fx.flat {
				if n := countShadeCells(b.grid); n != 0 {
					t.Fatalf("fixture %s mode %s: flat source gained %d shade cells", fx.name, m.name, n)
				}
				for i, c := range b.grid.Cells {
					if c.FG != c.BG {
						t.Fatalf("fixture %s mode %s cell %d: flat source composed fg %v != bg %v", fx.name, m.name, i, c.FG, c.BG)
					}
				}
			}
		}
	}
}

// TestShadedABCorpus runs the A/B over the committed browser-free corpus
// pages at the pinned 120x40 calibration grid and logs the same metric table.
// The browser-free corpus holds no dense-prose or multicolumn PNG (those two
// layouts exist only as CDP HTML fixtures), so the text-dense committed pages
// stand in: the Linux Matters page and the basic and colour test pages.
func TestShadedABCorpus(t *testing.T) {
	rast := newRasteriser(t)
	pages := []string{"linuxmatters.png", "testpage-02-basic.png", "testpage-04-color.png"}
	for _, card := range pages {
		src := loadRGBA(t, "../../testdata/"+card)
		pxW, pxH := CalibCols*cellraster.CellPxW, CalibRows*cellraster.CellPxH
		ref := metrics.Downscale(src, pxW, pxH)
		composeSrc := metrics.Downscale(src, CalibCols*2, CalibRows*4)
		ideal := metrics.IdealGrid(composeSrc, CalibCols, CalibRows)
		idealImg := rast.Rasterise(ideal)
		for _, m := range shadedABModes {
			frame := render.Frame{Img: src}
			a := composeShadedAB(t, rast, block.SchemeQuadrant, m.mode, frame, CalibCols, CalibRows)
			b := composeShadedAB(t, rast, block.SchemeShaded, m.mode, frame, CalibCols, CalibRows)
			logShadedABRow(t, pageName(card), m.name, "quadrant", a, ref, idealImg)
			logShadedABRow(t, pageName(card), m.name, "shaded", b, ref, idealImg)
		}
	}
}

// TestShadedABDiffPayload measures the Block diff payload per changed frame
// for both schemes under still, scrolling, and noisy sequences, through the
// production differ. The still frame must diff to zero bytes for both
// schemes; the scroll and noise numbers are the evidence rows for the
// payload budget question.
func TestShadedABDiffPayload(t *testing.T) {
	const cols, rows = shadedABCols, shadedABRows
	base := loadRGBA(t, "../../testdata/linuxmatters.png")
	frameAt := func(shift int, noise bool) render.Frame {
		w, h := cols*2, rows*4
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		scaled := metrics.Downscale(base, w, h+16)
		for y := range h {
			for x := range w {
				c := scaled.RGBAAt(x, (y+shift)%(h+16))
				if noise {
					n := uint8((x*31 + y*17 + shift*13) % 32)
					c.R += n
					c.G += n
					c.B += n
				}
				img.SetRGBA(x, y, c)
			}
		}
		return render.Frame{Img: img}
	}
	sequences := []struct {
		name   string
		frames []render.Frame
	}{
		{"still", []render.Frame{frameAt(0, false), frameAt(0, false)}},
		{"scroll", []render.Frame{frameAt(0, false), frameAt(4, false), frameAt(8, false)}},
		{"noise", []render.Frame{frameAt(0, true), frameAt(1, true), frameAt(2, true)}},
	}
	for _, m := range shadedABModes {
		for _, scheme := range []struct {
			name  string
			value block.Scheme
		}{{"quadrant", block.SchemeQuadrant}, {"shaded", block.SchemeShaded}} {
			for _, seq := range sequences {
				st := viewer.NewState(cols, rows+1)
				st.Backend = viewer.BackendBlock
				st.Scheme = scheme.value
				st.Colour = m.mode
				st.SetDiff(&block.GridDiff{})
				var sizes []int
				for _, frame := range seq.frames {
					var out bytes.Buffer
					if err := st.RenderStreamFrame(frame, &out); err != nil {
						t.Fatalf("RenderStreamFrame: %v", err)
					}
					sizes = append(sizes, out.Len())
				}
				if seq.name == "still" && sizes[len(sizes)-1] != 0 {
					t.Fatalf("mode %s scheme %s: unchanged frame diffed to %d bytes", m.name, scheme.name, sizes[len(sizes)-1])
				}
				t.Logf("shadedABPayload mode=%s scheme=%s sequence=%s firstFrameBytes=%d changedFrameBytes=%v", m.name, scheme.name, seq.name, sizes[0], sizes[1:])
			}
		}
	}
}

// TestShadedOverlayUnaffected proves the crisp text overlay and the flat
// boundary recompose behave identically under the Shaded scheme: overlay
// cells match the quadrant compose exactly, the wide trailing cell survives,
// and the recomposed boundary neighbours stay flat, so the existing flat
// boundary fallback does not harm Shaded fixtures.
func TestShadedOverlayUnaffected(t *testing.T) {
	const cols, rows = shadedABCols, shadedABRows
	img := shadedABImage(func(x, y float64) color.RGBA { return greyAB((x + y) / 2) })
	// The box spills a sub-cell residue into both boundary neighbours (columns
	// 2 and 11), so the flat boundary recompose runs on each side.
	cellCSSW := 16.0
	run := render.GlyphRun{
		Col: 3, Row: 2, Text: "Nyala 界", FG: color.RGBA{R: 0xff, G: 0xf0, B: 0x40, A: 0xff},
		X: 3*cellCSSW - 10, W: 8*cellCSSW + 22, CellW: cellCSSW,
	}
	frame := render.Frame{Img: img, Text: []render.GlyphRun{run}}
	rast := newRasteriser(t)
	a := composeShadedAB(t, rast, block.SchemeQuadrant, viewer.ColourXTerm256, frame, cols, rows)
	b := composeShadedAB(t, rast, block.SchemeShaded, viewer.ColourXTerm256, frame, cols, rows)
	overlayCells := 0
	for i := range b.grid.Cells {
		bc, ac := b.grid.Cells[i], a.grid.Cells[i]
		if bc.Text == "" && !bc.Wide {
			continue
		}
		overlayCells++
		if bc.Text != ac.Text || bc.Wide != ac.Wide || bc.FG != ac.FG || bc.BG != ac.BG {
			t.Fatalf("overlay cell %d differs between schemes: shaded %+v quadrant %+v", i, bc, ac)
		}
	}
	if overlayCells == 0 {
		t.Fatal("the overlay run claimed no cells")
	}
	for _, col := range []int{run.Col - 1, run.Col + render.DisplayWidth(run.Text)} {
		cell := b.grid.At(col, run.Row)
		if cell.Text != "" || cell.Wide {
			t.Fatalf("boundary cell (%d,%d) was claimed by the overlay: %+v", col, run.Row, *cell)
		}
		if cell.FG != cell.BG {
			t.Fatalf("boundary cell (%d,%d) was not recomposed flat: %+v", col, run.Row, *cell)
		}
	}
}
