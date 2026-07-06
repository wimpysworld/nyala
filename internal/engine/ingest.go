package engine

import (
	"os"
	"sync"
	"time"
)

// fpsTimingEnv is the environment variable that arms the screencast ingest
// counter. Set to any non-empty value the engine counts each delivered screencast
// frame for the NYALA_FPS_TIMING instrument; unset the counter is off and every
// method is a cheap no-op. It follows the NYALA_* diagnostic-switch convention and
// matches the viewer's fps switch, so the paint rate (Out) and the ingest rate (In)
// arm together.
const fpsTimingEnv = "NYALA_FPS_TIMING"

// ingestWindow is the trailing span the ingest counter reports its rolling rate
// over. It matches the viewer badge window, so In and Out decay on the same cadence.
const ingestWindow = time.Second

// ingestCounter counts screencast frames at the true ingest point
// (handleScreencastFrame), before the latest-wins slot drops any, so it reports the
// pre-drop ingest rate the paint meter cannot see. It holds a trailing window of
// frame instants for the live rolling rate and session totals (count, first and
// last instant) for the summary mean. The listener goroutine records; the viewer
// draw goroutine reads; a mutex guards both, and a screencast frame arrives at only
// ~10-60/s, so the lock cost is nil. It is armed once at engine construction from
// NYALA_FPS_TIMING; off, record is a bool check and the reporters return zero. It
// persists across sessions, so a resize or backend switch keeps the engine-lifetime
// ingest rate; the trailing window ages stale entries out on its own, so a session
// change needs no explicit reset.
type ingestCounter struct {
	on     bool
	window time.Duration

	mu     sync.Mutex
	times  []time.Time
	frames int
	first  time.Time
	last   time.Time
}

// newIngestCounter returns a counter armed when NYALA_FPS_TIMING is set, or a
// disabled counter whose methods are no-ops otherwise. The env read happens once at
// construction, so an off run does no per-frame work beyond the armed check.
func newIngestCounter() *ingestCounter {
	return &ingestCounter{on: os.Getenv(fpsTimingEnv) != "", window: ingestWindow}
}

// armed reports whether the counter records. handleScreencastFrame checks it before
// reading the clock, so an off run pays one bool check and no time.Now read. A nil
// counter is not armed, so a session built without one (a unit test) is safe.
func (c *ingestCounter) armed() bool {
	return c != nil && c.on
}

// record stamps one delivered screencast frame at now. It is called once per frame
// on the listener goroutine, before the slot drops any, so it counts true ingest. A
// nil or disabled counter returns at once, so a stray call is safe.
func (c *ingestCounter) record(now time.Time) {
	if !c.armed() {
		return
	}
	c.mu.Lock()
	c.times = append(c.times, now)
	if c.frames == 0 {
		c.first = now
	}
	c.frames++
	c.last = now
	c.mu.Unlock()
}

// fps prunes the trailing window to now and returns the rolling ingest rate over
// it, so an idle page (no fresh frames) decays the rate to 0. F instants in the
// window leave F-1 intervals over their span, so the rate is (F-1)/span. It is
// called on the viewer draw goroutine; the mutex guards it against record. A nil or
// disabled counter, or fewer than two frames in the window, reports 0.
func (c *ingestCounter) fps(now time.Time) float64 {
	if !c.armed() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := now.Add(-c.window)
	drop := 0
	for drop < len(c.times) && c.times[drop].Before(cutoff) {
		drop++
	}
	c.times = c.times[drop:]
	if len(c.times) < 2 {
		return 0
	}
	span := c.times[len(c.times)-1].Sub(c.times[0]).Seconds()
	if span <= 0 {
		return 0
	}
	return float64(len(c.times)-1) / span
}

// summary returns the session ingest frame count and the mean fps over the ingest
// span (first to last frame). It is read once at quit on the viewer draw goroutine.
// A nil or disabled counter, or a span too short to divide, reports zero.
func (c *ingestCounter) summary() (frames int, meanFPS float64) {
	if !c.armed() {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frames == 0 {
		return 0, 0
	}
	span := c.last.Sub(c.first).Seconds()
	if span <= 0 {
		return c.frames, 0
	}
	return c.frames, float64(c.frames-1) / span
}
