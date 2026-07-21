package engine

import (
	"context"
	"image"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

func TestRunTextSyncSnapshotsOncePerPaint(t *testing.T) {
	t.Parallel()
	var snaps atomic.Int64
	want := []render.GlyphRun{{Col: 2, Row: 1, Text: "hi"}}
	e := &ChromiumEngine{backend: backendBlock}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState) {
		snaps.Add(1)
		return want, scrollOffset{}, 0, overlayState{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := &textSync{}
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, ts, make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1))
		close(done)
	}()

	waitForSnapshotCount(t, &snaps, 2)
	time.Sleep(50 * time.Millisecond)
	if got := snaps.Load(); got != 2 {
		t.Fatalf("snapshots = %d on static page, want 2", got)
	}
	if got, _, _ := ts.latest(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("published runs = %v, want %v", got, want)
	}

	cancel()
	waitForPipelineStop(t, done)
}

func TestRunTextSyncWaitsForMainFrameFCP(t *testing.T) {
	t.Parallel()
	var snaps atomic.Int64
	lifecycle := newSessionLifecycle()
	e := &ChromiumEngine{backend: backendBlock, lifecycle: lifecycle}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState) {
		snaps.Add(1)
		return nil, scrollOffset{}, 0, overlayState{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1))
		close(done)
	}()
	cancel()
	waitForPipelineStop(t, done)
	if got := snaps.Load(); got != 0 {
		t.Fatalf("snapshots before FCP = %d, want 0", got)
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done = make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1))
		close(done)
	}()
	lifecycle.recordCommit(&cdp.Frame{ID: "main", LoaderID: "loader"})
	lifecycle.recordFCP("main")
	waitForSnapshotCount(t, &snaps, 2)
	cancel()
	waitForPipelineStop(t, done)
}

func TestRunDecodeCollectorReEmitsHeldFrameWithText(t *testing.T) {
	t.Parallel()
	bitmap := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e := &ChromiumEngine{backend: backendBlock}
	ts := &textSync{}
	decoded := make(chan decodedFrame, 1)
	frames := make(chan render.Frame, 1)
	reEmit := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.runDecodeCollector(ctx, decoded, frames, ts, make(chan struct{}, 1), reEmit)
		close(done)
	}()

	decoded <- decodedFrame{seq: 1, pending: pendingFrame{img: bitmap}}
	first := recvFrame(t, frames)
	if first.Img != bitmap || len(first.Text) != 0 {
		t.Fatalf("first frame = image %p, text %v; want image %p without text", first.Img, first.Text, bitmap)
	}

	runs := []render.GlyphRun{{Col: 1, Row: 0, Text: "ok"}}
	ts.store(runs, 0, 0)
	reEmit <- struct{}{}
	reframe := recvFrame(t, frames)
	if reframe.Img != bitmap {
		t.Fatalf("re-emitted image = %p, want held image %p", reframe.Img, bitmap)
	}
	if len(reframe.Text) != 1 || reframe.Text[0] != runs[0] {
		t.Fatalf("re-emitted text = %v, want %v", reframe.Text, runs)
	}

	cancel()
	waitForPipelineStop(t, done)
}

func TestRunTextSyncReSnapTriggers(t *testing.T) {
	t.Parallel()
	var snaps atomic.Int64
	e := countingSnapshotEngine(&snaps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reSnap := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, reSnap, make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1))
		close(done)
	}()

	waitForSnapshotCount(t, &snaps, 2)
	reSnap <- struct{}{}
	waitForSnapshotCount(t, &snaps, 3)
	cancel()
	waitForPipelineStop(t, done)
}

func TestRunTextSyncReflowTriggers(t *testing.T) {
	t.Parallel()
	var snaps atomic.Int64
	e := countingSnapshotEngine(&snaps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reflow := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, make(chan struct{}, 1), reflow, make(chan struct{}, 1), make(chan struct{}, 4))
		close(done)
	}()

	waitForSnapshotCount(t, &snaps, 2)
	reflow <- struct{}{}
	waitForSnapshotCount(t, &snaps, 3)
	cancel()
	waitForPipelineStop(t, done)
}

func TestRunTextSyncScrollTriggers(t *testing.T) {
	t.Parallel()
	var snaps atomic.Int64
	e := countingSnapshotEngine(&snaps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, make(chan struct{}, 1), make(chan struct{}, 1), scroll, reEmit)
		close(done)
	}()

	waitForSnapshotCount(t, &snaps, 2)
	for len(reEmit) > 0 {
		<-reEmit
	}
	scroll <- struct{}{}
	waitForSnapshotCount(t, &snaps, 3)
	select {
	case <-reEmit:
	case <-time.After(time.Second):
		t.Fatal("scroll re-snapshot did not poke re-emit")
	}
	cancel()
	waitForPipelineStop(t, done)
}

func TestScrollSnapDue(t *testing.T) {
	t.Parallel()
	base := time.Now()
	cases := []struct {
		name string
		last time.Time
		now  time.Time
		want bool
	}{
		{"first poke fires", time.Time{}, base, true},
		{"inside cadence suppressed", base, base.Add(100 * time.Millisecond), false},
		{"at cadence fires", base, base.Add(scrollCadence), true},
		{"past cadence fires", base, base.Add(300 * time.Millisecond), true},
	}
	for _, tc := range cases {
		if got := scrollSnapDue(tc.last, tc.now, scrollCadence); got != tc.want {
			t.Errorf("%s: scrollSnapDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRunTextSyncScrollCadence(t *testing.T) {
	t.Parallel()
	var snaps atomic.Int64
	e := countingSnapshotEngine(&snaps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scroll := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, make(chan struct{}, 1), make(chan struct{}, 1), scroll, make(chan struct{}, 16))
		close(done)
	}()

	waitForSnapshotCount(t, &snaps, 2)
	step := scrollCadence + 20*time.Millisecond
	for range 3 {
		scroll <- struct{}{}
		time.Sleep(step)
	}
	waitForSnapshotCount(t, &snaps, 4)
	cancel()
	waitForPipelineStop(t, done)
}

func TestRunDecodeCollectorPokesScrollOnOffsetChange(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{backend: backendBlock}
	decoded := make(chan decodedFrame, 3)
	frames := make(chan render.Frame, 3)
	scroll := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.runDecodeCollector(ctx, decoded, frames, &textSync{}, scroll, make(chan struct{}, 1))
		close(done)
	}()

	pending := func(y float64) pendingFrame {
		return pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 2, 2)), meta: &page.ScreencastFrameMetadata{ScrollOffsetY: y}}
	}
	decoded <- decodedFrame{seq: 1, pending: pending(0)}
	recvFrame(t, frames)
	select {
	case <-scroll:
		t.Fatal("first frame poked scroll")
	case <-time.After(50 * time.Millisecond):
	}

	decoded <- decodedFrame{seq: 2, pending: pending(120)}
	recvFrame(t, frames)
	select {
	case <-scroll:
	case <-time.After(time.Second):
		t.Fatal("scroll offset change did not poke scroll")
	}

	decoded <- decodedFrame{seq: 3, pending: pending(120)}
	recvFrame(t, frames)
	select {
	case <-scroll:
		t.Fatal("unchanged offset poked scroll")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	waitForPipelineStop(t, done)
}

func countingSnapshotEngine(snaps *atomic.Int64) *ChromiumEngine {
	e := &ChromiumEngine{backend: backendBlock}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState) {
		snaps.Add(1)
		return nil, scrollOffset{}, 0, overlayState{}
	}
	return e
}

func waitForSnapshotCount(t *testing.T, count *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("snapshot count = %d, want at least %d", count.Load(), want)
}

func waitForPipelineStop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pipeline goroutine did not stop")
	}
}
