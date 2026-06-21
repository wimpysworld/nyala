// Calibration measures the real metric ranges Nyala's block renderer reaches on
// the committed test cards, so task 7.6 sets thresholds from data instead of
// guesses.
//
// It block-renders each of the three test cards at the pinned 120x40 grid,
// scores the render against the downscaled input (absolute) and against the
// ideal block render (relative), and records SSIM, MS-SSIM, CIEDE2000 mean and
// p95, and PSNR per card. It also measures the intrinsic-noise floor: the metric
// spread across repeated identical renders of the same card. The block renderer
// is deterministic, so the floor is exactly zero, which is itself the recorded
// baseline a real per-run jitter would have to clear.
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
	"image"
	"math"
	"os"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// CalibCols and CalibRows are the pinned calibration grid: the Stage 0 120x40
// sweep grid, so the calibration numbers match the grid the sweep runs at.
const (
	CalibCols = 120
	CalibRows = 40
)

// CalibCards lists the three test cards the calibration measures, in the order
// they appear in the committed file. The Linux Matters page is excluded: the
// calibration measures the cards the SSIM/CER ranges are pinned from.
var CalibCards = []string{
	"testcard-01-greyscale.png",
	"testcard-02-colour.png",
	"testcard-03-images.png",
}

// CalibMetricSet is one card's quality scores, absolute or relative. It mirrors
// the headline metrics calibration pins ranges from: the structural similarity
// pair, PSNR, and the CIEDE2000 mean and p95.
type CalibMetricSet struct {
	SSIM       float64 `json:"ssim"`
	MSSSIM     float64 `json:"msssim"`
	PSNR       float64 `json:"psnr"`
	DeltaEMean float64 `json:"deltaEMean"`
	DeltaEP95  float64 `json:"deltaEP95"`
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
}

// CalibRange is the observed min and max of one metric across all measured
// cards, the corpus-wide span task 7.6 bands its thresholds within.
type CalibRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// CalibRanges holds the corpus-wide observed range of each headline metric over
// the measured cards, taken from the absolute scores.
type CalibRanges struct {
	SSIM       CalibRange `json:"ssim"`
	MSSSIM     CalibRange `json:"msssim"`
	PSNR       CalibRange `json:"psnr"`
	DeltaEMean CalibRange `json:"deltaEMean"`
	DeltaEP95  CalibRange `json:"deltaEP95"`
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
// Task 7.6 turns these numbers into thresholds banded above the noise floor.
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
// geometry the scores compare in. Channel order is RGBA.
func MeasureCalibration(rast *capture.Rasteriser, inputs []CalibInput) Calibration {
	cards := make([]CalibCard, 0, len(inputs))
	for _, in := range inputs {
		cards = append(cards, measureCard(rast, in))
	}
	return Calibration{
		SchemaVersion: SchemaVersion,
		Grid:          Viewport{Cols: CalibCols, Rows: CalibRows},
		Cards:         cards,
		Ranges:        rangesOf(cards),
	}
}

// measureCard block-renders one card at the pinned grid and scores it absolute,
// relative, and against a repeated identical render for the noise floor.
func measureCard(rast *capture.Rasteriser, in CalibInput) CalibCard {
	// Downscale the source so each cell maps to a 2x4 block, the geometry the
	// block compositor and the oracle both read.
	composeSrc := metrics.Downscale(in.Src, CalibCols*2, CalibRows*4)
	render1 := renderBlock(rast, composeSrc)
	render2 := renderBlock(rast, composeSrc)

	scores := metrics.Score(rast, render1, composeSrc, CalibCols, CalibRows)
	// Intrinsic-noise floor: score a second identical render against the first.
	// A deterministic renderer scores a perfect, zero-error match here.
	noise := metrics.ScorePair(render1, render2)

	return CalibCard{
		Card:       in.Card,
		Absolute:   toCalibSet(scores.Absolute),
		Relative:   toCalibSet(scores.Relative),
		NoiseFloor: noiseFloorOf(noise),
	}
}

// renderBlock composes the downscaled source onto the pinned block grid and
// rasterises it to the Route A pixel geometry.
func renderBlock(rast *capture.Rasteriser, composeSrc *image.RGBA) *image.RGBA {
	br := block.Renderer{Cols: CalibCols, Rows: CalibRows}
	grid := br.Compose(render.Frame{Img: composeSrc})
	return rast.Rasterise(grid)
}

// toCalibSet projects a metrics.MetricSet onto the calibration headline set.
func toCalibSet(m metrics.MetricSet) CalibMetricSet {
	return CalibMetricSet{
		SSIM:       m.SSIM,
		MSSSIM:     m.MSSSIM,
		PSNR:       m.PSNR,
		DeltaEMean: m.DeltaEMean,
		DeltaEP95:  m.DeltaEP95,
	}
}

// noiseFloorOf turns a render-vs-render MetricSet into the noise-floor spread:
// how far each metric sits from its perfect value. For the similarity metrics
// that is the gap below the ideal (1 for SSIM/MS-SSIM, the PSNR sentinel); for
// the error metrics it is the value itself. A deterministic render yields all
// zeros.
func noiseFloorOf(m metrics.MetricSet) CalibMetricSet {
	psnrGap := 0.0
	if m.PSNR != metrics.PSNRInfinite && !math.IsInf(m.PSNR, 1) {
		// Any finite PSNR on identical renders means per-run jitter; record how
		// far it falls short of the lossless sentinel as a positive spread.
		psnrGap = math.MaxFloat64 - m.PSNR
	}
	return CalibMetricSet{
		SSIM:       1 - m.SSIM,
		MSSSIM:     1 - m.MSSSIM,
		PSNR:       psnrGap,
		DeltaEMean: m.DeltaEMean,
		DeltaEP95:  m.DeltaEP95,
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
			first = false
			continue
		}
		r.SSIM = widen(r.SSIM, a.SSIM)
		r.MSSSIM = widen(r.MSSSIM, a.MSSSIM)
		r.PSNR = widen(r.PSNR, a.PSNR)
		r.DeltaEMean = widen(r.DeltaEMean, a.DeltaEMean)
		r.DeltaEP95 = widen(r.DeltaEP95, a.DeltaEP95)
	}
	return r
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

// CompareCalibration compares the marshalled calibration against the committed
// file at path. It returns ok true when they match byte for byte. When update is
// true it writes the bytes only if they differ (write-if-changed) and reports ok
// true once the file matches. A missing file under update is written.
func CompareCalibration(path string, c Calibration, update bool) (ok bool, wrote bool, err error) {
	want, err := MarshalCalibration(c)
	if err != nil {
		return false, false, err
	}
	got, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if bytes.Equal(got, want) {
			return true, false, nil
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
