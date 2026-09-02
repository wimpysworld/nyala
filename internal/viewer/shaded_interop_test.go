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

// TestBlockTerminalRoundTripShaded sends two Shaded frames through the
// production compose and cell-diff encoder for every compatible colour mode,
// decodes the stream with the independent VT emulator, and compares the
// visible terminal model with the exact grid the production writer consumed.
func TestBlockTerminalRoundTripShaded(t *testing.T) {
	const cols, rows = 12, 5
	for _, mode := range []ColourMode{ColourXTerm256, ColourXTermGrey} {
		state := NewState(cols, rows+1)
		state.Backend = BackendBlock
		state.Scheme = block.SchemeShaded
		state.Colour = mode
		state.SetDiff(&block.GridDiff{})

		var expected block.Grid
		var stream bytes.Buffer
		for frameNumber := range 2 {
			frame := blockInteropFrame(cols, rows, frameNumber)
			grid := state.ComposeBlockFrame(frame)
			expected = block.Grid{Cols: grid.Cols, Rows: grid.Rows, Cells: append([]block.Cell(nil), grid.Cells...)}
			if err := state.RenderStreamFrame(frame, &stream); err != nil {
				t.Fatalf("mode %d RenderStreamFrame %d: %v", mode, frameNumber, err)
			}
		}

		decoded, err := blockinterop.Decode(stream.Bytes(), cols, rows)
		if err != nil {
			t.Fatalf("mode %d blockinterop.Decode: %v", mode, err)
		}
		for row := range rows {
			for col := range cols {
				got, want := decoded.At(col, row), expected.At(col, row)
				if got.Wide != want.Wide || visibleText(got) != visibleText(want) {
					t.Fatalf("mode %d cell (%d,%d) = %+v, want visible %q wide %t", mode, col, row, *got, visibleText(want), want.Wide)
				}
				// The writer quantises overlay colours at emit, so the decoded
				// colours compare against the composed colours after the same
				// mode conversion.
				if got.Wide {
					continue
				}
				wantFG, wantBG := writerCanonical(mode, want.FG), writerCanonical(mode, want.BG)
				if got.FG != wantFG || got.BG != wantBG {
					t.Fatalf("mode %d cell (%d,%d) colours fg %v bg %v, want fg %v bg %v", mode, col, row, got.FG, got.BG, wantFG, wantBG)
				}
			}
		}
	}
}

// writerCanonical maps a composed cell colour onto the canonical RGBA the
// mode's writer index displays, mirroring fgSGRFunc and bgSGRFunc.
func writerCanonical(mode ColourMode, c color.RGBA) color.RGBA {
	if mode == ColourXTermGrey {
		return block.XTermGreyRGBA(block.XTermGrey(c))
	}
	return block.XTerm256RGBA(block.XTerm256(c))
}

// TestShadedCandidateWidths writes every Shaded candidate glyph through the
// production cell-diff writer as repeated runs with an ASCII sentinel after
// each run, decodes the stream with the VT emulator, and proves the sentinel
// lands in its own column: the four-byte SMP shade glyphs advance the cursor
// exactly one cell, so no candidate shifts the cells after it.
func TestShadedCandidateWidths(t *testing.T) {
	candidates := block.ShadedCandidates()
	const runLen = 3
	cols := runLen + 2
	rows := len(candidates)
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{A: 0xff}
	grid := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for row, glyph := range candidates {
		for col := range runLen {
			*grid.At(col, row) = block.Cell{Glyph: glyph, FG: fg, BG: bg}
		}
		*grid.At(runLen, row) = block.Cell{Glyph: 'X', FG: fg, BG: bg}
		*grid.At(runLen+1, row) = block.Cell{Glyph: '.', FG: fg, BG: bg}
	}

	var stream bytes.Buffer
	var diff block.GridDiff
	if err := diff.Emit(&stream, grid, fgSGRFunc(ColourXTerm256), bgSGRFunc(ColourXTerm256)); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	decoded, err := blockinterop.Decode(stream.Bytes(), cols, rows)
	if err != nil {
		t.Fatalf("blockinterop.Decode: %v", err)
	}
	for row, glyph := range candidates {
		for col := range runLen {
			if got := visibleText(decoded.At(col, row)); got != string(glyph) {
				t.Fatalf("row %d (%U) col %d = %q, want the candidate", row, glyph, col, got)
			}
		}
		if got := visibleText(decoded.At(runLen, row)); got != "X" {
			t.Fatalf("row %d (%U) sentinel = %q; the candidate run shifted the next cell", row, glyph, got)
		}
		if got := visibleText(decoded.At(runLen+1, row)); got != "." {
			t.Fatalf("row %d (%U) trailing cell = %q, want %q", row, glyph, got, ".")
		}
	}
}

// TestShadedRoundTripUsesShadeGlyphs proves the round trip above exercises
// the shade set for real: a soft diagonal ramp frame composed on the
// greyscale ramp, whose tone gaps shade mixing crosses between adjacent
// entries, carries at least one Symbols for Legacy Computing shade glyph end
// to end.
func TestShadedRoundTripUsesShadeGlyphs(t *testing.T) {
	const cols, rows = 12, 5
	state := NewState(cols, rows+1)
	state.Backend = BackendBlock
	state.Scheme = block.SchemeShaded
	state.Colour = ColourXTermGrey
	state.SetDiff(&block.GridDiff{})

	// The ramp is shallow on purpose: each cell spans well under one 10-step
	// grey bucket, so a cell that straddles a bucket boundary displays the
	// intermediate tone only through shade mixing, never through a solid
	// two-tone split.
	img := image.NewRGBA(image.Rect(0, 0, cols*2, rows*4))
	for y := range rows * 4 {
		for x := range cols * 2 {
			v := uint8(96 + x*64/(cols*2-1) + y*64/(rows*4-1)) //nolint:gosec // the ramp is bounded to 96..224
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 0xff})
		}
	}
	var stream bytes.Buffer
	if err := state.RenderStreamFrame(render.Frame{Img: img}, &stream); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	decoded, err := blockinterop.Decode(stream.Bytes(), cols, rows)
	if err != nil {
		t.Fatalf("blockinterop.Decode: %v", err)
	}
	for _, cell := range decoded.Cells {
		if r := []rune(cell.Text); len(r) == 1 && r[0] >= 0x1FB8C && r[0] <= 0x1FB94 {
			return
		}
	}
	t.Fatal("no shade glyph survived the terminal round trip")
}
