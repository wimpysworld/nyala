package metrics

import "testing"

// cellH is the CSS-pixel cell height the metric tests snap document rows with. It
// matches the 16 px Y step the fixtures use, so a run's Y divides to a whole row.
const testCellH = 16

// TestScrollDanceSingleFrame checks a sequence shorter than two frames scores the
// zero value, because a single frame cannot re-place anything.
func TestScrollDanceSingleFrame(t *testing.T) {
	frames := [][]ScrollRun{
		{{Text: "hello", Col: 0, Row: 0, X: 0, Y: 0}},
	}
	got := ScrollDance(frames, []float64{0}, testCellH, 4)
	if got != (ScrollDanceStats{}) {
		t.Errorf("ScrollDance single frame = %+v, want zero value", got)
	}
}

// TestScrollDanceScrollLenMismatch checks a frameScrollY whose length does not match
// the frame count scores the zero value: the scroll compensation cannot be trusted.
func TestScrollDanceScrollLenMismatch(t *testing.T) {
	frame := []ScrollRun{{Text: "x", Col: 0, Row: 0, X: 0, Y: 0}}
	got := ScrollDance([][]ScrollRun{frame, frame}, []float64{0}, testCellH, 4)
	if got != (ScrollDanceStats{}) {
		t.Errorf("ScrollDance scroll mismatch = %+v, want zero value", got)
	}
}

// TestScrollDanceStableOverlay checks an overlay that holds every run at the same
// cell across two frames at the same scroll scores zero danced runs and zero fixed
// runs: with no scroll between the frames there is no evidence of fixed positioning,
// so each still run scores one placement and is glued.
func TestScrollDanceStableOverlay(t *testing.T) {
	frame := []ScrollRun{
		{Text: "title", Col: 2, Row: 3, X: 16, Y: 48},
		{Text: "date", Col: 40, Row: 3, X: 320, Y: 48},
	}
	got := ScrollDance([][]ScrollRun{frame, frame}, []float64{0, 0}, testCellH, 4)
	if got.Runs != 2 {
		t.Errorf("Runs = %d, want 2", got.Runs)
	}
	if got.FixedRuns != 0 {
		t.Errorf("FixedRuns = %d, want 0 (no scroll, so no fixed evidence)", got.FixedRuns)
	}
	if got.DancedRuns != 0 {
		t.Errorf("DancedRuns = %d, want 0", got.DancedRuns)
	}
	if got.MaxPlacements != 1 {
		t.Errorf("MaxPlacements = %d, want 1", got.MaxPlacements)
	}
}

// TestScrollDanceCleanScroll checks a clean scroll where every run tracks the page
// scores zero dances: the document Y (viewport Y plus the frame scroll) is constant
// per run, so each rounds to one document row and stays glued.
func TestScrollDanceCleanScroll(t *testing.T) {
	first := []ScrollRun{
		{Text: "title", Col: 2, Row: 10, X: 16, Y: 160},
		{Text: "date", Col: 40, Row: 10, X: 320, Y: 160},
		{Text: "body", Col: 2, Row: 14, X: 16, Y: 224},
	}
	// The page scrolled up by 80 CSS px (5 rows at cell 16): every run's viewport Y
	// drops by 80, X holds, and the document Y (Y + 80) is unchanged.
	second := []ScrollRun{
		{Text: "title", Col: 2, Row: 5, X: 16, Y: 80},
		{Text: "date", Col: 40, Row: 5, X: 320, Y: 80},
		{Text: "body", Col: 2, Row: 9, X: 16, Y: 144},
	}
	got := ScrollDance([][]ScrollRun{first, second}, []float64{0, 80}, testCellH, 4)
	if got.Runs != 3 {
		t.Errorf("Runs = %d, want 3", got.Runs)
	}
	if got.DancedRuns != 0 {
		t.Errorf("DancedRuns = %d, want 0 (clean scroll), got stats %+v", got.DancedRuns, got)
	}
}

// TestScrollDanceFractionalScroll is the honesty regression: a fractional-cell scroll
// must not read a run as danced just because its viewport row rounds one cell off the
// scroll's whole-cell median. The page scrolled 88 CSS px (5.5 cells). Two runs land
// 5 viewport rows up, one lands 6, but every run's document Y is constant, so all are
// glued. The old median-shift metric counted the 6-row run against the 5-row median.
func TestScrollDanceFractionalScroll(t *testing.T) {
	first := []ScrollRun{
		{Text: "a", Col: 2, Row: 10, X: 16, Y: 168}, // docY 168
		{Text: "b", Col: 4, Row: 10, X: 40, Y: 164}, // docY 164
		{Text: "c", Col: 6, Row: 12, X: 64, Y: 200}, // docY 200
	}
	// Scroll up 88 px. docY holds; viewport rows: a 168-88=80 -> 5, b 164-88=76 -> 5
	// (round 4.75), c 200-88=112 -> 7. The raw viewport-row deltas are -5, -5, -5 here,
	// but the document rows round(docY/16) are 11, 10, 13 in both frames, so all glued.
	second := []ScrollRun{
		{Text: "a", Col: 2, Row: 5, X: 16, Y: 80},
		{Text: "b", Col: 4, Row: 5, X: 40, Y: 76},
		{Text: "c", Col: 6, Row: 7, X: 64, Y: 112},
	}
	got := ScrollDance([][]ScrollRun{first, second}, []float64{0, 88}, testCellH, 4)
	if got.DancedRuns != 0 {
		t.Errorf("DancedRuns = %d, want 0 (fractional scroll, docY stable), got stats %+v", got.DancedRuns, got)
	}
}

// TestScrollDanceFixedNav checks a position:fixed nav run that holds its viewport cell
// as the page scrolls under it is classified fixed, not danced: its viewport (Col,
// Row) is constant while the frame scroll changed. A single global shift would read it
// as danced against the body's scroll.
func TestScrollDanceFixedNav(t *testing.T) {
	first := []ScrollRun{
		{Text: "nav", Col: 2, Row: 1, X: 16, Y: 16},    // fixed, viewport top
		{Text: "body", Col: 2, Row: 20, X: 16, Y: 320}, // scrolls with the page
	}
	// Scroll up 160 px (10 rows). The nav stays at viewport row 1; the body moves up 10.
	second := []ScrollRun{
		{Text: "nav", Col: 2, Row: 1, X: 16, Y: 16},
		{Text: "body", Col: 2, Row: 10, X: 16, Y: 160},
	}
	got := ScrollDance([][]ScrollRun{first, second}, []float64{0, 160}, testCellH, 4)
	if got.Runs != 2 {
		t.Errorf("Runs = %d, want 2", got.Runs)
	}
	if got.FixedRuns != 1 {
		t.Errorf("FixedRuns = %d, want 1 (the fixed nav), got stats %+v", got.FixedRuns, got)
	}
	if got.DancedRuns != 0 {
		t.Errorf("DancedRuns = %d, want 0 (fixed nav is glued, body tracks scroll), got stats %+v", got.DancedRuns, got)
	}
}

// TestScrollDanceOneRunDances checks a run that jumps to a different document row is
// counted, while the runs that track the scroll are not. title and body track a clean
// 5-row scroll; badge lands one document row off in the second frame, so only it
// dances.
func TestScrollDanceOneRunDances(t *testing.T) {
	first := []ScrollRun{
		{Text: "title", Col: 2, Row: 10, X: 16, Y: 160},
		{Text: "date", Col: 40, Row: 10, X: 320, Y: 160},
		{Text: "badge", Col: 60, Row: 10, X: 480, Y: 160},
	}
	// Scroll up 80 px (5 rows). title and date track it (docY constant). badge's
	// viewport Y drops by 96, so its document Y jumps 16 px (one row): it dances.
	second := []ScrollRun{
		{Text: "title", Col: 2, Row: 5, X: 16, Y: 80},
		{Text: "date", Col: 40, Row: 5, X: 320, Y: 80},
		{Text: "badge", Col: 60, Row: 4, X: 480, Y: 64},
	}
	got := ScrollDance([][]ScrollRun{first, second}, []float64{0, 80}, testCellH, 4)
	if got.DancedRuns != 1 {
		t.Errorf("DancedRuns = %d, want 1, got stats %+v", got.DancedRuns, got)
	}
	if got.MaxPlacements != 2 {
		t.Errorf("MaxPlacements = %d, want 2", got.MaxPlacements)
	}
	// Placements: title 1 + date 1 + badge 2 = 4; non-fixed Runs 3; re-placement 1.
	if got.Placements != 4 {
		t.Errorf("Placements = %d, want 4", got.Placements)
	}
}

// TestScrollDanceColumnDance checks a run that changes only its Col (a horizontal
// re-placement with no scroll) is counted: the document row is stable, and the run
// takes two distinct columns.
func TestScrollDanceColumnDance(t *testing.T) {
	first := []ScrollRun{
		{Text: "code", Col: 50, Row: 4, X: 400, Y: 64},
		{Text: "anchor", Col: 2, Row: 4, X: 16, Y: 64},
	}
	second := []ScrollRun{
		{Text: "code", Col: 52, Row: 4, X: 400, Y: 64}, // same X bucket, drifted Col
		{Text: "anchor", Col: 2, Row: 4, X: 16, Y: 64},
	}
	got := ScrollDance([][]ScrollRun{first, second}, []float64{0, 0}, testCellH, 4)
	if got.DancedRuns != 1 {
		t.Errorf("DancedRuns = %d, want 1, got stats %+v", got.DancedRuns, got)
	}
}

// TestScrollDanceSameTextDistinct checks two runs with the same Text at different
// columns stay distinct identities: the X bucket keeps them apart, so a stable pair
// scores no dance.
func TestScrollDanceSameTextDistinct(t *testing.T) {
	frame := []ScrollRun{
		{Text: "12:00", Col: 2, Row: 4, X: 16, Y: 64},
		{Text: "12:00", Col: 60, Row: 4, X: 480, Y: 64},
	}
	got := ScrollDance([][]ScrollRun{frame, frame}, []float64{0, 0}, testCellH, 4)
	if got.Runs != 2 {
		t.Errorf("Runs = %d, want 2 (same text, distinct X)", got.Runs)
	}
	if got.DancedRuns != 0 {
		t.Errorf("DancedRuns = %d, want 0", got.DancedRuns)
	}
}

// TestScrollDanceBucketFallback checks a non-positive bucket falls back to a
// one-pixel bucket rather than dividing by zero.
func TestScrollDanceBucketFallback(t *testing.T) {
	frame := []ScrollRun{{Text: "x", Col: 0, Row: 0, X: 3, Y: 0}}
	got := ScrollDance([][]ScrollRun{frame, frame}, []float64{0, 0}, testCellH, 0)
	if got.Runs != 1 || got.DancedRuns != 0 {
		t.Errorf("bucket fallback = %+v, want Runs 1, DancedRuns 0", got)
	}
}
