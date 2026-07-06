package block

import (
	"bytes"
	"image/color"
	"strings"
	"testing"
)

// fixedFG and fixedBG are colour-mode-neutral SGR funcs for the differ tests: a
// short stable token per colour, so the assertions read the escape stream
// without truecolour noise.
func fixedFG(dst []byte, c color.RGBA) []byte { return append(dst, '<', 'f', '0'+c.R, '>') }
func fixedBG(dst []byte, c color.RGBA) []byte { return append(dst, '<', 'b', '0'+c.B, '>') }

func gridOf(cols, rows int, fill Cell) Grid {
	g := Grid{Cols: cols, Rows: rows, Cells: make([]Cell, cols*rows)}
	for i := range g.Cells {
		g.Cells[i] = fill
	}
	return g
}

// TestGridDiffSkipsUnchanged checks an unchanged second frame emits no escapes.
func TestGridDiffSkipsUnchanged(t *testing.T) {
	g := gridOf(4, 2, Cell{Glyph: 'a'})
	var d GridDiff
	var first bytes.Buffer
	if err := d.Emit(&first, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	if first.Len() == 0 {
		t.Fatal("first frame emitted nothing")
	}
	var second bytes.Buffer
	if err := d.Emit(&second, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	if second.Len() != 0 {
		t.Fatalf("unchanged frame emitted %q", second.String())
	}
}

// TestGridDiffSingleCell checks one changed cell moves the cursor once and emits
// that cell only, not the whole grid.
func TestGridDiffSingleCell(t *testing.T) {
	g := gridOf(4, 2, Cell{Glyph: 'a'})
	var d GridDiff
	if err := d.Emit(&bytes.Buffer{}, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	g.At(2, 1).Glyph = 'b'
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "\x1b[2;3H") {
		t.Fatalf("missing CUP to (2,1): %q", got)
	}
	if !strings.HasSuffix(got, "b") {
		t.Fatalf("changed glyph not last byte: %q", got)
	}
	if strings.Contains(got, "a") {
		t.Fatalf("emitted an unchanged cell: %q", got)
	}
	if n := strings.Count(got, "\x1b["); n != 1 {
		t.Fatalf("want one CUP for the single changed cell, got %d: %q", n, got)
	}
}

// TestGridDiffChangedCellCountDrivesEmit pins AC 4: the emitted-cell count
// tracks the changed-cell count, not the grid size. With a 6x3 grid (18 cells)
// and three scattered cells changed, exactly three cells emit, each with its own
// CUP, and no unchanged cell's glyph appears.
func TestGridDiffChangedCellCountDrivesEmit(t *testing.T) {
	g := gridOf(6, 3, Cell{Glyph: 'a'})
	var d GridDiff
	if err := d.Emit(&bytes.Buffer{}, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	// Three scattered changes, none adjacent, so each is its own run.
	changed := []struct{ col, row int }{{0, 0}, {3, 1}, {5, 2}}
	for _, p := range changed {
		g.At(p.col, p.row).Glyph = 'z'
	}
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "\x1b["); n != len(changed) {
		t.Fatalf("want %d CUP for %d scattered changed cells, got %d: %q",
			len(changed), len(changed), n, got)
	}
	if n := strings.Count(got, "z"); n != len(changed) {
		t.Fatalf("want %d changed glyphs, got %d: %q", len(changed), n, got)
	}
	if strings.Contains(got, "a") {
		t.Fatalf("emitted an unchanged cell: %q", got)
	}
}

// TestGridDiffUniformRowOneSGRPair pins AC 5: a uniform-colour changed row sends
// one SGR pair, not one per cell. The whole 5-cell row changes glyph but keeps
// one colour, so exactly one fg and one bg SGR token appear.
func TestGridDiffUniformRowOneSGRPair(t *testing.T) {
	g := gridOf(5, 1, Cell{Glyph: 'a', FG: color.RGBA{R: 2}, BG: color.RGBA{B: 3}})
	var d GridDiff
	if err := d.Emit(&bytes.Buffer{}, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	for col := range 5 {
		g.At(col, 0).Glyph = 'z'
	}
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "<f"); n != 1 {
		t.Fatalf("want one fg SGR for the uniform row, got %d: %q", n, got)
	}
	if n := strings.Count(got, "<b"); n != 1 {
		t.Fatalf("want one bg SGR for the uniform row, got %d: %q", n, got)
	}
}

// TestGridDiffContiguousRunOneCUP pins AC 6: a contiguous changed run emits a
// single CUP for the run, because the cursor advances in place across the run.
func TestGridDiffContiguousRunOneCUP(t *testing.T) {
	g := gridOf(6, 1, Cell{Glyph: 'a'})
	var d GridDiff
	if err := d.Emit(&bytes.Buffer{}, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	// Change a contiguous run (cols 1..3), leaving cols 0, 4, 5 unchanged.
	for _, col := range []int{1, 2, 3} {
		g.At(col, 0).Glyph = 'z'
	}
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "\x1b["); n != 1 {
		t.Fatalf("want one CUP for the contiguous run, got %d: %q", n, got)
	}
	if n := strings.Count(got, "z"); n != 3 {
		t.Fatalf("want 3 changed glyphs in the run, got %d: %q", n, got)
	}
}

// TestGridDiffReanchorsAfterNonASCIIOverlay checks a contiguous run keeps its
// single-CUP coalescing for pure-ASCII overlay text, but re-anchors the cursor
// with a fresh CUP for the cell after a non-ASCII overlay cell, so a terminal
// width mismatch cannot skew the rest of the row.
func TestGridDiffReanchorsAfterNonASCIIOverlay(t *testing.T) {
	// A pure-ASCII overlay run stays on the contiguous fast path: one CUP.
	ascii := gridOf(4, 1, Cell{})
	for col := range 4 {
		ascii.At(col, 0).Text = "x"
	}
	var da GridDiff
	var aout bytes.Buffer
	if err := da.Emit(&aout, ascii, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(aout.String(), "\x1b["); n != 1 {
		t.Fatalf("ASCII overlay run: want one CUP, got %d: %q", n, aout.String())
	}

	// A non-ASCII overlay cell mid-run forces a CUP for the cell after it.
	mixed := gridOf(4, 1, Cell{})
	mixed.At(0, 0).Text = "x"
	mixed.At(1, 0).Text = "’" // non-ASCII: untrusted width
	mixed.At(2, 0).Text = "y" // must re-anchor with a fresh CUP
	mixed.At(3, 0).Text = "z"
	var dm GridDiff
	var mout bytes.Buffer
	if err := dm.Emit(&mout, mixed, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := mout.String()
	// One CUP to start the run, one to re-anchor at column 2 after the non-ASCII
	// cell. Columns 2 and 3 then coalesce again, so exactly two CUPs total.
	if n := strings.Count(got, "\x1b["); n != 2 {
		t.Fatalf("mixed overlay run: want two CUPs, got %d: %q", n, got)
	}
	if !strings.Contains(got, "\x1b[1;3H") {
		t.Fatalf("mixed overlay run: missing re-anchor CUP to column 2: %q", got)
	}
}

// TestGridDiffSameColourRun checks a contiguous changed run of one colour sends
// one SGR pair and a single CUP.
func TestGridDiffSameColourRun(t *testing.T) {
	g := gridOf(4, 1, Cell{Glyph: 'a', FG: color.RGBA{R: 1}, BG: color.RGBA{B: 1}})
	var d GridDiff
	if err := d.Emit(&bytes.Buffer{}, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	for col := range 4 {
		g.At(col, 0).Glyph = 'z'
	}
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "<f"); n != 1 {
		t.Fatalf("want one fg SGR, got %d: %q", n, got)
	}
	if n := strings.Count(got, "<b"); n != 1 {
		t.Fatalf("want one bg SGR, got %d: %q", n, got)
	}
	if n := strings.Count(got, "\x1b[1;"); n != 1 {
		t.Fatalf("want one CUP for the run, got %d: %q", n, got)
	}
}
