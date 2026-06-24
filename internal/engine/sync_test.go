package engine

import (
	"errors"
	"image/color"
	"reflect"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

// errSnapshot is the canned snapshot-read failure the holdDecision tests feed,
// standing in for a real DOMSnapshot.captureSnapshot error. No browser involved.
var errSnapshot = errors.New("snapshot read failed")

// decodedRuns is the freshly decoded text, the "current snapshot" set. A held
// frame must never emit this set.
var decodedRuns = []render.GlyphRun{
	{Col: 0, Row: 0, Text: "now", FG: color.RGBA{R: 255, A: 255}},
}

// priorRuns is the last-matched text from an earlier successful frame. The hold
// case emits this set.
var priorRuns = []render.GlyphRun{
	{Col: 1, Row: 2, Text: "before", FG: color.RGBA{G: 255, A: 255}},
}

func TestHoldDecisionSuccessEmitsDecodedAndResetsClock(t *testing.T) {
	// A successful read emits the freshly decoded runs, records them as the new
	// last-matched, and clears the failure clock.
	now := time.Unix(100, 0)
	emit, last, failing := holdDecision(nil, decodedRuns, priorRuns, now, now)

	if !reflect.DeepEqual(emit, decodedRuns) {
		t.Errorf("emit = %v, want decoded runs %v", emit, decodedRuns)
	}
	if !reflect.DeepEqual(last, decodedRuns) {
		t.Errorf("last-matched = %v, want decoded runs %v", last, decodedRuns)
	}
	if !failing.IsZero() {
		t.Errorf("failingSince = %v, want zero (clock reset on success)", failing)
	}
}

func TestHoldDecisionFirstFailureHoldsPriorAndStartsClock(t *testing.T) {
	// The first failure of a run holds the prior matched text and starts the
	// failure clock at now, so the bound is measured from here.
	now := time.Unix(100, 0)
	emit, last, failing := holdDecision(errSnapshot, nil, priorRuns, time.Time{}, now)

	if !reflect.DeepEqual(emit, priorRuns) {
		t.Errorf("emit = %v, want held prior runs %v", emit, priorRuns)
	}
	if !reflect.DeepEqual(last, priorRuns) {
		t.Errorf("last-matched = %v, want unchanged prior runs %v", last, priorRuns)
	}
	if !failing.Equal(now) {
		t.Errorf("failingSince = %v, want the failure start %v", failing, now)
	}
}

func TestHoldDecisionWithinBoundHoldsPrior(t *testing.T) {
	// A failure still inside holdStalenessLimit holds the prior text. Exactly at the
	// bound still holds (the limit is inclusive).
	start := time.Unix(100, 0)
	now := start.Add(holdStalenessLimit)
	emit, last, failing := holdDecision(errSnapshot, nil, priorRuns, start, now)

	if !reflect.DeepEqual(emit, priorRuns) {
		t.Errorf("emit = %v, want held prior runs %v at the bound", emit, priorRuns)
	}
	if !reflect.DeepEqual(last, priorRuns) {
		t.Errorf("last-matched = %v, want unchanged prior runs %v", last, priorRuns)
	}
	if !failing.Equal(start) {
		t.Errorf("failingSince = %v, want unchanged start %v", failing, start)
	}
}

func TestHoldDecisionPastBoundDropsToBitmapOnly(t *testing.T) {
	// Past holdStalenessLimit of continuous failure the hold drops to a bitmap-only
	// frame (nil runs) rather than show stale text, and forgets the stale text so a
	// later success replaces it.
	start := time.Unix(100, 0)
	now := start.Add(holdStalenessLimit + time.Millisecond)
	emit, last, failing := holdDecision(errSnapshot, nil, priorRuns, start, now)

	if len(emit) != 0 {
		t.Errorf("emit = %v, want nil/empty bitmap-only past the bound", emit)
	}
	if len(last) != 0 {
		t.Errorf("last-matched = %v, want nil/empty past the bound", last)
	}
	if !failing.Equal(start) {
		t.Errorf("failingSince = %v, want the unchanged failure start %v", failing, start)
	}
}

func TestHoldDecisionFirstFailureNoPriorEmitsBitmapOnly(t *testing.T) {
	// A first failure with no prior match emits bitmap-only (nil), still within the
	// bound (the start is now), so there is no wrong text to hold.
	now := time.Unix(100, 0)
	emit, last, _ := holdDecision(errSnapshot, nil, nil, time.Time{}, now)

	if len(emit) != 0 {
		t.Errorf("emit = %v, want nil/empty bitmap-only", emit)
	}
	if len(last) != 0 {
		t.Errorf("last-matched = %v, want nil/empty", last)
	}
}

func TestSustainedScrollPlacesCurrentTextNotHeld(t *testing.T) {
	// Place-don't-fold: a sustained scroll shows the current text. Frame A decodes
	// runsA at its scroll; frame B decodes runsB at the next scroll. The old sync
	// held runsA on scroll divergence; the rewrite places by the snapshot's own
	// scroll, so frame B must emit runsB, never the held runsA.
	runsA := []render.GlyphRun{{Col: 0, Row: 0, Text: "frame-A"}}
	runsB := []render.GlyphRun{{Col: 0, Row: 5, Text: "frame-B"}}
	now := time.Unix(100, 0)

	emitA, lastA, _ := holdDecision(nil, runsA, nil, time.Time{}, now)
	if !reflect.DeepEqual(emitA, runsA) {
		t.Fatalf("frame A emit = %v, want runsA %v", emitA, runsA)
	}

	emitB, _, _ := holdDecision(nil, runsB, lastA, time.Time{}, now.Add(16*time.Millisecond))
	if reflect.DeepEqual(emitB, runsA) {
		t.Fatalf("frame B emit = %v, must not be the held frame-A text", emitB)
	}
	if !reflect.DeepEqual(emitB, runsB) {
		t.Errorf("frame B emit = %v, want the current runsB %v placed by its own scroll", emitB, runsB)
	}
}

func TestScrollOffsetsMatchWithinTolerance(t *testing.T) {
	// Apart by exactly the tolerance on each axis: still a match.
	a := scrollOffset{X: 100, Y: 200}
	b := scrollOffset{X: 100 + scrollMatchTolerancePx, Y: 200 + scrollMatchTolerancePx}
	if !scrollOffsetsMatch(a, b, 1) {
		t.Errorf("offsets within tolerance reported as diverged")
	}
}

func TestScrollOffsetsMatchDivergesPastTolerance(t *testing.T) {
	// Apart by more than the tolerance on Y: diverged.
	a := scrollOffset{X: 100, Y: 200}
	b := scrollOffset{X: 100, Y: 200 + scrollMatchTolerancePx + 1}
	if scrollOffsetsMatch(a, b, 1) {
		t.Errorf("offsets past tolerance reported as matching")
	}
}

func TestScrollOffsetsMatchPageScaleNormalises(t *testing.T) {
	// Raw offsets differ by 4 on Y, which exceeds tolerance. Dividing by the scale
	// of 4 shrinks the gap to 1 CSS pixel, equal to the tolerance, so it matches.
	a := scrollOffset{X: 0, Y: 0}
	b := scrollOffset{X: 0, Y: 4}
	if scrollOffsetsMatch(a, b, 1) {
		t.Fatal("raw offsets should diverge at scale 1; test setup is wrong")
	}
	if !scrollOffsetsMatch(a, b, 4) {
		t.Error("offsets should match after normalising by the page scale factor")
	}
}
