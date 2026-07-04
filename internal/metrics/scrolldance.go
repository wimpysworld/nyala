package metrics

import (
	"math"
	"strconv"
)

// ScrollRun is one text run in one frame, with the identity and placement the
// scroll-dance metric needs: the run's Text, its snapped Col and Row on the cell
// grid, and its box top-left X, Y in CSS pixels. It mirrors the per-frame
// text-map.json entry; the metric reads only these fields.
type ScrollRun struct {
	// Text is the run's text, the primary identity key across frames.
	Text string
	// Col is the snapped start column the frame placed the run at.
	Col int
	// Row is the snapped start row the frame placed the run at.
	Row int
	// X is the box left edge in CSS pixels, used with Text to identify the same
	// on-page run across frames; X is scroll-stable on a vertical scroll.
	X float64
	// Y is the box top edge in CSS pixels, on-screen (the frame's scroll already
	// subtracted). Adding the frame's own scroll offset recovers the scroll-invariant
	// document Y, which the scroll-compensated placement rounds to a document row.
	Y float64
}

// ScrollDanceStats aggregates overlay re-placement across a frame sequence. A run's
// placement is its column and a scroll-compensated document row. A run matched
// across frames that keeps one placement is glued; a run that takes two or more
// distinct placements has danced. The metric classifies each run against its own
// motion, not a single shared shift, so a page that mixes a position:fixed region
// (runs that stay at the viewport top on scroll) with scrolling body content scores
// each class honestly: a fixed run that holds its viewport cell is glued, and a body
// run that tracks the scroll is glued, without either being counted against the
// other's motion.
type ScrollDanceStats struct {
	// Runs is the number of distinct runs matched across the sequence.
	Runs int
	// FixedRuns is the number of matched runs classified as viewport-stationary: the
	// same viewport cell across frames whose page scroll offset differed. These are
	// position:fixed (or sticky) runs; they are glued by definition and never danced.
	FixedRuns int
	// DancedRuns is the number of matched runs that took two or more distinct
	// scroll-compensated placements. Fixed runs are excluded.
	DancedRuns int
	// Placements is the total distinct scroll-compensated placements summed over all
	// matched non-fixed runs. A glued run contributes 1; each extra placement adds 1.
	// So Placements minus (Runs minus FixedRuns) is the total re-placement count.
	Placements int
	// MaxPlacements is the largest distinct-placement count any single non-fixed run
	// took.
	MaxPlacements int
}

// ScrollDance measures overlay re-placement across a frame sequence. Each frame is
// one frame's runs in capture order, and frameScrollY is the page vertical scroll
// offset (CSS pixels) each frame was painted at, one per frame. cellH is the
// CSS-pixel cell height the rows were snapped with, used to convert a scroll-invariant
// document Y to a document row. It identifies the same on-page run across frames by
// Text and its box left edge X bucketed by bucketPx CSS pixels (X is scroll-stable on
// a vertical scroll, so a run keeps its identity as the page scrolls, while two runs
// with the same Text at different columns stay distinct).
//
// It classifies each matched run against its own motion rather than removing a single
// shared shift. A run that holds one viewport cell (Col, Row) across frames whose
// page scroll differed is viewport-stationary: a position:fixed or sticky run,
// counted in FixedRuns and never danced. Every other run is scored on its
// scroll-compensated placement, the column paired with the document row
// round((Y + frameScrollY)/cellH). Because Y already has the frame's scroll
// subtracted, Y + frameScrollY is the scroll-invariant document Y, so a run that
// tracks a clean scroll rounds to one document row across the frames and stays glued;
// a run that jumps to a different column or document row takes a second placement and
// dances. Rounding the continuous document Y once per frame avoids the one-row
// disagreement that rounding each frame's viewport row and then removing a shared
// shift produces on a fractional-cell scroll.
//
// Fewer than two frames, or a frameScrollY whose length does not match frames,
// yields the zero value: a single frame cannot re-place anything, and a mismatched
// scroll slice cannot be trusted to compensate. A non-positive bucketPx falls back to
// a one-CSS-pixel bucket; a non-positive cellH disables the document-row conversion
// and scores placements on the raw viewport row.
func ScrollDance(frames [][]ScrollRun, frameScrollY []float64, cellH, bucketPx float64) ScrollDanceStats {
	if len(frames) < 2 || len(frameScrollY) != len(frames) {
		return ScrollDanceStats{}
	}
	if bucketPx <= 0 {
		bucketPx = 1
	}
	indexed := make([]map[string]ScrollRun, len(frames))
	for i, runs := range frames {
		indexed[i] = indexRuns(runs, bucketPx)
	}
	// Collect each run identity's appearances across the frames it shows in, so the
	// classification reads the whole per-run trajectory rather than a neighbour median.
	// Ranging over frameScrollY (proven equal in length to frames above) keeps the
	// scroll read in bounds.
	seen := make(map[string][]scrollAppearance)
	for i, scrollY := range frameScrollY {
		for key, r := range indexed[i] {
			seen[key] = append(seen[key], scrollAppearance{run: r, scrollY: scrollY})
		}
	}
	var stats ScrollDanceStats
	for _, apps := range seen {
		if len(apps) < 2 {
			continue
		}
		stats.Runs++
		if viewportStationary(apps) {
			stats.FixedRuns++
			continue
		}
		n := countPlacements(apps, cellH)
		stats.Placements += n
		if n > 1 {
			stats.DancedRuns++
		}
		if n > stats.MaxPlacements {
			stats.MaxPlacements = n
		}
	}
	return stats
}

// scrollAppearance is one appearance of a run identity in the sequence: the run as a
// frame placed it, paired with the page scroll offset that frame was painted at. The
// per-run classification reads a run's list of appearances to decide fixed, glued, or
// danced.
type scrollAppearance struct {
	run     ScrollRun
	scrollY float64
}

// viewportStationary reports whether a run holds one viewport cell (Col, Row) across
// its appearances while the page scroll offset changed between at least two of them.
// A position:fixed or sticky run stays pinned to the viewport as the page scrolls, so
// its viewport cell is constant despite the differing scroll; this is the signature
// that marks it fixed and exempts it from the dance count. A run seen only at one
// scroll offset (no scroll happened while it was visible) is not classified fixed
// here, because its stillness carries no evidence of fixed positioning; the placement
// count then scores it, and a truly still run scores one placement and is glued
// anyway.
func viewportStationary(apps []scrollAppearance) bool {
	col, row := apps[0].run.Col, apps[0].run.Row
	scroll0 := apps[0].scrollY
	scrollMoved := false
	for _, a := range apps[1:] {
		if a.run.Col != col || a.run.Row != row {
			return false
		}
		if a.scrollY != scroll0 {
			scrollMoved = true
		}
	}
	return scrollMoved
}

// countPlacements returns the number of distinct scroll-compensated placements a run
// took across its appearances. A placement is the column paired with the document row
// round((Y + scrollY)/cellH): Y has the frame scroll subtracted, so Y + scrollY is the
// scroll-invariant document Y, and rounding it once per frame lands a clean scroll on
// one document row. A non-positive cellH scores on the raw viewport row instead, so
// the metric degrades to a plain column-and-row placement rather than dividing by a
// bad cell.
func countPlacements(apps []scrollAppearance, cellH float64) int {
	type placement struct {
		col, row int
	}
	set := make(map[placement]struct{}, len(apps))
	for _, a := range apps {
		row := a.run.Row
		if cellH > 0 {
			row = int(math.Round((a.run.Y + a.scrollY) / cellH))
		}
		set[placement{col: a.run.Col, row: row}] = struct{}{}
	}
	return len(set)
}

// indexRuns keys a frame's runs by Text and a horizontal position bucket
// (X bucketed by bucketPx CSS pixels). X is scroll-stable on a vertical scroll, so
// the key holds across frames as the page moves. When two runs collide on the same
// key (same Text at the same X bucket), the first in capture order wins, so a
// duplicated label does not split into two half-tracked identities. The bucket
// tolerates the sub-cell horizontal jitter a stable overlay leaves.
func indexRuns(runs []ScrollRun, bucketPx float64) map[string]ScrollRun {
	out := make(map[string]ScrollRun, len(runs))
	for _, r := range runs {
		k := keyString(r.Text, int(math.Round(r.X/bucketPx)))
		if _, ok := out[k]; ok {
			continue
		}
		out[k] = r
	}
	return out
}

// keyString packs a run's Text and X bucket into a comparable string for map use.
func keyString(text string, bx int) string {
	return text + "\x00" + strconv.Itoa(bx)
}
