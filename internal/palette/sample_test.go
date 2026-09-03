package palette

import (
	"image"
	"image/color"
	"testing"
)

func TestSamplePaletteExtremeAspectRatio(t *testing.T) {
	t.Parallel()
	const height = 65537
	src := image.NewRGBA(image.Rect(0, 0, 1, height))
	for y := range height {
		src.SetRGBA(0, y, color.RGBA{R: uint8(y), G: uint8(y >> 8), B: uint8(y >> 4), A: 0xff})
	}

	palette := SamplePalette(src, paletteColours)
	if len(palette) == 0 || len(palette) > paletteColours {
		t.Fatalf("SamplePalette() returned %d colours, want 1..%d", len(palette), paletteColours)
	}
}

func TestSamplePaletteIntoReusesScratch(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1024, 128))
	for i := range src.Pix {
		src.Pix[i] = uint8(i)
	}
	palette, sample := SamplePaletteInto(src, paletteColours, nil, nil)
	if sample == nil {
		t.Fatal("SamplePaletteInto() did not allocate sample scratch for an image over budget")
	}
	want := sample
	palette, sample = SamplePaletteInto(src, paletteColours, palette, sample)
	if sample != want {
		t.Fatal("SamplePaletteInto() replaced same-size sample scratch")
	}
	if len(palette) == 0 || len(palette) > paletteColours {
		t.Fatalf("SamplePaletteInto() returned %d colours, want 1..%d", len(palette), paletteColours)
	}
}
