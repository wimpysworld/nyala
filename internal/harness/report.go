// Package harness holds the diagnostics metrics sweep, the golden track, and the
// report schema that ties them together.
//
// Report is the per-(page, mode, run) record the sweep emits. Its keys are flat
// and stably ordered so an agent can diff two runs field by field. The shape is
// uniform across the block, sixel, and kitty modes: a metric that does not apply
// to a mode serialises as JSON null, never as zero, so a missing measurement
// never reads as a real value. Nullable fields are pointers for that reason.
package harness

import "github.com/wimpysworld/nyala/internal/cellraster"

// SchemaVersion is shared by the report, thresholds, baseline, and calibration
// JSON artefacts. Bump it when any of those shapes changes incompatibly.
const SchemaVersion = 2

// Viewport is the cell grid the sweep renders at.
type Viewport struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// PixelSize returns the Route A render extent in pixels: the cell grid scaled by
// the Route A cell pixel size. It is the geometry the reference image is
// downscaled to before scoring.
func (v Viewport) PixelSize() (w, h int) {
	return v.Cols * cellraster.CellPxW, v.Rows * cellraster.CellPxH
}

// ComposeSize returns the compose extent in pixels: two columns by four rows per
// cell, the 2x4-per-cell downscale geometry the block compositor and the oracle
// both read.
func (v Viewport) ComposeSize() (w, h int) {
	return v.Cols * 2, v.Rows * 4
}

// Quality holds the full-reference quality metrics for one render. Every metric
// field is a pointer so a not-applicable metric serialises as null, not zero.
type Quality struct {
	MSSSIM         *float64 `json:"msssim"`
	SSIM           *float64 `json:"ssim"`
	PSNR           *float64 `json:"psnr"`
	DeltaE2000Mean *float64 `json:"deltaE2000Mean"`
	DeltaE2000P95  *float64 `json:"deltaE2000P95"`
	DeltaEText     *float64 `json:"deltaEText"`
	DeltaEBitmap   *float64 `json:"deltaEBitmap"`
	HistDivergence *float64 `json:"histDivergence"`
	ContrastMin    *float64 `json:"contrastMin"`
	Sharpness      *float64 `json:"sharpness"`
}

// QualityRelative holds the relative quality metrics for one render, scored
// against the ideal block render rather than the downscaled input. It isolates
// the loss the real backend adds over the ideal block ceiling. Every metric
// field is a pointer so a not-applicable metric serialises as null, not zero.
// For kitty the ideal render equals the input, so its relative scores collapse
// toward the absolute scores.
type QualityRelative struct {
	MSSSIM         *float64 `json:"msssim"`
	SSIM           *float64 `json:"ssim"`
	PSNR           *float64 `json:"psnr"`
	MSE            *float64 `json:"mse"`
	GMSD           *float64 `json:"gmsd"`
	DeltaE2000Mean *float64 `json:"deltaE2000Mean"`
	DeltaE2000P95  *float64 `json:"deltaE2000P95"`
	HistDivergence *float64 `json:"histDivergence"`
}

// Perf holds the cost metrics for one render. Every metric field is a pointer so
// a not-applicable metric serialises as null, not zero.
type Perf struct {
	RenderMs       *float64 `json:"renderMs"`
	EncodeMs       *float64 `json:"encodeMs"`
	BytesPerFrame  *int     `json:"bytesPerFrame"`
	FPS            *float64 `json:"fps"`
	AllocsPerFrame *int     `json:"allocsPerFrame"`
	PeakRSSBytes   *int     `json:"peakRSSBytes"`
}

// CellStats counts the cells of the owned grid by class. The counts are always
// present, so they are plain integers, not pointers.
type CellStats struct {
	Total          int `json:"total"`
	TextCells      int `json:"textCells"`
	BitmapCells    int `json:"bitmapCells"`
	RegressedCells int `json:"regressedCells"`
}

// Artefacts holds the relative filenames of the emitted PNGs. QuantMap is a
// pointer because the kitty mode emits no quant-error map, so it serialises as
// null there.
type Artefacts struct {
	Composite string  `json:"composite"`
	Heatmap   string  `json:"heatmap"`
	CellMap   string  `json:"cellMap"`
	QuantMap  *string `json:"quantMap"`
	Histogram string  `json:"histogram"`
}

// Report is one sweep record for a single (page, mode, run). It marshals to a
// stable flat-key shape, uniform across all three modes.
type Report struct {
	SchemaVersion   int             `json:"schemaVersion"`
	Page            string          `json:"page"`
	Mode            string          `json:"mode"`
	Viewport        Viewport        `json:"viewport"`
	NyalaCommit     string          `json:"nyalaCommit"`
	Quality         Quality         `json:"quality"`
	QualityRelative QualityRelative `json:"qualityRelative"`
	Perf            Perf            `json:"perf"`
	CellStats       CellStats       `json:"cellStats"`
	Pass            bool            `json:"pass"`
	Artefacts       Artefacts       `json:"artefacts"`
}
