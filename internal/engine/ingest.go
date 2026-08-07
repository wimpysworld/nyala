package engine

import (
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
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
// before the live queue drops any, so it reports the
// pre-drop ingest rate the paint meter cannot see. It holds a trailing window of
// frame instants for the live rolling rate, session totals (count, first and last
// instant) for the summary mean, and the per-frame inter-arrival gaps for the
// min/max/median/worst-gap extremes that mirror the paint meter's Out stats. Under
// the decoupled receiver/worker model the arrival times are true per-frame instants,
// not coalesced bursts, so the gap extremes are meaningful. It also accumulates the
// worker decode budget (mean and max), fed from decode workers when armed. The listener
// goroutine records arrivals, the worker records decode times, the viewer draw
// goroutine reads; one mutex guards all three, and a screencast frame arrives at only
// ~10-60/s, so the lock cost is nil. It is armed once at engine construction from
// NYALA_FPS_TIMING; off, timing recorders are no-ops and timing reports are zero. It
// persists across sessions, so a resize or backend switch keeps the engine-lifetime
// ingest rate; the trailing window ages stale entries out on its own, so a session
// change needs no explicit reset. Decode failures are the exception to the timing
// gate: they are always counted so the default session-end warning can report them.
type ingestCounter struct {
	on     bool
	window time.Duration

	mu     sync.Mutex
	times  []time.Time
	frames int
	first  time.Time
	last   time.Time

	// prev is the previous arrival instant, so record measures the inter-arrival gap.
	// minGap/maxGap track the extremes; gaps collects every gap, sorted once at quit
	// for the median. haveGap is set once two frames have arrived.
	prev    time.Time
	haveGap bool
	minGap  time.Duration
	maxGap  time.Duration
	gaps    []time.Duration

	// decodeFrames/decodeTotal/decodeMax accumulate the worker decode budget: the
	// count and sum give the mean, decodeMax the slowest single decode. Fed by
	// recordDecode from the worker goroutine only when armed.
	decodeFrames int
	decodeTotal  float64
	decodeMax    float64

	// decodeDrops counts failed worker decodes without contending with the timing
	// mutex. decodeLatestError replaces one string-only slot, so a bad payload
	// cannot grow session storage.
	decodeDrops       atomic.Uint64
	decodeLatestError atomic.Pointer[string]

	// ackFrames/ackTotal/ackMax accumulate the acknowledgement round-trip budget:
	// the count and sum give the mean, ackMax the slowest single round trip. Fed by
	// recordAck from the ack pump goroutine only when armed.
	ackFrames int
	ackTotal  float64
	ackMax    float64
}

// newIngestCounter returns a timing counter armed when NYALA_FPS_TIMING is set.
// The env read happens once at construction, so an off run does no timing work.
// Decode failures remain active in either state.
func newIngestCounter() *ingestCounter {
	return &ingestCounter{on: os.Getenv(fpsTimingEnv) != "", window: ingestWindow}
}

// armed reports whether the counter records. The listener checks it before
// reading the clock, so an off run pays one bool check and no time.Now read. A nil
// counter is not armed, so a session built without one (a unit test) is safe.
func (c *ingestCounter) armed() bool {
	return c != nil && c.on
}

// record stamps one delivered screencast frame at now. It is called once per frame
// on the listener goroutine, before the queue drops any, so it counts true ingest and
// the inter-arrival gaps are the real per-frame arrival intervals. From the second
// frame on it measures the gap since the previous arrival, tracking the min and max
// and appending to the gap slice for the quit-time median. A nil or disabled counter
// returns at once, so a stray call is safe.
func (c *ingestCounter) record(now time.Time) {
	if !c.armed() {
		return
	}
	c.mu.Lock()
	c.times = append(c.times, now)
	if c.frames == 0 {
		c.first = now
	} else {
		gap := now.Sub(c.prev)
		if !c.haveGap || gap < c.minGap {
			c.minGap = gap
		}
		if !c.haveGap || gap > c.maxGap {
			c.maxGap = gap
		}
		c.haveGap = true
		c.gaps = append(c.gaps, gap)
	}
	c.frames++
	c.prev = now
	c.last = now
	c.mu.Unlock()
}

// recordDecode adds one worker decode wall time in milliseconds to the decode
// budget: the count and running sum feed the mean, and it tracks the slowest single
// decode. A decode worker calls it per successfully decoded
// frame, gated on the armed check, so an off run reads no clock and does no work
// here. A nil or disabled counter returns at once.
func (c *ingestCounter) recordDecode(ms float64) {
	if !c.armed() {
		return
	}
	c.mu.Lock()
	c.decodeFrames++
	c.decodeTotal += ms
	if ms > c.decodeMax {
		c.decodeMax = ms
	}
	c.mu.Unlock()
}

// recordDecodeDrop counts one failed worker decode and replaces the latest
// failure text. It uses atomics because several decode workers can fail at once,
// and a failure must never block frame ingest. Unlike the timing fields, it
// records when NYALA_FPS_TIMING is unset. Nil errors and nil counters are ignored.
func (c *ingestCounter) recordDecodeDrop(err error) {
	if err == nil || c == nil {
		return
	}
	c.decodeDrops.Add(1)
	reason := err.Error()
	c.decodeLatestError.Store(&reason)
}

// recordAck adds one acknowledgement command round trip in milliseconds: the
// count and running sum feed the mean, and it tracks the slowest single round
// trip. The ack pump calls it per completed acknowledgement, gated on the armed
// check, so an off run reads no clock and does no work here. A nil or disabled
// counter returns at once.
func (c *ingestCounter) recordAck(ms float64) {
	if !c.armed() {
		return
	}
	c.mu.Lock()
	c.ackFrames++
	c.ackTotal += ms
	if ms > c.ackMax {
		c.ackMax = ms
	}
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

// summary returns the session ingest statistics: the frame count, the mean fps over
// the ingest span (first to last frame), the min/max/median instantaneous fps and
// the worst inter-arrival gap from the collected gaps, the worker decode mean and
// max, and the acknowledgement round-trip mean and max. It is read once at quit on
// the viewer draw goroutine. It sorts the gap slice once here, off the hot path,
// for the median. Decode failures are returned even when timing is disabled or no
// frame decoded; every other field stays zero in those cases. A nil counter reports
// the zero value.
func (c *ingestCounter) summary() render.IngestStats {
	if c == nil {
		return render.IngestStats{}
	}
	stats := render.IngestStats{DecodeDrops: c.decodeDrops.Load()}
	if reason := c.decodeLatestError.Load(); reason != nil {
		stats.DecodeLatestError = *reason
	}
	if !c.armed() {
		return stats
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frames == 0 {
		return stats
	}
	stats.Frames = c.frames
	if span := c.last.Sub(c.first).Seconds(); span > 0 {
		stats.MeanFPS = float64(c.frames-1) / span
	}
	if c.haveGap {
		if s := c.maxGap.Seconds(); s > 0 {
			stats.MinFPS = 1 / s
		}
		if s := c.minGap.Seconds(); s > 0 {
			stats.MaxFPS = 1 / s
		}
		stats.WorstGapMS = float64(c.maxGap) / float64(time.Millisecond)
		stats.MedianFPS = medianFPSFromGaps(c.gaps)
	}
	if c.decodeFrames > 0 {
		stats.DecodeMeanMS = c.decodeTotal / float64(c.decodeFrames)
		stats.DecodeMaxMS = c.decodeMax
	}
	if c.ackFrames > 0 {
		stats.AckRTTMeanMS = c.ackTotal / float64(c.ackFrames)
		stats.AckRTTMaxMS = c.ackMax
	}
	return stats
}

// medianFPSFromGaps returns the median instantaneous fps from the inter-arrival
// gaps: it sorts them (once, at quit, off the hot path), takes the middle gap
// (averaging the two middle gaps for an even count), and inverts it to fps. The
// median ignores a one-off stall, so it reads the typical ingest rate. An empty or
// non-positive-median slice reports 0. It mirrors the viewer paint meter's medianFPS
// so In and Out share the same median semantics.
func medianFPSFromGaps(gaps []time.Duration) float64 {
	if len(gaps) == 0 {
		return 0
	}
	slices.Sort(gaps)
	n := len(gaps)
	med := gaps[n/2]
	if n%2 == 0 {
		med = (gaps[n/2-1] + gaps[n/2]) / 2
	}
	s := med.Seconds()
	if s <= 0 {
		return 0
	}
	return 1 / s
}
