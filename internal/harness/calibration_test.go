//go:build diagnostics

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCalibration measures the metric ranges on the three cards and compares the
// record against the committed testdata/calibration.json. Under
// UPDATE_CALIBRATION it rewrites the file (write-if-changed). It reads the shared
// calibration, so it adds no new measurement pass.
//
// The OCR ladder is gosseract-only and cgo-gated; the default build cannot run
// it. The shared calibration carries the OCR section of the committed file
// forward when it is present, so this path does not wipe any committed CER
// numbers.
func TestCalibration(t *testing.T) {
	cal := sharedCalibration(t)

	update := updateCalibration()
	ok, wrote, err := CompareCalibration(calibrationPath(), cal, update)
	if err != nil {
		t.Fatalf("CompareCalibration: %v", err)
	}
	if !ok {
		t.Fatalf("calibration mismatch; rerun with UPDATE_CALIBRATION=1 to refresh")
	}
	if update && wrote {
		t.Logf("calibration written to %s", calibrationPath())
	}

	if len(cal.Cards) != len(CalibCards) {
		t.Fatalf("got %d cards, want %d", len(cal.Cards), len(CalibCards))
	}
	for _, c := range cal.Cards {
		// SSIM and PSNR ranges are observed, not asserted to a fixed value, but
		// must be sane: SSIM in 0..1, PSNR positive, dE non-negative.
		if c.Absolute.SSIM < 0 || c.Absolute.SSIM > 1 {
			t.Errorf("%s absolute SSIM %v out of 0..1", c.Card, c.Absolute.SSIM)
		}
		if c.Absolute.PSNR <= 0 {
			t.Errorf("%s absolute PSNR %v not positive", c.Card, c.Absolute.PSNR)
		}
		if c.Absolute.DeltaEMean < 0 {
			t.Errorf("%s absolute dE mean %v negative", c.Card, c.Absolute.DeltaEMean)
		}
		// The block render is deterministic, so the intrinsic-noise floor is
		// exactly zero on every metric.
		assertZeroFloor(t, c)
		t.Logf("%s ABS ssim=%.4f msssim=%.4f psnr=%.2f dEmean=%.3f dEp95=%.3f",
			c.Card, c.Absolute.SSIM, c.Absolute.MSSSIM, c.Absolute.PSNR,
			c.Absolute.DeltaEMean, c.Absolute.DeltaEP95)
		t.Logf("%s REL ssim=%.4f msssim=%.4f psnr=%.2f dEmean=%.3f dEp95=%.3f",
			c.Card, c.Relative.SSIM, c.Relative.MSSSIM, c.Relative.PSNR,
			c.Relative.DeltaEMean, c.Relative.DeltaEP95)
	}
	t.Logf("ranges SSIM[%.4f,%.4f] PSNR[%.2f,%.2f] dEmean[%.3f,%.3f]",
		cal.Ranges.SSIM.Min, cal.Ranges.SSIM.Max,
		cal.Ranges.PSNR.Min, cal.Ranges.PSNR.Max,
		cal.Ranges.DeltaEMean.Min, cal.Ranges.DeltaEMean.Max)
}

// assertZeroFloor checks the intrinsic-noise floor is exactly zero on every
// metric, confirming the block render path is reproducible.
func assertZeroFloor(t *testing.T, c CalibCard) {
	t.Helper()
	for name, v := range map[string]float64{
		"ssim":   c.NoiseFloor.SSIM,
		"msssim": c.NoiseFloor.MSSSIM,
		"psnr":   c.NoiseFloor.PSNR,
		"dEmean": c.NoiseFloor.DeltaEMean,
		"dEp95":  c.NoiseFloor.DeltaEP95,
	} {
		if v != 0 {
			t.Errorf("%s noise floor %s = %v, want 0 (render not deterministic)", c.Card, name, v)
		}
	}
}

// TestCalibrationRangesCoverCards asserts every card's absolute scores fall
// inside the recorded corpus ranges, so the ranges are a true bound. It reads
// the shared calibration.
func TestCalibrationRangesCoverCards(t *testing.T) {
	cal := sharedCalibration(t)
	for _, c := range cal.Cards {
		within(t, c.Card, "ssim", c.Absolute.SSIM, cal.Ranges.SSIM)
		within(t, c.Card, "msssim", c.Absolute.MSSSIM, cal.Ranges.MSSSIM)
		within(t, c.Card, "psnr", c.Absolute.PSNR, cal.Ranges.PSNR)
		within(t, c.Card, "dEmean", c.Absolute.DeltaEMean, cal.Ranges.DeltaEMean)
		within(t, c.Card, "dEp95", c.Absolute.DeltaEP95, cal.Ranges.DeltaEP95)
	}
}

// within fails when v lies outside r (with a tiny epsilon for float equality).
func within(t *testing.T, card, name string, v float64, r CalibRange) {
	t.Helper()
	const eps = 1e-9
	if v < r.Min-eps || v > r.Max+eps {
		t.Errorf("%s %s %v outside range [%v,%v]", card, name, v, r.Min, r.Max)
	}
}

// TestCompareCalibrationTolerance checks the pass/fail comparison: a tiny float
// drift on a metric still passes, a real regression fails, and a passing run
// with no canonical-byte change leaves the file untouched. It writes a copy of
// the committed calibration to a temp file so it never churns the real one.
func TestCompareCalibrationTolerance(t *testing.T) {
	committed := loadCommittedCalibration(t)
	want, err := MarshalCalibration(committed)
	if err != nil {
		t.Fatalf("MarshalCalibration: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "calibration.json")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatalf("seed temp calibration: %v", err)
	}

	// A sub-ULP-scale drift on two fields is within tolerance: it passes and does
	// not rewrite the file, since the canonical bytes are unchanged at this scale.
	drift := committed
	drift.Cards[0].Absolute.SSIM += 1e-12
	drift.Cards[0].Absolute.PSNR += 1e-9
	ok, wrote, err := CompareCalibration(path, drift, false)
	if err != nil {
		t.Fatalf("CompareCalibration drift: %v", err)
	}
	if !ok {
		t.Errorf("within-tolerance drift failed comparison, want pass")
	}
	if wrote {
		t.Errorf("within-tolerance drift wrote the file, want no write")
	}

	// An SSIM shift of 0.01 is a real regression: it must fail.
	regSSIM := committed
	regSSIM.Cards[0].Absolute.SSIM += 0.01
	if ok, _, err := CompareCalibration(path, regSSIM, false); err != nil {
		t.Fatalf("CompareCalibration SSIM regression: %v", err)
	} else if ok {
		t.Errorf("SSIM regression of 0.01 passed comparison, want fail")
	}

	// A PSNR shift of 1 dB is a real regression: it must fail.
	regPSNR := committed
	regPSNR.Cards[0].Absolute.PSNR += 1.0
	if ok, _, err := CompareCalibration(path, regPSNR, false); err != nil {
		t.Fatalf("CompareCalibration PSNR regression: %v", err)
	} else if ok {
		t.Errorf("PSNR regression of 1 dB passed comparison, want fail")
	}

	// The temp file is unchanged after the no-update comparisons.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp calibration: %v", err)
	}
	if string(after) != string(want) {
		t.Errorf("temp calibration changed during no-update comparison")
	}
}
