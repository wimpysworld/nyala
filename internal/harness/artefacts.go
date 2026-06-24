// The agent-facing artefact PNGs.
//
// For one (page, mode) the sweep emits five PNGs that let an agent read a
// regression without opening a terminal:
//
//  1. composite: reference, render, and pixel diff side by side.
//  2. heatmap:   the per-pixel diff, brighter where the render disagrees.
//  3. cellMap:   a per-cell error map on the owned block grid, each cell shaded
//     by its mean CIEDE2000 dE, so the error reads in cell space, not pixels.
//  4. quantMap:  a block-mode quant-error map, each cell shaded by the dE the
//     block quantiser added over the ideal block render. Omitted for kitty,
//     which quantises nothing.
//  5. histogram: the reference and render colour histograms overlaid per
//     channel, so palette collapse and banding show at a glance.
//
// The diff and heatmap use raf555/pixelmatch; the composite and the two cell
// maps use fogleman/gg; the histogram uses gonum/plot. Every PNG is written
// through capture.EncodePNG, so no timestamp is embedded and the same inputs
// yield byte-identical files. Channel order is RGBA throughout.
package harness

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"

	"github.com/fogleman/gg"
	pixelmatch "github.com/raf555/pixelmatch"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	vgdraw "gonum.org/v1/plot/vg/draw"
	"gonum.org/v1/plot/vg/vgimg"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// Artefact filenames, fixed per (page, mode). The sweep records these names in
// the report so an agent can find each PNG without guessing.
const (
	compositeName = "composite.png"
	heatmapName   = "heatmap.png"
	cellMapName   = "cellmap.png"
	quantMapName  = "quantmap.png"
	histogramName = "histogram.png"
)

// diffThreshold is the pixelmatch matching tolerance in 0..1. A lower value
// flags more pixels as different; this mid value matches the pixelmatch default
// so anti-aliasing noise does not swamp the diff.
const diffThreshold = 0.1

// ArtefactSet holds the relative filenames of the PNGs Emit wrote for one
// (page, mode). QuantMap is empty for kitty, where no block quantisation
// happens, so the report serialises it as null.
type ArtefactSet struct {
	// Composite is the side-by-side reference, render, and diff PNG.
	Composite string
	// Heatmap is the per-pixel diff heatmap PNG.
	Heatmap string
	// CellMap is the per-cell CIEDE2000 error map PNG on the owned grid.
	CellMap string
	// QuantMap is the block-mode quant-error map PNG, empty for kitty.
	QuantMap string
	// Histogram is the reference-versus-render colour-histogram pair PNG.
	Histogram string
}

// ArtefactInput is the data Emit needs to draw one (page, mode) artefact set:
// the aligned reference and render images at the same dimensions, the owned
// block grid, and the ideal block grid the quant-error map measures against.
// Grid and Ideal may be nil for a mode with no owned grid (the quant map is
// then skipped); for block they are the composed grid and metrics.IdealGrid.
type ArtefactInput struct {
	// Reference is the reference image downscaled to the render dimensions.
	Reference *image.RGBA
	// Render is the captured render image, same dimensions as Reference.
	Render *image.RGBA
	// Grid is the owned block cell grid behind the render, or nil.
	Grid *block.Grid
	// Ideal is the ideal block grid for the quant-error map, or nil. It must
	// share Grid's dimensions when both are set.
	Ideal *block.Grid
	// Mode is the render mode ("block", "sixel", "kitty"). The quant-error map
	// is emitted only when Mode is "block".
	Mode string
}

// Emit writes the five (or four, for kitty) artefact PNGs for one (page, mode)
// into dir and returns their relative filenames. The composite, heatmap, and
// histogram always emit; the cell map emits when a grid is present; the
// quant-error map emits only for the block mode with a grid and an ideal grid.
// dir is created if missing. Reference and Render must share dimensions.
func Emit(dir string, in ArtefactInput) (ArtefactSet, error) {
	if in.Reference == nil || in.Render == nil {
		return ArtefactSet{}, fmt.Errorf("harness: artefact emit needs both reference and render images")
	}
	rb, gb := in.Reference.Bounds(), in.Render.Bounds()
	if rb.Dx() != gb.Dx() || rb.Dy() != gb.Dy() {
		return ArtefactSet{}, fmt.Errorf("harness: reference %v and render %v dimensions differ", rb.Size(), gb.Size())
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return ArtefactSet{}, err
	}

	var set ArtefactSet

	diff, err := diffImage(in.Reference, in.Render)
	if err != nil {
		return ArtefactSet{}, err
	}
	if err := writePNG(filepath.Join(dir, compositeName), compositeImage(in.Reference, in.Render, diff)); err != nil {
		return ArtefactSet{}, err
	}
	set.Composite = compositeName

	if err := writePNG(filepath.Join(dir, heatmapName), diff); err != nil {
		return ArtefactSet{}, err
	}
	set.Heatmap = heatmapName

	if in.Grid != nil {
		cell := cellErrorMap(in.Reference, in.Render, in.Grid)
		if err := writePNG(filepath.Join(dir, cellMapName), cell); err != nil {
			return ArtefactSet{}, err
		}
		set.CellMap = cellMapName
	}

	if in.Mode == "block" && in.Grid != nil && in.Ideal != nil {
		quant, err := quantErrorMap(in.Grid, in.Ideal)
		if err != nil {
			return ArtefactSet{}, err
		}
		if err := writePNG(filepath.Join(dir, quantMapName), quant); err != nil {
			return ArtefactSet{}, err
		}
		set.QuantMap = quantMapName
	}

	hist, err := histogramPair(in.Reference, in.Render)
	if err != nil {
		return ArtefactSet{}, err
	}
	if err := writePNG(filepath.Join(dir, histogramName), hist); err != nil {
		return ArtefactSet{}, err
	}
	set.Histogram = histogramName

	return set, nil
}

// diffImage runs pixelmatch over the reference and render and returns the diff
// image: mismatched pixels are drawn over a faded copy of the reference, so the
// heatmap reads as "where and how much the render disagrees".
func diffImage(reference, render *image.RGBA) (*image.RGBA, error) {
	out, _, err := pixelmatch.CompareToImage(
		reference, render,
		pixelmatch.WithThreshold(diffThreshold),
	)
	if err != nil {
		return nil, fmt.Errorf("harness: pixelmatch diff: %w", err)
	}
	return drawImageRGBA(out), nil
}

// compositeImage lays the reference, render, and diff side by side with a thin
// gutter and a label over each panel, so the three read as one strip.
func compositeImage(reference, render, diff *image.RGBA) *image.RGBA {
	b := reference.Bounds()
	w, h := b.Dx(), b.Dy()
	const gutter = 8
	const labelH = 16
	totalW := w*3 + gutter*2
	totalH := h + labelH

	dc := gg.NewContext(totalW, totalH)
	dc.SetRGB(1, 1, 1)
	dc.Clear()

	panels := []struct {
		img   *image.RGBA
		label string
	}{
		{reference, "reference"},
		{render, "render"},
		{diff, "diff"},
	}
	dc.SetRGB(0, 0, 0)
	for i, p := range panels {
		x := i * (w + gutter)
		dc.DrawImage(p.img, x, labelH)
		dc.DrawString(p.label, float64(x)+2, float64(labelH)-4)
	}
	return contextRGBA(dc)
}

// cellErrorMap draws one tile per grid cell, shaded by the mean CIEDE2000 dE
// between the render and the reference over that cell's pixel footprint. The
// map is on the owned grid, not a generic pixel diff: each tile is one cell, so
// the error reads in cell space. Hotter (more red) means a larger dE.
func cellErrorMap(reference, render *image.RGBA, g *block.Grid) *image.RGBA {
	const tile = 8
	dc := gg.NewContext(g.Cols*tile, g.Rows*tile)
	dc.SetRGB(0, 0, 0)
	dc.Clear()

	cw := render.Bounds().Dx() / max(g.Cols, 1)
	ch := render.Bounds().Dy() / max(g.Rows, 1)
	for row := range g.Rows {
		for col := range g.Cols {
			de := cellDeltaE(reference, render, col, row, cw, ch)
			r, gg2 := heatColour(de / maxCellDeltaE)
			dc.SetRGB(r, gg2, 0)
			dc.DrawRectangle(float64(col*tile), float64(row*tile), tile, tile)
			dc.Fill()
		}
	}
	return contextRGBA(dc)
}

// quantErrorMap draws one tile per grid cell, shaded by the CIEDE2000 dE between
// the real block cell's foreground colour and the ideal block cell's
// foreground colour. It isolates the colour error the block quantiser added
// over the ideal block render, cell by cell, on the owned grid. The grids must
// share dimensions.
func quantErrorMap(grid, ideal *block.Grid) (*image.RGBA, error) {
	if grid.Cols != ideal.Cols || grid.Rows != ideal.Rows {
		return nil, fmt.Errorf("harness: quant map grids differ: %dx%d vs %dx%d",
			grid.Cols, grid.Rows, ideal.Cols, ideal.Rows)
	}
	const tile = 8
	dc := gg.NewContext(grid.Cols*tile, grid.Rows*tile)
	dc.SetRGB(0, 0, 0)
	dc.Clear()

	for row := range grid.Rows {
		for col := range grid.Cols {
			gc := grid.At(col, row)
			ic := ideal.At(col, row)
			de := metrics.DeltaE2000(gc.FG.R, gc.FG.G, gc.FG.B, ic.FG.R, ic.FG.G, ic.FG.B)
			r, gg2 := heatColour(de / maxCellDeltaE)
			dc.SetRGB(r, gg2, 0)
			dc.DrawRectangle(float64(col*tile), float64(row*tile), tile, tile)
			dc.Fill()
		}
	}
	return contextRGBA(dc), nil
}

// maxCellDeltaE is the dE that maps to the hottest map colour. CIEDE2000 dE runs
// 0..100; clamping at 20 spends the colour range on the small differences a
// render actually produces, where 20 already reads as a clear mismatch.
const maxCellDeltaE = 20.0

// cellDeltaE returns the mean per-pixel CIEDE2000 dE between the reference and
// the render over cell (col, row)'s pixel footprint (cw by ch pixels).
func cellDeltaE(reference, render *image.RGBA, col, row, cw, ch int) float64 {
	if cw == 0 || ch == 0 {
		return 0
	}
	x0, y0 := col*cw, row*ch
	var sum float64
	var n int
	for dy := range ch {
		for dx := range cw {
			x, y := x0+dx, y0+dy
			p := image.Point{X: x, Y: y}
			if !p.In(reference.Bounds()) || !p.In(render.Bounds()) {
				continue
			}
			ra := reference.RGBAAt(x, y)
			rr := render.RGBAAt(x, y)
			sum += metrics.DeltaE2000(ra.R, ra.G, ra.B, rr.R, rr.G, rr.B)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// heatColour maps a 0..1 error level to the red and green of an RGB heat
// colour: black at 0, through red, to yellow at 1, with blue fixed at 0. Values
// outside 0..1 clamp. The ramp is monotone in brightness, so a larger error
// always reads as a hotter tile.
func heatColour(t float64) (r, g float64) {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	if t < 0.5 {
		// black -> red over the first half.
		return t * 2, 0
	}
	// red -> yellow over the second half.
	return 1, (t - 0.5) * 2
}

// histogramPair plots the reference and render per-channel colour histograms
// overlaid, so palette collapse (mass piling into few bins) and banding (mass
// shifting across bins) show at a glance. The reference channels draw solid,
// the render channels dashed, in red, green, and blue.
func histogramPair(reference, render *image.RGBA) (*image.RGBA, error) {
	p := plot.New()
	p.X.Label.Text = "channel value (0..255)"
	p.Y.Label.Text = "fraction of pixels"
	p.X.Min, p.X.Max = 0, 255

	refR, refG, refB := normChannelHist(reference)
	rndR, rndG, rndB := normChannelHist(render)

	channels := []struct {
		ref, rnd []float64
		c        color.RGBA
	}{
		{refR, rndR, color.RGBA{R: 220, A: 255}},
		{refG, rndG, color.RGBA{G: 180, A: 255}},
		{refB, rndB, color.RGBA{B: 230, A: 255}},
	}
	for _, ch := range channels {
		refLine, err := plotter.NewLine(histXYs(ch.ref))
		if err != nil {
			return nil, fmt.Errorf("harness: histogram reference line: %w", err)
		}
		refLine.Color = ch.c
		rndLine, err := plotter.NewLine(histXYs(ch.rnd))
		if err != nil {
			return nil, fmt.Errorf("harness: histogram render line: %w", err)
		}
		rndLine.Color = ch.c
		rndLine.Dashes = []vg.Length{vg.Points(2), vg.Points(2)}
		p.Add(refLine, rndLine)
	}

	const w, h = 5 * vg.Inch, 3 * vg.Inch
	canvas := vgimg.New(w, h)
	p.Draw(vgdraw.New(canvas))
	return drawImageRGBA(canvas.Image()), nil
}

// histXYs turns a 256-bin normalised histogram into plotter XY points, bin
// index against fraction.
func histXYs(hist []float64) plotter.XYs {
	pts := make(plotter.XYs, len(hist))
	for i, v := range hist {
		pts[i].X = float64(i)
		pts[i].Y = v
	}
	return pts
}

// normChannelHist returns the per-channel (red, green, blue) normalised 256-bin
// histograms of an image, each summing to 1 over a non-empty image. Channel
// order is RGBA; the alpha channel is ignored.
func normChannelHist(img *image.RGBA) (r, g, b []float64) {
	r = make([]float64, 256)
	g = make([]float64, 256)
	b = make([]float64, 256)
	bnd := img.Bounds()
	n := bnd.Dx() * bnd.Dy()
	if n == 0 {
		return r, g, b
	}
	for y := bnd.Min.Y; y < bnd.Max.Y; y++ {
		for x := bnd.Min.X; x < bnd.Max.X; x++ {
			c := img.RGBAAt(x, y)
			r[c.R]++
			g[c.G]++
			b[c.B]++
		}
	}
	inv := 1.0 / float64(n)
	for i := range 256 {
		r[i] *= inv
		g[i] *= inv
		b[i] *= inv
	}
	return r, g, b
}

// writePNG encodes img to path as a timestamp-free PNG through
// capture.EncodePNG, so the file is byte-identical for identical inputs. The
// write is non-atomic: it writes straight to the target, which is acceptable for
// the gitignored testdata artefacts and not a pattern to copy where a partial
// write would corrupt durable state.
func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := capture.EncodePNG(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// contextRGBA returns the gg context's image as an *image.RGBA, copying when
// the context is not already RGBA-backed.
func contextRGBA(dc *gg.Context) *image.RGBA {
	return drawImageRGBA(dc.Image())
}

// drawImageRGBA returns img as an *image.RGBA, returning it directly when it
// already is one and copying its pixels otherwise.
func drawImageRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(x-b.Min.X, y-b.Min.Y, img.At(x, y))
		}
	}
	return out
}
