package engine

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debugTimingEnv is the environment variable that turns on the load-path timing
// trace. Set to any non-empty value, the engine prints one stderr line per launch
// milestone with the elapsed milliseconds since launch; unset, it prints nothing.
// It follows the NYALA_* environment convention the rest of the project uses for
// diagnostic switches.
const debugTimingEnv = "NYALA_DEBUG_TIMING"

// The four load-path milestone labels are kept exact so debug output does not drift.
const (
	labelProcessSpawned    = "process spawned"
	labelScreencastStarted = "screencast started"
	labelFirstFrame        = "first frame emitted"
	labelFirstText         = "first text published"
)

// loadTimer traces the launch to first-frame to first-text load path when
// NYALA_DEBUG_TIMING is set. Armed, every milestone reports the milliseconds
// elapsed since launch. Disabled methods perform no clock read. A nil *loadTimer
// is also a valid disabled timer, so unit tests may call its methods safely.
type loadTimer struct {
	on    bool      // armed: reads the clock
	print bool      // prints the stderr trace (debug switch only)
	start time.Time // launch instant elapsed spans measure from
	frame sync.Once // guards the one-shot first-frame milestone
	text  sync.Once // guards the one-shot first-text milestone
}

// newLoadTimer returns a loadTimer armed at now when NYALA_DEBUG_TIMING is set,
// or a disabled timer whose methods are no-ops otherwise.
func newLoadTimer() *loadTimer {
	debug := os.Getenv(debugTimingEnv) != ""
	if !debug {
		return &loadTimer{}
	}
	return &loadTimer{on: true, print: true, start: time.Now()}
}

// mark prints the milestone's elapsed milliseconds when debug timing is enabled.
// It does nothing on a nil or disabled timer.
func (t *loadTimer) mark(label string) {
	if t == nil || !t.on {
		return
	}
	ms := msSince(t.start)
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
