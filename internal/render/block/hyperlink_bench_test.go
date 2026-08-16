package block

import (
	"bytes"
	"image/color"
	"io"
	"testing"
)

// BenchmarkGridDiffLinkFree drives the differ over a full-motion 120x40 grid
// with no hyperlinks, alternating two frames so every cell changes each pass.
// It measures the cost the hyperlink attribute adds to a link-free frame, so it
// builds its grids only from symbols the pre-hyperlink writer exports.
func BenchmarkGridDiffLinkFree(b *testing.B) {
	const cols, rows = linkGridCols, linkGridRows
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

// linkGridCols and linkGridRows fix the geometry of every benchmark link
// grid, and linkGridLinksPerRow and linkGridRunLen fix its link population.
const (
	linkGridCols        = 120
	linkGridRows        = 40
	linkGridLinksPerRow = 10
	linkGridRunLen      = 6
)

// linkGrid builds the fixed-geometry full-motion grid, shifted by phase, where
// every row carries linkGridLinksPerRow short hyperlink runs of linkGridRunLen
// cells. Each run gets a distinct Hyperlink pointer, matching the compose
// path, which rebuilds pointers every frame.
func linkGrid(phase int) Grid {
	cols, rows := linkGridCols, linkGridRows
	linksPerRow, runLen := linkGridLinksPerRow, linkGridRunLen
	g := motionGrid(cols, rows, phase)
	stride := cols / linksPerRow
	for row := range rows {
		for l := range linksPerRow {
			link := &Hyperlink{ID: "r", URI: "https://example.org/a"}
			start := l * stride
			for col := start; col < start+runLen && col < cols; col++ {
				g.Cells[row*cols+col].Link = link
			}
		}
	}
	return g
}

// BenchmarkGridDiffLinkHeavy drives the differ over a full-motion 120x40 grid
// where every row carries ten six-cell hyperlink runs, alternating two frames
// so every cell changes each pass. It pins the OSC 8 bracketing cost on a
// link-heavy scroll.
func BenchmarkGridDiffLinkHeavy(b *testing.B) {
	fg, bg := SGRFunc(AppendTrueColorFG), SGRFunc(AppendTrueColorBG)
	frames := [2]Grid{
		linkGrid(0),
		linkGrid(1),
	}
	var d GridDiff
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := d.Emit(io.Discard, frames[i&1], fg, bg); err != nil {
			b.Fatal(err)
		}
	}
}

// TestPartialRepaintEmitsOpensOnlyForRewrittenCells paints a link-heavy grid,
// then re-emits a frame where only one row changed. The diff must open only
// the links inside the rewritten row, not the full link population.
func TestPartialRepaintEmitsOpensOnlyForRewrittenCells(t *testing.T) {
	const cols, rows = linkGridCols, linkGridRows
	const linksPerRow = linkGridLinksPerRow
	fg, bg := SGRFunc(AppendTrueColorFG), SGRFunc(AppendTrueColorBG)

	first := linkGrid(0)
	second := linkGrid(0)
	const changedRow = 17
	for col := range cols {
		second.Cells[changedRow*cols+col].FG = color.RGBA{R: 1, G: 2, B: 3, A: 0xff}
	}

	var d GridDiff
	if err := d.Emit(io.Discard, first, fg, bg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := d.Emit(&out, second, fg, bg); err != nil {
		t.Fatal(err)
	}

	opens := bytes.Count(out.Bytes(), []byte("\x1b]8;id="))
	total := rows * linksPerRow
	if opens != linksPerRow {
		t.Fatalf("second frame emitted %d opens, want %d (one per link in the rewritten row, of %d links on the page)",
			opens, linksPerRow, total)
	}
}
