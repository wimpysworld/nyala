package engine

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
)

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
	frames, mean := c.summary()
	if frames != 5 {
		t.Fatalf("summary frames = %d, want 5", frames)
	}
	if mean < 9.99 || mean > 10.01 {
		t.Fatalf("summary mean = %.3f, want 10", mean)
	}
}

// TestIngestCounterGatedOffSilent checks a disabled counter records nothing and both
// reporters return zero, so an off run is silent on the hot path and the seam.
func TestIngestCounterGatedOffSilent(t *testing.T) {
	t.Parallel()
	c := &ingestCounter{window: time.Second} // on is false
	if c.armed() {
		t.Fatal("disabled counter reports armed")
	}
	c.record(time.Unix(0, 0))
	if frames, mean := c.summary(); frames != 0 || mean != 0 {
		t.Fatalf("disabled summary = (%d, %.2f), want (0, 0)", frames, mean)
	}
	if got := c.fps(time.Unix(0, 0)); got != 0 {
		t.Fatalf("disabled fps = %.3f, want 0", got)
	}
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
	if frames, _ := e.ingest.summary(); frames != 3 {
		t.Fatalf("ingest counted %d frames over 3 handleScreencastFrame calls, want 3", frames)
	}
}

// base64PNG builds a base64-encoded PNG of the given size, the wire shape a
// screencast frame carries, so handleScreencastFrame decodes it without a browser.
func base64PNG(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
