package metrics

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// Validation gate for the SSIM and MS-SSIM ports (tasks 4.2 and 4.3). It pins
// the ported metrics to external references on one fixed BT.601-greyscale
// fixture so any later drift in the structural core fails here, before any
// threshold consumes it.
//
// Oracle provenance, recorded so the numbers are reproducible:
//
//   - scikit-image is the authoritative 1e-4 oracle for both SSIM and MS-SSIM.
//     Computed with scikit-image 0.26.0, numpy 2.4.4, scipy 1.17.1, provisioned
//     through nixpkgs (`python3.withPackages [ scikit-image numpy imageio ]`).
//     SSIM uses skimage.metrics.structural_similarity with
//     gaussian_weights=True, sigma=1.5, use_sample_covariance=False,
//     data_range=255.0, matching the Go port's window, border, covariance, and
//     data range exactly. scikit-image ships no MS-SSIM, so the MS-SSIM oracle
//     composes the scikit SSIM core per Wang, Simoncelli, Bovik 2003 (CS term at
//     fine scales, full SSIM at the coarsest, 2x2 average-pool downsample,
//     renormalised weights), an independent code path from this package's
//     MSSSIM. Both Python oracle scripts ran against the identical luma planes
//     this fixture builds.
//
//   - ImageMagick and kornelski/dssim are the corroborating cross-checks named
//     in the plan. They are skipped here, not committed as constants:
//     ImageMagick 7.1.2-23 `compare -metric SSIM` is broken on this build
//     (identical inputs report 0, not 1; it also ignores compare:ssim-radius and
//     compare:ssim-sigma), so it cannot agree even within a loose tolerance, and
//     `dssim` is not available in the environment. See the Blocked note in the
//     task report for the exact commands to regenerate both cross-checks once
//     the tools are present.

const (
	// ssimFixtureW and ssimFixtureH are the fixture dimensions. 64x48 fits three
	// MS-SSIM scales: both sides stay >= ssimWinSize through 64x48 -> 32x24 ->
	// 16x12, then 8x6 drops below the window. This exercises the multiscale path
	// with the weight renormalisation rather than collapsing to one scale.
	ssimFixtureW = 64
	ssimFixtureH = 48

	// skSSIM is scikit-image's mean SSIM on the fixture (provenance above). The
	// Go SSIM port reproduces it to all printed digits; the 1e-4 gate guards
	// against any future numerical drift.
	skSSIM = 0.6995526230

	// skMSSSIM is the Wang-2003 MS-SSIM on the fixture, composed from the scikit
	// SSIM core (provenance above), and skMSSSIMScales is the scale count that
	// fits at 64x48. The Go MSSSIM port reproduces both.
	skMSSSIM       = 0.6149710947
	skMSSSIMScales = 3

	// ssimOracleTol is the validation tolerance the plan fixes for the
	// scikit-image agreement.
	ssimOracleTol = 1e-4
)

// ssimValidationFixture builds the two deterministic BT.601-greyscale images
// the oracle ran against. Each pixel is grey (R=G=B=v), so the BT.601 luma the
// metrics use equals v exactly, with no rounding between the Go and Python
// planes. Image b is image a shifted by a fixed offset over a 4x4 checker, a
// degraded but positively correlated copy that lands SSIM and MS-SSIM inside
// (0, 1) so the agreement is a real test, not the trivial identical case.
func ssimValidationFixture() (a, b *image.RGBA) {
	a = image.NewRGBA(image.Rect(0, 0, ssimFixtureW, ssimFixtureH))
	b = image.NewRGBA(image.Rect(0, 0, ssimFixtureW, ssimFixtureH))
	for y := range ssimFixtureH {
		for x := range ssimFixtureW {
			checker := 0
			if (x/4+y/4)%2 == 1 {
				checker = 40
			}
			va := uint8((x*5 + y*3 + checker) % 256)
			vb := uint8((x*5 + y*3 + checker + 18) % 256)
			a.SetRGBA(x, y, color.RGBA{R: va, G: va, B: va, A: 255})
			b.SetRGBA(x, y, color.RGBA{R: vb, G: vb, B: vb, A: 255})
		}
	}
	return a, b
}

// TestSSIMValidatesAgainstScikitImage pins the SSIM port to the scikit-image
// reference on the fixture, within 1e-4. Any drift in the window, sigma, border,
// covariance, data range, or crop moves the value off the committed constant and
// fails the gate.
func TestSSIMValidatesAgainstScikitImage(t *testing.T) {
	a, b := ssimValidationFixture()
	got := SSIM(a, b)
	if math.Abs(got-skSSIM) > ssimOracleTol {
		t.Errorf("SSIM = %.10f, scikit-image = %.10f, diff %.2e exceeds %.0e",
			got, skSSIM, math.Abs(got-skSSIM), ssimOracleTol)
	}
}

// TestMSSSIMValidatesAgainstScikitImage pins the MS-SSIM port to the
// Wang-2003-over-scikit reference on the fixture, within 1e-4, and pins the
// scale count the fixture exercises.
func TestMSSSIMValidatesAgainstScikitImage(t *testing.T) {
	a, b := ssimValidationFixture()
	got, scales := MSSSIM(a, b)
	if scales != skMSSSIMScales {
		t.Fatalf("scales = %d, want %d for a %dx%d fixture",
			scales, skMSSSIMScales, ssimFixtureW, ssimFixtureH)
	}
	if math.Abs(got-skMSSSIM) > ssimOracleTol {
		t.Errorf("MSSSIM = %.10f, reference = %.10f, diff %.2e exceeds %.0e",
			got, skMSSSIM, math.Abs(got-skMSSSIM), ssimOracleTol)
	}
}

// TestSSIMValidationCrossChecks records the ImageMagick and kornelski/dssim
// cross-checks the plan names. Both are skipped with their reason and the exact
// regeneration command rather than committing values from a broken or absent
// tool. Once the tools are present and agree within their documented tolerance,
// turn these skips into asserts.
func TestSSIMValidationCrossChecks(t *testing.T) {
	t.Run("ImageMagick", func(t *testing.T) {
		// ImageMagick 7.1.2-23 `compare -metric SSIM` reports 0 for identical
		// inputs on this build and ignores compare:ssim-radius/sigma, so it cannot
		// match the 11x11 scikit configuration even within a loose tolerance.
		// Regenerate with a fixed build via:
		//   magick compare -metric SSIM a.png b.png /tmp/diff.png
		// against grey PNGs of the fixture luma planes.
		t.Skip("ImageMagick compare -metric SSIM is broken on the available build; see provenance comment")
	})
	t.Run("dssim", func(t *testing.T) {
		// kornelski/dssim is not installed in the environment. It reports DSSIM
		// (1/SSIM - 1 over a multiscale internal pipeline), so it corroborates
		// direction, not the 1e-4 value. Regenerate with:
		//   dssim a.png b.png
		// against the fixture PNGs once the binary is present.
		t.Skip("kornelski/dssim is not available in the environment; see provenance comment")
	})
}
