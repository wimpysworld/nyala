package metrics

import (
	"math"

	"github.com/wimpysworld/nyala/internal/render"
)

// PlacementBox is one text run with the geometry the placement metric needs: the
// box bounds X, Y, W, H in CSS pixels, the binned start cell Col and Row, and the
// run's Text. It mirrors the per-frame text-map.json entry; the metric reads
// only these fields.
type PlacementBox struct {
	// X is the box left edge in CSS pixels, on-screen.
	X float64
	// Y is the box top edge in CSS pixels, on-screen.
	Y float64
	// W is the box width in CSS pixels.
	W float64
	// H is the box height in CSS pixels.
	H float64
	// Col is the binned start column, round(X/cellW). Page insets are arbitrary
	// sub-cell offsets, so round centres the residue at zero and stays
	// order-preserving, keeping the bins monotonic and non-overlapping.
	Col int
	// Row is the binned start row, round(Y/cellH). It mirrors Col on the vertical
	// axis.
	Row int
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
	// MeanAbsRowErr is the mean absolute row error in cells: the row the renderer
	// lands the run on (Row) versus where the box's top edge sits (Y/cellH). It
	// mirrors the right-edge error on the vertical axis.
	MeanAbsRowErr float64
	// Collisions counts pairs of runs on the same Row whose occupied cell ranges
	// [Col, Col+displayCells(Text)) intersect.
	Collisions int
	// OffGrid counts runs snapped past the grid's horizontal bounds or to a
	// negative coordinate: Col < 0, Row < 0, or Col at or beyond cols. Row at or
	// beyond rows is excluded, because the text map is document-absolute and
	// below-fold content is legitimate on a page taller than the viewport.
	OffGrid int
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

// Placement scores placement error for one frame's boxes at the given CSS-pixel
// cell size on a cols-by-rows grid. cellW and cellH are the fractional CSS-pixel
// cell so a scaled run (cell_px/Z) and the box geometry share units. For each box
// it counts the run's display cells with the renderer's width rule (a width-2
// cluster counts 2), compares that against the box's fractional width W/cellW,
// and measures how far the run's right edge misses the box's right edge. It also
// measures the row error (Row versus Y/cellH), counts same-row cell-overlap
// collisions, and counts runs snapped past the grid's horizontal bounds or to a
// negative coordinate (Col < 0, Row < 0, Col >= cols). Row >= rows is not
// off-grid: the text map is document-absolute, so below-fold content is
// legitimate on a page taller than the viewport. The off-grid check is skipped
// when cols or rows is non-positive, so an unknown grid never inflates the
// count. A non-positive cellW returns the zero value.
func Placement(boxes []PlacementBox, cellW, cellH float64, cols, rows int) PlacementStats {
	if cellW <= 0 {
		return PlacementStats{}
	}
	cw := cellW
	ch := cellH
	var stats PlacementStats
	if len(boxes) == 0 {
		return stats
	}
	stats.PerBox = make([]PlacementBoxError, len(boxes))
	var sumAbsEdge, sumAbsWidth, sumAbsRow float64
	gridKnown := cols > 0 && rows > 0
	for i, box := range boxes {
		boxCells := box.W / cw
		runCells := render.DisplayWidth(box.Text)
		widthErr := float64(runCells) - boxCells
		rightEdgeErr := float64(box.Col+runCells) - (box.X+box.W)/cw
		stats.PerBox[i] = PlacementBoxError{WidthErr: widthErr, RightEdgeErr: rightEdgeErr}
		absEdge := math.Abs(rightEdgeErr)
		sumAbsEdge += absEdge
		sumAbsWidth += math.Abs(widthErr)
		if ch > 0 {
			sumAbsRow += math.Abs(float64(box.Row) - box.Y/ch)
		}
		if absEdge > stats.MaxAbsRightEdgeErr {
			stats.MaxAbsRightEdgeErr = absEdge
		}
		if absEdge >= 1 {
			stats.DriftedBoxes++
		}
		if gridKnown && (box.Col < 0 || box.Row < 0 || box.Col >= cols) {
			stats.OffGrid++
		}
	}
	n := float64(len(boxes))
	stats.MeanAbsRightEdgeErr = sumAbsEdge / n
	stats.MeanAbsWidthErr = sumAbsWidth / n
	if ch > 0 {
		stats.MeanAbsRowErr = sumAbsRow / n
	}
	stats.Collisions = countCollisions(boxes)
	return stats
}

// countCollisions counts pairs of runs on the same Row whose occupied cell ranges
// [Col, Col+displayCells(Text)) intersect.
func countCollisions(boxes []PlacementBox) int {
	count := 0
	for i := range boxes {
		iStart := boxes[i].Col
		iEnd := iStart + render.DisplayWidth(boxes[i].Text)
		for j := i + 1; j < len(boxes); j++ {
			if boxes[j].Row != boxes[i].Row {
				continue
			}
			jStart := boxes[j].Col
			jEnd := jStart + render.DisplayWidth(boxes[j].Text)
			if iStart < jEnd && jStart < iEnd {
				count++
			}
		}
	}
	return count
}
