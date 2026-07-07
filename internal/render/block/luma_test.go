package block

import (
	"image/color"
	"testing"
)

// TestLumaWeightsSumTo256 checks the fixed-point weights sum to exactly 256, so
// the integer luma spans the full 0..255*256 range and no channel is dropped.
func TestLumaWeightsSumTo256(t *testing.T) {
	if sum := lumaWeightR + lumaWeightG + lumaWeightB; sum != 256 {
		t.Fatalf("luma weights sum to %d, want 256", sum)
	}
}

// TestLumaKnownValues spot-checks luma against hand-computed weighted sums for
// the channel extremes and one mixed colour, guarding the weights and channel
// order (a swapped R/B weight would fail the mixed case).
func TestLumaKnownValues(t *testing.T) {
	cases := []struct {
		name string
		c    color.RGBA
		want int
	}{
		{"black", color.RGBA{A: 0xff}, 0},
		{"white", color.RGBA{R: 255, G: 255, B: 255, A: 0xff}, 255 * 256},
		{"pure red", color.RGBA{R: 255, A: 0xff}, 255 * lumaWeightR},
		{"pure green", color.RGBA{G: 255, A: 0xff}, 255 * lumaWeightG},
		{"pure blue", color.RGBA{B: 255, A: 0xff}, 255 * lumaWeightB},
		{"mixed", color.RGBA{R: 10, G: 20, B: 30, A: 0xff}, 77*10 + 150*20 + 29*30},
	}
	for _, tc := range cases {
		if got := luma(tc.c); got != tc.want {
			t.Errorf("luma(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestLumaGreenBrightestChannel checks green outweighs red and blue at equal
// intensity, the Rec.601 ordering the binarise relies on to place mid-luma
// sub-cells on the correct side of the split.
func TestLumaGreenBrightestChannel(t *testing.T) {
	r := luma(color.RGBA{R: 200, A: 0xff})
	g := luma(color.RGBA{G: 200, A: 0xff})
	b := luma(color.RGBA{B: 200, A: 0xff})
	if g <= r || r <= b {
		t.Fatalf("luma channel order wrong: green=%d red=%d blue=%d, want green>red>blue", g, r, b)
	}
}
