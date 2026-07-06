package engine

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

// debugTimingEnv is the environment variable that turns on the load-path timing
// trace. Set to any non-empty value, the engine prints one stderr line per launch
// milestone with the elapsed milliseconds since launch; unset, it prints nothing.
// It follows the NYALA_* environment convention the rest of the project uses for
// diagnostic switches.
const debugTimingEnv = "NYALA_DEBUG_TIMING"

// The four load-path milestone labels. They are the stderr trace text (kept exact
// so the debug output does not drift) and the key the recorder maps to a span.
const (
	labelProcessSpawned    = "process spawned"
	labelScreencastStarted = "screencast started"
	labelFirstFrame        = "first frame emitted"
	labelFirstText         = "first text published"
)

// sessionTimings is the mutex-guarded holder the loadTimer records the four load
// spans into. The engine allocates one per capture run and shares it with the
// capture recorder, so the snapshot goroutine's markText write and the capture
// driver's snapshot read do not race. It stays nil off the capture path.
type sessionTimings struct {
	mu sync.Mutex
	t  render.LoadTimings
}

// record stores the elapsed milliseconds for the milestone named by label. An
// unknown label is ignored, so a new trace call that forgets to add a case cannot
// corrupt a span.
func (s *sessionTimings) record(label string, ms float64) {
	s.mu.Lock()
	switch label {
	case labelProcessSpawned:
		s.t.ProcessSpawnedMS = ms
	case labelScreencastStarted:
		s.t.ScreencastStartedMS = ms
	case labelFirstFrame:
		s.t.FirstFrameMS = ms
	case labelFirstText:
		s.t.FirstTextMS = ms
	}
	s.mu.Unlock()
}

// snapshot returns a copy of the recorded spans. The capture driver reads it at
// bundle-finalise time; an unrecorded milestone reads as zero.
func (s *sessionTimings) snapshot() render.LoadTimings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t
}

// loadTimer traces the launch to first-frame to first-text load path. It is armed
// when its session-timings recorder is set (the capture path) or NYALA_DEBUG_TIMING
// is set; armed, every milestone reports the milliseconds elapsed since launch. It
// records each span into the recorder when one is set, and prints one stderr line
// per milestone only when the debug switch is set, so a capture run records
// silently. Off (neither), every method is a nil-or-flag check with no clock read,
// so production pays nothing. A nil *loadTimer is a valid disabled timer, so a
// session that never armed one (a unit test) calls the methods safely.
type loadTimer struct {
	on    bool            // armed: reads the clock and records
	print bool            // prints the stderr trace (debug switch only)
	start time.Time       // launch instant elapsed spans measure from
	spans *sessionTimings // recorder, nil off the capture path
	frame sync.Once       // guards the one-shot first-frame milestone
	text  sync.Once       // guards the one-shot first-text milestone
}

// newLoadTimer returns a loadTimer armed at now when spans is set (capture) or
// NYALA_DEBUG_TIMING is set, or a disabled timer whose methods are no-ops
// otherwise. The env read happens once per session, so an off run does no work
// beyond it. spans is the capture recorder the armed timer writes each span into;
// nil off the capture path leaves recording off while the debug trace still prints.
func newLoadTimer(spans *sessionTimings) *loadTimer {
	debug := os.Getenv(debugTimingEnv) != ""
	if !debug && spans == nil {
		return &loadTimer{}
	}
	return &loadTimer{on: true, print: debug, start: time.Now(), spans: spans}
}

// mark records the milestone's elapsed milliseconds into the recorder when one is
// set, then prints the stderr trace line when the debug switch is on. It does
// nothing on a nil or disabled timer.
func (t *loadTimer) mark(label string) {
	if t == nil || !t.on {
		return
	}
	ms := msSince(t.start)
	if t.spans != nil {
		t.spans.record(label, ms)
	}
	if t.print {
		fmt.Fprintf(os.Stderr, "nyala timing: %s +%.1fms\n", label, ms)
	}
}

// markFrame marks the first-frame-emitted milestone once, then does nothing on
// later frames or a disabled timer, so the frame loop can call it every frame.
func (t *loadTimer) markFrame() {
	if t == nil || !t.on {
		return
	}
	t.frame.Do(func() { t.mark(labelFirstFrame) })
}

// markText marks the first-text-published milestone once, then does nothing on
// later publishes or a disabled timer, so the snapshot goroutine can call it on
// every publish.
func (t *loadTimer) markText() {
	if t == nil || !t.on {
		return
	}
	t.text.Do(func() { t.mark(labelFirstText) })
}
