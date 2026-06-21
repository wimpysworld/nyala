package harness

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// synthRGBA builds a w x h RGBA test image with a deterministic gradient so the
// reference and render differ in a known, non-trivial way.
func synthRGBA(w, h int, shift uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x) + shift,
				G: uint8(y),
				B: uint8(x+y) + shift,
				A: 0xff,
			})
		}
	}
	return img
}

// synthGrid builds a cols x rows block grid with distinct per-cell colours so
// the cell and quant maps have something to shade.
func synthGrid(cols, rows int, fgShift uint8) block.Grid {
	g := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			*g.At(col, row) = block.Cell{
				Glyph: 0x2588,
				FG:    color.RGBA{R: uint8(col * 7), G: uint8(row * 9), B: fgShift, A: 0xff},
				BG:    color.RGBA{A: 0xff},
			}
		}
	}
	return g
}

// decodePNGSize decodes a PNG file and returns its pixel dimensions, failing the
// test if the bytes are not a valid PNG.
func decodePNGSize(t *testing.T, path string) (w, h int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy()
}

func TestEmitBlockFivePNGs(t *testing.T) {
	const cols, rows = 6, 4
	w, h := cols*8, rows*16
	ref := synthRGBA(w, h, 0)
	render := synthRGBA(w, h, 12)
	grid := synthGrid(cols, rows, 30)
	ideal := synthGrid(cols, rows, 90)

	dir := t.TempDir()
	set, err := Emit(dir, ArtefactInput{
		Reference: ref,
		Render:    render,
		Grid:      &grid,
		Ideal:     &ideal,
		Mode:      "block",
	})
	if err != nil {
		t.Fatalf("Emit block: %v", err)
	}

	names := []string{set.Composite, set.Heatmap, set.CellMap, set.QuantMap, set.Histogram}
	for _, n := range names {
		if n == "" {
			t.Fatalf("block mode omitted an artefact: %+v", set)
		}
		path := filepath.Join(dir, n)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", n, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("artefact %s is empty", n)
		}
		decodePNGSize(t, path) // fails on a non-PNG.
	}

	// The cell map and quant map use the owned grid, not a generic pixel diff:
	// their pixel size is the cell-tile size times the grid dimensions.
	const tile = 8
	if cw, ch := decodePNGSize(t, filepath.Join(dir, set.CellMap)); cw != cols*tile || ch != rows*tile {
		t.Fatalf("cell map size = %dx%d, want %dx%d", cw, ch, cols*tile, rows*tile)
	}
	if cw, ch := decodePNGSize(t, filepath.Join(dir, set.QuantMap)); cw != cols*tile || ch != rows*tile {
		t.Fatalf("quant map size = %dx%d, want %dx%d", cw, ch, cols*tile, rows*tile)
	}
}

func TestEmitKittyOmitsQuantMap(t *testing.T) {
	w, h := 6*8, 3*16
	ref := synthRGBA(w, h, 0)
	render := synthRGBA(w, h, 5)

	dir := t.TempDir()
	set, err := Emit(dir, ArtefactInput{
		Reference: ref,
		Render:    render,
		Mode:      "kitty",
	})
	if err != nil {
		t.Fatalf("Emit kitty: %v", err)
	}
	if set.QuantMap != "" {
		t.Fatalf("kitty mode emitted a quant map: %q", set.QuantMap)
	}
	if _, err := os.Stat(filepath.Join(dir, quantMapName)); !os.IsNotExist(err) {
		t.Fatalf("kitty quant map file present, want absent: err=%v", err)
	}
	// The composite, heatmap, and histogram always emit; the cell map needs a
	// grid, absent here.
	for _, n := range []string{set.Composite, set.Heatmap, set.Histogram} {
		if n == "" {
			t.Fatalf("kitty omitted a required artefact: %+v", set)
		}
		decodePNGSize(t, filepath.Join(dir, n))
	}
	if set.CellMap != "" {
		t.Fatalf("kitty without a grid emitted a cell map: %q", set.CellMap)
	}
}

func TestEmitTimestampFree(t *testing.T) {
	const cols, rows = 5, 3
	w, h := cols*8, rows*16
	ref := synthRGBA(w, h, 0)
	render := synthRGBA(w, h, 8)
	grid := synthGrid(cols, rows, 20)
	ideal := synthGrid(cols, rows, 80)

	in := ArtefactInput{Reference: ref, Render: render, Grid: &grid, Ideal: &ideal, Mode: "block"}

	dirA, dirB := t.TempDir(), t.TempDir()
	if _, err := Emit(dirA, in); err != nil {
		t.Fatalf("Emit A: %v", err)
	}
	if _, err := Emit(dirB, in); err != nil {
		t.Fatalf("Emit B: %v", err)
	}
	for _, n := range []string{compositeName, heatmapName, cellMapName, quantMapName, histogramName} {
		a, err := os.ReadFile(filepath.Join(dirA, n))
		if err != nil {
			t.Fatalf("read A %s: %v", n, err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, n))
		if err != nil {
			t.Fatalf("read B %s: %v", n, err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("artefact %s differs across runs: PNG is not timestamp-free", n)
		}
	}
}

func TestEmitDimensionMismatch(t *testing.T) {
	ref := synthRGBA(48, 64, 0)
	render := synthRGBA(40, 64, 0)
	if _, err := Emit(t.TempDir(), ArtefactInput{Reference: ref, Render: render, Mode: "block"}); err == nil {
		t.Fatal("Emit accepted mismatched dimensions, want error")
	}
}
