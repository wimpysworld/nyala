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
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
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
