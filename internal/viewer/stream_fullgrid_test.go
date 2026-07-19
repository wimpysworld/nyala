package viewer

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestRenderBlockComposesFullGridNoLetterbox verifies that the block stream fills
// the grid without aspect-fit letterboxing. The engine sizes the block frame to the
// exact grid geometry (cols*cellW by rows*cellH device pixels) and bins each text
// run in whole-cell coordinates, so the bitmap must map edge-to-edge across every
// column. An aspect fit would letterbox the bitmap onto fewer columns whenever the
// real cell aspect differs, blanking margin columns while the overlay still uses
// the full grid and shearing text progressively farther from the bitmap.
//
// Each case fills a full-frame source with one colour, draws it through
// RenderStreamFrame on the block backend, captures the composed grid, and asserts
// no column is a blank letterbox margin: the leftmost and rightmost columns carry
// the fill and the whole grid holds zero blank cells. The 8x16 leg is the 1:2 cell
// aspect where a fit fills the full width; the 16x33 and 13x27 legs are real
// non-1:2 terminal geometries where letterboxing would blank the edge columns.
func TestRenderBlockComposesFullGridNoLetterbox(t *testing.T) {
	fill := color.RGBA{R: 0xff, G: 0x40, B: 0x20, A: 0xff}
	cases := []struct {
		name         string
		cols, rows   int
		cellW, cellH int
	}{
		{"1to2-aspect-8x16", 40, 20, 8, 16},
		{"non-1to2-aspect-16x33", 40, 20, 16, 33},
		{"non-1to2-aspect-13x27", 40, 20, 13, 27},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Size the source the way the engine does: the full grid in device
			// pixels, filled edge to edge with the marker colour.
			src := image.NewRGBA(image.Rect(0, 0, c.cols*c.cellW, c.rows*c.cellH))
			for i := range src.Pix {
				switch i % 4 {
				case 0:
					src.Pix[i] = fill.R
				case 1:
					src.Pix[i] = fill.G
				case 2:
					src.Pix[i] = fill.B
				default:
					src.Pix[i] = fill.A
				}
			}

			st := NewState(c.cols, c.rows)
			st.Backend = BackendBlock
			st.SetDiff(&block.GridDiff{})

			sink := st.ComposeBlockFrame(render.Frame{Img: src})

			var buf bytes.Buffer
			if err := st.RenderStreamFrame(render.Frame{Img: src}, &buf); err != nil {
				t.Fatalf("RenderStreamFrame: %v", err)
			}
			if sink == nil {
				t.Fatal("ComposeBlockFrame returned nil")
			}
			if sink.Cols != c.cols || sink.Rows != c.rows {
				t.Fatalf("grid = %dx%d, want %dx%d", sink.Cols, sink.Rows, c.cols, c.rows)
			}

			// No column may be a blank letterbox margin: every cell carries the
			// fill, so the leftmost and rightmost columns hold content and the grid
			// has zero blank cells. A reintroduced letterbox blanks the edge columns
			// at a non-1:2 aspect and trips both checks.
			blanks := 0
			for row := range sink.Rows {
				for col := range sink.Cols {
					if !cellHasFill(sink.At(col, row), fill) {
						blanks++
					}
				}
			}
			if blanks != 0 {
				t.Errorf("%d blank cells, want 0: a letterbox margin reappeared", blanks)
			}
			for row := range sink.Rows {
				if !cellHasFill(sink.At(0, row), fill) {
					t.Errorf("leftmost column blank at row %d: bitmap does not reach column 0", row)
				}
				if !cellHasFill(sink.At(sink.Cols-1, row), fill) {
					t.Errorf("rightmost column blank at row %d: bitmap does not reach column %d", row, sink.Cols-1)
				}
			}
		})
	}
}

// cellHasFill reports whether a composed cell carries the fill colour rather than
// a blank letterbox margin. A uniform source composes to a flat cell whose fg and
// bg both equal the fill, so the background alone identifies it; a blank cell has a
// zero-value (transparent black) background.
func cellHasFill(c *block.Cell, fill color.RGBA) bool {
	return c.BG == fill
}
