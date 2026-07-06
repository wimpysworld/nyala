package engine

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"math"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

// approxEq reports whether got is within tol of want, for the rate assertions that
// carry floating-point residue.
func approxEq(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

// TestIngestCounterWindowAndSummary checks the counter tallies each recorded frame,
// reports the rolling window rate, decays to 0 when idle, and reports the session
// mean over the ingest span. Times are injected, so the windowing is deterministic.
func TestIngestCounterWindowAndSummary(t *testing.T) {
	t.Parallel()
	c := &ingestCounter{on: true, window: time.Second}
	base := time.Unix(0, 0)
	// Five frames at 100ms spacing: four intervals over 400ms is 10 fps.
	for i := range 5 {
		c.record(base.Add(time.Duration(i) * 100 * time.Millisecond))
	}
	if got := c.fps(base.Add(400 * time.Millisecond)); got < 9.99 || got > 10.01 {
		t.Fatalf("window ingest fps = %.3f, want 10", got)
	}
	// Long idle: the window empties, so the live rate decays to 0.
	if got := c.fps(base.Add(5 * time.Second)); got != 0 {
		t.Fatalf("idle ingest fps = %.3f, want 0", got)
	}
	stats := c.summary()
	if stats.Frames != 5 {
		t.Fatalf("summary frames = %d, want 5", stats.Frames)
	}
	if !approxEq(stats.MeanFPS, 10, 0.01) {
		t.Fatalf("summary mean = %.3f, want 10", stats.MeanFPS)
	}
}

// TestIngestCounterGapExtremes checks the summary mirrors the paint meter's Out
// extremes: in_min is 1/maxGap, in_max is 1/minGap, in_median is the median gap
// inverted, and in_worst_gap_ms is the largest gap. Times are injected.
func TestIngestCounterGapExtremes(t *testing.T) {
	t.Parallel()
	c := &ingestCounter{on: true, window: time.Second}
	base := time.Unix(0, 0)
	// Arrivals at 0, 100, 200, 500 ms: gaps of 100, 100, 300 ms.
	for _, ms := range []int{0, 100, 200, 500} {
		c.record(base.Add(time.Duration(ms) * time.Millisecond))
	}
	stats := c.summary()
	if stats.Frames != 4 {
		t.Fatalf("frames = %d, want 4", stats.Frames)
	}
	// Fastest single frame is the 100ms gap = 10 fps; slowest is the 300ms gap = 3.33.
	if !approxEq(stats.MaxFPS, 10, 0.01) {
		t.Fatalf("in_max = %.3f, want 10 (1/minGap)", stats.MaxFPS)
	}
	if !approxEq(stats.MinFPS, 1000.0/300.0, 0.01) {
		t.Fatalf("in_min = %.3f, want 3.333 (1/maxGap)", stats.MinFPS)
	}
	// Median gap of [100,100,300] is 100ms = 10 fps.
	if !approxEq(stats.MedianFPS, 10, 0.01) {
		t.Fatalf("in_median = %.3f, want 10", stats.MedianFPS)
	}
	if !approxEq(stats.WorstGapMS, 300, 0.01) {
		t.Fatalf("in_worst_gap_ms = %.3f, want 300", stats.WorstGapMS)
	}
	// Mean over the 500ms span with 4 frames is (4-1)/0.5 = 6 fps.
	if !approxEq(stats.MeanFPS, 6, 0.01) {
		t.Fatalf("in_mean = %.3f, want 6", stats.MeanFPS)
	}
}

// TestIngestCounterDecodeBudget checks the worker decode timings accumulate into the
// summary mean and max.
func TestIngestCounterDecodeBudget(t *testing.T) {
	t.Parallel()
	c := &ingestCounter{on: true, window: time.Second}
	c.record(time.Unix(0, 0)) // one frame, so summary reports
	for _, ms := range []float64{4, 8, 21} {
		c.recordDecode(ms)
	}
	stats := c.summary()
	if !approxEq(stats.DecodeMeanMS, 11, 0.001) {
		t.Fatalf("in_decode_mean_ms = %.3f, want 11 (mean of 4,8,21)", stats.DecodeMeanMS)
	}
	if !approxEq(stats.DecodeMaxMS, 21, 0.001) {
		t.Fatalf("in_decode_max_ms = %.3f, want 21", stats.DecodeMaxMS)
	}
}

// TestIngestCounterGatedOffSilent checks a disabled counter records nothing (frames,
// gaps, and decode all no-op) and the summary is the zero value, so an off run is
// silent on the hot path and the seam.
func TestIngestCounterGatedOffSilent(t *testing.T) {
	t.Parallel()
	c := &ingestCounter{window: time.Second} // on is false
	if c.armed() {
		t.Fatal("disabled counter reports armed")
	}
	c.record(time.Unix(0, 0))
	c.record(time.Unix(0, int64(100*time.Millisecond)))
	c.recordDecode(9)
	stats := c.summary()
	if stats != (render.IngestStats{}) {
		t.Fatalf("disabled summary = %+v, want the zero value", stats)
	}
	if got := c.fps(time.Unix(0, 0)); got != 0 {
		t.Fatalf("disabled fps = %.3f, want 0", got)
	}
}

// TestWorkerRecordsDecodeWhenArmed checks the runFrameLoop worker feeds decode times
// to the ingest counter when armed, off the receiver goroutine. It runs the loop with
// an armed counter and a decode hook, delivers frames via handleScreencastFrame, and
// asserts the decode budget populates.
func TestWorkerRecordsDecodeWhenArmed(t *testing.T) {
	t.Parallel()
	bitmap := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e := &ChromiumEngine{
		backend: "block",
		acks:    make(chan int64, ackBufferDepth),
		ingest:  &ingestCounter{on: true, window: ingestWindow},
		decode:  staticDecode(bitmap),
	}
	slot := newFrameSlot()
	ts := &textSync{}
	ctx := t.Context()
	frames := make(chan render.Frame, 1)
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 1)
	go e.runFrameLoop(ctx, slot, frames, ts, scroll, reEmit)

	e.handleScreencastFrame(slot, &page.EventScreencastFrame{Data: "p", SessionID: 1})
	recvFrame(t, frames)

	stats := e.ingest.summary()
	if stats.Frames < 1 {
		t.Fatalf("ingest counted %d frames, want at least 1", stats.Frames)
	}
	// The decode ran on the worker and was recorded; a real time.Now delta is >= 0.
	if stats.DecodeMaxMS < 0 {
		t.Fatalf("in_decode_max_ms = %.3f, want a recorded (>=0) worker decode time", stats.DecodeMaxMS)
	}
	if c := decodeCount(e.ingest); c != 1 {
		t.Fatalf("worker recorded %d decode timings, want 1", c)
	}
}

// decodeCount reads the recorded decode-timing count under the counter lock, so a
// test can assert the worker fed exactly one timing per frame.
func decodeCount(c *ingestCounter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.decodeFrames
}

// TestHandleScreencastFrameCountsIngest checks the true ingest point tallies one
// frame per delivered screencast frame, before the latest-wins slot drops any. No
// browser is involved: frames are fed straight to the handler with a nil acks
// channel (ackFrame drops), so the slot's replace path never blocks.
func TestHandleScreencastFrameCountsIngest(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{ingest: &ingestCounter{on: true, window: ingestWindow}}
	slot := newFrameSlot()
	data := base64PNG(t, 2, 2)
	for i := range 3 {
		e.handleScreencastFrame(slot, &page.EventScreencastFrame{Data: data, SessionID: int64(i + 1)})
	}
	if stats := e.ingest.summary(); stats.Frames != 3 {
		t.Fatalf("ingest counted %d frames over 3 handleScreencastFrame calls, want 3", stats.Frames)
	}
}

// base64PNG builds a base64-encoded PNG of the given size, the wire shape a
// screencast frame carries, so a decode has real bytes without a browser.
func base64PNG(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
