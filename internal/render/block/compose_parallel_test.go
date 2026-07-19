package block

import (
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/rowband"
)

// composeCases is the grid geometry sweep the byte-identity test runs for both
// schemes: sizes that split evenly and unevenly into bands, a single-row grid, and
// a sub-threshold grid that must fall back to serial.
var composeCases = []struct {
	name       string
	cols, rows int
}{
	{"120x40", 120, 40}, // even split into 8 bands
	{"240x86", 240, 86}, // uneven remainder across 8 bands
	{"80x41", 80, 41},   // odd height, uneven remainder
	{"60x3", 60, 3},     // below the band minimum: serial fallback
	{"60x1", 60, 1},     // single-row grid: serial fallback
	{"1x40", 1, 40},     // single column, tall grid
}

// TestParallelComposeByteIdentical proves the pool-split compose is byte-for-byte
// identical to the serial Renderer.ComposeInto for both the quadrant and sextant
// schemes, across grids that do and do not divide evenly into bands, a single-row
// grid, and a sub-threshold grid. It runs both with and without a text overlay, so
// the post-join overlay fold is covered too. Byte identity is the correctness
// contract: the parallel binarise must never shift a single cell from the serial
// path.
func TestParallelComposeByteIdentical(t *testing.T) {
	// Force eight workers so any grid tall enough splits into several bands,
	// exercising the parallel path even on a low-core test runner.
	pool := rowband.NewSized(8)
	defer pool.Close()
	pc := NewParallelCompose()

	schemes := []struct {
		name string
		s    Scheme
		rs   render.BlockScheme
	}{
		{"quadrant", SchemeQuadrant, render.BlockQuadrant},
		{"sextant", SchemeSextant, render.BlockSextant},
		{"halfblock", SchemeHalfBlock, render.BlockHalf},
	}
	for _, sc := range schemes {
		for _, overlay := range []bool{false, true} {
			for _, g := range composeCases {
				name := sc.name + "/" + g.name
				if overlay {
					name += "/overlay"
				}
				t.Run(name, func(t *testing.T) {
					br := Renderer{Cols: g.cols, Rows: g.rows, Scheme: sc.s}
					pxW, pxH := render.BlockCaptureSize(sc.rs, g.cols, g.rows)
					f := render.Frame{Img: benchImage(pxW, pxH)}
					if overlay {
						f.Text = benchRuns(g.rows, true)
					}

					var want Grid
					br.ComposeInto(&want, f)

					var got Grid
					pc.ComposeInto(&br, &got, f, pool)

					assertGridEqual(t, want, got)
				})
			}
		}
	}
}

// TestParallelComposeNilPool proves a nil pool runs the serial binarise.
func TestParallelComposeNilPool(t *testing.T) {
	pc := NewParallelCompose()
	br := Renderer{Cols: 120, Rows: 40, Scheme: SchemeQuadrant}
	pxW, pxH := render.BlockCaptureSize(render.BlockQuadrant, 120, 40)
	f := render.Frame{Img: benchImage(pxW, pxH), Text: benchRuns(40, true)}

	var want Grid
	br.ComposeInto(&want, f)
	var got Grid
	pc.ComposeInto(&br, &got, f, nil)
	assertGridEqual(t, want, got)
}

// TestParallelComposeReuse proves one driver and pool stay byte-identical across
// repeated calls into a reused grid, since the live path reuses all three every
// frame. A stale WaitGroup or leaked band would surface as drift on a later call.
func TestParallelComposeReuse(t *testing.T) {
	pool := rowband.NewSized(8)
	defer pool.Close()
	pc := NewParallelCompose()
	br := Renderer{Cols: 240, Rows: 86, Scheme: SchemeSextant}
	pxW, pxH := render.BlockCaptureSize(render.BlockSextant, 240, 86)
	f := render.Frame{Img: benchImage(pxW, pxH), Text: benchRuns(86, true)}

	var want Grid
	br.ComposeInto(&want, f)

	var got Grid
	for i := range 20 {
		pc.ComposeInto(&br, &got, f, pool)
		if !gridsEqual(want, got) {
			t.Fatalf("call %d: parallel compose differs from serial", i)
		}
	}
}

// gridsEqual reports whether two grids hold the same dimensions and cells.
func gridsEqual(a, b Grid) bool {
	if a.Cols != b.Cols || a.Rows != b.Rows || len(a.Cells) != len(b.Cells) {
		return false
	}
	for i := range a.Cells {
		if a.Cells[i] != b.Cells[i] {
			return false
		}
	}
	return true
}

// assertGridEqual fails the test with the first differing cell when a and b differ.
func assertGridEqual(t *testing.T, a, b Grid) {
	t.Helper()
	if a.Cols != b.Cols || a.Rows != b.Rows || len(a.Cells) != len(b.Cells) {
		t.Fatalf("grid shape differs: serial %dx%d (%d cells), parallel %dx%d (%d cells)",
			a.Cols, a.Rows, len(a.Cells), b.Cols, b.Rows, len(b.Cells))
	}
	for i := range a.Cells {
		if a.Cells[i] != b.Cells[i] {
			t.Fatalf("cell %d differs: serial %+v, parallel %+v", i, a.Cells[i], b.Cells[i])
		}
	}
}
