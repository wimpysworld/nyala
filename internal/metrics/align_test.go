package metrics

import (
	"image"
	"image/color"
	"reflect"
	"testing"

	"golang.org/x/image/draw"
)

// TestDownscaleDimensions checks the reference resizes to the exact render-grid
// size.
func TestDownscaleDimensions(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 640, 480))
	const wantW, wantH = 120, 40
	dst := Downscale(src, wantW, wantH)
	if got := dst.Bounds().Dx(); got != wantW {
		t.Errorf("width = %d, want %d", got, wantW)
	}
	if got := dst.Bounds().Dy(); got != wantH {
		t.Errorf("height = %d, want %d", got, wantH)
	}
}

// TestDownscaleReturnsRGBA checks the output keeps RGBA channel order.
func TestDownscaleReturnsRGBA(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			src.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	dst := Downscale(src, 4, 4)
	if dst.ColorModel() != color.RGBAModel {
		t.Fatalf("colour model = %v, want RGBA", dst.ColorModel())
	}
	got := dst.RGBAAt(1, 1)
	if got.R <= got.G || got.G <= got.B {
		t.Errorf("channel order wrong: got %+v, want R>G>B", got)
	}
}

// TestAlignFilterPinnedCatmullRom asserts the pinned filter is CatmullRom.
func TestAlignFilterPinnedCatmullRom(t *testing.T) {
	if !reflect.DeepEqual(AlignFilter, draw.Scaler(draw.CatmullRom)) {
		t.Errorf("AlignFilter is not draw.CatmullRom")
	}
}

// TestDownscaleLiveIntoAreaAverage checks the live downscale area-averages: a 4x4
// source of four known 2x2 quadrants downscaled to 2x2 must yield each destination
// pixel as the mean of its quadrant. The values are picked so each quadrant averages
// to an exact integer, so the assertion is against a hand-computed reference.
func TestDownscaleLiveIntoAreaAverage(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	// Each 2x2 quadrant is filled with a colour whose channel means are exact.
	// Top-left mean 20, top-right mean 100, bottom-left mean 160, bottom-right 240.
	quads := [2][2][4]uint8{
		{{10, 20, 30, 255}, {90, 100, 110, 255}},
		{{150, 160, 170, 255}, {230, 240, 250, 255}},
	}
	for qy := range 2 {
		for qx := range 2 {
			c := quads[qy][qx]
			for dy := range 2 {
				for dx := range 2 {
					src.SetRGBA(qx*2+dx, qy*2+dy, color.RGBA{R: c[0], G: c[1], B: c[2], A: c[3]})
				}
			}
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, 2, 2))
	DownscaleLiveInto(dst, src)
	want := [2][2]color.RGBA{
		{{R: 10, G: 20, B: 30, A: 255}, {R: 90, G: 100, B: 110, A: 255}},
		{{R: 150, G: 160, B: 170, A: 255}, {R: 230, G: 240, B: 250, A: 255}},
	}
	for y := range 2 {
		for x := range 2 {
			if got := dst.RGBAAt(x, y); got != want[y][x] {
				t.Errorf("dst(%d,%d) = %+v, want the quadrant mean %+v", x, y, got, want[y][x])
			}
		}
	}
}
