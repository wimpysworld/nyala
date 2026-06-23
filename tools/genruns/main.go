// Command genruns deterministically generates the Stage 2 overlay run-set
// fixtures (testdata/overlay/*.runs) with contrasting foreground colours. For
// each run it composes the page through the real overlay path, reads each
// touched cell's bitmap background average, and picks a black or white
// foreground by the run region's mean background luma, so every glyph contrasts
// with its local background. The same inputs always yield the same colours, so
// the fixtures stay reproducible. It is a generator, not part of the shipped
// binary; run it from the repo root with `go run ./tools/genruns`.
package main

import (
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

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

// contrastFG picks black or white for a run by the mean background luma of the
// cells the run's graphemes land on, read from the composed grid. A light region
// gets black text, a dark region white, so the glyph contrasts with its local
// background.
func contrastFG(g block.Grid, run render.GlyphRun) color.RGBA {
	var sum, n float64
	col := run.Col
	row := run.Row
	for range []rune(run.Text) {
		if row < 0 || row >= g.Rows || col < 0 || col >= g.Cols {
			col++
			continue
		}
		c := g.At(col, row)
		sum += 0.299*float64(c.BG.R) + 0.587*float64(c.BG.G) + 0.114*float64(c.BG.B)
		n++
		col++
	}
	mean := 0.0
	if n > 0 {
		mean = sum / n
	}
	if mean > 128 {
		return color.RGBA{A: 0xFF}
	}
	return color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
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
