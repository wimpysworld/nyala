package ocr

import (
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// newTestRasteriser builds a Route A rasteriser or fails the test.
func newTestRasteriser(t *testing.T) *capture.Rasteriser {
	t.Helper()
	r, err := capture.NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	return r
}

// TestMatcherExactRoundTrip composes a grid with a known overlay run, rasterises
// it, recovers the text with the matcher, and asserts a zero CER.
func TestMatcherExactRoundTrip(t *testing.T) {
	rast := newTestRasteriser(t)
	truth := "Text Size 16px"

	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	br := block.Renderer{Cols: len([]rune(truth)) + 2, Rows: 1}
	g := br.Compose(render.Frame{
		Text: []render.GlyphRun{{Col: 0, Row: 0, Text: truth, FG: white}},
	})
	img := rast.Rasterise(g)

	m := NewMatcher(rast, []string{truth})
	got := m.Recover(img, g)
	if cer := CER(truth, got); cer != 0 {
		t.Fatalf("round-trip CER = %v (got %q, want %q)", cer, got, truth)
	}
}

// TestMatcherRecoversBothPolarities composes one row of dark-on-light cells and
// one row of light-on-dark cells with the same text, rasterises, and asserts both
// rows round-trip at CER 0. The adaptive Otsu split plus the polarity-invariant
// match recover the glyph whichever way the contrast runs.
func TestMatcherRecoversBothPolarities(t *testing.T) {
	rast := newTestRasteriser(t)
	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	black := color.RGBA{A: 0xFF}
	truth := "Text Size 16px"
	cols := len([]rune(truth)) + 2

	cases := []struct {
		name   string
		fg, bg color.RGBA
	}{
		{"light-on-dark", white, black},
		{"dark-on-light", black, white},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cells := make([]block.Cell, 0, cols)
			rs := []rune(truth)
			for i := range cols {
				if i < len(rs) {
					cells = append(cells, block.Cell{Text: string(rs[i]), FG: tc.fg, BG: tc.bg})
					continue
				}
				cells = append(cells, block.Cell{FG: tc.fg, BG: tc.bg})
			}
			g := block.Grid{Cols: cols, Rows: 1, Cells: cells}
			img := rast.Rasterise(g)

			m := NewMatcher(rast, []string{truth})
			got := m.Recover(img, g)
			if cer := CER(truth, got); cer != 0 {
				t.Fatalf("%s round-trip CER = %v (got %q, want %q)", tc.name, cer, got, truth)
			}
		})
	}
}

// TestMatcherCatchesBlockShowThrough leaves a cell as a block glyph instead of
// the overlay grapheme and asserts the matcher recovers the block glyph, not the
// expected letter. This is how a failed overlay shows up as a non-zero CER.
func TestMatcherCatchesBlockShowThrough(t *testing.T) {
	rast := newTestRasteriser(t)
	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	black := color.RGBA{A: 0xFF}

	// One row, two cells: a text "A" and a full-block cell standing in for a
	// place the overlay should have written but did not.
	g := block.Grid{
		Cols: 2,
		Rows: 1,
		Cells: []block.Cell{
			{Text: "A", FG: white, BG: black},
			{Glyph: '█', FG: white, BG: black, Text: "█"},
		},
	}
	img := rast.Rasterise(g)

	m := NewMatcher(rast, []string{"A"})
	got := m.Recover(img, g)
	if got == "" {
		t.Fatal("recovered empty text")
	}
	// The first cell recovers "A"; the second recovers the full block, not a
	// letter, so the join carries the block glyph.
	if []rune(got)[0] != 'A' {
		t.Errorf("first cell recovered %q, want A", string([]rune(got)[0]))
	}
	if []rune(got)[1] != '█' {
		t.Errorf("block-show-through cell recovered %q, want █", string([]rune(got)[1]))
	}
}
