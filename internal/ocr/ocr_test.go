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

func TestConditioningScale(t *testing.T) {
	c := DefaultConditioning()
	// Target cap height 32, range 2-4x.
	if got := c.Scale(8); got != 4 { // 8*4=32, exact
		t.Errorf("Scale(8) = %d, want 4", got)
	}
	if got := c.Scale(16); got != 2 { // 16*2=32, exact
		t.Errorf("Scale(16) = %d, want 2", got)
	}
	if got := c.Scale(0); got != c.MaxScale {
		t.Errorf("Scale(0) = %d, want MaxScale %d", got, c.MaxScale)
	}
	// Out-of-range source clamps into 2-4x.
	if got := c.Scale(100); got < c.MinScale || got > c.MaxScale {
		t.Errorf("Scale(100) = %d, out of [%d,%d]", got, c.MinScale, c.MaxScale)
	}
}

func TestConditionDarkOnLight(t *testing.T) {
	// A black background with white text (light-on-dark) must come out
	// dark-on-light: most pixels light (255), the ink pixels dark (0).
	src := image.NewRGBA(image.Rect(0, 0, 20, 10))
	// Fill black, opaque.
	for y := range 10 {
		for x := range 20 {
			src.SetRGBA(x, y, color.RGBA{R: 0, G: 0, B: 0, A: 255})
		}
	}
	// A few white "ink" pixels.
	for x := 2; x < 6; x++ {
		src.SetRGBA(x, 5, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	}

	out := Condition(src, 8, DefaultConditioning())
	var light, dark int
	for _, v := range out.Pix {
		switch v {
		case 255:
			light++
		case 0:
			dark++
		}
	}
	if light == 0 || dark == 0 {
		t.Fatalf("Condition produced no two-tone output: light=%d dark=%d", light, dark)
	}
	if dark >= light {
		t.Errorf("expected dark-on-light (more paper than ink): light=%d dark=%d", light, dark)
	}
}

func TestConditionPadAndScale(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 10, 8))
	for y := range 8 {
		for x := range 10 {
			src.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	c := DefaultConditioning()
	scale := c.Scale(8) // src cap 8 -> 4x
	out := Condition(src, 8, c)
	wantW := 10*scale + 2*c.Pad
	wantH := 8*scale + 2*c.Pad
	if out.Bounds().Dx() != wantW || out.Bounds().Dy() != wantH {
		t.Errorf("conditioned size = %dx%d, want %dx%d",
			out.Bounds().Dx(), out.Bounds().Dy(), wantW, wantH)
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
