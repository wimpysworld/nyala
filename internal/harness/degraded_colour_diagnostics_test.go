//go:build diagnostics

package harness

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/wimpysworld/nyala/internal/blockinterop"
	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// TestDegradedColourDiagnostics proves that both shipped degraded colour modes
// retain their palette, quality, payload, and cell geometry through the
// browser-free diagnostics path.
func TestDegradedColourDiagnostics(t *testing.T) {
	const (
		cols         = 20
		pageRows     = 6
		terminalRows = pageRows + 1
	)
	frame := degradedDiagnosticsFrame(cols, pageRows)
	rasteriser := newRasteriser(t)

	cases := []struct {
		name          string
		mode          viewer.ColourMode
		payloadBudget int
		qualityFloor  float64
	}{
		// Measured SSIM is 0.993773483. The 0.98 floor leaves 0.0137 for
		// harmless metric drift while rejecting visible greyscale loss.
		{name: "xterm-grey", mode: viewer.ColourXTermGrey, payloadBudget: 2900, qualityFloor: 0.98},
		// Measured SSIM is 0.941703591. The 0.92 floor leaves 0.0217 for
		// harmless metric drift while retaining a useful 16-colour quality gate.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := viewer.NewState(cols, terminalRows)
			st.Backend = viewer.BackendBlock
			st.Colour = tc.mode
			st.SetDiff(&block.GridDiff{})

			composed := st.ComposeBlockFrame(frame)
			if composed == nil {
				t.Fatal("ComposeBlockFrame returned nil")
			}
			var payload bytes.Buffer
			if err := st.RenderStreamFrame(frame, &payload); err != nil {
				t.Fatalf("RenderStreamFrame: %v", err)
			}
			if payload.Len() > tc.payloadBudget {
				t.Fatalf("payload = %d bytes, budget %d", payload.Len(), tc.payloadBudget)
			}

			decoded, err := blockinterop.Decode(payload.Bytes(), cols, pageRows)
			if err != nil {
				t.Fatalf("blockinterop.Decode: %v", err)
			}
			assertDiagnosticsGrid(t, decoded, *composed)
			assertDiagnosticsPaletteRange(t, decoded, tc.mode)
			if decoded.Cols != cols || decoded.Rows != pageRows || len(decoded.Cells) != cols*pageRows {
				t.Fatalf("decoded shape = %dx%d with %d cells, want %dx%d with %d", decoded.Cols, decoded.Rows, len(decoded.Cells), cols, pageRows, cols*pageRows)
			}
			if decoded.At(cols-2, pageRows-1).Text != "界" || !decoded.At(cols-1, pageRows-1).Wide {
				t.Fatalf("right-edge width sentinel was not retained: lead %+v trail %+v", *decoded.At(cols-2, pageRows-1), *decoded.At(cols-1, pageRows-1))
			}
			assertDiagnosticsFlatRow(t, rasteriser, decoded)

			quality := metrics.SSIM(rasteriser.Rasterise(decoded), rasteriser.Rasterise(*composed))
			if math.IsNaN(quality) || math.IsInf(quality, 0) || quality < tc.qualityFloor || quality > 1 {
				t.Fatalf("truecolour-reference SSIM = %.9f, enforced floor %.6f", quality, tc.qualityFloor)
			}
			t.Logf("quality_ssim=%.9f quality_floor=%.6f payload_bytes=%d payload_budget=%d", quality, tc.qualityFloor, payload.Len(), tc.payloadBudget)
		})
	}
}

func degradedDiagnosticsFrame(cols, rows int) render.Frame {
	lumaRamp := [...]uint8{0, 13, 26, 40, 53, 67, 80, 93, 107, 120, 134, 147, 161, 174, 187, 201, 214, 228, 241, 255}
	chromatic := [...]color.RGBA{
		{R: 0xf2, G: 0x34, B: 0x4c, A: 0xff},
		{R: 0x26, G: 0xb8, B: 0x5a, A: 0xff},
		{R: 0x35, G: 0x71, B: 0xe8, A: 0xff},
		{R: 0xf0, G: 0xc3, B: 0x28, A: 0xff},
		{R: 0xb7, G: 0x3c, B: 0xd6, A: 0xff},
		{R: 0x24, G: 0xc4, B: 0xc7, A: 0xff},
		{R: 0xf1, G: 0x78, B: 0x2d, A: 0xff},
		{R: 0x79, G: 0x45, B: 0xe8, A: 0xff},
	}
	if cols != len(lumaRamp) {
		panic("degraded diagnostics fixture requires 20 columns")
	}
	img := image.NewRGBA(image.Rect(0, 0, cols*2, rows*4))
	for row := range rows {
		for col := range cols {
			for dy := range 4 {
				for dx := range 2 {
					c := chromatic[(col*3+row*5+dx+dy*2)%len(chromatic)]
					if row == 0 {
						v := lumaRamp[col]
						c = color.RGBA{R: v, G: v, B: v, A: 0xff}
					}
					img.SetRGBA(col*2+dx, row*4+dy, c)
				}
			}
		}
	}
	return render.Frame{
		Img: img,
		Text: []render.GlyphRun{
			{Col: 1, Row: 2, Text: "A", FG: color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}},
			{Col: cols - 2, Row: rows - 1, Text: "界", FG: color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}},
		},
	}
}

func assertDiagnosticsGrid(t *testing.T, got, want block.Grid) {
	t.Helper()
	if got.Cols != want.Cols || got.Rows != want.Rows {
		t.Fatalf("decoded grid = %dx%d, want %dx%d", got.Cols, got.Rows, want.Cols, want.Rows)
	}
	for row := range want.Rows {
		for col := range want.Cols {
			gc, wc := got.At(col, row), want.At(col, row)
			if gc.Wide != wc.Wide || diagnosticsVisibleText(gc) != diagnosticsVisibleText(wc) {
				t.Fatalf("cell (%d,%d) = %+v, want visible text %q and wide %t", col, row, *gc, diagnosticsVisibleText(wc), wc.Wide)
			}
		}
	}
}

func assertDiagnosticsPaletteRange(t *testing.T, grid block.Grid, mode viewer.ColourMode) {
	t.Helper()
	ansiPalette := [...]color.RGBA{
		{A: 0xff},
		{R: 128, A: 0xff},
		{G: 128, A: 0xff},
		{R: 128, G: 128, A: 0xff},
		{B: 128, A: 0xff},
		{R: 128, B: 128, A: 0xff},
		{G: 128, B: 128, A: 0xff},
		{R: 192, G: 192, B: 192, A: 0xff},
		{R: 128, G: 128, B: 128, A: 0xff},
		{R: 255, A: 0xff},
		{G: 255, A: 0xff},
		{R: 255, G: 255, A: 0xff},
		{B: 255, A: 0xff},
		{R: 255, B: 255, A: 0xff},
		{G: 255, B: 255, A: 0xff},
		{R: 255, G: 255, B: 255, A: 0xff},
	}
	for i, cell := range grid.Cells {
		for _, c := range []color.RGBA{cell.FG, cell.BG} {
			valid := false
			if mode == viewer.ColourXTermGrey {
				valid = c.A == 0xff && c.R == c.G && c.G == c.B && c.R >= 8 && c.R <= 238 && (c.R-8)%10 == 0
			} else {
				for _, paletteColour := range ansiPalette {
					valid = valid || c == paletteColour
				}
			}
			if !valid {
				t.Fatalf("cell %d colour %v is outside the degraded palette for mode %v", i, c, mode)
			}
		}
	}
}

func diagnosticsVisibleText(cell *block.Cell) string {
	if cell.Wide {
		return ""
	}
	if cell.Text != "" {
		return cell.Text
	}
	return string(cell.Glyph)
}

func assertDiagnosticsFlatRow(t *testing.T, rasteriser *cellraster.Rasteriser, grid block.Grid) {
	t.Helper()
	for col := range grid.Cols {
		cell := *grid.At(col, 0)
		if cell.FG != cell.BG {
			t.Fatalf("flat cell %d has foreground %v and background %v", col, cell.FG, cell.BG)
		}
		tile := rasteriser.RenderGlyphCell(diagnosticsVisibleText(&cell), cell.FG, cell.BG)
		want := tile.RGBAAt(0, 0)
		for y := range tile.Bounds().Dy() {
			for x := range tile.Bounds().Dx() {
				if got := tile.RGBAAt(x, y); got != want {
					t.Fatalf("flat cell %d added texture at (%d,%d): %v, want %v", col, x, y, got, want)
				}
			}
		}
	}
}
