package engine

import (
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

// holdStalenessLimit bounds how long the overlay holds the last-matched text
// while the DOM snapshot read keeps failing. Past it the frame drops to
// bitmap-only (nil runs) rather than show text that no longer matches the page.
// It is a wall-clock bound, not a frame count, because the hold guards a snapshot
// read failure whose duration is wall-clock and the usable-frame cadence varies by
// backend and load. 250 ms sits below the ~400 ms threshold where a human reads
// stale on-screen text as wrong, and spans a few snapshot retries at the engine's
// frame cadence. It is the sole sync-tolerance knob now: the placement rewrite
// decodes each snapshot's runs at its own scroll offset, so there is no offset
// match tolerance, only this staleness bound on the error hold.
const holdStalenessLimit = 250 * time.Millisecond

// scrollOffset is one CSS-pixel scroll position. The bitmap pair comes from
// page.ScreencastFrameMetadata and the snapshot pair from
// domsnapshot.DocumentSnapshot. The placement rewrite decodes runs at the
// snapshot's own offset, so the two no longer need to match; the pair type is
// kept for the offset reads and the pure compare helper.
type scrollOffset struct {
	X float64
	Y float64
}

// holdDecision is the pure, single bounded error-hold decision for the text
// overlay. On a successful snapshot it emits the freshly decoded runs (already
// placed at the snapshot's own scroll), records them as the last-matched, and
// resets the failure clock. On a snapshot read error it holds the last-matched
// runs for at most holdStalenessLimit of continuous failure, then drops to nil
// (a bitmap-only frame) rather than show text that no longer matches the page.
// failingSince is the wall-clock time the current failure run began (the zero
// time when not failing); now is the current time. It returns the runs to emit,
// the new last-matched set, and the new failure-run start. It takes no browser
// and an injected clock, so it is unit-testable on canned errors and times.
func holdDecision(
	snapshotErr error,
	decoded, lastMatched []render.GlyphRun,
	failingSince, now time.Time,
) (emit, newLastMatched []render.GlyphRun, newFailingSince time.Time) {
	if snapshotErr == nil {
		// Success: emit the decoded runs and clear the failure clock.
		return decoded, decoded, time.Time{}
	}
	// First failure of a run starts the clock at now.
	if failingSince.IsZero() {
		failingSince = now
	}
	if now.Sub(failingSince) <= holdStalenessLimit {
		// Within the bound: hold the last-matched text. Last-matched is unchanged so
		// a later success can still re-sync, and the next failure keeps the clock.
		return lastMatched, lastMatched, failingSince
	}
	// Past the bound: drop to a bitmap-only frame and forget the stale text, so a
	// later success replaces it rather than the overlay flickering back to it.
	return nil, nil, failingSince
}
