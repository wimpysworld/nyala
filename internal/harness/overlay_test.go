package harness

import (
	"image/color"
	"path/filepath"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestLoadRunSet parses a committed run set and checks the run count, the first
// run's fields in RGBA order, and that comment and blank lines are skipped.
func TestLoadRunSet(t *testing.T) {
	runs, err := LoadRunSet(filepath.Join(OverlayDir(), "testpage-01-grayscale.runs"))
	if err != nil {
		t.Fatalf("LoadRunSet: %v", err)
	}
	if len(runs) != 4 {
		t.Fatalf("run count = %d, want 4 (comment and blank lines skipped)", len(runs))
	}
	first := runs[0]
	if first.Col != 4 || first.Row != 2 {
		t.Errorf("first run position = (%d,%d), want (4,2)", first.Col, first.Row)
	}
	if first.Text != "Text Size 8px" {
		t.Errorf("first run text = %q, want %q", first.Text, "Text Size 8px")
	}
	// The grayscale page is light, so the first run's foreground is black: the
	// run colours contrast with their local cell background.
	wantFG := color.RGBA{A: 0xFF}
	if first.FG != wantFG {
		t.Errorf("first run FG = %+v, want %+v (RGBA order)", first.FG, wantFG)
	}
}

// TestLowContrastTextCells counts a low-contrast and a high-contrast text cell,
// and confirms bitmap and wide cells are ignored.
func TestLowContrastTextCells(t *testing.T) {
	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	black := color.RGBA{A: 0xFF}
	g := block.Grid{
		Cols: 4,
		Rows: 1,
		Cells: []block.Cell{
			{Text: "A", FG: white, BG: white},  // low contrast (1:1)
			{Text: "B", FG: white, BG: black},  // high contrast (21:1)
			{Glyph: '#', FG: white, BG: white}, // bitmap cell, ignored
			{Wide: true, FG: white, BG: white}, // wide trailing cell, ignored
		},
	}
	if got := LowContrastTextCells(g); got != 1 {
		t.Errorf("LowContrastTextCells = %d, want 1", got)
	}
}
