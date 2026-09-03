//go:build diagnostics

// The scene-fitted greyscale A/B diagnostics.
//
// The question this file answers is the greyscale sibling of the one
// adaptive_palette_diagnostics_test.go answers: does ColourXTermGrey with an
// answered OSC 4 palette query (the scene-fitted luma ramp in registers
// 16..255) draw a frame closer to the truecolour reference than the same mode
// with an unanswered query (the fixed xterm ramp, registers 232..255), on real
// page content and on gradients? The verdict is set through
// State.SetPaletteQueryOK. Both techniques compose the same grid, so the
// composed grid is the shared truecolour reference and the two techniques
// differ only in the register each cell colour lands on.
//
// The terminal-visible grid is modelled with production functions only:
// block.XTermGrey and block.XTermGreyRGBA for the fixed ramp, and
// block.PaletteIndex over palette.BuildLumaLUT rebuilt from the ramp parsed
// back out of the emitted OSC 4 bytes for the fitted ramp. The fixed leg checks
// that model against blockinterop.Decode, where canonical registers apply, so
// the modelling step is proven on the mode that can be decoded.
//
// Channel order is RGBA throughout.
package harness

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/palette"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// greyFixtures lists the greyscale A/B inputs. The greyscale page is the case
// the fixed ramp already sits on, the podcast page is real mixed content, the
// narrow blue band is a low-luma scene with chroma the luma path discards, and
// the dark gradient is the synthetic case the fixed ramp handles worst: its
// luma 2..62 spans about six of the fixed ramp's 24 steps.
var greyFixtures = []adaptiveFixture{
	{Name: "testpage-01-grayscale", PNG: "testpage-01-grayscale.png"},
	{Name: "linuxmatters", PNG: "linuxmatters.png"},
	{Name: "gradient-narrow-blue", Synth: narrowBlueGradient},
	{Name: "gradient-dark", Synth: greyDarkGradient},
}

// greyDeltaEFixtures names the fixtures the fitted ramp must beat the fixed
// ramp on mean CIEDE2000 as well as SSIM. The luma path discards chroma, so a
// chromatic scene measures a dE dominated by the missing hue in both modes and
// the rule would not separate them; the two neutral scenes are where the ramp
// fit is the whole difference.
var greyDeltaEFixtures = map[string]bool{
	"testpage-01-grayscale": true,
	"gradient-dark":         true,
}

// TestAdaptiveGreyDiagnostics scores ColourXTermGrey with the palette query
// answered (scene-fitted luma ramp) against ColourXTermGrey with the query
// unanswered (fixed xterm ramp) on two page rasters and two synthetic
// gradients, natively and at viewing distance, and writes the whole measurement
// set to gitignored testdata/diagnostics/adaptive-grey/scores.json.
//
// The floors below are per-mode absolute floors plus the relative rules: the
// fitted ramp must score at least the fixed ramp's SSIM on every fixture,
// natively and at viewing distance, and at most the fixed ramp's mean dE on the
// neutral fixtures. The relative rules are the point of the phase; the absolute
// floors stop either mode degrading silently.
func TestAdaptiveGreyDiagnostics(t *testing.T) {
	rasteriser := newRasteriser(t)
	records := make(map[string][]adaptiveScore, len(greyFixtures))

	for _, fx := range greyFixtures {
		t.Run(fx.Name, func(t *testing.T) {
			frame := adaptiveFrame(t, fx)
			reference := composeReference(t, frame)
			raster := rasteriser.Rasterise(reference)

			fixed := greyScoreMode(t, rasteriser, frame, reference, raster, false)
			fitted := greyScoreMode(t, rasteriser, frame, reference, raster, true)
			records[fx.Name] = []adaptiveScore{fixed, fitted}

			for _, s := range []adaptiveScore{fixed, fitted} {
				t.Logf("mode=%s ssim_native=%.9f ssim_distant=%.9f de_mean=%.9f de_p95=%.9f payload_bytes=%d registers=%d",
					s.Mode, s.SSIMNative, s.SSIMDistant, s.DeltaEMean, s.DeltaEP95, s.PayloadBytes, s.Registers)
			}

			// The ramp holds one entry per occupied luma level up to the budget,
			// so a sparse scene writes fewer than 240 registers and the check is
			// a range, not an equality.
			if fitted.Registers <= 0 || fitted.Registers > palette.BudgetColours {
				t.Fatalf("fitted ramp wrote %d registers, want 1..%d", fitted.Registers, palette.BudgetColours)
			}
			if fixed.Registers != 0 {
				t.Fatalf("fixed ramp wrote %d palette registers, want none", fixed.Registers)
			}
			assertGreyWins(t, fx.Name, fixed, fitted)
			assertGreyFloors(t, fx.Name, fixed, fitted)
		})
	}

	writeGreyEvidence(t, records)
}

// greyFloors holds the per-fixture absolute floors and ceilings for the two
// greyscale modes. Every number beside a floor is the value measured on amd64
// at the pinned 120x40 grid; the floor is the measurement rounded down with
// headroom, so a later reader can re-derive the margin without rerunning.
type greyFloors struct {
	// FixedSSIM is the floor on the fixed-ramp native SSIM.
	FixedSSIM float64
	// FittedSSIM is the floor on the fitted-ramp native SSIM.
	FittedSSIM float64
	// FittedSSIMDistant is the floor on the fitted-ramp viewing-distance SSIM.
	FittedSSIMDistant float64
	// FittedDeltaE is the ceiling on the fitted-ramp mean CIEDE2000 dE.
	FittedDeltaE float64
}

// greyFloorTable records one floor set per fixture. Every row's comment
// carries the amd64 measurement the floors come from, in this order: the
// fitted native SSIM, the fitted viewing-distance SSIM, the fitted mean dE,
// then the fixed native SSIM and the fixed mean dE. The SSIM floors sit about
// 0.01 below the measurement and the dE ceilings about 25% above it, so the
// gate rejects a real regression and tolerates metric drift.
var greyFloorTable = map[string]greyFloors{
	// fitted 0.994671991 / 0.995332034 / 0.562, fixed 0.972685247 / 2.393.
	"testpage-01-grayscale": {FixedSSIM: 0.96, FittedSSIM: 0.98, FittedSSIMDistant: 0.98, FittedDeltaE: 0.75},
	// fitted 0.994250809 / 0.996825422 / 2.628, fixed 0.984175238 / 2.945.
	"linuxmatters": {FixedSSIM: 0.97, FittedSSIM: 0.98, FittedSSIMDistant: 0.98, FittedDeltaE: 3.30},
	// fitted 0.995870087 / 0.987586726 / 22.509, fixed 0.990556990 / 22.540. The
	// dE is the discarded blue chroma, equal in both modes; SSIM is the rule here.
	"gradient-narrow-blue": {FixedSSIM: 0.98, FittedSSIM: 0.98, FittedSSIMDistant: 0.97, FittedDeltaE: 28.20},
	// fitted 0.981689249 / 0.972287925 / 0.518, fixed 0.966301880 / 0.669.
	"gradient-dark": {FixedSSIM: 0.95, FittedSSIM: 0.97, FittedSSIMDistant: 0.96, FittedDeltaE: 0.65},
}

// assertGreyWins applies the relative rules: on every fixture the fitted ramp
// must score at least the fixed ramp's SSIM, natively and at viewing distance,
// and on the neutral fixtures in greyDeltaEFixtures it must also score at most
// the fixed ramp's mean dE.
func assertGreyWins(t *testing.T, name string, fixed, fitted adaptiveScore) {
	t.Helper()
	if fitted.SSIMNative < fixed.SSIMNative-metricEpsilon {
		t.Fatalf("fitted native SSIM = %.9f, below the fixed ramp at %.9f", fitted.SSIMNative, fixed.SSIMNative)
	}
	if fitted.SSIMDistant < fixed.SSIMDistant-metricEpsilon {
		t.Fatalf("fitted viewing-distance SSIM = %.9f, below the fixed ramp at %.9f", fitted.SSIMDistant, fixed.SSIMDistant)
	}
	if greyDeltaEFixtures[name] && fitted.DeltaEMean > fixed.DeltaEMean+metricEpsilon {
		t.Fatalf("fitted mean dE = %.9f, above the fixed ramp at %.9f", fitted.DeltaEMean, fixed.DeltaEMean)
	}
}

// assertGreyFloors applies the fixture's absolute floors to both modes and
// fails naming the mode, the metric, the measurement, and the floor.
func assertGreyFloors(t *testing.T, name string, fixed, fitted adaptiveScore) {
	t.Helper()
	floors, ok := greyFloorTable[name]
	if !ok {
		t.Fatalf("fixture %q has no floor entry", name)
	}
	assertFinite(t, "fixed ssim_native", fixed.SSIMNative)
	assertFinite(t, "fitted ssim_native", fitted.SSIMNative)
	assertFinite(t, "fitted ssim_distant", fitted.SSIMDistant)
	assertFinite(t, "fitted de_mean", fitted.DeltaEMean)

	if fixed.SSIMNative < floors.FixedSSIM-metricEpsilon {
		t.Fatalf("fixed ramp native SSIM = %.9f, floor %.6f", fixed.SSIMNative, floors.FixedSSIM)
	}
	if fitted.SSIMNative < floors.FittedSSIM-metricEpsilon {
		t.Fatalf("fitted ramp native SSIM = %.9f, floor %.6f", fitted.SSIMNative, floors.FittedSSIM)
	}
	if fitted.SSIMDistant < floors.FittedSSIMDistant-metricEpsilon {
		t.Fatalf("fitted ramp viewing-distance SSIM = %.9f, floor %.6f", fitted.SSIMDistant, floors.FittedSSIMDistant)
	}
	if fitted.DeltaEMean > floors.FittedDeltaE+metricEpsilon {
		t.Fatalf("fitted ramp mean dE = %.9f, ceiling %.6f", fitted.DeltaEMean, floors.FittedDeltaE)
	}
}

// greyScoreMode renders ColourXTermGrey with the given palette-query verdict
// through the production stream writer, models the terminal-visible grid, and
// scores it against the truecolour reference raster natively and at viewing
// distance. It is scoreMode with the grey mode selected.
func greyScoreMode(t *testing.T, rast *cellraster.Rasteriser, frame render.Frame, reference block.Grid, referenceRaster *image.RGBA, paletteOK bool) adaptiveScore {
	t.Helper()
	st := viewer.NewState(CalibCols, CalibRows+1)
	st.Backend = viewer.BackendBlock
	st.Colour = viewer.ColourXTermGrey
	st.SetPaletteQueryOK(paletteOK)
	st.SetDiff(&block.GridDiff{})

	var payload bytes.Buffer
	if err := st.RenderStreamFrame(frame, &payload); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}

	entries, base := parseOSC4(t, payload.Bytes())
	visible := greyTerminalGrid(t, reference, paletteOK, entries, base)
	if !paletteOK {
		assertDecodedMatchesModel(t, payload.Bytes(), visible)
	}

	raster := rast.Rasterise(visible)
	distantRef := metrics.Downscale(referenceRaster, referenceRaster.Bounds().Dx()/viewingDistanceFactor, referenceRaster.Bounds().Dy()/viewingDistanceFactor)
	distantGot := metrics.Downscale(raster, raster.Bounds().Dx()/viewingDistanceFactor, raster.Bounds().Dy()/viewingDistanceFactor)
	de := metrics.CIEDE2000(raster, referenceRaster)

	return adaptiveScore{
		Mode:         greyLabel(paletteOK),
		SSIMNative:   metrics.SSIM(raster, referenceRaster),
		SSIMDistant:  metrics.SSIM(distantGot, distantRef),
		DeltaEMean:   de.Mean,
		DeltaEP95:    de.P95,
		PayloadBytes: payload.Len(),
		Registers:    len(entries),
	}
}

// greyLabel returns the technique label for a palette-query verdict, so the
// evidence record and the log lines name which of the two grey paths ran.
func greyLabel(paletteOK bool) string {
	if paletteOK {
		return "grey-palette"
	}
	return "grey"
}

// greyTerminalGrid returns the reference grid with every cell colour replaced
// by the colour the terminal shows for the given palette-query verdict in the
// grey mode. An unanswered query maps through block.XTermGrey onto the fixed
// xterm ramp; an answered query maps through block.PaletteIndex onto a luma
// LUT rebuilt from the ramp the frame wrote, so the mapping is the production
// one in both cases. Every entry of a luma ramp is a pure grey, so the ramp's
// levels are the entries' red channel; a non-grey entry fails the test.
func greyTerminalGrid(t *testing.T, reference block.Grid, paletteOK bool, entries color.Palette, base int) block.Grid {
	t.Helper()
	var lut []uint8
	if paletteOK {
		if len(entries) == 0 {
			t.Fatal("fitted grey frame wrote no OSC 4 palette")
		}
		if base < palette.BudgetBase || base+len(entries)-1 > 255 {
			t.Fatalf("fitted ramp covers registers %d..%d, want a run inside %d..255", base, base+len(entries)-1, palette.BudgetBase)
		}
		levels := make([]uint8, 0, len(entries))
		for i, entry := range entries {
			r, g, b, _ := entry.RGBA()
			if r != g || g != b {
				t.Fatalf("OSC 4 entry %d = %v, want a pure grey", i, entry)
			}
			levels = append(levels, uint8(r>>8)) //nolint:gosec // A 16-bit channel shifted to 8 bits.
		}
		lut = palette.BuildLumaLUT(nil, levels, base)
	}
	resolve := func(c color.RGBA) color.RGBA {
		if lut == nil {
			return block.XTermGreyRGBA(block.XTermGrey(c))
		}
		reg := int(block.PaletteIndex(lut, c))
		if reg < base || reg >= base+len(entries) {
			t.Fatalf("register %d is outside the written range %d..%d", reg, base, base+len(entries)-1)
		}
		r, g, b, _ := entries[reg-base].RGBA()
		return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0xff}
	}
	out := block.Grid{Cols: reference.Cols, Rows: reference.Rows, Cells: make([]block.Cell, len(reference.Cells))}
	copy(out.Cells, reference.Cells)
	for i := range out.Cells {
		out.Cells[i].FG = resolve(out.Cells[i].FG)
		out.Cells[i].BG = resolve(out.Cells[i].BG)
	}
	return out
}

// greyDarkGradient builds a neutral synthetic fixture confined to the dark end
// of the range: luma rises 2..62 left to right, and the rise steepens top to
// bottom, so the scene holds every level in that band at unequal populations.
// The fixed ramp steps by 10, so it spends about six of its 24 entries on the
// whole scene, while the fitted ramp can spend one register per level.
func greyDarkGradient(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		slope := 0.5 + 0.5*float64(y)/float64(max(height-1, 1))
		for x := range width {
			v := 2 + 60*float64(x)/float64(max(width-1, 1))*slope
			g := uint8(math.Round(v)) //nolint:gosec // v lies in 2..62.
			img.SetRGBA(x, y, color.RGBA{R: g, G: g, B: g, A: 0xff})
		}
	}
	return img
}

// writeGreyEvidence writes the whole measurement set as JSON under gitignored
// testdata/diagnostics/adaptive-grey/, so an operator can read every number
// the run produced without rerunning it.
func writeGreyEvidence(t *testing.T, records map[string][]adaptiveScore) {
	t.Helper()
	dir := filepath.Join(diagnosticsOutDir(), "adaptive-grey")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body, err := json.MarshalIndent(struct {
		Grid                  Viewport
		ViewingDistanceFactor int
		Fixtures              map[string][]adaptiveScore
	}{
		Grid:                  Viewport{Cols: CalibCols, Rows: CalibRows},
		ViewingDistanceFactor: viewingDistanceFactor,
		Fixtures:              records,
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal grey scores: %v", err)
	}
	path := filepath.Join(dir, "scores.json")
	if err := os.WriteFile(path, append(body, '\n'), fileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("adaptive grey evidence: wrote %s", path)
}
