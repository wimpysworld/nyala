package ocr

import (
	"fmt"
	"image"
	"testing"
)

func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	return img
}

// truthRecogniser marks a rung legible when its size is in legibleSizes. Legible
// rungs return the exact truth (CER 0); the rest return an all-substituted
// string (CER 1). It lets the ladder logic run without Tesseract.
type truthRecogniser struct {
	legibleSizes map[int]bool
}

func (r truthRecogniser) ScoreImage(_ image.Image, _ int, truth string) (Result, error) {
	var sz int
	if _, err := fmt.Sscanf(truth, "Text Size %dpx", &sz); err != nil {
		return Result{}, err
	}
	if r.legibleSizes[sz] {
		return Score(truth, truth), nil
	}
	bad := make([]rune, len([]rune(truth)))
	for i := range bad {
		bad[i] = '#'
	}
	return Score(truth, string(bad)), nil
}

func TestLadderText(t *testing.T) {
	if got := LadderText(16); got != "Text Size 16px" {
		t.Errorf("LadderText(16) = %q, want %q", got, "Text Size 16px")
	}
}

func TestSpecRungsGeometry(t *testing.T) {
	src := testImage(200, 100)
	spec := Spec{
		Sizes:     []int{8, 12, 16},
		Left:      4,
		Width:     120,
		Top:       2,
		RowHeight: 10,
	}
	rungs := spec.Rungs(src)
	if len(rungs) != 3 {
		t.Fatalf("got %d rungs, want 3", len(rungs))
	}
	b0 := rungs[0].Crop.Bounds()
	if b0.Min.X != 4 || b0.Max.X != 124 || b0.Min.Y != 2 || b0.Max.Y != 12 {
		t.Errorf("rung 0 crop = %v, want (4,2)-(124,12)", b0)
	}
	b2 := rungs[2].Crop.Bounds()
	if b2.Min.Y != 22 || b2.Max.Y != 32 {
		t.Errorf("rung 2 crop y = [%d,%d), want [22,32)", b2.Min.Y, b2.Max.Y)
	}
	if rungs[1].Truth != "Text Size 12px" {
		t.Errorf("rung 1 truth = %q, want %q", rungs[1].Truth, "Text Size 12px")
	}
	if rungs[0].SrcCapHeight != 8 {
		t.Errorf("rung 0 cap height = %d, want 8 (size fallback)", rungs[0].SrcCapHeight)
	}
}

func TestSpecRungsZeroWidthSpansRightEdge(t *testing.T) {
	src := testImage(200, 100)
	spec := Spec{Sizes: []int{10}, Left: 30, Top: 0, RowHeight: 20}
	r := spec.Rungs(src)[0].Crop.Bounds()
	if r.Min.X != 30 || r.Max.X != 200 {
		t.Errorf("zero-width crop x = [%d,%d), want [30,200)", r.Min.X, r.Max.X)
	}
}

func TestSpecCapHeightFor(t *testing.T) {
	spec := Spec{
		Sizes:        []int{8, 16},
		RowHeight:    10,
		CapHeightFor: func(sz int) int { return sz * 2 },
	}
	rungs := spec.Rungs(testImage(100, 100))
	if rungs[0].SrcCapHeight != 16 || rungs[1].SrcCapHeight != 32 {
		t.Errorf("cap heights = %d,%d, want 16,32", rungs[0].SrcCapHeight, rungs[1].SrcCapHeight)
	}
}

func TestSpecRungsDefaultSizes(t *testing.T) {
	spec := Spec{RowHeight: 1}
	rungs := spec.Rungs(testImage(50, 50))
	if len(rungs) != len(LadderSizes) {
		t.Fatalf("default rung count = %d, want %d", len(rungs), len(LadderSizes))
	}
}

func TestLadderAndMinLegibleSize(t *testing.T) {
	src := testImage(200, 100)
	spec := Spec{Sizes: []int{8, 12, 16}, Left: 0, Top: 0, RowHeight: 10}
	rec := truthRecogniser{legibleSizes: map[int]bool{12: true, 16: true}}
	results, err := Ladder(src, spec, rec, 0.1)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0].Legible || !results[1].Legible || !results[2].Legible {
		t.Errorf("legibility = %v,%v,%v, want false,true,true",
			results[0].Legible, results[1].Legible, results[2].Legible)
	}
	min, ok := MinLegibleSize(results)
	if !ok || min != 12 {
		t.Errorf("MinLegibleSize = %d,%v, want 12,true", min, ok)
	}
}

func TestLadderStableAcrossRuns(t *testing.T) {
	src := testImage(200, 100)
	spec := Spec{Sizes: []int{8, 12, 16}, RowHeight: 10}
	rec := truthRecogniser{legibleSizes: map[int]bool{12: true, 16: true}}
	first, err := Ladder(src, spec, rec, 0.1)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	for i := range 3 {
		got, err := Ladder(src, spec, rec, 0.1)
		if err != nil {
			t.Fatalf("repeat Ladder %d: %v", i, err)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Errorf("run %d rung %d differs: %+v != %+v", i, j, got[j], first[j])
			}
		}
		m1, _ := MinLegibleSize(first)
		m2, _ := MinLegibleSize(got)
		if m1 != m2 {
			t.Errorf("run %d min legible size %d != %d", i, m2, m1)
		}
	}
}

func TestMinLegibleSizeNone(t *testing.T) {
	results := []LadderResult{
		{SizePx: 8, Legible: false},
		{SizePx: 16, Legible: false},
	}
	if _, ok := MinLegibleSize(results); ok {
		t.Error("MinLegibleSize reported a size with no legible rung")
	}
}

func TestMinLegibleSizeUnsorted(t *testing.T) {
	results := []LadderResult{
		{SizePx: 24, Legible: true},
		{SizePx: 12, Legible: true},
		{SizePx: 16, Legible: true},
	}
	if min, ok := MinLegibleSize(results); !ok || min != 12 {
		t.Errorf("MinLegibleSize over unsorted = %d,%v, want 12,true", min, ok)
	}
}

func TestSortBySize(t *testing.T) {
	in := []LadderResult{{SizePx: 16}, {SizePx: 8}, {SizePx: 12}}
	out := SortBySize(in)
	if out[0].SizePx != 8 || out[1].SizePx != 12 || out[2].SizePx != 16 {
		t.Errorf("SortBySize = %d,%d,%d, want 8,12,16",
			out[0].SizePx, out[1].SizePx, out[2].SizePx)
	}
	if in[0].SizePx != 16 {
		t.Error("SortBySize mutated its input")
	}
}
