//go:build blockinterop

package viewer

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/blockinterop"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestBlockTerminalRoundTrip sends two frames through the production live Block
// compose and cell-diff encoder, decodes the resulting CUP/SGR/UTF-8 stream with
// an independent VT emulator, then compares the visible terminal model with the
// exact grid consumed by the production writer.
func TestBlockTerminalRoundTrip(t *testing.T) {
	const cols, rows = 12, 5
	state := NewState(cols, rows, cols*2, rows*4)
	state.Backend = BackendBlock
	state.Colour = ColourTrue
	state.SetDiff(&block.GridDiff{})

	var expected block.Grid
	var stream bytes.Buffer
	for frameNumber := range 2 {
		frame := blockInteropFrame(cols, rows, frameNumber)
		grid := state.ComposeBlockFrame(frame)
		expected = block.Grid{Cols: grid.Cols, Rows: grid.Rows, Cells: append([]block.Cell(nil), grid.Cells...)}
		if err := state.RenderStreamFrame(frame, &stream); err != nil {
			t.Fatalf("RenderStreamFrame %d: %v", frameNumber, err)
		}
	}

	decoded, err := blockinterop.Decode(stream.Bytes(), cols, rows)
	if err != nil {
		t.Fatalf("blockinterop.Decode: %v", err)
	}
	assertVisibleGridEqual(t, decoded, expected)
}

func blockInteropFrame(cols, rows, frameNumber int) render.Frame {
	img := image.NewRGBA(image.Rect(0, 0, cols*2, rows*4))
	for y := range img.Bounds().Dy() {
		for x := range img.Bounds().Dx() {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 19) % 256),
				G: uint8((y * 23) % 256),
				B: uint8((x*7 + y*11) % 256),
				A: 0xff,
			})
		}
	}
	return render.Frame{
		Img: img,
		Text: []render.GlyphRun{
			{Col: 1 + frameNumber, Row: 1, Text: "Nyala", FG: color.RGBA{R: 0xff, G: 0xf0, B: 0x40, A: 0xff}},
			{Col: 8, Row: 3, Text: "世", FG: color.RGBA{R: 0x20, G: 0xff, B: 0xd0, A: 0xff}},
		},
	}
}

func assertVisibleGridEqual(t *testing.T, got, want block.Grid) {
	t.Helper()
	if got.Cols != want.Cols || got.Rows != want.Rows {
		t.Fatalf("decoded grid = %dx%d, want %dx%d", got.Cols, got.Rows, want.Cols, want.Rows)
	}
	for row := range want.Rows {
		for col := range want.Cols {
			gotCell, wantCell := got.At(col, row), want.At(col, row)
			if gotCell.Wide != wantCell.Wide || gotCell.FG != wantCell.FG || gotCell.BG != wantCell.BG || visibleText(gotCell) != visibleText(wantCell) {
				t.Fatalf("cell (%d,%d) = %+v, want visible content %q fg %v bg %v wide %t", col, row, *gotCell, visibleText(wantCell), wantCell.FG, wantCell.BG, wantCell.Wide)
			}
		}
	}
}

func visibleText(cell *block.Cell) string {
	if cell.Wide {
		return ""
	}
	if cell.Text != "" {
		return cell.Text
	}
	return string(cell.Glyph)
}
