// The baseline and the data-driven thresholds the sweep gates on.
//
// A Baseline is the committed per-(page, mode) snapshot of the headline quality
// and cost metrics from a known-good run. Thresholds are the per-metric bands a
// metric may move within before the move counts as a regression. The gate
// compares a fresh sweep against the baseline, applies the bands, and returns a
// breach for any metric that moved the wrong way past its band: a quality metric
// breaches on a drop, a cost metric on a rise.
//
// The bands are data-driven, set from the committed calibration
// (testdata/calibration.json), not guessed. That run measured the intrinsic-noise
// floor on repeated identical block renders: it is exactly zero, because the
// block render path is deterministic. The global bands therefore sit strictly
// above the measured noise floor. Each global band is also a small fraction of
// the metric's observed corpus range, so a real regression trips the gate while
// ordinary run-to-run jitter stays inside the band. The calibration records all
// six ranges: the similarity, PSNR, and colour ranges from the absolute block
// scores, the histDivergence range from the same scores, and the bytesPerFrame
// range from the per-card frame byte cost across the deterministic backends, so
// every global band traces to data.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// Metrics maps a metric name to its band and polarity. This is the global
	// band a metric is gated by unless a per-mode override in Modes applies.
	Metrics map[string]MetricThreshold `json:"metrics"`
	// Modes holds per-mode band overrides keyed by render mode then metric name.
	// A mode and metric present here replaces the global band for that one mode,
	// so a backend with a wider intrinsic-noise floor can keep a looser band. A
	// mode or metric absent here falls back to the global Metrics band.
	Modes map[string]map[string]MetricThreshold `json:"modes,omitempty"`
}

// metricName is a gated-metric identifier. It is a string-kinded type so a
// metric name is a named constant at every gate site and threshold map key
// rather than a free-form literal, which closes the fail-open typo risk: a
// misspelt name no longer silently drops a metric from the gate. The wire form
// (the thresholds map key, the breach metric field) stays the same string.
type metricName string

// The six gated metric names, in one place. Their string values are the JSON
// map keys in testdata/thresholds.json and the report field names the gate
// reads, so they must not change.
const (
	// ssimMetric is the structural similarity index.
	ssimMetric metricName = "ssim"
	// msssimMetric is the multiscale structural similarity index.
	msssimMetric metricName = "msssim"
	// psnrMetric is the peak signal-to-noise ratio in dB.
	psnrMetric metricName = "psnr"
	// deltaE2000MeanMetric is the mean per-pixel CIEDE2000 colour difference.
	deltaE2000MeanMetric metricName = "deltaE2000Mean"
	// histDivergenceMetric is one minus the colour-histogram intersection.
	histDivergenceMetric metricName = "histDivergence"
	// bytesPerFrameMetric marks the bytesPerFrame band as a relative fraction of
	// the baseline rather than an absolute count, since frame sizes span orders of
	// magnitude across the three backends.
	bytesPerFrameMetric metricName = "bytesPerFrame"
)

// requiredGatedMetrics is the set of metric names that must have a global band
// for the gate to be complete: every metric the gate checks. It is the
// gatedMetrics table names plus the bytesPerFrame cost metric, which gateOne
// handles after the table loop. Sourced from the const set, not literals.
func requiredGatedMetrics() []metricName {
	names := make([]metricName, 0, len(gatedMetrics)+1)
	for _, m := range gatedMetrics {
		names = append(names, m.name)
	}
	return append(names, bytesPerFrameMetric)
}

// validateGated checks every gated metric has a global band in t.Metrics, so a
// present-but-incomplete thresholds file cannot silently disable a gate. A
// per-mode override does not substitute for the global band, which is the
// fallback every mode relies on. It returns one error naming the missing
// metric(s) on a gap, nil otherwise.
func (t Thresholds) validateGated() error {
	var missing []string
	for _, name := range requiredGatedMetrics() {
		if _, ok := t.Metrics[string(name)]; !ok {
			missing = append(missing, string(name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("harness: thresholds missing global band for gated metric(s): %v", missing)
	}
	return nil
}

// thresholdFor returns the band for a metric under a given mode: the per-mode
// override in Modes when present, else the global Metrics band. The second
// return is false when no band gates the metric for that mode.
func (t Thresholds) thresholdFor(mode string, metric metricName) (MetricThreshold, bool) {
	if m, ok := t.Modes[mode]; ok {
		if th, ok := m[string(metric)]; ok {
			return th, true
		}
	}
	th, ok := t.Metrics[string(metric)]
	return th, ok
}

// DefaultThresholds returns the diagnostics thresholds banded above the committed
// calibration noise floor.
//
// The calibration measured the intrinsic-noise floor on repeated identical block
// renders as exactly zero on every metric, so each global band below is strictly
// above the floor. The global bands are small fractions of the observed corpus
// ranges in testdata/calibration.json (SSIM 0.539..0.621, MS-SSIM 0.556..0.568,
// PSNR 14.09..16.77 dB, dEMean 4.81..10.16): 0.02 on the 0..1 similarity indices,
// 0.5 dB on PSNR, 0.5 dE on the colour mean, 0.05 on the histDivergence range,
// and a relative 5% on the frame byte cost range. Block, kitty, and sixel are
// deterministic, so they use these tight global bands.
func DefaultThresholds() Thresholds {
	return Thresholds{
		SchemaVersion: SchemaVersion,
		Source:        "task 7.5 calibration (testdata/calibration.json); global bands above the zero block and sixel noise floors, fractions of the observed corpus ranges",
		Metrics: map[string]MetricThreshold{
			string(ssimMetric):           {Band: 0.02, HigherIsBetter: true},
			string(msssimMetric):         {Band: 0.02, HigherIsBetter: true},
			string(psnrMetric):           {Band: 0.5, HigherIsBetter: true},
			string(deltaE2000MeanMetric): {Band: 0.5, HigherIsBetter: false},
			string(histDivergenceMetric): {Band: 0.05, HigherIsBetter: false},
			string(bytesPerFrameMetric):  {Band: 0.05, HigherIsBetter: false},
		},
	}
}

// BaselineMetrics is one (page, mode) snapshot of the gated metrics. A nil
// pointer means the metric did not apply to that mode, so the gate skips it.
type BaselineMetrics struct {
	SSIM           *float64 `json:"ssim"`
	MSSSIM         *float64 `json:"msssim"`
	PSNR           *float64 `json:"psnr"`
	DeltaE2000Mean *float64 `json:"deltaE2000Mean"`
	HistDivergence *float64 `json:"histDivergence"`
	BytesPerFrame  *int     `json:"bytesPerFrame"`
	// LowContrastTextCells is the count of overlay text cells whose contrast
	// falls below the WCAG AA bound. It is nil for every non-overlay row, so the
	// nil-skip gate leaves those rows alone; only overlay rows carry a non-nil
	// count. It sits beside the other nilable per-entry pointers.
	LowContrastTextCells *int `json:"lowContrastTextCells,omitempty"`
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
// returns DefaultThresholds and no error, so the gate always has bands. Both the
// decoded file and the default fallback are checked for gate completeness, so a
// present-but-incomplete file or a corrupt default fails loud rather than
// silently disabling a gate.
func LoadThresholds(path string) (Thresholds, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t := DefaultThresholds()
		if verr := t.validateGated(); verr != nil {
			return Thresholds{}, verr
		}
		return t, nil
	}
	if err != nil {
		return Thresholds{}, err
	}
	var t Thresholds
	if err := json.Unmarshal(data, &t); err != nil {
		return Thresholds{}, fmt.Errorf("harness: decode thresholds %s: %w", path, err)
	}
	if verr := t.validateGated(); verr != nil {
		return Thresholds{}, verr
	}
	return t, nil
}

// dirMode and fileMode are the permission bits for directories and files the
// harness writes under testdata.
const (
	dirMode  = 0o755
	fileMode = 0o644
)

// compareFile writes want to path under update (write-if-changed) and reports
// whether the committed bytes match. It is the shared body behind the baseline
// and thresholds compare helpers. The write is non-atomic: it writes straight to
// the target, which is acceptable for the gitignored testdata artefacts and not
// a pattern to copy where a partial write would corrupt durable state.
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
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return false, false, err
	}
	if err := os.WriteFile(path, want, fileMode); err != nil {
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
