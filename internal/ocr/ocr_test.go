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
		{"abc", "", 1},
	}
	for _, c := range cases {
		if got := CER(c.truth, c.got); got != c.want {
			t.Errorf("CER(%q,%q) = %v, want %v", c.truth, c.got, got, c.want)
		}
	}
}

func TestWER(t *testing.T) {
	cases := []struct {
		truth, got string
		want       float64
	}{
		{"the quick brown fox", "the quick brown fox", 0},
		{"", "", 0},
		{"a b c d", "a x c d", 0.25},
		{"one two", "", 1},
	}
	for _, c := range cases {
		if got := WER(c.truth, c.got); got != c.want {
			t.Errorf("WER(%q,%q) = %v, want %v", c.truth, c.got, got, c.want)
		}
	}
}

func TestScore(t *testing.T) {
	r := Score("Text Size 12px", "Text Size 12px")
	if r.CER != 0 || r.WER != 0 {
		t.Fatalf("identity Score: CER=%v WER=%v, want 0,0", r.CER, r.WER)
	}
	if r.Truth != "Text Size 12px" || r.Got != "Text Size 12px" {
		t.Fatalf("Score did not carry strings: %+v", r)
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
