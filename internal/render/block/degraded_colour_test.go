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
	if got, want := ANSI16Tonal(a), ANSI16Tonal(b); got != want {
		t.Errorf("ANSI16Tonal equal-luma results differ: %d and %d", got, want)
	}
}

func TestDegradedColourMonotonicGreyAndRanges(t *testing.T) {
	previousGrey := uint8(232)
	previousRank := 0
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

		ansi := ANSI16Tonal(c)
		if ansi > 15 {
			t.Fatalf("ANSI16Tonal(%d) = %d, outside 0..15", value, ansi)
		}
		rank := tonalRank(ansi)
		if rank < previousRank {
			t.Fatalf("ANSI16Tonal rank decreased at %d from %d to %d", value, previousRank, rank)
		}
		previousRank = rank
	}
}

func TestANSI16TonalReferenceOrderAndTies(t *testing.T) {
	palette := [16]color.RGBA{
		{R: 0, G: 0, B: 0},
		{R: 128, G: 0, B: 0},
		{R: 0, G: 128, B: 0},
		{R: 128, G: 128, B: 0},
		{R: 0, G: 0, B: 128},
		{R: 128, G: 0, B: 128},
		{R: 0, G: 128, B: 128},
		{R: 192, G: 192, B: 192},
		{R: 128, G: 128, B: 128},
		{R: 255, G: 0, B: 0},
		{R: 0, G: 255, B: 0},
		{R: 255, G: 255, B: 0},
		{R: 0, G: 0, B: 255},
		{R: 255, G: 0, B: 255},
		{R: 0, G: 255, B: 255},
		{R: 255, G: 255, B: 255},
	}
	wantOrder := [16]uint8{0, 4, 12, 1, 5, 2, 9, 6, 13, 3, 8, 10, 14, 7, 11, 15}
	wantLumas := [16]int{0, 3712, 7395, 9856, 13568, 19200, 19635, 22912, 27030, 29056, 32768, 38250, 45645, 49152, 57885, 65280}
	for rank, want := range wantOrder {
		if got := ANSI16Tonal(palette[want]); got != want {
			t.Errorf("tonal rank %d mapped canonical index %d to %d", rank, want, got)
		}
	}

	ties := 0
	for rank := range len(wantOrder) - 1 {
		lower := wantLumas[rank]
		upper := wantLumas[rank+1]
		if (lower+upper)%2 != 0 {
			continue
		}
		key := (lower + upper) / 2
		c, ok := colourWithLuma(key)
		if !ok {
			continue
		}
		ties++
		if got := ANSI16Tonal(c); got != wantOrder[rank] {
			t.Errorf("tie between ranks %d and %d mapped to %d, want darker index %d", rank, rank+1, got, wantOrder[rank])
		}
	}
	if ties == 0 {
		t.Fatal("no exact ANSI16Tonal tie was tested")
	}
}

func TestDegradedColourSampledRGBReferenceAndDeterminism(t *testing.T) {
	for r := 0; r <= 255; r += 17 {
		for g := 0; g <= 255; g += 17 {
			for b := 0; b <= 255; b += 17 {
				c := color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b)}
				grey := XTermGrey(c)
				ansi := ANSI16Tonal(c)
				if greyValue := int(grey); greyValue < 232 || greyValue > 255 {
					t.Fatalf("XTermGrey(%d,%d,%d) = %d, outside 232..255", r, g, b, grey)
				}
				if ansi > 15 {
					t.Fatalf("ANSI16Tonal(%d,%d,%d) = %d, outside 0..15", r, g, b, ansi)
				}
				if grey != referenceXTermGrey(c) {
					t.Fatalf("XTermGrey(%d,%d,%d) = %d, want %d", r, g, b, grey, referenceXTermGrey(c))
				}
				if ansi != referenceANSI16Tonal(c) {
					t.Fatalf("ANSI16Tonal(%d,%d,%d) = %d, want %d", r, g, b, ansi, referenceANSI16Tonal(c))
				}
				for range 4 {
					if XTermGrey(c) != grey || ANSI16Tonal(c) != ansi {
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
	if got := testing.AllocsPerRun(1000, func() { _ = ANSI16Tonal(c) }); got != 0 {
		t.Fatalf("ANSI16Tonal allocations = %g, want 0", got)
	}
}

func tonalRank(index uint8) int {
	for rank, candidate := range ansi16TonalIndexes {
		if candidate == index {
			return rank
		}
	}
	return -1
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

func referenceANSI16Tonal(c color.RGBA) uint8 {
	indexes := [16]uint8{0, 4, 12, 1, 5, 2, 9, 6, 13, 3, 8, 10, 14, 7, 11, 15}
	lumas := [16]int{0, 3712, 7395, 9856, 13568, 19200, 19635, 22912, 27030, 29056, 32768, 38250, 45645, 49152, 57885, 65280}
	key := luma(c)
	bestRank := 0
	bestDistance := key
	for rank := 1; rank < len(indexes); rank++ {
		distance := lumas[rank] - key
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			bestRank = rank
			bestDistance = distance
		}
	}
	return indexes[bestRank]
}

func colourWithLuma(want int) (color.RGBA, bool) {
	for r := range 256 {
		for g := range 256 {
			remainder := want - 77*r - 150*g
			if remainder >= 0 && remainder%29 == 0 && remainder/29 <= 255 {
				return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(remainder / 29)}, true
			}
		}
	}
	return color.RGBA{}, false
}
