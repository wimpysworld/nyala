package engine

import (
	"math"

	"github.com/wimpysworld/nyala/internal/render"
)

// scrollMatchTolerancePx is the per-axis scroll-offset slack, in CSS pixels,
// within which the bitmap and the DOM snapshot count as the same page state. The
// bitmap and the text carry no shared frame id, so they are matched by their
// scroll offset: a synchronous snapshot taken right after a bitmap usually agrees
// to the pixel, but a scroll landing between the bitmap swap and the snapshot read
// drifts them apart. This is the one knob the post-build tuning spike measures, so
// it is a named constant, never a buried literal.
const scrollMatchTolerancePx = 1

// scrollOffset is one CSS-pixel scroll position, the axis pair compared by the
// frame sync. The bitmap pair comes from page.ScreencastFrameMetadata and the
// snapshot pair from domsnapshot.DocumentSnapshot.
type scrollOffset struct {
	X float64
	Y float64
}

// syncDecision is the pure frame-sync core: given the bitmap and snapshot scroll
// offsets, the page scale factor, the freshly decoded text runs, and the
// last-matched runs, it decides which runs to fold onto the frame and what the new
// last-matched set is. It takes no browser, so it is unit-testable on canned
// offset pairs.
//
// Both offsets are normalised by pageScaleFactor (a units or zoom skew otherwise
// fails the compare) and matched per axis within scrollMatchTolerancePx. Within
// tolerance the decoded runs are emitted and become the new last-matched. On
// divergence the page scrolled between the bitmap swap and the snapshot read, so
// the decoded runs disagree with the bitmap: the last-matched runs are held
// (emitted with the current bitmap) and stay the last-matched, or nil is emitted
// for a bitmap-only frame when no prior match exists. The next consumed frame
// re-syncs. No text buffer is kept; the latest-wins slot plus the synchronous
// capture collapses the two cadences into one.
func syncDecision(bitmap, snapshot scrollOffset, pageScaleFactor float64, decoded, lastMatched []render.GlyphRun) (emit, newLastMatched []render.GlyphRun) {
	if scrollOffsetsMatch(bitmap, snapshot, pageScaleFactor) {
		return decoded, decoded
	}
	return lastMatched, lastMatched
}

// errorHoldRuns decides which glyph runs to emit when the synchronous snapshot
// read fails. On an error it holds the last-matched runs so the overlay holds
// rather than flickers; with no error it emits the freshly decoded runs. It is
// pure, taking no browser, so it is unit-testable on a canned error and run sets.
func errorHoldRuns(snapshotErr error, decoded, lastMatched []render.GlyphRun) []render.GlyphRun {
	if snapshotErr != nil {
		return lastMatched
	}
	return decoded
}

// scrollOffsetsMatch reports whether the bitmap and snapshot scroll offsets agree
// within scrollMatchTolerancePx on both axes after normalising by
// pageScaleFactor. A non-positive scale factor is treated as 1 so a missing or
// zero PageScaleFactor compares the raw CSS-pixel offsets rather than dividing by
// zero.
func scrollOffsetsMatch(bitmap, snapshot scrollOffset, pageScaleFactor float64) bool {
	scale := pageScaleFactor
	if scale <= 0 {
		scale = 1
	}
	dx := math.Abs(bitmap.X/scale - snapshot.X/scale)
	dy := math.Abs(bitmap.Y/scale - snapshot.Y/scale)
	return dx <= scrollMatchTolerancePx && dy <= scrollMatchTolerancePx
}
