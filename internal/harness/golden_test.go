package harness

import (
	"image"
	"image/color"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// goldenFixtures lists the five committed pages and their source PNGs under
// testdata/. Each page has one txtar archive under testdata/golden/.
var goldenFixtures = []struct {
	page string
	png  string
}{
	{"linuxmatters", "linuxmatters.png"},
	{"testpage-01-grayscale", "testpage-01-grayscale.png"},
	{"testpage-02-basic", "testpage-02-basic.png"},
	{"testpage-03-cmyk", "testpage-03-cmyk.png"},
	{"testpage-04-color", "testpage-04-color.png"},
}

// goldenGrid composes a fixture PNG into the pinned 120x40 block grid, matching
// the sweep and calibration path. It first downscales the source to
// CalibCols*2 x CalibRows*4, the geometry Compose maps to one 2x4 block per cell;
// without this the full-resolution image is sampled at 2x4 px per cell, so only
// its top-left corner is read and a margined page composes to near-blank.
func goldenGrid(t *testing.T, png string) block.Grid {
	t.Helper()
	img := loadRGBA(t, filepath.Join("..", "..", "testdata", png))
	composeSrc := metrics.Downscale(img, CalibCols*2, CalibRows*4)
	br := block.Renderer{Cols: CalibCols, Rows: CalibRows}
	return br.Compose(render.Frame{Img: composeSrc})
}

// loadRGBA decodes a PNG into an *image.RGBA.
func loadRGBA(t *testing.T, path string) *image.RGBA {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if rgba, ok := src.(*image.RGBA); ok {
		return rgba
	}
	b := src.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
	return dst
}

// goldenPath returns the archive path for a page.
func goldenPath(page string) string {
	return filepath.Join("..", "..", "testdata", "golden", page+".txtar")
}

// updateGolden reports whether the UPDATE_GOLDEN environment flag is set.
func updateGolden() bool {
	v := os.Getenv("UPDATE_GOLDEN")
	return v != "" && v != "0"
}

// TestGoldenTrack compares each fixture's block grid against its committed
// txtar archive. Under UPDATE_GOLDEN the archives rewrite (write-if-changed).
func TestGoldenTrack(t *testing.T) {
	update := updateGolden()
	for _, fx := range goldenFixtures {
		t.Run(fx.page, func(t *testing.T) {
			g := goldenGrid(t, fx.png)
			ok, _, err := CompareGolden(goldenPath(fx.page), fx.page, g, nil, update)
			if err != nil {
				t.Fatalf("CompareGolden: %v", err)
			}
			if !ok {
				t.Fatalf("golden mismatch for %s; rerun with UPDATE_GOLDEN=1 to refresh", fx.page)
			}
		})
	}
}

// TestEncodeGridDeterministic checks the encoder is byte-stable across calls on
// the same grid.
func TestEncodeGridDeterministic(t *testing.T) {
	g := goldenGrid(t, "testpage-01-grayscale.png")
	a := EncodeGrid("testpage-01-grayscale", g, nil)
	b := EncodeGrid("testpage-01-grayscale", g, nil)
	if string(a) != string(b) {
		t.Fatal("EncodeGrid output differs across identical calls")
	}
}

// TestDriftFails checks a drifted grid fails the comparison without update.
func TestDriftFails(t *testing.T) {
	g := goldenGrid(t, "testpage-02-basic.png")
	*g.At(0, 0) = block.Cell{Glyph: '?', FG: color.RGBA{A: 0xff}, BG: color.RGBA{A: 0xff}}
	ok, wrote, err := CompareGolden(goldenPath("testpage-02-basic"), "testpage-02-basic", g, nil, false)
	if err != nil {
		t.Fatalf("CompareGolden: %v", err)
	}
	if ok || wrote {
		t.Fatalf("drifted grid passed (ok=%v wrote=%v), want fail", ok, wrote)
	}
}

// TestUpdateWritesOnlyOnChange checks write-if-changed: a matching grid does not
// rewrite the file, a drifted grid does.
func TestUpdateWritesOnlyOnChange(t *testing.T) {
	g := goldenGrid(t, "testpage-03-cmyk.png")
	tmp := filepath.Join(t.TempDir(), "page.txtar")
	if err := os.WriteFile(tmp, EncodeGrid("testpage-03-cmyk", g, nil), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ok, wrote, err := CompareGolden(tmp, "testpage-03-cmyk", g, nil, true)
	if err != nil || !ok {
		t.Fatalf("matching update: ok=%v wrote=%v err=%v", ok, wrote, err)
	}
	if wrote {
		t.Fatal("matching grid rewrote the file, want write-if-changed to skip")
	}

	*g.At(1, 1) = block.Cell{Glyph: '#', FG: color.RGBA{A: 0xff}, BG: color.RGBA{A: 0xff}}
	ok, wrote, err = CompareGolden(tmp, "testpage-03-cmyk", g, nil, true)
	if err != nil || !ok {
		t.Fatalf("drifted update: ok=%v wrote=%v err=%v", ok, wrote, err)
	}
	if !wrote {
		t.Fatal("drifted grid did not rewrite the file, want write-if-changed to write")
	}
}
