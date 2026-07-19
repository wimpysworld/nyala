package ocr

import (
	"image"
	"image/color"
	"testing"
)

func TestCER(t *testing.T) {
	cases := []struct {
		truth, got string
		want       float64
	}{
		{"hello", "hello", 0},
		{"", "", 0},
		{"", "x", 1},
		{"abcd", "abxd", 0.25},
		{"abcd", "abxcd", 0.25},
		{"abc", "", 1},
		{"cafe", "café", 0.25},
	}
	for _, c := range cases {
		if got := CER(c.truth, c.got); got != c.want {
			t.Errorf("CER(%q,%q) = %v, want %v", c.truth, c.got, got, c.want)
		}
	}
}

func TestOtsuTwoTone(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 10, 1))
	for x := range 5 {
		g.SetGray(x, 0, color.Gray{Y: 20})
	}
	for x := 5; x < 10; x++ {
		g.SetGray(x, 0, color.Gray{Y: 220})
	}
	tt := otsuThreshold(g)
	if tt < 20 || tt > 220 {
		t.Errorf("otsu threshold %d not between the two tones", tt)
	}
}
