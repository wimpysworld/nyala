package block

import (
	"image/color"
	"io"
	"testing"
)

// motionGrid builds a cols x rows grid whose every cell carries a distinct
// glyph colour, shifted by phase, so consecutive frames change every cell. It is
// the worst case for the differ: a full-motion scroll where no cell coalesces.
func motionGrid(cols, rows, phase int) Grid {
	g := Grid{Cols: cols, Rows: rows, Cells: make([]Cell, cols*rows)}
	for row := range rows {
		for col := range cols {
			v := byte((col + row + phase) * 7 & 0xff)
			g.Cells[row*cols+col] = Cell{
				Glyph: 0x2588,
				FG:    color.RGBA{R: v, G: v ^ 0x55, B: v + 17, A: 0xff},
				BG:    color.RGBA{R: v + 3, G: v, B: v ^ 0x22, A: 0xff},
			}
		}
	}
	return g
}

// BenchmarkGridDiff drives the differ over a full-motion 120x40 grid in
// truecolour, alternating two frames so every cell changes each pass. It pins the
// per-cell allocation cost of the SGR and glyph builds on the live paint path.
func BenchmarkGridDiff(b *testing.B) {
	const cols, rows = 120, 40
	fg, bg := SGRFunc(AppendTrueColorFG), SGRFunc(AppendTrueColorBG)
	frames := [2]Grid{motionGrid(cols, rows, 0), motionGrid(cols, rows, 1)}
	var d GridDiff
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := d.Emit(io.Discard, frames[i&1], fg, bg); err != nil {
			b.Fatal(err)
		}
	}
}
