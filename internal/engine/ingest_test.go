package engine

import (
	"context"
	"image"
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

func TestDecodeWorkerRecordsDecodeWhenArmed(t *testing.T) {
	t.Parallel()
	bitmap := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e := &ChromiumEngine{
		backend: backendBlock,
		acks:    newAckQueue(),
		ingest:  &ingestCounter{on: true, window: ingestWindow},
		decode:  func(string) (*image.RGBA, error) { return bitmap, nil },
	}
	queue := newRawFrameQueue(1)
	decoded := make(chan decodedFrame, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runDecodeWorker(ctx, queue, decoded)

	e.handleScreencastFrameLive(queue, 1, &page.EventScreencastFrame{Data: "payload", SessionID: 1})
	ack, ok := e.acks.pop()
	if !ok {
		t.Fatal("live receive did not acknowledge before decode completed")
	}
	if ack != 1 {
		t.Fatalf("acknowledged SessionID %d, want 1", ack)
	}
	select {
	case got := <-decoded:
		if got.pending.img != bitmap {
			t.Fatalf("decoded image = %p, want %p", got.pending.img, bitmap)
		}
	case <-time.After(time.Second):
		t.Fatal("decode worker did not deliver frame")
	}

	stats := e.ingest.summary()
	if stats.Frames != 1 {
		t.Fatalf("ingest counted %d frames, want 1", stats.Frames)
	}
	if got := ingestDecodeCount(e.ingest); got != 1 {
		t.Fatalf("worker recorded %d decode timings, want 1", got)
	}
}

func TestHandleScreencastFrameLiveCountsIngestBeforeQueueDrop(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{ingest: &ingestCounter{on: true, window: ingestWindow}}
	queue := newRawFrameQueue(1)
	for i := range 3 {
		e.handleScreencastFrameLive(queue, uint64(i+1), &page.EventScreencastFrame{Data: "payload", SessionID: int64(i + 1)})
	}
	if stats := e.ingest.summary(); stats.Frames != 3 {
		t.Fatalf("ingest counted %d frames over 3 delivered frames, want 3", stats.Frames)
	}
	select {
	case got := <-queue.items:
		if got.seq != 3 {
			t.Fatalf("queue retained sequence %d, want latest 3", got.seq)
		}
	default:
		t.Fatal("queue retained no frame")
	}
}

func ingestDecodeCount(c *ingestCounter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.decodeFrames
}
