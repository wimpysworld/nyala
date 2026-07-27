package harness

import (
	"image/color"
	"path/filepath"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestLoadRunSet parses a committed run set and checks the run count, the first
// run's fields in RGBA order, its stacking rank, and that comment and blank lines
// are skipped.
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
	// The fixture ranks run 0..n-1 in file order, so the first run is front-most.
	if first.StackRank != 0 {
		t.Errorf("first run StackRank = %d, want 0 (front-most)", first.StackRank)
	}
}

// TestParseRunRejectsLegacyLine confirms the rank field is required: a
// four-field line in the old "col row fgHex text" format is an error, not a run
// whose text starts with the size.
func TestParseRunRejectsLegacyLine(t *testing.T) {
	for _, line := range []string{"4 2 000000 Text", "4 2 000000 Text Size 8px"} {
		if _, err := parseRun(line); err == nil {
			t.Errorf("parseRun(%q) = nil error, want an error (rank field missing)", line)
		}
	}
}

// TestParseRunReadsNumericLeadingLegacyLinePositionally documents the one legacy
// line the mandatory rank field cannot reject: a four-field
// "col row fgHex text" line whose text begins with an integer still holds five
// space-separated fields, so it parses with every column shifted one to the left.
// "10 5 FFFFFF 404 Not Found" was a run of "404 Not Found" and comes back as rank
// 404 holding "Not Found".
//
// The parser reads the five fields positionally and guesses nothing, which is
// exactly why the rank field is mandatory: sniffing whether the fourth field is a
// rank or the start of the text would be ambiguous on this line. Migrating an old
// fixture means adding the rank column by hand; nothing detects a missed one.
func TestParseRunReadsNumericLeadingLegacyLinePositionally(t *testing.T) {
	run, err := parseRun("10 5 FFFFFF 404 Not Found")
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}
	if run.StackRank != 404 {
		t.Errorf("StackRank = %d, want 404 (the fourth field is read as the rank, never sniffed)", run.StackRank)
	}
	if run.Text != "Not Found" {
		t.Errorf("Text = %q, want %q (the text starts at the fifth field)", run.Text, "Not Found")
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
