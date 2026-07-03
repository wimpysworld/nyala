package metrics

import (
	"math"
	"testing"
)

const placeEps = 1e-9

// TestPlacementAligned checks a run that exactly fills its box scores near zero
// error: width 5 cells, text "hello", X on a cell boundary.
func TestPlacementAligned(t *testing.T) {
	boxes := []PlacementBox{
		{X: 0, W: 40, Col: 0, Text: "hello"}, // 5 graphemes, 5 cells, W/8 = 5
	}
	got := Placement(boxes, 8, 16, 0, 0)
	if math.Abs(got.MeanAbsRightEdgeErr) > placeEps {
		t.Errorf("MeanAbsRightEdgeErr = %v, want ~0", got.MeanAbsRightEdgeErr)
	}
	if math.Abs(got.MeanAbsWidthErr) > placeEps {
		t.Errorf("MeanAbsWidthErr = %v, want ~0", got.MeanAbsWidthErr)
	}
	if got.DriftedBoxes != 0 {
		t.Errorf("DriftedBoxes = %d, want 0", got.DriftedBoxes)
	}
	if math.Abs(got.PerBox[0].WidthErr) > placeEps {
		t.Errorf("WidthErr = %v, want ~0", got.PerBox[0].WidthErr)
	}
	if math.Abs(got.PerBox[0].RightEdgeErr) > placeEps {
		t.Errorf("RightEdgeErr = %v, want ~0", got.PerBox[0].RightEdgeErr)
	}
}

// TestPlacementNarrowRun checks a run narrower per glyph than its box: 8 cells of
// text against a box 16 cells wide. The run spans fewer cells, so widthErr is
// negative and the right edge falls short.
func TestPlacementNarrowRun(t *testing.T) {
	boxes := []PlacementBox{
		{X: 0, W: 128, Col: 0, Text: "abcdefgh"}, // 8 cells, boxCells = 16
	}
	got := Placement(boxes, 8, 16, 0, 0)
	// runCells 8 - boxCells 16 = -8
	if math.Abs(got.PerBox[0].WidthErr+8) > placeEps {
		t.Errorf("WidthErr = %v, want -8", got.PerBox[0].WidthErr)
	}
	// right edge: (0+8) - 128/8 = 8 - 16 = -8
	if math.Abs(got.PerBox[0].RightEdgeErr+8) > placeEps {
		t.Errorf("RightEdgeErr = %v, want -8", got.PerBox[0].RightEdgeErr)
	}
	if math.Abs(got.MaxAbsRightEdgeErr-8) > placeEps {
		t.Errorf("MaxAbsRightEdgeErr = %v, want 8", got.MaxAbsRightEdgeErr)
	}
	if got.DriftedBoxes != 1 {
		t.Errorf("DriftedBoxes = %d, want 1", got.DriftedBoxes)
	}
}

// TestPlacementWideCluster checks a width-2 cluster counts two cells. A single
// CJK ideograph in a box one cell wide spans two cells, so widthErr is +1 and the
// right edge overshoots by one cell.
func TestPlacementWideCluster(t *testing.T) {
	boxes := []PlacementBox{
		{X: 0, W: 8, Col: 0, Text: "中"}, // width-2 cluster, boxCells = 1
	}
	got := Placement(boxes, 8, 16, 0, 0)
	// runCells 2 - boxCells 1 = 1
	if math.Abs(got.PerBox[0].WidthErr-1) > placeEps {
		t.Errorf("WidthErr = %v, want 1", got.PerBox[0].WidthErr)
	}
	// right edge: (0+2) - 8/8 = 2 - 1 = 1
	if math.Abs(got.PerBox[0].RightEdgeErr-1) > placeEps {
		t.Errorf("RightEdgeErr = %v, want 1", got.PerBox[0].RightEdgeErr)
	}
	if got.DriftedBoxes != 1 {
		t.Errorf("DriftedBoxes = %d, want 1", got.DriftedBoxes)
	}
}

// TestPlacementRowError checks the row-error term: a run landed on the wrong row
// scores nonzero MeanAbsRowErr, and a run landed on its own row scores near zero.
func TestPlacementRowError(t *testing.T) {
	// Box top Y=16, cellH=16, so Y/cellH = 1. Row=3 misses by 2 cells.
	wrong := []PlacementBox{
		{X: 0, Y: 16, W: 40, Col: 0, Row: 3, Text: "hello"},
	}
	got := Placement(wrong, 8, 16, 0, 0)
	if math.Abs(got.MeanAbsRowErr-2) > placeEps {
		t.Errorf("MeanAbsRowErr = %v, want 2", got.MeanAbsRowErr)
	}
	// Row=1 matches Y/cellH = 1, so no row error.
	aligned := []PlacementBox{
		{X: 0, Y: 16, W: 40, Col: 0, Row: 1, Text: "hello"},
	}
	got = Placement(aligned, 8, 16, 0, 0)
	if math.Abs(got.MeanAbsRowErr) > placeEps {
		t.Errorf("MeanAbsRowErr = %v, want ~0", got.MeanAbsRowErr)
	}
}

// TestPlacementCollisions checks the same-row cell-overlap count: two runs sharing
// a cell on one row collide, non-overlapping same-row runs do not, and identical
// columns on different rows do not.
func TestPlacementCollisions(t *testing.T) {
	// "hello" at Col 0 spans cells [0,5); "world" at Col 3 spans [3,8). Overlap.
	overlap := []PlacementBox{
		{X: 0, W: 40, Col: 0, Row: 0, Text: "hello"},
		{X: 24, W: 40, Col: 3, Row: 0, Text: "world"},
	}
	if got := Placement(overlap, 8, 16, 0, 0); got.Collisions != 1 {
		t.Errorf("Collisions = %d, want 1", got.Collisions)
	}
	// "hello" spans [0,5); "world" at Col 5 spans [5,10). Touch but do not overlap.
	adjacent := []PlacementBox{
		{X: 0, W: 40, Col: 0, Row: 0, Text: "hello"},
		{X: 40, W: 40, Col: 5, Row: 0, Text: "world"},
	}
	if got := Placement(adjacent, 8, 16, 0, 0); got.Collisions != 0 {
		t.Errorf("Collisions = %d, want 0 (adjacent, same row)", got.Collisions)
	}
	// Same columns but different rows never collide.
	stacked := []PlacementBox{
		{X: 0, Y: 0, W: 40, Col: 0, Row: 0, Text: "hello"},
		{X: 0, Y: 16, W: 40, Col: 0, Row: 1, Text: "world"},
	}
	if got := Placement(stacked, 8, 16, 0, 0); got.Collisions != 0 {
		t.Errorf("Collisions = %d, want 0 (different rows)", got.Collisions)
	}
	// "中" at Col 0 is width-2, so it spans cells [0,2); "a" at Col 1 spans [1,2).
	// The width-1 run lands inside the wide cluster's trailing cell, so they collide.
	wideOverlap := []PlacementBox{
		{X: 0, W: 8, Col: 0, Row: 0, Text: "中"},
		{X: 8, W: 8, Col: 1, Row: 0, Text: "a"},
	}
	if got := Placement(wideOverlap, 8, 16, 0, 0); got.Collisions != 1 {
		t.Errorf("Collisions = %d, want 1 (width-2 cluster overlaps Col 1)", got.Collisions)
	}
	// The same wide cluster spans [0,2); "a" at Col 2 spans [2,3). They touch but
	// do not overlap, so no collision.
	wideAdjacent := []PlacementBox{
		{X: 0, W: 8, Col: 0, Row: 0, Text: "中"},
		{X: 16, W: 8, Col: 2, Row: 0, Text: "a"},
	}
	if got := Placement(wideAdjacent, 8, 16, 0, 0); got.Collisions != 0 {
		t.Errorf("Collisions = %d, want 0 (width-2 cluster adjacent to Col 2)", got.Collisions)
	}
}

// TestPlacementOffGrid checks the off-grid count with a positive grid, the skip
// guard when cols or rows is non-positive, and that below-fold content (Row past
// rows, in-range Col) does not count, because the text map is document-absolute.
func TestPlacementOffGrid(t *testing.T) {
	// Col 12 lands past a 10-column grid; Row 2 is in range.
	boxes := []PlacementBox{
		{X: 96, Y: 32, W: 40, Col: 12, Row: 2, Text: "hello"},
	}
	if got := Placement(boxes, 8, 16, 10, 4); got.OffGrid != 1 {
		t.Errorf("OffGrid = %d, want 1", got.OffGrid)
	}
	// The same boxes with an unknown grid never inflate the count.
	if got := Placement(boxes, 8, 16, 0, 0); got.OffGrid != 0 {
		t.Errorf("OffGrid = %d, want 0 (grid unknown)", got.OffGrid)
	}
	// A below-fold box (Row past the 4-row grid, in-range Col) is legitimate
	// document-absolute content, so it does not count as off-grid.
	belowFold := []PlacementBox{
		{X: 0, Y: 96, W: 40, Col: 0, Row: 6, Text: "hello"},
	}
	if got := Placement(belowFold, 8, 16, 10, 4); got.OffGrid != 0 {
		t.Errorf("OffGrid = %d, want 0 (below-fold content)", got.OffGrid)
	}
}

// TestPlacementEmpty checks no boxes yields the zero value with no division by
// zero.
func TestPlacementEmpty(t *testing.T) {
	got := Placement(nil, 8, 16, 0, 0)
	if got.MeanAbsRightEdgeErr != 0 || got.MaxAbsRightEdgeErr != 0 || got.DriftedBoxes != 0 {
		t.Errorf("empty input = %+v, want zero", got)
	}
}

// TestPlacementBadCellW checks a non-positive cell width returns the zero value.
func TestPlacementBadCellW(t *testing.T) {
	boxes := []PlacementBox{{X: 0, W: 8, Col: 0, Text: "a"}}
	if got := Placement(boxes, 0, 16, 0, 0); len(got.PerBox) != 0 {
		t.Errorf("cellW=0 returned %+v, want zero", got)
	}
}
