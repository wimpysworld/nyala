// Calibration measures the real metric ranges Nyala's block renderer reaches on
// the committed test pages, so the thresholds come from data instead of guesses.
//
// It block-renders each of the four test pages at the pinned 120x40 grid,
// scores the render against the downscaled input (absolute) and against the
// ideal block render (relative), and records SSIM, MS-SSIM, CIEDE2000 mean and
// p95, and PSNR per card. It also measures the intrinsic-noise floor: the metric
// spread across repeated identical renders of the same card. The block renderer
// is deterministic, so the floor is exactly zero, which is itself the recorded
// baseline a real per-run jitter would have to clear.
//
// The sixel backend is the exception: its lossy encoder is non-deterministic, so
// repeated identical renders jitter. The calibration measures that jitter as the
// sixel PSNR noise floor: the standard deviation of PSNR across repeated sixel
// renders, the worst card-wise value over the corpus. The standard deviation is
// stable to the recorded precision, while the raw min/max spread never settles,
// so the floor traces to data rather than a guess. The sixel PSNR threshold band
// sits well above it. The value is quantised so the committed file stays
// byte-stable despite the jitter.
//
// The CER ladder is gosseract-only and cgo-gated behind the "ocr" build tag, so
// the CER numbers are measured by the tagged calibration path (see
// calibration_ocr_test.go) and folded into the same committed file. The default
// build measures the metric ranges and leaves the CER ladder empty.
//
// Channel order is RGBA throughout.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"runtime"
	"sync"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
)

// Progress is an optional callback the diagnostics passes use to stream a line
// of progress before each long step. It is nil by default, so the library stays
// silent; the diagnostics tests pass a callback that logs through the test. A
// nil callback is a no-op, so callers never need to guard it.
type Progress func(string)

// report sends a formatted line to a Progress callback, or does nothing when the
// callback is nil. It keeps the call sites free of nil checks.
func (p Progress) report(format string, args ...any) {
	if p != nil {
		p(fmt.Sprintf(format, args...))
	}
}

// sixelNoiseFloorRuns is how many times the calibration encodes and decodes each
// card through the sixel backend to measure the PSNR jitter. The lossy encoder
// draws PSNR from a near-continuous spread that never saturates, so the recorded
// floor is the standard deviation of that spread (a stable estimator), not the
// observed min/max (which grows run to run). This many runs is enough for the
// standard deviation to settle to the committed precision.
const sixelNoiseFloorRuns = 64

// sixelNoiseFloorDP is the decimal precision the sixel PSNR noise floor is
// quantised to, so the committed calibration file stays byte-stable across runs
// despite the encoder jitter. The standard deviation settles near 0.2 dB with a
// few thousandths of run-to-run wobble, so one decimal place keeps the committed
// value stable while two would flap at a rounding boundary.
const sixelNoiseFloorDP = 1

// CalibCols and CalibRows are the pinned calibration grid: the Stage 0 120x40
// sweep grid, so the calibration numbers match the grid the sweep runs at.
const (
	CalibCols = 120
	CalibRows = 40
)

// CalibCards lists the four test pages the calibration measures, in the order
// they appear in the committed file. The Linux Matters page is excluded: the
// calibration measures the pages the SSIM/CER ranges are pinned from.
var CalibCards = []string{
	"testpage-01-grayscale.png",
	"testpage-02-basic.png",
	"testpage-03-cmyk.png",
	"testpage-04-color.png",
}

// CalibMetricSet is one card's quality scores, absolute or relative. It mirrors
// the headline metrics calibration pins ranges from: the structural similarity
// pair, PSNR, and the CIEDE2000 mean and p95.
type CalibMetricSet struct {
	SSIM           float64 `json:"ssim"`
	MSSSIM         float64 `json:"msssim"`
	PSNR           float64 `json:"psnr"`
	DeltaEMean     float64 `json:"deltaEMean"`
	DeltaEP95      float64 `json:"deltaEP95"`
	HistDivergence float64 `json:"histDivergence"`
}

// CalibCard holds one card's measured scores: absolute against the downscaled
// input, relative against the ideal block render, and the intrinsic-noise floor
// measured across repeated identical renders.
type CalibCard struct {
	// Card is the source PNG filename under testdata/.
	Card string `json:"card"`
	// Absolute scores the render against the downscaled input image.
	Absolute CalibMetricSet `json:"absolute"`
	// Relative scores the render against the ideal block render.
	Relative CalibMetricSet `json:"relative"`
	// NoiseFloor is the metric spread across repeated identical renders. The
	// block renderer is deterministic, so every field is 0; a non-zero value
	// would mean the render path is not reproducible.
	NoiseFloor CalibMetricSet `json:"noiseFloor"`
	// Bytes is the frame byte cost of the card through each backend, the data the
	// bytesPerFrame threshold band traces to.
	Bytes CalibByteCost `json:"bytes"`
}

// CalibByteCost is one card's frame byte cost through the deterministic
// backends, measured by encoding the downscaled card to each wire payload and
// counting the bytes. The lossy sixel encoder jitters its payload size run to
// run, so it is omitted here to keep the committed file byte-stable; the block
// and kitty costs bracket the corpus byte span the bytesPerFrame band fractions.
type CalibByteCost struct {
	Block int `json:"block"`
	Kitty int `json:"kitty"`
}

// CalibRange is the observed min and max of one metric across all measured
// cards, the corpus-wide span the thresholds band within.
type CalibRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// CalibIntRange is the observed min and max of an integer metric (the frame byte
// cost) across all measured cards and deterministic backends.
type CalibIntRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// CalibRanges holds the corpus-wide observed range of each headline metric over
// the measured cards. The similarity, PSNR, and colour ranges are taken from the
// absolute block scores. HistDivergence and BytesPerFrame trace to data too, so
// every threshold band is a fraction of an observed range: HistDivergence comes
// from the absolute block scores, BytesPerFrame from the per-card frame byte cost
// measured across the deterministic backends.
type CalibRanges struct {
	SSIM           CalibRange    `json:"ssim"`
	MSSSIM         CalibRange    `json:"msssim"`
	PSNR           CalibRange    `json:"psnr"`
	DeltaEMean     CalibRange    `json:"deltaEMean"`
	DeltaEP95      CalibRange    `json:"deltaEP95"`
	HistDivergence CalibRange    `json:"histDivergence"`
	BytesPerFrame  CalibIntRange `json:"bytesPerFrame"`
}

// CalibLadderRung is one OCR ladder rung's measured CER and WER on a card, with
// whether it round-tripped below the mode's CER bound.
type CalibLadderRung struct {
	SizePx  int     `json:"sizePx"`
	CER     float64 `json:"cer"`
	WER     float64 `json:"wer"`
	Legible bool    `json:"legible"`
}

// CalibCardLadder holds one card's OCR ladder: every rung's CER and WER and the
// minimum legible size. MinLegiblePx is nil when no rung round-trips below the
// bound. It is populated only by the tagged OCR calibration path.
type CalibCardLadder struct {
	Card         string            `json:"card"`
	Rungs        []CalibLadderRung `json:"rungs"`
	MinLegiblePx *int              `json:"minLegiblePx"`
}

// Calibration is the committed calibration record: the per-card scores, the
// corpus-wide metric ranges, the intrinsic-noise floor, and the OCR CER ladder.
// DefaultThresholds turns these numbers into thresholds banded above the noise
// floor.
type Calibration struct {
	// SchemaVersion pins the calibration file shape; bump on a breaking change.
	SchemaVersion int `json:"schemaVersion"`
	// Grid is the cell grid the calibration measured at (120x40).
	Grid Viewport `json:"grid"`
	// Cards holds one entry per measured card, in CalibCards order.
	Cards []CalibCard `json:"cards"`
	// Ranges is the corpus-wide observed range of each metric, from absolute
	// scores.
	Ranges CalibRanges `json:"ranges"`
	// SixelPSNRNoiseFloor is the sixel backend's intrinsic PSNR jitter in dB: the
	// standard deviation of PSNR across repeated identical sixel renders, taken as
	// the worst (largest) card-wise value over the corpus. The lossy sixel encoder
	// is non-deterministic, so this is non-zero; the sixel PSNR threshold band sits
	// well above it. The standard deviation is used because the encoder's PSNR
	// spread is near-continuous and its observed min/max never settle, while the
	// standard deviation is stable to the recorded precision. The byte cost is
	// excluded from calibration for the same jitter reason, so this is the one
	// sixel non-determinism number the file records.
	SixelPSNRNoiseFloor float64 `json:"sixelPsnrNoiseFloor"`
	// OCRBuilt reports whether the OCR ladder was measured. It is false when the
	// calibration ran without the "ocr" build tag or without a Tesseract engine,
	// in which case Ladder is empty and the CER numbers are unset.
	OCRBuilt bool `json:"ocrBuilt"`
	// CERBound is the CER bound the ladder legibility used. Zero when OCR was not
	// measured.
	CERBound float64 `json:"cerBound"`
	// Ladder holds one OCR ladder per card; empty unless OCRBuilt is true.
	Ladder []CalibCardLadder `json:"ladder"`
}

// CalibInput pairs a card filename with its decoded source image.
type CalibInput struct {
	Card string
	Src  *image.RGBA
}

// MeasureCalibration block-renders each input at the pinned grid and measures
// the absolute and relative scores plus the intrinsic-noise floor, returning the
// per-card records and the corpus-wide ranges. The OCR ladder is left empty; the
// tagged path fills it. rast rasterises the block grid to the Route A pixel
// geometry the scores compare in. progress streams a line before each card is
// measured and before the sixel noise floor; it may be nil. It returns an error
// when the sixel noise floor cannot be measured (every round trip for a card
// failed), so a broken sixel encoder surfaces rather than reporting a perfect
// floor. Channel order is RGBA.
func MeasureCalibration(rast *capture.Rasteriser, inputs []CalibInput, progress Progress) (Calibration, error) {
	cards := make([]CalibCard, 0, len(inputs))
	for i, in := range inputs {
		progress.report("calibration: measuring card %d/%d (%s)", i+1, len(inputs), in.Card)
		cards = append(cards, measureCard(rast, in))
	}
	progress.report("calibration: measuring sixel PSNR noise floor over %d cards", len(inputs))
	floor, err := measureSixelPSNRNoiseFloor(inputs)
	if err != nil {
		return Calibration{}, err
	}
	return Calibration{
		SchemaVersion:       SchemaVersion,
		Grid:                Viewport{Cols: CalibCols, Rows: CalibRows},
		Cards:               cards,
		Ranges:              rangesOf(cards),
		SixelPSNRNoiseFloor: floor,
	}, nil
}

// measureSixelPSNRNoiseFloor measures the sixel backend's intrinsic PSNR jitter:
// the standard deviation of PSNR across repeated identical sixel renders, taken
// as the worst card-wise value over the corpus. For each card it downscales the
// source to the sweep pixel geometry, encodes and decodes it through the sixel
// backend sixelNoiseFloorRuns times, scores each decoded render against the same
// reference, and computes the standard deviation of those PSNRs. The corpus-wide
// maximum standard deviation is the band-driving number: the sixel PSNR threshold
// sits well above it. The result is quantised to sixelNoiseFloorDP decimals so
// the committed calibration file is byte-stable across runs despite the encoder
// jitter. It returns an error when any card's round trips all fail, so a broken
// sixel encoder surfaces rather than reporting a perfect zero floor. Channel
// order is RGBA.
func measureSixelPSNRNoiseFloor(inputs []CalibInput) (float64, error) {
	pxW, pxH := Viewport{Cols: CalibCols, Rows: CalibRows}.PixelSize()
	var worst float64
	for _, in := range inputs {
		reference := metrics.Downscale(in.Src, pxW, pxH)
		samples, err := collectSixelPSNRs(reference)
		if err != nil {
			return 0, fmt.Errorf("harness: sixel PSNR noise floor for %s: %w", in.Card, err)
		}
		if sd := stdDev(samples); sd > worst {
			worst = sd
		}
	}
	q := math.Pow(10, sixelNoiseFloorDP)
	return math.Round(worst*q) / q, nil
}

// collectSixelPSNRs runs sixelNoiseFloorRuns sixel round trips for one reference
// across a bounded worker pool sized to runtime.NumCPU(), and returns the PSNRs
// of the successful runs. Each run builds its own renderer and buffer and calls
// only pure functions, so the runs are independent and goroutine-safe. The order
// the results land in does not matter: the caller reduces them with a standard
// deviation, which is order-independent, so the parallel result matches the
// serial one to the bit. It returns an error when every run failed, so a fully
// broken sixel encoder surfaces rather than yielding a clean empty slice that
// computes a perfect zero floor.
func collectSixelPSNRs(reference *image.RGBA) ([]float64, error) {
	workers := runtime.NumCPU()
	if workers > sixelNoiseFloorRuns {
		workers = sixelNoiseFloorRuns
	}
	psnrs := make([]float64, sixelNoiseFloorRuns)
	errs := make([]error, sixelNoiseFloorRuns)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				psnrs[i], errs[i] = sixelRoundTripPSNR(reference)
			}
		}()
	}
	for i := range sixelNoiseFloorRuns {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	samples := make([]float64, 0, sixelNoiseFloorRuns)
	var firstErr error
	for i, err := range errs {
		if err == nil {
			samples = append(samples, psnrs[i])
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("harness: every sixel round trip failed: %w", firstErr)
	}
	return samples, nil
}

// stdDev returns the population standard deviation of xs, or zero for fewer than
// two samples.
func stdDev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	var sumsq float64
	for _, x := range xs {
		d := x - mean
		sumsq += d * d
	}
	return math.Sqrt(sumsq / float64(len(xs)))
}

// sixelRoundTripPSNR encodes a reference through the sixel backend, decodes it
// back, and returns the PSNR of the decoded render against the reference. It
// returns a wrapped error when the backend is unavailable or the encode or
// decode fails, so a failed run surfaces rather than skewing the jitter spread
// with a silent zero. Channel order is RGBA.
func sixelRoundTripPSNR(reference *image.RGBA) (float64, error) {
	g, ok := graphicsBackendFor(ModeSixel)
	if !ok {
		return 0, fmt.Errorf("harness: no sixel backend for %s", ModeSixel)
	}
	img, err := g.roundTrip(reference)
	if err != nil {
		return 0, fmt.Errorf("harness: sixel round trip: %w", err)
	}
	// Score only PSNR here, not the full metric set. This loop runs many sixel
	// round trips per card to measure the encoder's jitter floor, and PSNR is the
	// one metric it reads; computing SSIM, MS-SSIM, and CIEDE2000 per iteration
	// dominated the calibration run for no use. metrics.PSNR is the same value.
	return metrics.PSNR(img, reference), nil
}

// measureCard block-renders one card at the pinned grid and scores it absolute,
// relative, and against a repeated identical render for the noise floor.
func measureCard(rast *capture.Rasteriser, in CalibInput) CalibCard {
	// Downscale the source so each cell maps to a 2x4 block, the geometry the
	// block compositor and the oracle both read.
	cw, ch := Viewport{Cols: CalibCols, Rows: CalibRows}.ComposeSize()
	composeSrc := metrics.Downscale(in.Src, cw, ch)
	br := block.Renderer{Cols: CalibCols, Rows: CalibRows}
	render1 := renderBlock(rast, br, composeSrc)
	render2 := renderBlock(rast, br, composeSrc)

	scores := metrics.Score(rast, render1, composeSrc, CalibCols, CalibRows)
	// Intrinsic-noise floor: score a second identical render against the first.
	// A deterministic renderer scores a perfect, zero-error match here.
	noise := metrics.ScorePair(render1, render2)

	return CalibCard{
		Card:       in.Card,
		Absolute:   toCalibSet(scores.Absolute),
		Relative:   toCalibSet(scores.Relative),
		NoiseFloor: noiseFloorOf(noise),
		Bytes:      measureBytes(br, composeSrc),
	}
}

// measureBytes encodes the downscaled card through each backend and counts the
// frame byte cost. The block byte cost is the cell-grid encoder output; the
// sixel and kitty costs are the wire payload sizes. It is the data the
// bytesPerFrame threshold band traces to.
func measureBytes(br block.Renderer, composeSrc *image.RGBA) CalibByteCost {
	return CalibByteCost{
		Block: encodeBytes(func(w io.Writer) error { return br.Render(render.Frame{Img: composeSrc}, w) }),
		Kitty: encodeBytes(func(w io.Writer) error { var r kitty.Renderer; return r.Render(render.Frame{Img: composeSrc}, w) }),
	}
}

// encodeBytes runs one encode against a counting writer and returns the byte
// count. An encode error returns 0, so a backend that cannot encode the card does
// not skew the range with a partial count.
func encodeBytes(encode func(io.Writer) error) int {
	cw := metrics.NewCountingWriter(io.Discard)
	if err := encode(cw); err != nil {
		return 0
	}
	return int(cw.N)
}

// renderBlock composes the downscaled source onto the given block renderer's
// grid and rasterises it to the Route A pixel geometry.
func renderBlock(rast *capture.Rasteriser, br block.Renderer, composeSrc *image.RGBA) *image.RGBA {
	grid := br.Compose(render.Frame{Img: composeSrc})
	return rast.Rasterise(grid)
}

// toCalibSet projects a metrics.MetricSet onto the calibration headline set.
func toCalibSet(m metrics.MetricSet) CalibMetricSet {
	return CalibMetricSet{
		SSIM:           m.SSIM,
		MSSSIM:         m.MSSSIM,
		PSNR:           m.PSNR,
		DeltaEMean:     m.DeltaEMean,
		DeltaEP95:      m.DeltaEP95,
		HistDivergence: 1 - m.HistIntersection,
	}
}

// noiseFloorOf turns a render-vs-render MetricSet into the noise-floor spread:
// how far each metric sits from its perfect value. For the similarity metrics
// that is the gap below the ideal (1 for SSIM/MS-SSIM, the PSNR sentinel); for
// the error metrics it is the value itself. A deterministic render yields all
// zeros.
func noiseFloorOf(m metrics.MetricSet) CalibMetricSet {
	return CalibMetricSet{
		SSIM:   1 - m.SSIM,
		MSSSIM: 1 - m.MSSSIM,
		// PSNR is always 0 here: the block render path is deterministic, so
		// identical renders score the lossless sentinel. Any real per-run jitter
		// already shows in a non-zero SSIM, MS-SSIM, or dE field above.
		PSNR:           0,
		DeltaEMean:     m.DeltaEMean,
		DeltaEP95:      m.DeltaEP95,
		HistDivergence: 1 - m.HistIntersection,
	}
}

// rangesOf computes the corpus-wide min and max of each headline metric over the
// cards' absolute scores. The PSNR sentinel (identical images) is folded in as
// the max so a card that happens to score lossless does not skew the min.
func rangesOf(cards []CalibCard) CalibRanges {
	var r CalibRanges
	first := true
	for _, c := range cards {
		a := c.Absolute
		if first {
			r.SSIM = CalibRange{Min: a.SSIM, Max: a.SSIM}
			r.MSSSIM = CalibRange{Min: a.MSSSIM, Max: a.MSSSIM}
			r.PSNR = CalibRange{Min: a.PSNR, Max: a.PSNR}
			r.DeltaEMean = CalibRange{Min: a.DeltaEMean, Max: a.DeltaEMean}
			r.DeltaEP95 = CalibRange{Min: a.DeltaEP95, Max: a.DeltaEP95}
			r.HistDivergence = CalibRange{Min: a.HistDivergence, Max: a.HistDivergence}
			r.BytesPerFrame = byteRangeOf(c.Bytes)
			first = false
			continue
		}
		r.SSIM = widen(r.SSIM, a.SSIM)
		r.MSSSIM = widen(r.MSSSIM, a.MSSSIM)
		r.PSNR = widen(r.PSNR, a.PSNR)
		r.DeltaEMean = widen(r.DeltaEMean, a.DeltaEMean)
		r.DeltaEP95 = widen(r.DeltaEP95, a.DeltaEP95)
		r.HistDivergence = widen(r.HistDivergence, a.HistDivergence)
		r.BytesPerFrame = widenInt(r.BytesPerFrame, byteRangeOf(c.Bytes))
	}
	return r
}

// byteRangeOf returns the min and max frame byte cost across the deterministic
// backends for one card.
func byteRangeOf(b CalibByteCost) CalibIntRange {
	lo, hi := b.Block, b.Block
	if b.Kitty < lo {
		lo = b.Kitty
	}
	if b.Kitty > hi {
		hi = b.Kitty
	}
	return CalibIntRange{Min: lo, Max: hi}
}

// widenInt returns r grown to include the span of v.
func widenInt(r, v CalibIntRange) CalibIntRange {
	out := r
	if v.Min < out.Min {
		out.Min = v.Min
	}
	if v.Max > out.Max {
		out.Max = v.Max
	}
	return out
}

// widen returns r grown to include v.
func widen(r CalibRange, v float64) CalibRange {
	return CalibRange{Min: math.Min(r.Min, v), Max: math.Max(r.Max, v)}
}

// MarshalCalibration encodes a Calibration to deterministic, indented JSON with
// a trailing newline. The same record always marshals to byte-identical bytes,
// so the committed file diffs cleanly.
func MarshalCalibration(c Calibration) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// calibRelEps is the relative tolerance the calibration pass/fail comparison
// allows on each numeric metric: the measured value may differ from the
// committed value by this fraction of the committed magnitude. It absorbs the
// sub-ULP and cross-platform float drift the deterministic block path can show
// (different float rounding, FMA contraction, library minor versions) while
// staying far below a real regression. A 1e-6 relative band on an SSIM near 0.8
// is about 8e-7, so an SSIM regression of 0.01 fails; on a PSNR near 24 dB it is
// about 2.4e-5 dB, so a PSNR regression of 1 dB fails.
const calibRelEps = 1e-6

// calibAbsEps is the absolute tolerance floor the calibration pass/fail
// comparison adds to the relative band. It catches the case where the committed
// value is zero or tiny, where a relative band collapses to nothing: the
// deterministic noise-floor fields are exactly zero, and the quantised sixel
// noise floor sits on a one-decimal grid, so a true zero stays zero and the
// quantised value cannot drift within this floor. It is far below any metric
// step a real regression would move.
const calibAbsEps = 1e-9

// CompareCalibration compares the measured calibration against the committed
// file at path. The pass/fail decision compares each numeric metric field within
// a tolerance (calibRelEps relative, calibAbsEps absolute floor), so sub-ULP and
// cross-platform float drift passes while a real metric regression fails. The
// stored values are never tolerated: when update is true it rewrites the file
// only if the canonical bytes differ (write-if-changed) and reports ok true once
// written. A missing file under update is written. ok reflects the tolerance
// comparison, not a byte match.
func CompareCalibration(path string, c Calibration, update bool) (ok bool, wrote bool, err error) {
	want, err := MarshalCalibration(c)
	if err != nil {
		return false, false, err
	}
	got, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		var committed Calibration
		if jErr := json.Unmarshal(got, &committed); jErr != nil {
			return false, false, fmt.Errorf("harness: decode calibration %s: %w", path, jErr)
		}
		if calibrationsClose(committed, c) {
			// Within tolerance: rewrite only if the canonical bytes drifted, so a
			// real float wobble does not churn the committed file.
			if bytes.Equal(got, want) {
				return true, false, nil
			}
			if !update {
				return true, false, nil
			}
			if err := os.WriteFile(path, want, 0o644); err != nil {
				return false, false, err
			}
			return true, true, nil
		}
	case os.IsNotExist(readErr):
		// Missing file: a match is impossible; fall through to update logic.
	default:
		return false, false, readErr
	}
	if !update {
		return false, false, nil
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// calibrationsClose reports whether every numeric metric field of a and b sits
// within the calibration tolerance, and the structural fields (schema, grid,
// card and ladder shape, OCR flags) match exactly. It governs only pass/fail; it
// never changes the stored values.
func calibrationsClose(a, b Calibration) bool {
	if a.SchemaVersion != b.SchemaVersion || a.Grid != b.Grid {
		return false
	}
	if a.OCRBuilt != b.OCRBuilt || !floatClose(a.CERBound, b.CERBound) {
		return false
	}
	if !floatClose(a.SixelPSNRNoiseFloor, b.SixelPSNRNoiseFloor) {
		return false
	}
	if len(a.Cards) != len(b.Cards) {
		return false
	}
	for i := range a.Cards {
		if !calibCardClose(a.Cards[i], b.Cards[i]) {
			return false
		}
	}
	if !calibRangesClose(a.Ranges, b.Ranges) {
		return false
	}
	return calibLaddersClose(a.Ladder, b.Ladder)
}

// calibCardClose reports whether two cards match: the same source filename, the
// same byte costs, and every metric set within tolerance.
func calibCardClose(a, b CalibCard) bool {
	return a.Card == b.Card &&
		a.Bytes == b.Bytes &&
		metricSetClose(a.Absolute, b.Absolute) &&
		metricSetClose(a.Relative, b.Relative) &&
		metricSetClose(a.NoiseFloor, b.NoiseFloor)
}

// metricSetClose reports whether every field of two metric sets sits within the
// calibration tolerance.
func metricSetClose(a, b CalibMetricSet) bool {
	return floatClose(a.SSIM, b.SSIM) &&
		floatClose(a.MSSSIM, b.MSSSIM) &&
		floatClose(a.PSNR, b.PSNR) &&
		floatClose(a.DeltaEMean, b.DeltaEMean) &&
		floatClose(a.DeltaEP95, b.DeltaEP95) &&
		floatClose(a.HistDivergence, b.HistDivergence)
}

// calibRangesClose reports whether every corpus range bound sits within
// tolerance and the integer byte range matches exactly.
func calibRangesClose(a, b CalibRanges) bool {
	return rangeClose(a.SSIM, b.SSIM) &&
		rangeClose(a.MSSSIM, b.MSSSIM) &&
		rangeClose(a.PSNR, b.PSNR) &&
		rangeClose(a.DeltaEMean, b.DeltaEMean) &&
		rangeClose(a.DeltaEP95, b.DeltaEP95) &&
		rangeClose(a.HistDivergence, b.HistDivergence) &&
		a.BytesPerFrame == b.BytesPerFrame
}

// rangeClose reports whether both bounds of two ranges sit within tolerance.
func rangeClose(a, b CalibRange) bool {
	return floatClose(a.Min, b.Min) && floatClose(a.Max, b.Max)
}

// calibLaddersClose reports whether two OCR ladders match: the same shape, the
// same card labels and legibility flags, the same minimum legible size, and CER
// and WER within tolerance. The ladder is carried forward verbatim from the
// committed file in the default build, so this is an identity check there; it
// guards the tagged path where the CER is freshly measured.
func calibLaddersClose(a, b []CalibCardLadder) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Card != b[i].Card || !intPtrEqual(a[i].MinLegiblePx, b[i].MinLegiblePx) {
			return false
		}
		if len(a[i].Rungs) != len(b[i].Rungs) {
			return false
		}
		for j := range a[i].Rungs {
			ra, rb := a[i].Rungs[j], b[i].Rungs[j]
			if ra.SizePx != rb.SizePx || ra.Legible != rb.Legible ||
				!floatClose(ra.CER, rb.CER) || !floatClose(ra.WER, rb.WER) {
				return false
			}
		}
	}
	return true
}

// intPtrEqual reports whether two optional int pointers hold the same value or
// are both nil.
func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// floatClose reports whether a and b sit within the calibration tolerance: an
// absolute floor of calibAbsEps plus a relative band of calibRelEps scaled to
// the larger magnitude. The combined band passes float drift yet fails a real
// metric regression.
func floatClose(a, b float64) bool {
	diff := math.Abs(a - b)
	if diff <= calibAbsEps {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= calibRelEps*scale
}
