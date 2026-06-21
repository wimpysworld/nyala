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
// Thresholds are data-driven. The bands come from the task 7.5 calibration
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

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// SweepModes lists the three backends the sweep runs, in report order. The block
// mode owns a cell grid and a quant-error map; sixel and kitty do not.
var SweepModes = []string{"block", "sixel", "kitty"}

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
	// Cols and Rows are the cell grid the sweep renders at. Stage 0 pins 120x40.
	Cols, Rows int
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

// RunSweep runs the corpus across the three backends, scores each render, emits
// the report files and the artefact PNGs, and gates on the baseline and the
// thresholds. It returns the per-(page, mode) reports, any threshold breaches,
// and the written file paths. A non-empty Breaches slice means the run regressed;
// the caller exits non-zero on it. baseline and thresholds gate the run; a nil
// baseline skips the gate and reports no breaches, which seeds a first baseline.
func RunSweep(rast *capture.Rasteriser, cfg SweepConfig, inputs []SweepInput, baseline *Baseline, thresholds Thresholds) (SweepResult, error) {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return SweepResult{}, err
	}

	reports := make([]Report, 0, len(inputs)*len(SweepModes))
	for _, in := range inputs {
		for _, mode := range SweepModes {
			rep, err := runOne(rast, cfg, in, mode)
			if err != nil {
				return SweepResult{}, fmt.Errorf("sweep %s/%s: %w", in.Page, mode, err)
			}
			reports = append(reports, rep)
		}
	}

	breaches := gate(reports, baseline, thresholds)
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
func runOne(rast *capture.Rasteriser, cfg SweepConfig, in SweepInput, mode string) (Report, error) {
	pxW, pxH := cfg.Cols*capture.CellPxW, cfg.Rows*capture.CellPxH
	reference := metrics.Downscale(in.Src, pxW, pxH)

	rep := Report{
		SchemaVersion: SchemaVersion,
		Page:          in.Page,
		Mode:          mode,
		Viewport:      Viewport{Cols: cfg.Cols, Rows: cfg.Rows},
		NyalaCommit:   cfg.NyalaCommit,
	}

	var (
		renderImg *image.RGBA
		grid      *block.Grid
		ideal     *block.Grid
		perf      metrics.FramePerf
		err       error
	)
	switch mode {
	case "block":
		renderImg, grid, ideal, perf, err = captureBlock(rast, cfg, in.Src)
	case "sixel":
		renderImg, perf, err = captureSixel(reference)
	case "kitty":
		renderImg, perf, err = captureKitty(reference)
	default:
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
		composeSrc := metrics.Downscale(in.Src, cfg.Cols*2, cfg.Rows*4)
		ig := metrics.IdealGrid(composeSrc, cfg.Cols, cfg.Rows)
		ideal = &ig
	}
	idealImg := rast.Rasterise(*ideal)

	rep.Quality = scoreQuality(mode, renderImg, reference, grid, rast)
	rep.QualityRelative = scoreQualityRelative(renderImg, idealImg)
	rep.Perf = toPerf(perf)
	rep.CellStats = cellStats(cfg, grid)

	art, err := Emit(filepath.Join(cfg.OutDir, in.Page, mode), ArtefactInput{
		Reference: reference,
		Render:    renderImg,
		Grid:      grid,
		Ideal:     ideal,
		Mode:      mode,
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
func captureBlock(rast *capture.Rasteriser, cfg SweepConfig, src *image.RGBA) (*image.RGBA, *block.Grid, *block.Grid, metrics.FramePerf, error) {
	composeSrc := metrics.Downscale(src, cfg.Cols*2, cfg.Rows*4)
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

// captureSixel encodes the reference image as a Sixel payload, decodes it back,
// and measures the encode cost. The decoded image is the scored render, so the
// score reflects the round trip, not the encoder input.
func captureSixel(reference *image.RGBA) (*image.RGBA, metrics.FramePerf, error) {
	var r sixel.Renderer
	var buf bytes.Buffer
	cw := metrics.NewCountingWriter(&buf)
	perf, err := metrics.CaptureFrame(
		cw, nil,
		func() error { return r.Render(render.Frame{Img: reference}, cw) },
	)
	if err != nil {
		return nil, metrics.FramePerf{}, err
	}
	img, err := capture.DecodeSixel(buf.Bytes())
	if err != nil {
		return nil, metrics.FramePerf{}, err
	}
	return img, perf, nil
}

// captureKitty encodes the reference image as a Kitty payload, decodes it back,
// and measures the encode cost. Kitty is lossless, so the decoded image equals
// the reference up to channel layout.
func captureKitty(reference *image.RGBA) (*image.RGBA, metrics.FramePerf, error) {
	var r kitty.Renderer
	var buf bytes.Buffer
	cw := metrics.NewCountingWriter(&buf)
	perf, err := metrics.CaptureFrame(
		cw, nil,
		func() error { return r.Render(render.Frame{Img: reference}, cw) },
	)
	if err != nil {
		return nil, metrics.FramePerf{}, err
	}
	img, err := capture.DecodeKitty(buf.Bytes())
	if err != nil {
		return nil, metrics.FramePerf{}, err
	}
	return img, perf, nil
}

// scoreQuality scores one render against the reference and fills the report
// Quality block. Sharpness (Laplacian variance on the decoded render) is reported
// for all three modes. WCAG contrast is the owned cell-grid contrast for block;
// for sixel and kitty, which own no grid, it is derived from the decoded pixels
// over the ladder region. OCR scores come from the build-tagged hook: null in the
// default cgo-free build, measured under the "ocr" tag.
func scoreQuality(mode string, renderImg, reference *image.RGBA, grid *block.Grid, rast *capture.Rasteriser) Quality {
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
	if mode == "block" && grid != nil {
		wc := metrics.WCAGGrid(*grid)
		q.ContrastMin = f64v(wc.Min)
	} else {
		q.ContrastMin = f64v(pixelContrast(renderImg))
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

// pixelContrast derives a WCAG contrast ratio from the decoded pixels of a
// gridless render (sixel, kitty). It splits the render's luminance into two
// clusters by an Otsu threshold, takes each cluster's mean colour as a
// representative foreground and background, and returns the WCAG contrast of that
// pair. It is the pixel-derived contrast co-signal the per-mode matrix wants for
// the gridless modes, where no owned cell grid carries per-cell foreground and
// background colours. An empty image, or one whose pixels do not split, returns a
// 1:1 ratio. Channel order is RGBA.
func pixelContrast(img *image.RGBA) float64 {
	b := img.Bounds()
	if b.Empty() {
		return 1
	}
	// Build the 8-bit luminance histogram and find the Otsu split.
	var hist [256]int
	var total int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			hist[lumaByte(c.R, c.G, c.B)]++
			total++
		}
	}
	if total == 0 {
		return 1
	}
	t := otsuSplit(hist[:], total)

	// Accumulate each cluster's mean colour: below the threshold is the darker
	// set, at or above is the lighter set.
	var loR, loG, loB, loN float64
	var hiR, hiG, hiB, hiN float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			if int(lumaByte(c.R, c.G, c.B)) < t {
				loR, loG, loB, loN = loR+float64(c.R), loG+float64(c.G), loB+float64(c.B), loN+1
			} else {
				hiR, hiG, hiB, hiN = hiR+float64(c.R), hiG+float64(c.G), hiB+float64(c.B), hiN+1
			}
		}
	}
	if loN == 0 || hiN == 0 {
		return 1
	}
	return metrics.WCAGContrast(
		meanByte(loR, loN), meanByte(loG, loN), meanByte(loB, loN),
		meanByte(hiR, hiN), meanByte(hiG, hiN), meanByte(hiB, hiN),
	)
}

// lumaByte returns the BT.601 luma of an 8-bit RGB triple, rounded to a byte. It
// feeds the Otsu histogram for the pixel-derived contrast split.
func lumaByte(r, g, b uint8) uint8 {
	y := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	return uint8(y + 0.5)
}

// meanByte returns sum/count rounded and clamped to an 8-bit value.
func meanByte(sum, count float64) uint8 {
	v := sum/count + 0.5
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// otsuSplit returns the Otsu luminance threshold for a 256-bin histogram: the
// level that maximises the between-class variance, splitting the pixels into a
// dark and a light cluster. total is the pixel count. It returns 0 when the
// histogram has no separable mass, which the caller treats as no split.
func otsuSplit(hist []int, total int) int {
	var sum float64
	for i, n := range hist {
		sum += float64(i) * float64(n)
	}
	var sumB, wB float64
	var best float64
	bestT := 0
	for t := 0; t < 256; t++ {
		wB += float64(hist[t])
		if wB == 0 {
			continue
		}
		wF := float64(total) - wB
		if wF == 0 {
			break
		}
		sumB += float64(t) * float64(hist[t])
		mB := sumB / wB
		mF := (sum - sumB) / wF
		between := wB * wF * (mB - mF) * (mB - mF)
		if between > best {
			best = between
			bestT = t
		}
	}
	// The split lands at the level above the best threshold, so the darker set is
	// strictly below it.
	return bestT + 1
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
		if m == mode {
			return i
		}
	}
	return len(SweepModes)
}
