//go:build ocr

package ocr

import (
	"image"
	"image/color"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	rfont "github.com/wimpysworld/nyala/internal/render/font"
)

// renderLine draws s as dark text on a light ground at the given pixel cap
// height, using the embedded FiraCode Nerd Font Mono face. It returns the image and the source
// capital height that drives conditioning.
func renderLine(t *testing.T, s string, capPx int) (*image.RGBA, int) {
	t.Helper()
	a, err := rfont.Load()
	if err != nil {
		t.Fatalf("font atlas load: %v", err)
	}
	fnt, _, ok := a.Resolve('A')
	if !ok {
		t.Fatalf("font atlas has no glyph for 'A'")
	}
	// Cap height ~70% of em, so size the face above the target cap.
	size := float64(capPx) / 0.7
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{
		Size: size,
		DPI:  72,
	})
	if err != nil {
		t.Fatalf("new face: %v", err)
	}
	defer face.Close()

	adv := font.MeasureString(face, s)
	w := adv.Ceil() + 8
	h := int(size*1.4) + 8
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255 // opaque white ground
	}
	d := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.RGBA{A: 255}),
		Face: face,
		Dot:  fixed.P(4, int(size)),
	}
	d.DrawString(s)
	return img, capPx
}

// TestEngineSyntheticLine OCRs a synthetic ladder line through the fixed
// conditioning path and asserts a low CER. It skips when Tesseract is absent so
// the default test run stays green without the engine.
func TestEngineSyntheticLine(t *testing.T) {
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("Tesseract engine unavailable, skipping OCR round-trip: %v", err)
	}
	defer eng.Close()

	const truth = "Text Size 16px"
	img, capPx := renderLine(t, truth, 16)

	r, err := eng.ScoreImage(img, capPx, truth)
	if err != nil {
		t.Fatalf("ScoreImage: %v", err)
	}
	t.Logf("OCR got %q CER=%.3f WER=%.3f", r.Got, r.CER, r.WER)
	if r.CER > 0.15 {
		t.Errorf("CER %.3f above bound 0.15 for truth %q, got %q", r.CER, truth, r.Got)
	}
}

// TestEngineStableAcrossRuns asserts the conditioning path and OCR are stable on
// the same input across repeated runs. It skips when the engine is absent.
func TestEngineStableAcrossRuns(t *testing.T) {
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("Tesseract engine unavailable, skipping stability check: %v", err)
	}
	defer eng.Close()

	const truth = "Text Size 24px"
	img, capPx := renderLine(t, truth, 24)

	first, err := eng.Recognise(img, capPx)
	if err != nil {
		t.Fatalf("first recognise: %v", err)
	}
	for i := 0; i < 2; i++ {
		got, err := eng.Recognise(img, capPx)
		if err != nil {
			t.Fatalf("repeat recognise %d: %v", i, err)
		}
		if got != first {
			t.Errorf("run %d differs: %q != %q", i, got, first)
		}
	}
}
