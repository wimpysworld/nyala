package block

import (
	"image/color"
	"testing"
)

func TestXTermGreyExtremesAndBoundaries(t *testing.T) {
	if got := XTermGrey(color.RGBA{}); got != 232 {
		t.Fatalf("black mapped to %d, want 232", got)
	}
	if got := XTermGrey(color.RGBA{R: 255, G: 255, B: 255}); got != 255 {
		t.Fatalf("white mapped to %d, want 255", got)
	}

	for n := range 23 {
		midpoint := uint8(13 + 10*n)
		below := color.RGBA{R: midpoint - 1, G: midpoint - 1, B: midpoint - 1}
		at := color.RGBA{R: midpoint, G: midpoint, B: midpoint}
		if got := XTermGrey(below); got != uint8(232+n) {
			t.Errorf("boundary %d below mapped to %d, want %d", n, got, 232+n)
		}
		if got := XTermGrey(at); got != uint8(233+n) {
			t.Errorf("boundary %d midpoint mapped to %d, want %d", n, got, 233+n)
		}
	}
}

func TestDegradedColourEqualLumaChromaticInputs(t *testing.T) {
	a := color.RGBA{R: 150}
	b := color.RGBA{G: 77}
	if luma(a) != luma(b) {
		t.Fatal("test colours must have equal luma")
	}
	if got, want := XTermGrey(a), XTermGrey(b); got != want {
		t.Errorf("XTermGrey equal-luma results differ: %d and %d", got, want)
	}
}

func TestDegradedColourMonotonicGreyAndRanges(t *testing.T) {
	previousGrey := uint8(232)
	for value := range 256 {
		c := color.RGBA{R: uint8(value), G: uint8(value), B: uint8(value)}
		grey := XTermGrey(c)
		if greyValue := int(grey); greyValue < 232 || greyValue > 255 {
			t.Fatalf("XTermGrey(%d) = %d, outside 232..255", value, grey)
		}
		if grey < previousGrey {
			t.Fatalf("XTermGrey decreased at %d from %d to %d", value, previousGrey, grey)
		}
		previousGrey = grey
	}
}

func TestDegradedColourSampledRGBReferenceAndDeterminism(t *testing.T) {
	for r := 0; r <= 255; r += 17 {
		for g := 0; g <= 255; g += 17 {
			for b := 0; b <= 255; b += 17 {
				c := color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b)}
				grey := XTermGrey(c)
				if greyValue := int(grey); greyValue < 232 || greyValue > 255 {
					t.Fatalf("XTermGrey(%d,%d,%d) = %d, outside 232..255", r, g, b, grey)
				}
				if grey != referenceXTermGrey(c) {
					t.Fatalf("XTermGrey(%d,%d,%d) = %d, want %d", r, g, b, grey, referenceXTermGrey(c))
				}
				for range 4 {
					if XTermGrey(c) != grey {
						t.Fatalf("non-deterministic result for (%d,%d,%d)", r, g, b)
					}
				}
			}
		}
	}
}

func TestDegradedColourNoAllocations(t *testing.T) {
	c := color.RGBA{R: 37, G: 149, B: 211, A: 93}
	if got := testing.AllocsPerRun(1000, func() { _ = XTermGrey(c) }); got != 0 {
		t.Fatalf("XTermGrey allocations = %g, want 0", got)
	}
}

func referenceXTermGrey(c color.RGBA) uint8 {
	n := (luma(c) - 8*256 + 5*256) / (10 * 256)
	n = max(0, min(n, 23))
	return [...]uint8{
		232, 233, 234, 235, 236, 237, 238, 239,
		240, 241, 242, 243, 244, 245, 246, 247,
		248, 249, 250, 251, 252, 253, 254, 255,
	}[n]
}
