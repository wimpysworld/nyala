//go:build ocr

package ocr

import (
	"image"
	"image/draw"
	"testing"
)

// renderLadder draws each rung's "Text Size NNpx" line stacked vertically into
// one tall image, returning the composite and the per-rung row height. Each line
// is drawn at its own pixel cap height by reusing renderLine (tesseract_test.go).
// The composite models a card's left-column ladder closely enough to exercise
// the crop-and-OCR path end to end.
func renderLadder(t *testing.T, sizes []int, rowH int) *image.RGBA {
	t.Helper()
	maxW := 0
	lines := make([]*image.RGBA, len(sizes))
	for i, sz := range sizes {
		img, _ := renderLine(t, LadderText(sz), sz)
		lines[i] = img
		if img.Bounds().Dx() > maxW {
			maxW = img.Bounds().Dx()
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, maxW+8, rowH*len(sizes)))
	for i := range out.Pix {
		out.Pix[i] = 255 // opaque white ground
	}
	for i, img := range lines {
		// Vertically centre each rendered line in its row band.
		dy := i*rowH + (rowH-img.Bounds().Dy())/2
		if dy < i*rowH {
			dy = i * rowH
		}
		r := image.Rect(4, dy, 4+img.Bounds().Dx(), dy+img.Bounds().Dy())
		draw.Draw(out, r, img, img.Bounds().Min, draw.Over)
	}
	return out
}

// TestLadderEngine runs the OCR ladder over a rendered synthetic ladder through
// a live Tesseract engine: one result per line with CER and WER, and a minimum
// legible size at or below the smallest legible rung. It skips when the engine
// is absent so the default tagged run stays green without Tesseract.
func TestLadderEngine(t *testing.T) {
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("Tesseract engine unavailable, skipping ladder: %v", err)
	}
	defer eng.Close()

	sizes := []int{12, 16, 20, 24}
	const rowH = 48
	src := renderLadder(t, sizes, rowH)

	spec := Spec{Sizes: sizes, Left: 0, Top: 0, RowHeight: rowH}
	const cerBound = 0.15
	results, err := Ladder(src, spec, eng, cerBound)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	if len(results) != len(sizes) {
		t.Fatalf("got %d results, want %d", len(results), len(sizes))
	}
	for _, r := range results {
		t.Logf("%dpx: got %q CER=%.3f WER=%.3f legible=%v",
			r.SizePx, r.Result.Got, r.Result.CER, r.Result.WER, r.Legible)
	}
	min, ok := MinLegibleSize(results)
	if !ok {
		t.Fatalf("no rung legible at CER<=%.2f; ladder cannot report a minimum size", cerBound)
	}
	if min > 24 {
		t.Errorf("minimum legible size %dpx above the largest rung", min)
	}
}

// TestLadderEngineStable asserts the ladder result and the minimum legible size
// are stable across repeated runs for a fixed fixture. It skips without the
// engine.
func TestLadderEngineStable(t *testing.T) {
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("Tesseract engine unavailable, skipping ladder stability: %v", err)
	}
	defer eng.Close()

	sizes := []int{16, 24}
	const rowH = 48
	src := renderLadder(t, sizes, rowH)
	spec := Spec{Sizes: sizes, RowHeight: rowH}

	first, err := Ladder(src, spec, eng, 0.15)
	if err != nil {
		t.Fatalf("first ladder: %v", err)
	}
	firstMin, _ := MinLegibleSize(first)
	for i := 0; i < 2; i++ {
		got, err := Ladder(src, spec, eng, 0.15)
		if err != nil {
			t.Fatalf("repeat ladder %d: %v", i, err)
		}
		for j := range got {
			if got[j].Result.Got != first[j].Result.Got || got[j].Legible != first[j].Legible {
				t.Errorf("run %d rung %d differs: %+v != %+v", i, j, got[j], first[j])
			}
		}
		if m, _ := MinLegibleSize(got); m != firstMin {
			t.Errorf("run %d min legible size %d != %d", i, m, firstMin)
		}
	}
}
