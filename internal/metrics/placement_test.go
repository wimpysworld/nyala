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
	got := Placement(boxes, 8, 16)
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
	got := Placement(boxes, 8, 16)
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
	got := Placement(boxes, 8, 16)
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

// TestPlacementEmpty checks no boxes yields the zero value with no division by
// zero.
func TestPlacementEmpty(t *testing.T) {
	got := Placement(nil, 8, 16)
	if got.MeanAbsRightEdgeErr != 0 || got.MaxAbsRightEdgeErr != 0 || got.DriftedBoxes != 0 {
		t.Errorf("empty input = %+v, want zero", got)
	}
}

// TestPlacementBadCellW checks a non-positive cell width returns the zero value.
func TestPlacementBadCellW(t *testing.T) {
	boxes := []PlacementBox{{X: 0, W: 8, Col: 0, Text: "a"}}
	if got := Placement(boxes, 0, 16); len(got.PerBox) != 0 {
		t.Errorf("cellW=0 returned %+v, want zero", got)
	}
}
