package block

import (
	"bytes"
	"errors"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

var errDiffWrite = errors.New("diff write")

// fixedFG and fixedBG are colour-mode-neutral SGR funcs for the differ tests: a
// short stable token per colour, so the assertions read the escape stream
// without truecolour noise.
func fixedFG(dst []byte, c color.RGBA) []byte       { return append(dst, '<', 'f', '0'+c.R, '>') }
func fixedBG(dst []byte, c color.RGBA) []byte       { return append(dst, '<', 'b', '0'+c.B, '>') }
func wrappedTrueFG(dst []byte, c color.RGBA) []byte { return AppendTrueColorFG(dst, c) }
func wrappedTrueBG(dst []byte, c color.RGBA) []byte { return AppendTrueColorBG(dst, c) }

func gridOf(cols, rows int, fill Cell) Grid {
	g := Grid{Cols: cols, Rows: rows, Cells: make([]Cell, cols*rows)}
	for i := range g.Cells {
		g.Cells[i] = fill
	}
	return g
}

// TestGridDiffCUPScratchByteIdentical compares the retained CUP scratch against
// an independent stack-buffer encoder across every row and column from 0
// through 999. This covers each decimal-width boundary used by practical grids.
func TestGridDiffCUPScratchByteIdentical(t *testing.T) {
	var d GridDiff
	var got bytes.Buffer
	for row := range 1000 {
		for col := range 1000 {
			got.Reset()
			if err := d.writeCUP(&got, row, col); err != nil {
				t.Fatal(err)
			}
			want := legacyCUP(row, col)
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("CUP (%d,%d) = %q, want %q", col, row, got.Bytes(), want)
			}
		}
	}
}

// TestGridDiffCUPFreshScratchNoAlloc checks that a zero-value differ can emit
// its first CUP without allocating backing storage for the fixed scratch array.
func TestGridDiffCUPFreshScratchNoAlloc(t *testing.T) {
	var d GridDiff
	var writeErr error
	allocs := testing.AllocsPerRun(1000, func() {
		d = GridDiff{}
		writeErr = d.writeCUP(io.Discard, 85, 239)
	})
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if allocs != 0 {
		t.Fatalf("fresh CUP allocations = %v, want 0", allocs)
	}
}

func legacyCUP(row, col int) []byte {
	var buf [16]byte
	b := buf[:0]
	b = append(b, '\x1b', '[')
	b = appendInt(b, row+1)
	b = append(b, ';')
	b = appendInt(b, col+1)
	return append(b, 'H')
}

// TestGridDiffTrueColourMatchesGenericState compares the combined specialised
// stream with the separate-SGR generic stream through an independent VT model.
func TestGridDiffTrueColourMatchesGenericState(t *testing.T) {
	fg, bg := SGRFunc(wrappedTrueFG), SGRFunc(wrappedTrueBG)
	base := gridOf(4, 2, Cell{
		Glyph: 'a', FG: color.RGBA{R: 1, G: 2, B: 3, A: 0xff},
		BG: color.RGBA{R: 4, G: 5, B: 6, A: 0xff},
	})
	zero := gridOf(4, 2, Cell{Glyph: 'a'})
	black := gridOf(4, 2, Cell{Glyph: 'a', FG: color.RGBA{A: 0xff}, BG: color.RGBA{A: 0xff}})
	frames := []Grid{zero, black, cloneDiffGrid(base), cloneDiffGrid(base)}

	fgOnly := cloneDiffGrid(base)
	fgOnly.At(1, 0).FG.R++
	frames = append(frames, fgOnly)
	bgOnly := cloneDiffGrid(fgOnly)
	bgOnly.At(2, 0).BG.B++
	frames = append(frames, bgOnly)
	both := cloneDiffGrid(bgOnly)
	both.At(0, 1).FG.G++
	both.At(0, 1).BG.R++
	frames = append(frames, both)
	alphaOnly := cloneDiffGrid(both)
	alphaOnly.At(0, 1).FG.A = 1
	alphaOnly.At(0, 1).BG.A = 2
	frames = append(frames, alphaOnly)
	sparse := cloneDiffGrid(alphaOnly)
	sparse.At(0, 0).Glyph = 'x'
	sparse.At(3, 1).Glyph = 'y'
	frames = append(frames, sparse)
	motion := cloneDiffGrid(sparse)
	for i := range motion.Cells {
		motion.Cells[i].Glyph = rune('k' + i)
		motion.Cells[i].FG.R += uint8(i + 1)
		motion.Cells[i].BG.B += uint8(i + 1)
	}
	frames = append(frames, motion)
	overlay := cloneDiffGrid(motion)
	overlay.At(0, 0).Text = "A"
	overlay.At(1, 0).Text = "界"
	overlay.At(2, 0).Wide = true
	overlay.At(3, 0).Text = "’"
	frames = append(frames, overlay)
	frames = append(frames, gridOf(5, 3, base.Cells[0]))

	var fast, generic GridDiff
	var fastStream, genericStream bytes.Buffer
	for i, frame := range frames {
		var fastOut, genericOut bytes.Buffer
		if err := fast.EmitTrueColour(&fastOut, frame); err != nil {
			t.Fatalf("frame %d fast: %v", i, err)
		}
		if err := generic.Emit(&genericOut, frame, fg, bg); err != nil {
			t.Fatalf("frame %d generic: %v", i, err)
		}
		if fastOut.Len() > genericOut.Len() {
			t.Fatalf("frame %d combined payload = %d, separate = %d", i, fastOut.Len(), genericOut.Len())
		}
		fastStream.Write(fastOut.Bytes())
		genericStream.Write(genericOut.Bytes())
		assertVTEquivalent(t, i, fastStream.Bytes(), genericStream.Bytes())
	}
	alphaRun := gridOf(2, 1, Cell{
		Glyph: 'a', FG: color.RGBA{R: 1, G: 2, B: 3, A: 1},
		BG: color.RGBA{R: 4, G: 5, B: 6, A: 1},
	})
	alphaRun.At(1, 0).FG.A = 2
	alphaRun.At(1, 0).BG.A = 2
	var alphaOut bytes.Buffer
	if err := (&GridDiff{}).EmitTrueColour(&alphaOut, alphaRun); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(alphaOut.Bytes(), []byte("m")); got != 1 {
		t.Fatalf("alpha-only run emitted %d RGB SGRs, want one combined SGR", got)
	}
	fast.Reset()
	generic.Reset()
	var fastOut, genericOut bytes.Buffer
	if err := fast.EmitTrueColour(&fastOut, base); err != nil {
		t.Fatal(err)
	}
	if err := generic.Emit(&genericOut, base, fg, bg); err != nil {
		t.Fatal(err)
	}
	assertVTEquivalent(t, len(frames), fastOut.Bytes(), genericOut.Bytes())
}

func assertVTEquivalent(t *testing.T, frame int, combined, separate []byte) {
	t.Helper()
	combinedVT := vt.NewEmulator(12, 8)
	separateVT := vt.NewEmulator(12, 8)
	writeChunks := func(e *vt.Emulator, stream []byte, chunk int) {
		for len(stream) > 0 {
			n := min(chunk, len(stream))
			if _, err := e.Write(stream[:n]); err != nil {
				t.Fatalf("frame %d VT write: %v", frame, err)
			}
			stream = stream[n:]
		}
	}
	writeChunks(combinedVT, combined, 1)
	writeChunks(separateVT, separate, 7)
	if got, want := combinedVT.CursorPosition(), separateVT.CursorPosition(); got != want {
		t.Fatalf("frame %d cursor = %v, want %v", frame, got, want)
	}
	// A probe written without another SGR exposes the retained RGB rendition.
	writeChunks(combinedVT, []byte("\x1b[8;1H#"), 1)
	writeChunks(separateVT, []byte("\x1b[8;1H#"), 2)
	for row := range 8 {
		for col := range 12 {
			got, want := combinedVT.CellAt(col, row), separateVT.CellAt(col, row)
			if (got == nil) != (want == nil) || (got != nil && !got.Equal(want)) {
				t.Fatalf("frame %d cell (%d,%d) = %#v, want %#v", frame, col, row, got, want)
			}
		}
	}
	if got, want := combinedVT.CursorPosition(), separateVT.CursorPosition(); got != want {
		t.Fatalf("frame %d probe cursor = %v, want %v", frame, got, want)
	}
}

func TestGridDiffTrueColourCombinedSequence(t *testing.T) {
	base := Cell{
		Glyph: 'a',
		FG:    color.RGBA{R: 1, G: 2, B: 3, A: 1},
		BG:    color.RGBA{R: 4, G: 5, B: 6, A: 2},
	}
	t.Run("both", func(t *testing.T) {
		var out bytes.Buffer
		if err := (&GridDiff{}).EmitTrueColour(&out, gridOf(1, 1, base)); err != nil {
			t.Fatal(err)
		}
		const want = "\x1b[1;1H\x1b[38;2;1;2;3;48;2;4;5;6ma"
		if got := out.String(); got != want {
			t.Fatalf("combined output = %q, want %q", got, want)
		}
	})
	t.Run("foreground only", func(t *testing.T) {
		changed := base
		changed.Glyph, changed.FG.R = 'b', 7
		g := Grid{Cols: 2, Rows: 1, Cells: []Cell{base, changed}}
		var out bytes.Buffer
		if err := (&GridDiff{}).EmitTrueColour(&out, g); err != nil {
			t.Fatal(err)
		}
		const want = "\x1b[1;1H\x1b[38;2;1;2;3;48;2;4;5;6ma\x1b[38;2;7;2;3mb"
		if got := out.String(); got != want {
			t.Fatalf("foreground output = %q, want %q", got, want)
		}
	})
	t.Run("background only", func(t *testing.T) {
		changed := base
		changed.Glyph, changed.BG.B = 'b', 7
		g := Grid{Cols: 2, Rows: 1, Cells: []Cell{base, changed}}
		var out bytes.Buffer
		if err := (&GridDiff{}).EmitTrueColour(&out, g); err != nil {
			t.Fatal(err)
		}
		const want = "\x1b[1;1H\x1b[38;2;1;2;3;48;2;4;5;6ma\x1b[48;2;4;5;7mb"
		if got := out.String(); got != want {
			t.Fatalf("background output = %q, want %q", got, want)
		}
	})
}

func cloneDiffGrid(g Grid) Grid {
	clone := Grid{Cols: g.Cols, Rows: g.Rows, Cells: make([]Cell, len(g.Cells))}
	copy(clone.Cells, g.Cells)
	return clone
}

type failDiffWriter struct {
	writes int
	failAt int
}

func (w *failDiffWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, errDiffWrite
	}
	return len(p), nil
}

// TestGridDiffTrueColourErrors checks that combined, FG-only, and BG-only SGR
// writes return the writer error without advancing to the glyph.
func TestGridDiffTrueColourErrors(t *testing.T) {
	for _, cell := range []Cell{
		{Glyph: 'a', FG: color.RGBA{R: 1}, BG: color.RGBA{B: 2}},
		{Glyph: 'a', FG: color.RGBA{R: 1}},
		{Glyph: 'a', BG: color.RGBA{B: 2}},
	} {
		for failAt := 1; failAt <= 3; failAt++ {
			var d GridDiff
			w := &failDiffWriter{failAt: failAt}
			err := d.EmitTrueColour(w, gridOf(1, 1, cell))
			if !errors.Is(err, errDiffWrite) || w.writes != failAt {
				t.Fatalf("cell %#v failAt %d: error = %v after %d writes", cell, failAt, err, w.writes)
			}
			var retry bytes.Buffer
			if err := d.EmitTrueColour(&retry, gridOf(1, 1, cell)); err != nil {
				t.Fatalf("cell %#v failAt %d retry: %v", cell, failAt, err)
			}
			if retry.Len() == 0 {
				t.Fatalf("cell %#v failAt %d retry emitted nothing", cell, failAt)
			}
		}
	}
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

// TestGridDiffWideOverlayByteStream checks that a wide grapheme skips its
// trailing cell and re-anchors the following cell with the same CUP bytes.
func TestGridDiffWideOverlayByteStream(t *testing.T) {
	fg := color.RGBA{A: 0xff}
	bg := color.RGBA{A: 0xff}
	g := Grid{Cols: 3, Rows: 1, Cells: []Cell{
		{Text: "界", FG: fg, BG: bg},
		{Wide: true, FG: fg, BG: bg},
		{Text: "x", FG: fg, BG: bg},
	}}
	var d GridDiff
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	const want = "\x1b[1;1H<f0><b0>界\x1b[1;3Hx"
	if got := out.String(); got != want {
		t.Fatalf("wide overlay stream = %q, want %q", got, want)
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

// linkOpens counts OSC 8 open sequences in an emitted stream. The differ always
// writes the id= parameter, so the close sequence never matches this prefix.
func linkOpens(s string) int { return strings.Count(s, "\x1b]8;id=") }

// linkCloses counts OSC 8 close sequences in an emitted stream.
func linkCloses(s string) int { return strings.Count(s, "\x1b]8;;\x1b\\") }

// linkedGrid builds a cols x 1 grid whose every cell carries text "x" and the
// same shared hyperlink pointer, the shape the overlay fold stamps for one run.
func linkedGrid(cols int, l *Hyperlink) Grid {
	g := gridOf(cols, 1, Cell{Text: "x", Link: l})
	return g
}

// TestGridDiffLinkedRunOneOpenBalanced pins the run bracket: a run of cells
// sharing one hyperlink emits exactly one open, with the shared id, and one
// close, so every emitted frame is open/close balanced.
func TestGridDiffLinkedRunOneOpenBalanced(t *testing.T) {
	l := &Hyperlink{ID: "l1", URI: "https://example.org/a"}
	var d GridDiff
	var out bytes.Buffer
	if err := d.Emit(&out, linkedGrid(3, l), fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := linkOpens(got); n != 1 {
		t.Fatalf("want one link open for the shared run, got %d: %q", n, got)
	}
	if !strings.Contains(got, "\x1b]8;id=l1;https://example.org/a\x1b\\") {
		t.Fatalf("missing the open with the shared id: %q", got)
	}
	if linkOpens(got) != linkCloses(got) {
		t.Fatalf("unbalanced links: %d opens, %d closes: %q", linkOpens(got), linkCloses(got), got)
	}
}

// TestGridDiffTwoLinksDistinctIDs pins that two different links on one row emit
// two opens with distinct ids, and the frame stays balanced.
func TestGridDiffTwoLinksDistinctIDs(t *testing.T) {
	a := &Hyperlink{ID: "a1", URI: "https://example.org/a"}
	b := &Hyperlink{ID: "b2", URI: "https://example.org/b"}
	g := gridOf(4, 1, Cell{Text: "x"})
	g.At(0, 0).Link = a
	g.At(1, 0).Link = a
	g.At(2, 0).Link = b
	g.At(3, 0).Link = b
	var d GridDiff
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "\x1b]8;id=a1;") || !strings.Contains(got, "\x1b]8;id=b2;") {
		t.Fatalf("missing one of the two distinct link ids: %q", got)
	}
	if linkOpens(got) != 2 || linkOpens(got) != linkCloses(got) {
		t.Fatalf("want two balanced opens, got %d opens, %d closes: %q", linkOpens(got), linkCloses(got), got)
	}
}

// TestGridDiffPartialLinkedRunRewriteReopens pins the diff path: a later frame
// that rewrites part of a linked run must re-open the link with the same id
// around the rewritten cell and close it, so the terminal never paints the cell
// link-free.
func TestGridDiffPartialLinkedRunRewriteReopens(t *testing.T) {
	l := &Hyperlink{ID: "l1", URI: "https://example.org/a"}
	g := linkedGrid(3, l)
	var d GridDiff
	if err := d.Emit(&bytes.Buffer{}, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	next := cloneDiffGrid(g)
	next.At(1, 0).Text = "y"
	var out bytes.Buffer
	if err := d.Emit(&out, next, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "\x1b]8;id=l1;") {
		t.Fatalf("rewritten linked cell missing a re-open with the run's id: %q", got)
	}
	if linkOpens(got) != 1 || linkCloses(got) != 1 {
		t.Fatalf("want one open and one close, got %d and %d: %q", linkOpens(got), linkCloses(got), got)
	}
}

// TestGridDiffLinkChangeRepaints pins cellUnchanged: a cell identical in glyph,
// colours, and text but with a changed link must repaint, or the terminal keeps
// the stale target.
func TestGridDiffLinkChangeRepaints(t *testing.T) {
	g := linkedGrid(2, &Hyperlink{ID: "l1", URI: "https://example.org/a"})
	var d GridDiff
	if err := d.Emit(&bytes.Buffer{}, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	next := cloneDiffGrid(g)
	changed := &Hyperlink{ID: "l1", URI: "https://example.org/b"}
	next.At(0, 0).Link = changed
	next.At(1, 0).Link = changed
	var out bytes.Buffer
	if err := d.Emit(&out, next, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "https://example.org/b") {
		t.Fatalf("link-only change did not repaint with the new target: %q", got)
	}
	if linkOpens(got) != linkCloses(got) {
		t.Fatalf("unbalanced links: %q", got)
	}
}

// TestGridDiffLinkFreeFrameByteFixture pins the revert proof: a frame with no
// links emits exactly the byte stream the pre-hyperlink differ emitted. The
// fixture is the captured expected stream for this grid, not a re-derivation.
func TestGridDiffLinkFreeFrameByteFixture(t *testing.T) {
	g := gridOf(2, 1, Cell{Glyph: 'a', FG: color.RGBA{R: 1}, BG: color.RGBA{B: 2}})
	var d GridDiff
	var out bytes.Buffer
	if err := d.Emit(&out, g, fixedFG, fixedBG); err != nil {
		t.Fatal(err)
	}
	const want = "\x1b[1;1H<f1><b2>aa"
	if got := out.String(); got != want {
		t.Fatalf("link-free frame = %q, want the pre-hyperlink bytes %q", got, want)
	}
}
