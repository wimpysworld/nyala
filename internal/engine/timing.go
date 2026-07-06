package engine

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debugTimingEnv is the environment variable that turns on the load-path timing
// trace. Set to any non-empty value, the engine prints one stderr line per launch
// milestone with the elapsed milliseconds since launch; unset, it reads no clock
// and prints nothing. It follows the NYALA_* environment convention the rest of
// the project uses for diagnostic switches.
const debugTimingEnv = "NYALA_DEBUG_TIMING"

// loadTimer traces the launch to first-frame to first-text load path to stderr
// when NYALA_DEBUG_TIMING is set, and does nothing otherwise. newLoadTimer arms it
// once per session at launch and every milestone reports the milliseconds elapsed
// since that moment. Off, every method is a nil-or-flag check with no clock read,
// so production pays nothing. A nil *loadTimer is a valid disabled timer, so a
// session that never armed one (a unit test) calls the methods safely.
type loadTimer struct {
	on    bool
	start time.Time
	frame sync.Once // guards the one-shot first-frame line
	text  sync.Once // guards the one-shot first-text line
}

// newLoadTimer returns a loadTimer armed at now when NYALA_DEBUG_TIMING is set, or
// a disabled timer whose methods are no-ops otherwise. The env read happens once
// per session, so an off run does no work beyond it.
func newLoadTimer() *loadTimer {
	if os.Getenv(debugTimingEnv) == "" {
		return &loadTimer{}
	}
	return &loadTimer{on: true, start: time.Now()}
}

// mark prints one milestone line with the elapsed milliseconds since launch when
// timing is on, and does nothing on a nil or disabled timer.
func (t *loadTimer) mark(label string) {
	if t == nil || !t.on {
		return
	}
	fmt.Fprintf(os.Stderr, "nyala timing: %s +%.1fms\n", label, msSince(t.start))
}

// markFrame prints the first-frame-emitted milestone once, then does nothing on
// later frames or a disabled timer, so the frame loop can call it every frame.
func (t *loadTimer) markFrame() {
	if t == nil || !t.on {
		return
	}
	t.frame.Do(func() { t.mark("first frame emitted") })
}

// markText prints the first-text-published milestone once, then does nothing on
// later publishes or a disabled timer, so the snapshot goroutine can call it on
// every publish.
func (t *loadTimer) markText() {
	if t == nil || !t.on {
		return
	}
	t.text.Do(func() { t.mark("first text published") })
}
