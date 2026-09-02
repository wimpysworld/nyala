package viewer

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
)

// TestBlockTerminalRoundTripDegradedColourModes is the parent proof for the
// degraded greyscale mode. It starts the mode through the normal flag
// resolver, sends a deterministic frame through State.RenderStreamFrame, and
// checks the terminal-visible grid against the unquantised composed grid.
func TestBlockTerminalRoundTripDegradedColourModes(t *testing.T) {
	const (
		cols         = 20
		terminalRows = 7
	)
	frame := degradedColourFrame(cols, engineRows(terminalRows))
	rasteriser, err := cellraster.NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}

	cases := []struct {
		name          string
		flag          string
		mode          ColourMode
		payloadBudget int
		qualityFloor  float64
	}{
		// Measured SSIM is 0.993773483. The 0.98 floor leaves 0.0137 for
		// harmless metric drift while rejecting visible greyscale loss.
		{name: "xterm-grey", flag: "grey", mode: ColourXTermGrey, payloadBudget: 2900, qualityFloor: 0.98},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, composed := renderDegradedColourFrame(t, tc.flag, tc.mode, cols, terminalRows, frame)
			repeat, _ := renderDegradedColourFrame(t, tc.flag, tc.mode, cols, terminalRows, frame)
			if !bytes.Equal(payload, repeat) {
				t.Fatal("fresh renders produced different terminal payloads")
			}
			if len(payload) > tc.payloadBudget {
				t.Fatalf("payload = %d bytes, budget %d", len(payload), tc.payloadBudget)
			}
			if len(payload) >= frameBufferSize(cols, terminalRows) {
				t.Fatalf("payload = %d bytes, frame buffer size %d", len(payload), frameBufferSize(cols, terminalRows))
			}

			decoded, err := blockinterop.Decode(payload, cols, engineRows(terminalRows))
			if err != nil {
				t.Fatalf("blockinterop.Decode: %v", err)
			}
			assertDegradedVisibleGrid(t, decoded, composed)
			assertDegradedPaletteRange(t, decoded, tc.mode)
			assertDegradedSentinelAndWidth(t, decoded, cols, engineRows(terminalRows))
			assertNoFlatCellTexture(t, rasteriser, decoded)

			gotImage := rasteriser.Rasterise(decoded)
			referenceImage := rasteriser.Rasterise(composed)
			quality := metrics.SSIM(gotImage, referenceImage)
			if math.IsNaN(quality) || math.IsInf(quality, 0) || quality < tc.qualityFloor || quality > 1 {
				t.Fatalf("truecolour-reference SSIM = %.9f, enforced floor %.6f", quality, tc.qualityFloor)
			}
			t.Logf("quality_ssim=%.9f quality_floor=%.6f payload_bytes=%d payload_budget=%d frame_buffer_size=%d", quality, tc.qualityFloor, len(payload), tc.payloadBudget, frameBufferSize(cols, terminalRows))
		})
	}
}

func renderDegradedColourFrame(t *testing.T, flag string, mode ColourMode, cols, terminalRows int, frame render.Frame) ([]byte, block.Grid) {
	t.Helper()
	st := NewState(cols, terminalRows)
	st.Backend = BackendBlock
	st.configureColour(flag, []string{"NO_COLOR=1"}, ColourXTerm256, true)
	if st.Colour != mode {
		t.Fatalf("startup flag %q resolved to %v, want %v", flag, st.Colour, mode)
	}
	st.SetDiff(&block.GridDiff{})
	grid := st.ComposeBlockFrame(frame)
	if grid == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	composed := cloneDegradedGrid(*grid)

	var out bytes.Buffer
	if err := st.RenderStreamFrame(frame, &out); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	return append([]byte(nil), out.Bytes()...), composed
}

func degradedColourFrame(cols, rows int) render.Frame {
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
		panic("degraded colour fixture requires 20 columns")
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

func cloneDegradedGrid(src block.Grid) block.Grid {
	return block.Grid{Cols: src.Cols, Rows: src.Rows, Cells: append([]block.Cell(nil), src.Cells...)}
}

func assertDegradedVisibleGrid(t *testing.T, got, want block.Grid) {
	t.Helper()
	if got.Cols != want.Cols || got.Rows != want.Rows {
		t.Fatalf("decoded grid = %dx%d, want %dx%d", got.Cols, got.Rows, want.Cols, want.Rows)
	}
	for row := range want.Rows {
		for col := range want.Cols {
			gc, wc := got.At(col, row), want.At(col, row)
			if gc.Wide != wc.Wide || degradedVisibleText(gc) != degradedVisibleText(wc) {
				t.Fatalf("cell (%d,%d) = %+v, want visible text %q and wide %t", col, row, *gc, degradedVisibleText(wc), wc.Wide)
			}
		}
	}
}

func assertDegradedPaletteRange(t *testing.T, grid block.Grid, mode ColourMode) {
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
			if mode == ColourXTermGrey {
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

func degradedVisibleText(cell *block.Cell) string {
	if cell.Wide {
		return ""
	}
	if cell.Text != "" {
		return cell.Text
	}
	return string(cell.Glyph)
}

func assertDegradedSentinelAndWidth(t *testing.T, grid block.Grid, cols, rows int) {
	t.Helper()
	if grid.Cols != cols || grid.Rows != rows || len(grid.Cells) != cols*rows {
		t.Fatalf("decoded grid shape = %dx%d with %d cells, want %dx%d with %d", grid.Cols, grid.Rows, len(grid.Cells), cols, rows, cols*rows)
	}
	lead := grid.At(cols-2, rows-1)
	trail := grid.At(cols-1, rows-1)
	if lead.Text != "界" || lead.Wide || !trail.Wide {
		t.Fatalf("right-edge sentinel = lead %+v trail %+v, want width-2 界 in final two columns", *lead, *trail)
	}
}

func assertNoFlatCellTexture(t *testing.T, rasteriser *cellraster.Rasteriser, grid block.Grid) {
	t.Helper()
	for col := range grid.Cols {
		cell := *grid.At(col, 0)
		if cell.FG != cell.BG {
			t.Fatalf("flat cell %d has foreground %v and background %v", col, cell.FG, cell.BG)
		}
		tile := rasteriser.RenderGlyphCell(degradedVisibleText(&cell), cell.FG, cell.BG)
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
