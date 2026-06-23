package engine

import (
	"errors"
	"image/color"
	"reflect"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// errSnapshot is the canned snapshot-read failure the errorHoldRuns tests feed,
// standing in for a real DOMSnapshot.captureSnapshot error. No browser involved.
var errSnapshot = errors.New("snapshot read failed")

// decodedRuns is the freshly decoded text, the "current snapshot" set. The hold
// case must never emit this set.
var decodedRuns = []render.GlyphRun{
	{Col: 0, Row: 0, Text: "now", FG: color.RGBA{R: 255, A: 255}},
}

// priorRuns is the last-matched text from an earlier in-sync frame. The hold
// case emits this set.
var priorRuns = []render.GlyphRun{
	{Col: 1, Row: 2, Text: "before", FG: color.RGBA{G: 255, A: 255}},
}

func TestSyncDecisionEqualOffsetsFoldsAndEmitsDecoded(t *testing.T) {
	off := scrollOffset{X: 100, Y: 200}
	emit, newLast := syncDecision(off, off, 1, decodedRuns, priorRuns)

	if !reflect.DeepEqual(emit, decodedRuns) {
		t.Errorf("emit = %v, want decoded runs %v", emit, decodedRuns)
	}
	if !reflect.DeepEqual(newLast, decodedRuns) {
		t.Errorf("new last-matched = %v, want decoded runs %v", newLast, decodedRuns)
	}
}

func TestErrorHoldRunsHoldsLastMatchedOnError(t *testing.T) {
	// A non-nil snapshot error holds the prior matched runs, never the freshly
	// decoded set, so the overlay holds rather than flickers.
	got := errorHoldRuns(errSnapshot, decodedRuns, priorRuns)
	if reflect.DeepEqual(got, decodedRuns) {
		t.Errorf("got = %v, must not be the decoded runs on error", got)
	}
	if !reflect.DeepEqual(got, priorRuns) {
		t.Errorf("got = %v, want held prior runs %v", got, priorRuns)
	}
}

func TestErrorHoldRunsEmitsDecodedOnNilError(t *testing.T) {
	// No error: emit the freshly decoded runs.
	got := errorHoldRuns(nil, decodedRuns, priorRuns)
	if !reflect.DeepEqual(got, decodedRuns) {
		t.Errorf("got = %v, want decoded runs %v", got, decodedRuns)
	}
}

func TestErrorHoldRunsNilPriorOnErrorEmitsNil(t *testing.T) {
	// Error with no prior match: emit nil for a bitmap-only frame.
	got := errorHoldRuns(errSnapshot, decodedRuns, nil)
	if len(got) != 0 {
		t.Errorf("got = %v, want nil/empty for a bitmap-only frame", got)
	}
}

func TestSyncDecisionWithinToleranceFoldsAndEmitsDecoded(t *testing.T) {
	// Apart by exactly the tolerance on each axis: still a match.
	bitmap := scrollOffset{X: 100, Y: 200}
	snapshot := scrollOffset{X: 100 + scrollMatchTolerancePx, Y: 200 + scrollMatchTolerancePx}
	emit, newLast := syncDecision(bitmap, snapshot, 1, decodedRuns, priorRuns)

	if !reflect.DeepEqual(emit, decodedRuns) {
		t.Errorf("emit = %v, want decoded runs %v", emit, decodedRuns)
	}
	if !reflect.DeepEqual(newLast, decodedRuns) {
		t.Errorf("new last-matched = %v, want decoded runs %v", newLast, decodedRuns)
	}
}

func TestSyncDecisionDivergenceHoldsPriorMatched(t *testing.T) {
	// Apart by more than the tolerance: hold the prior matched text.
	bitmap := scrollOffset{X: 100, Y: 200}
	snapshot := scrollOffset{X: 100, Y: 200 + scrollMatchTolerancePx + 1}
	emit, newLast := syncDecision(bitmap, snapshot, 1, decodedRuns, priorRuns)

	// The plan flags this: the held text is the prior matched value, never the
	// current snapshot's decoded runs.
	if reflect.DeepEqual(emit, decodedRuns) {
		t.Errorf("emit = %v, must not be the current snapshot's decoded runs", emit)
	}
	if !reflect.DeepEqual(emit, priorRuns) {
		t.Errorf("emit = %v, want held prior runs %v", emit, priorRuns)
	}
	if !reflect.DeepEqual(newLast, priorRuns) {
		t.Errorf("new last-matched = %v, want unchanged prior runs %v", newLast, priorRuns)
	}
}

func TestSyncDecisionFirstDivergenceNoPriorEmitsBitmapOnly(t *testing.T) {
	// Diverged with no prior match: emit bitmap-only (nil runs), last-matched
	// stays nil.
	bitmap := scrollOffset{X: 0, Y: 0}
	snapshot := scrollOffset{X: 0, Y: 50}
	emit, newLast := syncDecision(bitmap, snapshot, 1, decodedRuns, nil)

	if len(emit) != 0 {
		t.Errorf("emit = %v, want nil/empty for a bitmap-only frame", emit)
	}
	if len(newLast) != 0 {
		t.Errorf("new last-matched = %v, want nil/empty", newLast)
	}
}

func TestSyncDecisionDivergenceOnXOnlyHoldsPriorMatched(t *testing.T) {
	// Y equal, X apart by more than the tolerance: the X-axis compare must
	// diverge symmetrically with the Y-axis case above. A regression that only
	// compared Y would wrongly match here and emit decoded.
	bitmap := scrollOffset{X: 100, Y: 200}
	snapshot := scrollOffset{X: 100 + scrollMatchTolerancePx + 1, Y: 200}
	emit, newLast := syncDecision(bitmap, snapshot, 1, decodedRuns, priorRuns)

	if reflect.DeepEqual(emit, decodedRuns) {
		t.Errorf("emit = %v, must not be the current snapshot's decoded runs on X divergence", emit)
	}
	if !reflect.DeepEqual(emit, priorRuns) {
		t.Errorf("emit = %v, want held prior runs %v", emit, priorRuns)
	}
	if !reflect.DeepEqual(newLast, priorRuns) {
		t.Errorf("new last-matched = %v, want unchanged prior runs %v", newLast, priorRuns)
	}
}

func TestSyncDecisionPageScaleNormalisesBeforeCompare(t *testing.T) {
	// Raw offsets differ by 4 on Y, which exceeds tolerance. Dividing by the
	// scale of 4 shrinks the gap to 1 CSS pixel, equal to the tolerance, so it
	// matches. The decode must normalise before comparing, not compare raw.
	bitmap := scrollOffset{X: 0, Y: 0}
	snapshot := scrollOffset{X: 0, Y: 4}
	const scale = 4.0

	// Sanity: without normalising these would diverge.
	if scrollOffsetsMatch(bitmap, snapshot, 1) {
		t.Fatal("raw offsets should diverge at scale 1; test setup is wrong")
	}

	emit, newLast := syncDecision(bitmap, snapshot, scale, decodedRuns, priorRuns)

	if !reflect.DeepEqual(emit, decodedRuns) {
		t.Errorf("emit = %v, want decoded runs %v after scale normalisation", emit, decodedRuns)
	}
	if !reflect.DeepEqual(newLast, decodedRuns) {
		t.Errorf("new last-matched = %v, want decoded runs %v", newLast, decodedRuns)
	}
}
