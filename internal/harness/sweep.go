// The sweep, the baseline, and the data-driven thresholds.
//
// RunSweep runs the corpus across the three backends (block, sixel, kitty),
// captures each render back to an image, scores its quality and cost, and emits
// one report.json, one report.csv, and one markdown summary, plus the five
// artefact PNGs per (page, mode). It then loads the committed baseline.json and
// thresholds.json, computes per-metric deltas against the baseline, marks each
// metric pass or fail, and returns a breach when any metric regresses past its
// threshold: a quality metric fails on a drop, a cost metric on a rise.
//
// Capture path per mode. The block mode rasterises its owned cell grid through
// Route A, so its render image is the deterministic 8x16-per-cell raster the
// metrics and the oracle both read. The sixel and kitty modes encode the frame
// image to their wire payload, then decode it back, so the scored image is the
// true round-tripped render, not the encoder's input. Every render is compared
// against the reference downscaled to the render's dimensions.
//
// Thresholds are data-driven. The bands come from the committed calibration
// (testdata/calibration.json): the intrinsic-noise floor on repeated identical
// block renders is exactly zero, so every band sits above that floor while
// staying a small fraction of the observed metric range, so a real regression
// trips it and run-to-run jitter does not. DefaultThresholds records the chosen
// bands and their rationale.
//
// Channel order is RGBA throughout.
package harness

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

// Mode identifies one render backend the sweep runs. It is a string-kinded type
// so backend identity is compiler-checked at every switch and parameter, while
// the wire form (report.Mode, baseline mode, CSV cell) stays the same string.
type Mode string

// The three backends the sweep runs, in report order.
const (
	// ModeBlock is the owned-grid block-cell backend; it owns a cell grid and a
	// quant-error map.
	ModeBlock Mode = "block"
	// ModeSixel is the Sixel graphics backend; it owns no grid.
	ModeSixel Mode = "sixel"
	// ModeKitty is the Kitty graphics backend; it owns no grid.
	ModeKitty Mode = "kitty"
)

// SweepModes lists the three backends the sweep runs, in report order. The block
// mode owns a cell grid and a quant-error map; sixel and kitty do not.
var SweepModes = []Mode{ModeBlock, ModeSixel, ModeKitty}

// Output filenames the sweep writes into the run directory, alongside the
// per-(page, mode) artefact subdirectories.
const (
	reportJSONName = "report.json"
	reportCSVName  = "report.csv"
	summaryMDName  = "summary.md"
)

// SweepInput pairs a page name with its decoded source image. The page name is
// the artefact subdirectory and the report key; the image is the full-resolution
// reference the renders are scored against.
type SweepInput struct {
	// Page is the fixture name, for example "linuxmatters". It must be a clean
	// path segment: it names the artefact subdirectory and the report key.
	Page string
	// Src is the full-resolution reference image. Channel order is RGBA.
	Src *image.RGBA
}

// SweepConfig is the fixed configuration for one sweep run: the pinned grid, the
// output directory, and the commit the renders came from.
type SweepConfig struct {
	// Viewport is the cell grid the sweep renders at. Stage 0 pins 120x40. It
	// carries the cell-to-pixel and compose-extent geometry the sweep needs.
	Viewport
	// OutDir is the directory the report files and the per-(page, mode) artefact
	// subdirectories are written into. It is created if missing.
	OutDir string
	// NyalaCommit is the short commit the renders came from, recorded in every
	// report record. It may be empty.
	NyalaCommit string
}

// SweepResult is the outcome of one sweep run: the per-(page, mode) reports in a
// stable order, the breaches found against the baseline and thresholds, and the
// paths of the three written report files.
type SweepResult struct {
	// Reports holds one record per (page, mode), ordered page then mode.
	Reports []Report
	// Breaches lists every metric that regressed past its threshold. An empty
	// slice means the run passed.
	Breaches []Breach
	// ReportJSON, ReportCSV, and SummaryMD are the absolute paths of the written
	// report files.
	ReportJSON, ReportCSV, SummaryMD string
}

// Breach is one metric that regressed past its threshold for a (page, mode): the
// baseline value, the measured value, the signed delta, and the band the delta
// exceeded. Quality metrics breach on a drop, cost metrics on a rise.
type Breach struct {
	Page     string  `json:"page"`
	Mode     string  `json:"mode"`
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Measured float64 `json:"measured"`
	Delta    float64 `json:"delta"`
	Band     float64 `json:"band"`
}

// String renders a breach as a single human line for the summary and errors.
func (b Breach) String() string {
	return fmt.Sprintf("%s/%s %s: measured %.4f vs baseline %.4f (delta %+.4f, band %.4f)",
		b.Page, b.Mode, b.Metric, b.Measured, b.Baseline, b.Delta, b.Band)
}

// GateConfig carries what RunSweep gates and reports with, folding the three
// loosely related controls into one named struct so each carries its nil-meaning
// at the field rather than at a positional argument.
type GateConfig struct {
	// Baseline is the committed snapshot the run gates against. A nil Baseline
	// skips the gate and reports no breaches, which seeds a first baseline.
	Baseline *Baseline
	// Thresholds are the per-metric regression bands the gate applies.
	Thresholds Thresholds
	// Progress streams a line before each (page, mode) is scored. A nil Progress
	// is a silent no-op.
	Progress Progress
}

// RunSweep runs the corpus across the three backends, scores each render, emits
// the report files and the artefact PNGs, and gates on the baseline and the
// thresholds. It returns the per-(page, mode) reports, any threshold breaches,
// and the written file paths. A non-empty Breaches slice means the run regressed;
// the caller exits non-zero on it. The gate config carries the baseline, the
// thresholds, and the progress callback; each documents its own nil meaning.
func RunSweep(rast *cellraster.Rasteriser, cfg SweepConfig, inputs []SweepInput, gc GateConfig) (SweepResult, error) {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return SweepResult{}, err
	}

	reports := make([]Report, 0, len(inputs)*len(SweepModes))
	for _, in := range inputs {
		for _, mode := range SweepModes {
			gc.Progress.report("sweep: page %s mode %s", in.Page, mode)
			rep, err := runOne(rast, cfg, in, mode)
			if err != nil {
				return SweepResult{}, fmt.Errorf("sweep %s/%s: %w", in.Page, mode, err)
			}
			reports = append(reports, rep)
		}
	}

	breaches := gate(reports, gc.Baseline, gc.Thresholds)
	for i := range reports {
		reports[i].Pass = !pageModeBreached(breaches, reports[i].Page, reports[i].Mode)
	}

	res := SweepResult{
		Reports:    reports,
		Breaches:   breaches,
		ReportJSON: filepath.Join(cfg.OutDir, reportJSONName),
		ReportCSV:  filepath.Join(cfg.OutDir, reportCSVName),
		SummaryMD:  filepath.Join(cfg.OutDir, summaryMDName),
	}
	if err := writeReports(res); err != nil {
		return SweepResult{}, err
	}
	return res, nil
}

// runOne renders one (page, mode), captures it back to an image, scores quality
// and cost, emits the artefacts, and builds the report record. Pass is left
// false; the gate sets it.
func runOne(rast *cellraster.Rasteriser, cfg SweepConfig, in SweepInput, mode Mode) (Report, error) {
	pxW, pxH := cfg.PixelSize()
	reference := metrics.Downscale(in.Src, pxW, pxH)

	rep := Report{
		SchemaVersion: SchemaVersion,
		Page:          in.Page,
		Mode:          string(mode),
		Viewport:      cfg.Viewport,
		NyalaCommit:   cfg.NyalaCommit,
	}

	var (
		renderImg *image.RGBA
		grid      *block.Grid
		ideal     *block.Grid
		perf      metrics.FramePerf
		err       error
	)
	if mode == ModeBlock {
		renderImg, grid, ideal, perf, err = captureBlock(rast, cfg, in.Src)
	} else if g, ok := graphicsBackendFor(mode); ok {
		renderImg, perf, err = g.capture(reference)
	} else {
		return Report{}, fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		return Report{}, err
	}

	// The ideal block render is the relative-score reference: the tightest block
	// render a quadrant backend could reach. For block, captureBlock already built
	// the ideal grid; for sixel and kitty, build it from the same downscaled
	// source so every mode scores against the one ceiling. Channel order is RGBA.
	if ideal == nil {
		cw, ch := cfg.ComposeSize()
		composeSrc := metrics.Downscale(in.Src, cw, ch)
		ig := metrics.IdealGrid(composeSrc, cfg.Cols, cfg.Rows)
		ideal = &ig
	}
	idealImg := rast.Rasterise(*ideal)

	rep.Quality = scoreQuality(mode, renderImg, reference, grid, rast)
	rep.QualityRelative = scoreQualityRelative(renderImg, idealImg)
	rep.Perf = toPerf(perf)
	rep.CellStats = cellStats(cfg, grid)

	art, err := Emit(filepath.Join(cfg.OutDir, in.Page, string(mode)), ArtefactInput{
		Reference: reference,
		Render:    renderImg,
		Grid:      grid,
		Ideal:     ideal,
		Mode:      string(mode),
	})
	if err != nil {
		return Report{}, err
	}
	rep.Artefacts = toArtefacts(art)
	return rep, nil
}

// captureBlock composes the source onto the pinned grid, rasterises it through
// Route A, and measures the block render's compose and encode cost. It returns
// the render image, the owned grid, the ideal grid for the quant-error map, and
// the captured cost.
func captureBlock(rast *cellraster.Rasteriser, cfg SweepConfig, src *image.RGBA) (*image.RGBA, *block.Grid, *block.Grid, metrics.FramePerf, error) {
	cw0, ch0 := cfg.ComposeSize()
	composeSrc := metrics.Downscale(src, cw0, ch0)
	br := block.Renderer{Cols: cfg.Cols, Rows: cfg.Rows}

	var grid block.Grid
	cw := metrics.NewCountingWriter(io.Discard)
	perf, err := metrics.CaptureFrame(
		cw,
		func() error { grid = br.Compose(render.Frame{Img: composeSrc}); return nil },
		func() error { return br.Render(render.Frame{Img: composeSrc}, cw) },
	)
	if err != nil {
		return nil, nil, nil, metrics.FramePerf{}, err
	}
	renderImg := rast.Rasterise(grid)
	ideal := metrics.IdealGrid(composeSrc, cfg.Cols, cfg.Rows)
	return renderImg, &grid, &ideal, perf, nil
}

// graphicsBackend is one gridless backend's round-trip: encode a frame to a wire
// payload, then decode it back. The sixel and kitty arms differ only in these two
// functions, so one descriptor and the shared capture and roundTrip methods below
// fold the four near-identical per-backend functions into one pipeline each.
// Channel order is RGBA.
type graphicsBackend struct {
	// mode is the backend's report mode.
	mode Mode
	// render encodes a frame to w. A fresh renderer is built per call, matching the
	// per-call renderer the sweep used before the fold.
	render func(img *image.RGBA, w io.Writer) error
	// decode reads the wire payload back to an image.
	decode func(payload []byte) (*image.RGBA, error)
}

// graphicsBackends lists the gridless backends in report order. The block mode
// owns a grid, so it is not here; runOne handles it separately.
var graphicsBackends = []graphicsBackend{
	{
		mode: ModeSixel,
		render: func(img *image.RGBA, w io.Writer) error {
			var r sixel.Renderer
			return r.Render(render.Frame{Img: img}, w)
		},
		decode: renderdecode.DecodeSixel,
	},
	{
		mode: ModeKitty,
		render: func(img *image.RGBA, w io.Writer) error {
			var r kitty.Renderer
			return r.Render(render.Frame{Img: img}, w)
		},
		decode: renderdecode.DecodeKitty,
	},
}

// graphicsBackendFor returns the gridless backend descriptor for a mode, and
// whether one exists. The block mode owns a grid, so it has no descriptor.
func graphicsBackendFor(mode Mode) (graphicsBackend, bool) {
	for _, b := range graphicsBackends {
		if b.mode == mode {
			return b, true
		}
	}
	return graphicsBackend{}, false
}

// capture encodes reference through the backend, decodes it back, and measures
// the encode cost. The decoded image is the scored render, so the score reflects
// the round trip, not the encoder input. Channel order is RGBA.
func (g graphicsBackend) capture(reference *image.RGBA) (*image.RGBA, metrics.FramePerf, error) {
	var buf bytes.Buffer
	cw := metrics.NewCountingWriter(&buf)
	perf, err := metrics.CaptureFrame(cw, nil, func() error { return g.render(reference, cw) })
	if err != nil {
		return nil, metrics.FramePerf{}, err
	}
	img, err := g.decode(buf.Bytes())
	if err != nil {
		return nil, metrics.FramePerf{}, err
	}
	return img, perf, nil
}

// roundTrip encodes img through the backend and decodes it back, returning the
// round-tripped render. It is the no-cost path the callers that do not measure
// byte cost share. Channel order is RGBA.
func (g graphicsBackend) roundTrip(img *image.RGBA) (*image.RGBA, error) {
	var buf bytes.Buffer
	if err := g.render(img, &buf); err != nil {
		return nil, err
	}
	return g.decode(buf.Bytes())
}

// scoreQuality scores one render against the reference and fills the report
// Quality block. Sharpness (Laplacian variance on the decoded render) is reported
// for all three modes. WCAG contrast is the owned cell-grid contrast for block;
// for sixel and kitty, which own no grid, it is derived from the decoded pixels
// over the ladder region. OCR scores come from the build-tagged hook: null in the
// default cgo-free build, measured under the "ocr" tag.
func scoreQuality(mode Mode, renderImg, reference *image.RGBA, grid *block.Grid, rast *cellraster.Rasteriser) Quality {
	abs := metrics.ScorePair(renderImg, reference)
	q := Quality{
		MSSSIM:         f64v(abs.MSSSIM),
		SSIM:           f64v(abs.SSIM),
		PSNR:           f64v(abs.PSNR),
		DeltaE2000Mean: f64v(abs.DeltaEMean),
		DeltaE2000P95:  f64v(abs.DeltaEP95),
		HistDivergence: f64v(1 - abs.HistIntersection),
		Sharpness:      f64v(metrics.LaplacianVariance(renderImg)),
	}
	if mode == ModeBlock && grid != nil {
		wc := metrics.WCAGGrid(*grid)
		q.ContrastMin = f64v(wc.Min)
	} else {
		q.ContrastMin = f64v(metrics.PixelContrast(renderImg))
	}
	if cer, wer, minLeg := ocrScores(rast, mode, renderImg); cer != nil || wer != nil || minLeg != nil {
		q.OCRCER = cer
		q.OCRWER = wer
		q.MinLegiblePx = minLeg
	}
	return q
}

// scoreQualityRelative scores one render against the ideal block render and fills
// the report QualityRelative block. The ideal render is the tightest block render
// a quadrant backend could reach, so the relative scores isolate the loss the
// real backend adds over that ceiling. For kitty the ideal render tracks the
// input, so the relative scores collapse toward the absolute scores; the computed
// values are emitted, never null. Channel order is RGBA.
func scoreQualityRelative(renderImg, idealImg *image.RGBA) QualityRelative {
	rel := metrics.ScorePair(renderImg, idealImg)
	return QualityRelative{
		MSSSIM:         f64v(rel.MSSSIM),
		SSIM:           f64v(rel.SSIM),
		PSNR:           f64v(rel.PSNR),
		MSE:            f64v(rel.MSE),
		GMSD:           f64v(rel.GMSD),
		DeltaE2000Mean: f64v(rel.DeltaEMean),
		DeltaE2000P95:  f64v(rel.DeltaEP95),
		HistDivergence: f64v(1 - rel.HistIntersection),
	}
}

// cellStats counts the owned grid cells for the block mode; non-block modes own
// no grid, so every count is zero. Stage 0 has no text/bitmap classifier wired
// into the sweep, so the class splits stay zero and only Total carries.
func cellStats(cfg SweepConfig, grid *block.Grid) CellStats {
	if grid == nil {
		return CellStats{}
	}
	return CellStats{Total: cfg.Cols * cfg.Rows}
}

// toPerf projects a metrics.FramePerf onto the report Perf block. Times are
// reported in milliseconds; the byte and alloc counts are present, so they are
// non-null pointers.
func toPerf(p metrics.FramePerf) Perf {
	bytesPerFrame := int(p.Bytes)
	allocs := clampUint64ToInt(p.AllocBytes)
	rss := clampUint64ToInt(p.PeakRSSBytes)
	return Perf{
		RenderMs:       f64v(float64(p.Compose.Nanoseconds()) / 1e6),
		EncodeMs:       f64v(float64(p.Encode.Nanoseconds()) / 1e6),
		BytesPerFrame:  &bytesPerFrame,
		FPS:            f64v(p.FPS),
		AllocsPerFrame: &allocs,
		PeakRSSBytes:   &rss,
	}
}

// toArtefacts projects an ArtefactSet onto the report Artefacts block. An empty
// QuantMap (the kitty and sixel modes) serialises as null.
func toArtefacts(a ArtefactSet) Artefacts {
	out := Artefacts{
		Composite: a.Composite,
		Heatmap:   a.Heatmap,
		CellMap:   a.CellMap,
		Histogram: a.Histogram,
	}
	if a.QuantMap != "" {
		q := a.QuantMap
		out.QuantMap = &q
	}
	return out
}

// f64v returns a pointer to v, for the nullable report fields.
func f64v(v float64) *float64 { return &v }

// clampUint64ToInt converts a uint64 byte count to int, saturating at the
// platform int max so a value beyond int range cannot wrap to a negative count.
func clampUint64ToInt(v uint64) int {
	if v > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(v)
}

// pageModeBreached reports whether any breach names the given (page, mode).
func pageModeBreached(breaches []Breach, page, mode string) bool {
	for _, b := range breaches {
		if b.Page == page && b.Mode == mode {
			return true
		}
	}
	return false
}

// writeReports writes report.json, report.csv, and the markdown summary for a
// completed sweep.
func writeReports(res SweepResult) error {
	if err := writeJSON(res.ReportJSON, res.Reports); err != nil {
		return err
	}
	if err := writeCSV(res.ReportCSV, res.Reports); err != nil {
		return err
	}
	return writeSummary(res.SummaryMD, res)
}

// writeJSON marshals the reports to deterministic, indented JSON with a trailing
// newline so the file diffs cleanly.
func writeJSON(path string, reports []Report) error {
	b, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// csvHeader is the report.csv column order: the keys, then the headline quality
// and cost metrics one column each, so a run reads as a flat table.
var csvHeader = []string{
	"page", "mode", "cols", "rows",
	"ssim", "msssim", "psnr", "deltaE2000Mean", "deltaE2000P95", "histDivergence",
	"contrastMin", "sharpness",
	"renderMs", "encodeMs", "bytesPerFrame", "fps", "pass",
}

// writeCSV writes the reports as a flat CSV with one row per (page, mode). A
// null metric writes an empty cell, never a zero, mirroring the JSON nulls.
func writeCSV(path string, reports []Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range reports {
		row := []string{
			r.Page, r.Mode, strconv.Itoa(r.Viewport.Cols), strconv.Itoa(r.Viewport.Rows),
			fcell(r.Quality.SSIM), fcell(r.Quality.MSSSIM), fcell(r.Quality.PSNR),
			fcell(r.Quality.DeltaE2000Mean), fcell(r.Quality.DeltaE2000P95), fcell(r.Quality.HistDivergence),
			fcell(r.Quality.ContrastMin), fcell(r.Quality.Sharpness),
			fcell(r.Perf.RenderMs), fcell(r.Perf.EncodeMs), icell(r.Perf.BytesPerFrame), fcell(r.Perf.FPS),
			strconv.FormatBool(r.Pass),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// writeSummary writes a markdown summary: a result line, a breach list, and one
// table row per (page, mode) with the headline metrics.
func writeSummary(path string, res SweepResult) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Nyala Stage 0 sweep\n\n")
	if len(res.Breaches) == 0 {
		fmt.Fprintf(&b, "Result: PASS. %d records, no threshold breaches.\n\n", len(res.Reports))
	} else {
		fmt.Fprintf(&b, "Result: FAIL. %d threshold breaches.\n\n", len(res.Breaches))
		for _, br := range res.Breaches {
			fmt.Fprintf(&b, "- %s\n", br.String())
		}
		fmt.Fprintf(&b, "\n")
	}
	fmt.Fprintf(&b, "| page | mode | ssim | msssim | psnr | dEmean | dEp95 | bytes | pass |\n")
	fmt.Fprintf(&b, "|------|------|------|--------|------|--------|-------|-------|------|\n")
	for _, r := range res.Reports {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %v |\n",
			r.Page, r.Mode,
			fcell(r.Quality.SSIM), fcell(r.Quality.MSSSIM), fcell(r.Quality.PSNR),
			fcell(r.Quality.DeltaE2000Mean), fcell(r.Quality.DeltaE2000P95),
			icell(r.Perf.BytesPerFrame), r.Pass)
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// fcell formats a nullable float for a CSV or markdown cell: empty for null, else
// %g for a compact, trailing-zero-free value.
func fcell(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

// icell formats a nullable int for a CSV or markdown cell: empty for null.
func icell(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

// sortReports orders reports by page then mode so a run is byte-stable.
func sortReports(reports []Report) {
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].Page != reports[j].Page {
			return reports[i].Page < reports[j].Page
		}
		return modeOrder(reports[i].Mode) < modeOrder(reports[j].Mode)
	})
}

// modeOrder returns the SweepModes index of a mode, for stable report ordering.
func modeOrder(mode string) int {
	for i, m := range SweepModes {
		if string(m) == mode {
			return i
		}
	}
	return len(SweepModes)
}
