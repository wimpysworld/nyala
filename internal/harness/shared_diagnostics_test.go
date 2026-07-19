//go:build diagnostics

package harness

import (
	"path/filepath"
	"sync"
	"testing"
)

// diagnosticsOutDir is the persistent sweep output directory the shared sweep
// writes to, the same path the golden and gate run use. It survives the whole
// tagged run, so the file-emission checks can stat the artefacts after the test
// that triggered the sweep returns.
func diagnosticsOutDir() string {
	return filepath.Join("..", "..", "testdata", "diagnostics")
}

// The diagnostics tag runs the heavy quality gate: a full corpus sweep and the
// metric calibration. Both passes drive the rasteriser over every fixture, so
// each costs tens of seconds. The tagged tests below share one sweep and one
// calibration through these once-guarded caches, so the whole tagged run does
// one sweep and one calibration rather than repeating them per test.

var (
	sweepOnce   sync.Once
	sweepResult *SweepResult
	sweepDir    string

	calibOnce sync.Once
	calibVal  Calibration
)

// sharedSweep runs the corpus sweep once and caches the result and the temp
// output directory. Every tagged test that needs a sweep reads this shared
// result rather than calling RunSweep again. It gates against the committed
// baseline and thresholds, mirroring the diagnostics gate.
func sharedSweep(t *testing.T) (*SweepResult, string) {
	t.Helper()
	sweepOnce.Do(func() {
		rast := newRasteriser(t)
		dir := diagnosticsOutDir()
		thresholds, err := LoadThresholds(thresholdsPath())
		if err != nil {
			t.Fatalf("LoadThresholds: %v", err)
		}
		baseline, err := LoadBaseline(baselinePath())
		if err != nil {
			t.Fatalf("LoadBaseline: %v", err)
		}
		res, err := RunSweep(rast, sweepConfig(dir), loadSweepInputs(t), GateConfig{
			Baseline:   baseline,
			Thresholds: thresholds,
			Progress:   logProgress(t),
		})
		if err != nil {
			t.Fatalf("RunSweep: %v", err)
		}
		sweepResult = &res
		sweepDir = dir
	})
	return sweepResult, sweepDir
}

// sharedCalibration measures the calibration once and caches it. Every tagged
// calibration test reads this shared record.
func sharedCalibration(t *testing.T) Calibration {
	t.Helper()
	calibOnce.Do(func() {
		rast := newRasteriser(t)
		cal, err := MeasureCalibration(rast, loadCalibInputs(t), logProgress(t))
		if err != nil {
			t.Fatalf("MeasureCalibration: %v", err)
		}
		calibVal = cal
	})
	return calibVal
}
