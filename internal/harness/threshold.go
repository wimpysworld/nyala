// The baseline and the data-driven thresholds the sweep gates on.
//
// A Baseline is the committed per-(page, mode) snapshot of the headline quality
// and cost metrics from a known-good run. Thresholds are the per-metric bands a
// metric may move within before the move counts as a regression. The gate
// compares a fresh sweep against the baseline, applies the bands, and returns a
// breach for any metric that moved the wrong way past its band: a quality metric
// breaches on a drop, a cost metric on a rise.
//
// The bands are data-driven, set from the task 7.5 calibration
// (testdata/calibration.json), not guessed. That run measured the intrinsic-noise
// floor on repeated identical block renders: it is exactly zero, because the
// block render path is deterministic. Every band therefore sits strictly above
// the measured noise floor. Each band is also a small fraction of the metric's
// observed corpus range, so a real regression trips the gate while ordinary
// run-to-run jitter (which the calibration showed to be zero for block, and
// which only the lossy sixel path can introduce) stays inside the band. The
// calibration records all six ranges: the similarity, PSNR, and colour ranges
// from the absolute block scores, the histDivergence range from the same scores,
// and the bytesPerFrame range from the per-card frame byte cost across the
// deterministic backends, so every band traces to data.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// MetricThreshold is the regression band for one metric and the direction a
// regression moves. Band is the magnitude a metric may move before it breaches;
// HigherIsBetter records the metric's polarity, so the gate knows a quality
// drop and a cost rise are the two regressions.
type MetricThreshold struct {
	// Band is the maximum allowed regression magnitude before a breach. It is
	// always positive. A move within the band, or any move the good way, passes.
	Band float64 `json:"band"`
	// HigherIsBetter is true for a quality metric (SSIM, PSNR), where a drop is
	// the regression, and false for a cost metric (bytes), where a rise is.
	HigherIsBetter bool `json:"higherIsBetter"`
}

// Thresholds is the set of per-metric regression bands the gate applies. A
// metric absent from the map is not gated. Keys are the metric names the gate
// reads from each report: "ssim", "msssim", "psnr", "deltaE2000Mean",
// "histDivergence", and "bytesPerFrame".
type Thresholds struct {
	// SchemaVersion pins the thresholds file shape; bump on a breaking change.
	SchemaVersion int `json:"schemaVersion"`
	// Source notes where the bands came from, for provenance.
	Source string `json:"source"`
	// Metrics maps a metric name to its band and polarity.
	Metrics map[string]MetricThreshold `json:"metrics"`
}

// DefaultThresholds returns the Stage 0 thresholds banded above the task 7.5
// calibration noise floor.
//
// The calibration measured the intrinsic-noise floor on repeated identical block
// renders as exactly zero on every metric, so each band below is strictly above
// the floor. The bands are small fractions of the observed corpus ranges in
// testdata/calibration.json (SSIM 0.539..0.621, MS-SSIM 0.556..0.568, PSNR
// 14.09..16.77 dB, dEMean 4.81..10.16): 0.02 on the 0..1 similarity indices,
// 0.5 dB on PSNR, 0.5 dE on the colour mean, 0.05 on the histDivergence range,
// and a relative 5% on the frame byte cost range. Each band is large enough to
// absorb the lossy sixel round trip's run-to-run jitter yet small enough that a
// genuine regression trips it.
func DefaultThresholds() Thresholds {
	return Thresholds{
		SchemaVersion: SchemaVersion,
		Source:        "task 7.5 calibration (testdata/calibration.json); bands above the zero noise floor, fractions of the observed corpus ranges including the histDivergence and bytesPerFrame ranges",
		Metrics: map[string]MetricThreshold{
			"ssim":           {Band: 0.02, HigherIsBetter: true},
			"msssim":         {Band: 0.02, HigherIsBetter: true},
			"psnr":           {Band: 0.5, HigherIsBetter: true},
			"deltaE2000Mean": {Band: 0.5, HigherIsBetter: false},
			"histDivergence": {Band: 0.05, HigherIsBetter: false},
			"bytesPerFrame":  {Band: 0.05, HigherIsBetter: false},
		},
	}
}

// bytesPerFrameMetric marks the bytesPerFrame band as a relative fraction of the
// baseline rather than an absolute count, since frame sizes span orders of
// magnitude across the three backends.
const bytesPerFrameMetric = "bytesPerFrame"

// BaselineMetrics is one (page, mode) snapshot of the gated metrics. A nil
// pointer means the metric did not apply to that mode, so the gate skips it.
type BaselineMetrics struct {
	SSIM           *float64 `json:"ssim"`
	MSSSIM         *float64 `json:"msssim"`
	PSNR           *float64 `json:"psnr"`
	DeltaE2000Mean *float64 `json:"deltaE2000Mean"`
	HistDivergence *float64 `json:"histDivergence"`
	BytesPerFrame  *int     `json:"bytesPerFrame"`
}

// BaselineEntry is one baseline record keyed by page and mode.
type BaselineEntry struct {
	Page    string          `json:"page"`
	Mode    string          `json:"mode"`
	Metrics BaselineMetrics `json:"metrics"`
}

// Baseline is the committed snapshot the gate compares a fresh sweep against. It
// holds one entry per (page, mode).
type Baseline struct {
	// SchemaVersion pins the baseline file shape; bump on a breaking change.
	SchemaVersion int `json:"schemaVersion"`
	// Grid is the cell grid the baseline was measured at (120x40).
	Grid Viewport `json:"grid"`
	// Entries holds one snapshot per (page, mode), page then mode order.
	Entries []BaselineEntry `json:"entries"`
}

// lookup returns the baseline entry for a (page, mode) and whether it exists.
func (b *Baseline) lookup(page, mode string) (BaselineMetrics, bool) {
	for i := range b.Entries {
		if b.Entries[i].Page == page && b.Entries[i].Mode == mode {
			return b.Entries[i].Metrics, true
		}
	}
	return BaselineMetrics{}, false
}

// BaselineFromReports builds a Baseline snapshot from a completed sweep's
// reports, recording only the gated metrics. It is how a first baseline is
// seeded and how the committed file is refreshed after an intended change.
func BaselineFromReports(grid Viewport, reports []Report) Baseline {
	sorted := append([]Report(nil), reports...)
	sortReports(sorted)
	entries := make([]BaselineEntry, 0, len(sorted))
	for _, r := range sorted {
		entries = append(entries, BaselineEntry{
			Page: r.Page,
			Mode: r.Mode,
			Metrics: BaselineMetrics{
				SSIM:           r.Quality.SSIM,
				MSSSIM:         r.Quality.MSSSIM,
				PSNR:           r.Quality.PSNR,
				DeltaE2000Mean: r.Quality.DeltaE2000Mean,
				HistDivergence: r.Quality.HistDivergence,
				BytesPerFrame:  r.Perf.BytesPerFrame,
			},
		})
	}
	return Baseline{SchemaVersion: SchemaVersion, Grid: grid, Entries: entries}
}

// gate compares each report against the baseline under the thresholds and
// returns every metric that regressed past its band. A nil baseline skips the
// gate (a first run seeds the baseline rather than failing). A report with no
// matching baseline entry is skipped, so adding a page never fails the gate.
func gate(reports []Report, baseline *Baseline, thresholds Thresholds) []Breach {
	if baseline == nil {
		return nil
	}
	var breaches []Breach
	for _, r := range reports {
		base, ok := baseline.lookup(r.Page, r.Mode)
		if !ok {
			continue
		}
		breaches = append(breaches, gateOne(r, base, thresholds)...)
	}
	return breaches
}

// gateOne checks one report's gated metrics against its baseline entry.
func gateOne(r Report, base BaselineMetrics, t Thresholds) []Breach {
	var out []Breach
	check := func(metric string, measured, baseline *float64) {
		if measured == nil || baseline == nil {
			return
		}
		th, ok := t.Metrics[metric]
		if !ok {
			return
		}
		if br, breached := evalBand(r.Page, r.Mode, metric, *baseline, *measured, th, 0); breached {
			out = append(out, br)
		}
	}
	check("ssim", r.Quality.SSIM, base.SSIM)
	check("msssim", r.Quality.MSSSIM, base.MSSSIM)
	check("psnr", r.Quality.PSNR, base.PSNR)
	check("deltaE2000Mean", r.Quality.DeltaE2000Mean, base.DeltaE2000Mean)
	check("histDivergence", r.Quality.HistDivergence, base.HistDivergence)

	if th, ok := t.Metrics[bytesPerFrameMetric]; ok && r.Perf.BytesPerFrame != nil && base.BytesPerFrame != nil {
		measured := float64(*r.Perf.BytesPerFrame)
		baseline := float64(*base.BytesPerFrame)
		// The byte band is relative: a fraction of the baseline frame size,
		// since payload sizes span orders of magnitude across the backends.
		effBand := th.Band * baseline
		if br, breached := evalBand(r.Page, r.Mode, bytesPerFrameMetric, baseline, measured, MetricThreshold{Band: effBand, HigherIsBetter: th.HigherIsBetter}, th.Band); breached {
			out = append(out, br)
		}
	}
	return out
}

// evalBand decides whether a single metric breached its band. For a quality
// metric (HigherIsBetter) the regression is a drop, so it breaches when the
// measured value falls more than Band below the baseline. For a cost metric it
// breaches when the measured value rises more than Band above the baseline. A
// move the good way, or within the band, passes. reportBand is the band recorded
// in the breach (the relative fraction for bytesPerFrame, else the absolute
// band).
func evalBand(page, mode, metric string, baseline, measured float64, th MetricThreshold, reportBand float64) (Breach, bool) {
	delta := measured - baseline
	band := reportBand
	if band == 0 {
		band = th.Band
	}
	var breached bool
	if th.HigherIsBetter {
		// Regression is a drop: measured below baseline by more than the band.
		breached = baseline-measured > th.Band+gateEps
	} else {
		// Regression is a rise: measured above baseline by more than the band.
		breached = measured-baseline > th.Band+gateEps
	}
	if !breached {
		return Breach{}, false
	}
	return Breach{
		Page:     page,
		Mode:     mode,
		Metric:   metric,
		Baseline: baseline,
		Measured: measured,
		Delta:    delta,
		Band:     band,
	}, true
}

// gateEps absorbs floating-point noise at the band edge so a value exactly on
// the band does not flap between pass and fail across runs.
const gateEps = 1e-9

// MarshalBaseline encodes a Baseline to deterministic, indented JSON with a
// trailing newline so the committed file diffs cleanly.
func MarshalBaseline(b Baseline) ([]byte, error) {
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// MarshalThresholds encodes a Thresholds to deterministic, indented JSON with a
// trailing newline.
func MarshalThresholds(t Thresholds) ([]byte, error) {
	out, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// LoadBaseline reads and decodes a committed baseline file. A missing file
// returns a nil Baseline and no error, which the gate treats as "seed a first
// baseline".
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("harness: decode baseline %s: %w", path, err)
	}
	return &b, nil
}

// LoadThresholds reads and decodes a committed thresholds file. A missing file
// returns DefaultThresholds and no error, so the gate always has bands.
func LoadThresholds(path string) (Thresholds, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DefaultThresholds(), nil
	}
	if err != nil {
		return Thresholds{}, err
	}
	var t Thresholds
	if err := json.Unmarshal(data, &t); err != nil {
		return Thresholds{}, fmt.Errorf("harness: decode thresholds %s: %w", path, err)
	}
	return t, nil
}

// compareFile writes want to path under update (write-if-changed) and reports
// whether the committed bytes match. It is the shared body behind the baseline
// and thresholds compare helpers.
func compareFile(path string, want []byte, update bool) (ok bool, wrote bool, err error) {
	got, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if bytes.Equal(got, want) {
			return true, false, nil
		}
	case os.IsNotExist(readErr):
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

// CompareBaseline compares a Baseline against the committed file at path,
// writing it under update (write-if-changed).
func CompareBaseline(path string, b Baseline, update bool) (ok bool, wrote bool, err error) {
	want, err := MarshalBaseline(b)
	if err != nil {
		return false, false, err
	}
	return compareFile(path, want, update)
}

// CompareThresholds compares a Thresholds against the committed file at path,
// writing it under update (write-if-changed).
func CompareThresholds(path string, t Thresholds, update bool) (ok bool, wrote bool, err error) {
	want, err := MarshalThresholds(t)
	if err != nil {
		return false, false, err
	}
	return compareFile(path, want, update)
}
