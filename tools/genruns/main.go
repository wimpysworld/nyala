// Command genruns deterministically generates the overlay run-set
// fixtures (testdata/overlay/*.runs) with contrasting foreground colours. For
// each run it composes the page through the real overlay path, reads each
// touched cell's bitmap background average, and picks the black or white
// foreground with the higher WCAG contrast against that average, so every glyph
// contrasts with its local background. The same inputs always yield the same
// colours, so the fixtures stay reproducible. It is a generator, not part of the
// shipped binary; run it from the repo root with `go run ./tools/genruns`.
package main

import (
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/wimpysworld/nyala/internal/harness"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

func main() {
	for _, fx := range harness.OverlayFixtures {
		if err := regenerate(fx); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", fx.Page, err)
			os.Exit(1)
		}
	}
}

// regenerate composes one fixture and rewrites its .runs file with contrasting
// foreground colours, preserving the run positions, order, and text.
func regenerate(fx harness.OverlayFixture) error {
	img, err := loadRGBA(filepath.Join("testdata", fx.PNG))
	if err != nil {
		return err
	}
	composeSrc := downscale(img)
	runsPath := filepath.Join("testdata", "overlay", fx.Runs)
	runs, err := harness.LoadRunSet(runsPath)
	if err != nil {
		return err
	}
	br := block.Renderer{Cols: harness.CalibCols, Rows: harness.CalibRows}
	g := br.Compose(render.Frame{Img: composeSrc, Text: runs})

	out := make([]render.GlyphRun, len(runs))
	for i, run := range runs {
		fg := contrastFG(g, run)
		out[i] = render.GlyphRun{Col: run.Col, Row: run.Row, Text: run.Text, FG: fg}
	}
	return writeRuns(runsPath, fx.Page, out)
}

// contrastFG picks black or white for a run by WCAG contrast against the mean
// background of the cells the run's graphemes land on, read from the composed
// grid. It steps by grapheme cluster and display width, matching applyOverlay, so
// wide or clustered text samples the lead cell of each cluster and advances by
// its width. It accumulates the average background RGB over those cells, then
// returns whichever of black or white has the higher WCAGContrast against that
// average. An exact tie returns black, so the choice stays deterministic.
func contrastFG(g block.Grid, run render.GlyphRun) color.RGBA {
	var sumR, sumG, sumB, n float64
	col := run.Col
	row := run.Row
	gr := uniseg.NewGraphemes(run.Text)
	for gr.Next() {
		w := runewidth.StringWidth(gr.Str())
		if w >= 2 {
			w = 2
		} else {
			w = 1
		}
		if row < 0 || row >= g.Rows || col < 0 || col >= g.Cols {
			col += w
			continue
		}
		c := g.At(col, row)
		sumR += float64(c.BG.R)
		sumG += float64(c.BG.G)
		sumB += float64(c.BG.B)
		n++
		col += w
	}
	black := color.RGBA{A: 0xFF}
	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	if n == 0 {
		return black
	}
	bgR := meanByte(sumR, n)
	bgG := meanByte(sumG, n)
	bgB := meanByte(sumB, n)
	blackContrast := metrics.WCAGContrast(black.R, black.G, black.B, bgR, bgG, bgB)
	whiteContrast := metrics.WCAGContrast(white.R, white.G, white.B, bgR, bgG, bgB)
	if whiteContrast > blackContrast {
		return white
	}
	return black
}

// meanByte returns sum/count rounded to an 8-bit value, the average background
// channel the WCAG contrast choice reads.
func meanByte(sum, count float64) uint8 {
	v := sum/count + 0.5
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// writeRuns writes the run set back to path, one "col row fgHex text" line per
// run, with a short header naming the page.
func writeRuns(path, page string, runs []render.GlyphRun) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Overlay run set for the %s page.\n", page)
	b.WriteString("# Format: col row fgHex text. text runs to end of line.\n")
	b.WriteString("# Foreground is chosen per run to contrast with its local background.\n")
	for _, r := range runs {
		fmt.Fprintf(&b, "%d %d %02X%02X%02X %s\n", r.Col, r.Row, r.FG.R, r.FG.G, r.FG.B, r.Text)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// downscale resizes the source to the 2x4-per-cell compose extent of the pinned
// 120x40 grid through metrics.Downscale, the exact geometry and filter
// composeOverlay uses, so the generated colours match the grade harness.
func downscale(img *image.RGBA) *image.RGBA {
	return metrics.Downscale(img, harness.CalibCols*2, harness.CalibRows*4)
}

// loadRGBA decodes a PNG into an *image.RGBA.
func loadRGBA(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	if rgba, ok := src.(*image.RGBA); ok {
		return rgba, nil
	}
	bb := src.Bounds()
	dst := image.NewRGBA(bb)
	for y := bb.Min.Y; y < bb.Max.Y; y++ {
		for x := bb.Min.X; x < bb.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
	return dst, nil
}
