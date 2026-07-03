package metrics

import (
	"math"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

// PlacementBox is one text run with the geometry the placement metric needs: the
// box left X and width W in CSS pixels, the binned start column Col, and the
// run's Text. It mirrors the per-frame text-map.json entry; the metric reads
// only these fields.
type PlacementBox struct {
	// X is the box left edge in CSS pixels, on-screen.
	X float64
	// W is the box width in CSS pixels.
	W float64
	// Col is the binned start column, round(X/cellW). Page insets are arbitrary
	// sub-cell offsets, so round centres the residue at zero and stays
	// order-preserving, keeping the bins monotonic and non-overlapping.
	Col int
	// Text is the run's text, graphemes packed one per cell from Col.
	Text string
}

// PlacementStats aggregates horizontal placement error over one frame's boxes.
// rightEdgeErr is the cell gap between where the renderer lands a run's right
// edge (Col + runCells) and where the box's right edge sits ((X+W)/cellW). A
// proportional run wider per glyph than the box drifts its right edge rightward;
// the renderer ignores W, so the drift accumulates across a row.
type PlacementStats struct {
	// MeanAbsRightEdgeErr is the mean absolute right-edge error in cells.
	MeanAbsRightEdgeErr float64
	// MaxAbsRightEdgeErr is the max absolute right-edge error in cells.
	MaxAbsRightEdgeErr float64
	// MeanAbsWidthErr is the mean absolute width error in cells (runCells vs
	// boxCells).
	MeanAbsWidthErr float64
	// DriftedBoxes counts boxes whose absolute right-edge error is at least one
	// cell.
	DriftedBoxes int
	// PerBox holds the per-box errors in the input order.
	PerBox []PlacementBoxError
}

// PlacementBoxError holds one box's placement errors, all in cells. WidthErr is
// runCells minus boxCells; RightEdgeErr is the run right edge minus the box right
// edge.
type PlacementBoxError struct {
	// WidthErr is runCells minus the fractional box width in cells.
	WidthErr float64
	// RightEdgeErr is (Col+runCells) minus the box right edge in cells.
	RightEdgeErr float64
}

// Placement scores horizontal placement error for one frame's boxes at the given
// cell size. For each box it counts the run's display cells with the Stage 2
// width rule (a width-2 cluster counts 2), compares that against the box's
// fractional width W/cellW, and measures how far the run's right edge misses the
// box's right edge. cellH is taken for parity with the bundle's cell_px but does
// not enter the horizontal maths. A non-positive cellW returns the zero value.
func Placement(boxes []PlacementBox, cellW, cellH int) PlacementStats {
	_ = cellH
	if cellW <= 0 {
		return PlacementStats{}
	}
	cw := float64(cellW)
	var stats PlacementStats
	if len(boxes) == 0 {
		return stats
	}
	stats.PerBox = make([]PlacementBoxError, len(boxes))
	var sumAbsEdge, sumAbsWidth float64
	for i, box := range boxes {
		boxCells := box.W / cw
		runCells := displayCells(box.Text)
		widthErr := float64(runCells) - boxCells
		rightEdgeErr := float64(box.Col+runCells) - (box.X+box.W)/cw
		stats.PerBox[i] = PlacementBoxError{WidthErr: widthErr, RightEdgeErr: rightEdgeErr}
		absEdge := math.Abs(rightEdgeErr)
		sumAbsEdge += absEdge
		sumAbsWidth += math.Abs(widthErr)
		if absEdge > stats.MaxAbsRightEdgeErr {
			stats.MaxAbsRightEdgeErr = absEdge
		}
		if absEdge >= 1 {
			stats.DriftedBoxes++
		}
	}
	n := float64(len(boxes))
	stats.MeanAbsRightEdgeErr = sumAbsEdge / n
	stats.MeanAbsWidthErr = sumAbsWidth / n
	return stats
}

// displayCells counts the display cells a run occupies, summing each grapheme
// cluster under the Stage 2 width rule: a cluster of display width 2 or more
// counts 2, every other cluster counts 1. It mirrors the engine's cellWidth so
// the metric and the renderer agree on how many cells a run fills.
func displayCells(text string) int {
	total := 0
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		if runewidth.StringWidth(gr.Str()) >= 2 {
			total += 2
		} else {
			total++
		}
	}
	return total
}
