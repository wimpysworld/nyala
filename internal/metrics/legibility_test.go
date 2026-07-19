package metrics

import (
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestWCAGContrastKnownPairs checks the closed-form contrast ratio against
// reference values: black on white is the 21:1 maximum, identical colours are
// 1:1, and #767676 on white sits just above the 4.5:1 AA boundary (the W3C
// reference value is 4.54:1).
func TestWCAGContrastKnownPairs(t *testing.T) {
	tests := []struct {
		name                   string
		r1, g1, b1, r2, g2, b2 uint8
		want                   float64
		tol                    float64
	}{
		{"black on white", 0, 0, 0, 255, 255, 255, 21.0, 1e-9},
		{"white on black", 255, 255, 255, 0, 0, 0, 21.0, 1e-9},
		{"identical grey", 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 1.0, 1e-9},
		{"767676 on white", 0x76, 0x76, 0x76, 255, 255, 255, 4.54, 0.01},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WCAGContrast(tc.r1, tc.g1, tc.b1, tc.r2, tc.g2, tc.b2)
			if math.Abs(got-tc.want) > tc.tol {
				t.Fatalf("WCAGContrast = %.6f, want %.6f (tol %g)", got, tc.want, tc.tol)
			}
		})
	}
}

// TestWCAGContrastSymmetric confirms the ratio does not depend on which colour
// is named the foreground: it always divides the lighter luminance by the
// darker one.
func TestWCAGContrastSymmetric(t *testing.T) {
	a := WCAGContrast(0x33, 0x66, 0x99, 0xee, 0xdd, 0xcc)
	b := WCAGContrast(0xee, 0xdd, 0xcc, 0x33, 0x66, 0x99)
	if math.Abs(a-b) > 1e-12 {
		t.Fatalf("WCAGContrast not symmetric: %.12f vs %.12f", a, b)
	}
}

// TestWCAGGridBelowCount builds a synthetic grid with a known split: three
// high-contrast cells (black on white, 21:1) and two low-contrast cells (grey
// on white, below 4.5:1). It asserts the below-threshold count, the minimum,
// and the mean.
func TestWCAGGridBelowCount(t *testing.T) {
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	black := color.RGBA{R: 0, G: 0, B: 0, A: 255}
	grey := color.RGBA{R: 0xaa, G: 0xaa, B: 0xaa, A: 255} // grey on white < 4.5:1

	g := block.Grid{Cols: 5, Rows: 1, Cells: make([]block.Cell, 5)}
	for i := range 3 {
		g.Cells[i] = block.Cell{Glyph: 'X', FG: black, BG: white}
	}
	for i := 3; i < 5; i++ {
		g.Cells[i] = block.Cell{Glyph: 'X', FG: grey, BG: white}
	}

	stats := WCAGGrid(g)
	if stats.BelowThreshold != 2 {
		t.Fatalf("BelowThreshold = %d, want 2", stats.BelowThreshold)
	}

	greyRatio := WCAGContrast(grey.R, grey.G, grey.B, white.R, white.G, white.B)
	if greyRatio >= WCAGThreshold {
		t.Fatalf("grey-on-white ratio %.3f unexpectedly >= %.1f", greyRatio, WCAGThreshold)
	}
	if math.Abs(stats.Min-greyRatio) > 1e-9 {
		t.Fatalf("Min = %.6f, want grey ratio %.6f", stats.Min, greyRatio)
	}
	wantMean := (3*21.0 + 2*greyRatio) / 5
	if math.Abs(stats.Mean-wantMean) > 1e-9 {
		t.Fatalf("Mean = %.6f, want %.6f", stats.Mean, wantMean)
	}
}

// TestWCAGGridEmpty confirms an empty grid yields the zero-value stats with no
// divide-by-zero.
func TestWCAGGridEmpty(t *testing.T) {
	stats := WCAGGrid(block.Grid{})
	if stats != (WCAGStats{}) {
		t.Fatalf("WCAGGrid(empty) = %+v, want zero value", stats)
	}
}

// TestLaplacianVarianceSharperRises checks the sharpness co-signal: a hard
// black/white edge (sharp) must yield a higher Laplacian variance than a smooth
// luma ramp (blurred) over the same dimensions.
func TestLaplacianVarianceSharperRises(t *testing.T) {
	const w, h = 16, 16

	sharp := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8(0)
			if x >= w/2 {
				v = 255
			}
			sharp.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}

	smooth := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8(x * 255 / (w - 1))
			smooth.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}

	sharpVar := LaplacianVariance(sharp)
	smoothVar := LaplacianVariance(smooth)
	if !(sharpVar > smoothVar) {
		t.Fatalf("expected sharp variance > smooth variance, got %.6f vs %.6f", sharpVar, smoothVar)
	}
}

// TestLaplacianVarianceFlat confirms a uniform crop has zero Laplacian
// variance: a flat field has no edges.
func TestLaplacianVarianceFlat(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range flat.Pix {
		flat.Pix[i] = 128
	}
	if v := LaplacianVariance(flat); v != 0 {
		t.Fatalf("LaplacianVariance(flat) = %.6f, want 0", v)
	}
}

// TestLaplacianVarianceTooSmall confirms an image with no interior pixels
// returns 0 rather than panicking.
func TestLaplacianVarianceTooSmall(t *testing.T) {
	small := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if v := LaplacianVariance(small); v != 0 {
		t.Fatalf("LaplacianVariance(2x2) = %.6f, want 0", v)
	}
}
