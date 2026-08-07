package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
	renderkitty "github.com/wimpysworld/nyala/internal/render/kitty"
)

// seqDecode returns a decode hook that reads the arrival sequence back from the raw
// payload and yields a 1x1 bitmap whose single pixel red channel encodes that
// sequence, so a test can recover a published frame's sequence from its bitmap. It
// sleeps for the given per-sequence duration first, so a test can force a worker to
// finish an older frame after a newer one (the out-of-order stale case). The payload
// is the sequence number rendered as decimal ASCII.
func seqDecode(sleep func(seq int) time.Duration) func(string) (*image.RGBA, error) {
	return func(data string) (*image.RGBA, error) {
		seq := 0
		for _, c := range data {
			seq = seq*10 + int(c-'0')
		}
		if sleep != nil {
			if d := sleep(seq); d > 0 {
				time.Sleep(d)
			}
		}
		return seqImage(uint8(seq)), nil
	}
}

// seqOf recovers the sequence a seqDecode bitmap encodes from its single red channel.
func seqOf(img *image.RGBA) int { return int(img.RGBAAt(0, 0).R) }

func seqImage(seq uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.SetRGBA(0, 0, color.RGBA{R: seq, A: 255})
	return img
}

// drivePool starts a decode pool (queue, workers, collector) for a test and returns
// the queue to feed and the frames channel to read, plus a stop func. No browser is
// involved: the raw frames are fed straight into the queue.
func drivePool(t *testing.T, e *ChromiumEngine, workers int) (*rawFrameQueue, <-chan render.Frame, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rawQ := newRawFrameQueue(workers + 1)
	frames := make(chan render.Frame, 256)
	decoded := make(chan decodedFrame, workers)
	var wg sync.WaitGroup
	wg.Add(workers + 1)
	for range workers {
		go func() { defer wg.Done(); e.runDecodeWorker(ctx, rawQ, decoded) }()
	}
	go func() {
		defer wg.Done()
		e.runDecodeCollector(ctx, decoded, frames, &textSync{}, make(chan struct{}, 1), make(chan struct{}, 1))
	}()
	stop := func() {
		cancel()
		wg.Wait()
	}
	return rawQ, frames, stop
}

// TestDecodePoolNeverPublishesStaleFrame proves the monotonic ordering guarantee: a
// slower worker that finishes an older frame after a newer one has already published
// must have its frame dropped. It makes the decode of an early sequence sleep long and
// a later one return at once, so with two workers the later frame publishes first; the
// early frame must then be gated out. The published sequence must be strictly
// increasing.
func TestDecodePoolNeverPublishesStaleFrame(t *testing.T) {
	t.Parallel()
	// Sequence 1 decodes slowly, 2 fast: worker A grabs 1 and sleeps, worker B grabs 2
	// and returns first, so 2 publishes before 1 finishes. 1 must then be dropped.
	e := &ChromiumEngine{backend: backendBlock, decode: seqDecode(func(seq int) time.Duration {
		if seq == 1 {
			return 120 * time.Millisecond
		}
		return 0
	})}
	rawQ, frames, stop := drivePool(t, e, 2)
	defer stop()

	rawQ.put(seqRawFrame{seq: 1, data: "1"})
	// Let worker A pick up sequence 1 and start sleeping before 2 arrives.
	time.Sleep(10 * time.Millisecond)
	rawQ.put(seqRawFrame{seq: 2, data: "2"})

	// Read every frame that lands within a settle window, asserting strict increase.
	var last int
	deadline := time.After(400 * time.Millisecond)
	got := 0
loop:
	for {
		select {
		case f := <-frames:
			seq := seqOf(f.Img.(*image.RGBA))
			if seq <= last {
				t.Fatalf("published sequence %d after %d, want strictly increasing (a stale frame replaced a newer one)", seq, last)
			}
			last = seq
			got++
		case <-deadline:
			break loop
		}
	}
	if got == 0 {
		t.Fatal("no frame published")
	}
	if last != 2 {
		t.Fatalf("last published sequence = %d, want 2 (the newest frame is the resting frame)", last)
	}
}

// TestDecodePoolMonotonicUnderRace floods the pool with many frames whose decode times
// vary, on several workers, and asserts the published sequence never steps backward.
// Run with -race it also proves the queue, workers, and collector carry no data race.
func TestDecodePoolMonotonicUnderRace(t *testing.T) {
	t.Parallel()
	// Alternate slow and fast decodes so workers finish out of order and the seq gate is
	// exercised on nearly every frame.
	e := &ChromiumEngine{backend: backendBlock, decode: seqDecode(func(seq int) time.Duration {
		if seq%2 == 0 {
			return time.Millisecond
		}
		return 0
	})}
	rawQ, frames, stop := drivePool(t, e, 4)
	defer stop()

	const n = 200
	go func() {
		for i := 1; i <= n; i++ {
			rawQ.put(seqRawFrame{seq: uint64(i), data: itoa(i)})
			if i%8 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	var last int
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-frames:
			seq := seqOf(f.Img.(*image.RGBA))
			if seq <= last {
				t.Fatalf("published sequence %d after %d, want strictly increasing", seq, last)
			}
			last = seq
			if last == n {
				return
			}
		case <-deadline:
			// The drop-oldest queue may coalesce, so the last sequence need not be exactly
			// n; any strictly increasing run is a pass. Reaching here without a backward
			// step is fine.
			if last == 0 {
				t.Fatal("no frame published before deadline")
			}
			return
		}
	}
}

// TestCollectorDropsHeldBitmapOnGeometryChange proves the re-emit hold is
// geometry-tagged: after a simulated resize (the grid moves under the engine,
// as Resize does before restarting the stream), a re-emit poke must not re-push
// the held old-geometry bitmap onto the new grid. At unchanged geometry the
// same poke re-pushes the hold, so the static-page text repaint stays intact.
func TestCollectorDropsHeldBitmapOnGeometryChange(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{backend: backendBlock, cellW: 8, cellH: 16, cols: 80, rows: 24}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decoded := make(chan decodedFrame, 1)
	frames := make(chan render.Frame, 8)
	reEmit := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.runDecodeCollector(ctx, decoded, frames, &textSync{}, nil, reEmit)
	}()

	recv := func(want string) render.Frame {
		t.Helper()
		select {
		case f := <-frames:
			return f
		case <-time.After(time.Second):
			t.Fatalf("no frame published: %s", want)
			return render.Frame{}
		}
	}

	decoded <- decodedFrame{seq: 1, pending: pendingFrame{img: seqImage(1)}}
	recv("initial decoded frame")

	// Unchanged geometry: the poke re-pushes the held bitmap.
	reEmit <- struct{}{}
	if f := recv("re-emit at unchanged geometry"); seqOf(f.Img.(*image.RGBA)) != 1 {
		t.Fatal("re-emit did not re-push the held bitmap")
	}

	// Simulated resize: the grid moves under the engine, as Resize does before
	// restartStreamLocked pokes the snapshot goroutine.
	e.mu.Lock()
	e.cols, e.rows = 120, 40
	e.mu.Unlock()

	reEmit <- struct{}{}
	select {
	case f := <-frames:
		t.Fatalf("re-emit after a geometry change re-pushed the stale bitmap (seq %d)", seqOf(f.Img.(*image.RGBA)))
	case <-time.After(200 * time.Millisecond):
	}

	// The hold is dropped, not merely skipped: a later poke at the new geometry
	// still re-emits nothing until a fresh frame arrives and retags the hold.
	reEmit <- struct{}{}
	select {
	case <-frames:
		t.Fatal("dropped hold re-emitted after a second poke")
	case <-time.After(200 * time.Millisecond):
	}

	decoded <- decodedFrame{seq: 2, pending: pendingFrame{img: seqImage(2)}}
	recv("fresh frame at the new geometry")
	reEmit <- struct{}{}
	if f := recv("re-emit of the retagged hold"); seqOf(f.Img.(*image.RGBA)) != 2 {
		t.Fatal("retagged hold did not re-emit the fresh bitmap")
	}
	cancel()
	<-done
}

// TestTeardownSessionDropsCancelledCollectorFrames proves a torn-down live session
// cannot leave a decoded frame on the engine-lifetime channel for the replacement
// session's viewer to consume.
func TestTeardownSessionDropsCancelledCollectorFrames(t *testing.T) {
	for range 64 {
		e := &ChromiumEngine{backend: backendBlock, frames: make(chan render.Frame, frameChannelBuffer)}
		ctx, cancel := context.WithCancel(context.Background())
		e.ctx, e.cancel = ctx, cancel
		decoded := make(chan decodedFrame, 1)
		decoded <- decodedFrame{seq: 1, pending: pendingFrame{img: seqImage(1)}}
		done := make(chan struct{})
		e.framePublisherDone = done
		go func() {
			defer close(done)
			e.runDecodeCollector(ctx, decoded, e.frames, &textSync{}, nil, nil)
		}()

		teardownDone := make(chan struct{})
		go func() {
			e.teardownSession()
			close(teardownDone)
		}()
		select {
		case <-teardownDone:
		case <-time.After(time.Second):
			t.Fatal("teardown deadlocked waiting for the live collector")
		}

		e.frames <- render.Frame{Img: seqImage(2)}
		select {
		case got := <-e.frames:
			if seq := seqOf(got.Img.(*image.RGBA)); seq != 2 {
				t.Fatalf("replacement received sequence %d, want 2; cancelled session left a stale frame", seq)
			}
		case <-time.After(time.Second):
			t.Fatal("replacement frame was not available after teardown")
		}
	}
}

// TestAdaptiveDecodeWorkers proves the core-adaptive default is procs/2 bounded to
// [minDecodeWorkers, maxDefaultDecodeWorkers]. It takes the core count as an argument,
// so it does not depend on the test box's cores.
func TestAdaptiveDecodeWorkers(t *testing.T) {
	t.Parallel()
	if maxDefaultDecodeWorkers != 4 {
		t.Fatalf("maxDefaultDecodeWorkers = %d, want 4", maxDefaultDecodeWorkers)
	}
	cases := []struct {
		procs, want int
	}{
		{0, minDecodeWorkers},         // degenerate: floors to 2
		{1, minDecodeWorkers},         // one core: floors to 2
		{2, minDecodeWorkers},         // two cores: half is 1, floors to 2
		{3, minDecodeWorkers},         // three cores: half is 1, floors to 2 (low-core heuristic)
		{4, 2},                        // half the cores
		{6, 3},                        // half the cores below the ceiling
		{8, maxDefaultDecodeWorkers},  // half the cores
		{16, maxDefaultDecodeWorkers}, // adaptive ceiling
		{32, maxDefaultDecodeWorkers}, // adaptive ceiling on a many-core box
		{64, maxDefaultDecodeWorkers}, // adaptive ceiling remains fixed
	}
	for _, c := range cases {
		if got := adaptiveDecodeWorkers(c.procs); got != c.want {
			t.Errorf("adaptiveDecodeWorkers(%d) = %d, want %d", c.procs, got, c.want)
		}
		if got := adaptiveDecodeWorkers(c.procs); got < minDecodeWorkers {
			t.Errorf("adaptiveDecodeWorkers(%d) = %d, below the floor %d", c.procs, got, minDecodeWorkers)
		}
	}
}

// TestDecodeWorkerCountManualOverride proves the diagnostic override remains manual:
// it can select counts above the adaptive ceiling up to the existing hard maximum.
func TestDecodeWorkerCountManualOverride(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"1", 1},
		{"4", 4},
		{"5", 5},
		{"16", 16},
		{"17", maxDecodeWorkers},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(decodeWorkersEnv, tc.value)
			if got := decodeWorkerCount(); got != tc.want {
				t.Fatalf("decodeWorkerCount() = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("unset adaptive", func(t *testing.T) {
		t.Setenv(decodeWorkersEnv, "")
		if got := decodeWorkerCount(); got != defaultDecodeWorkers() {
			t.Fatalf("decodeWorkerCount() = %d, want adaptive default %d", got, defaultDecodeWorkers())
		}
	})
}

// itoa renders a small non-negative int as decimal ASCII, the seqDecode payload.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestDecodePoolByteIdenticalToSerial proves a raw payload decoded through the pool is
// byte-identical to the same payload decoded by the serial native path.
// Each worker owns its own decode call and destination bitmap, so parallelism cannot
// alter the pixels. It feeds a real base64 PNG (production decode, not a hook).
func TestDecodePoolByteIdenticalToSerial(t *testing.T) {
	t.Parallel()
	payload := pngPayload(t)
	want, err := decodeScreencastNative(payload)
	if err != nil {
		t.Fatalf("serial decode failed: %v", err)
	}

	e := &ChromiumEngine{backend: backendBlock} // nil decode: workers use the native decoder
	rawQ, frames, stop := drivePool(t, e, 3)
	defer stop()

	rawQ.put(seqRawFrame{seq: 1, data: payload})
	got := recvFrame(t, frames)
	if !got.Img.Bounds().Eq(want.Bounds()) {
		t.Fatalf("pool-decoded bounds = %v, want %v", got.Img.Bounds(), want.Bounds())
	}
	for y := want.Bounds().Min.Y; y < want.Bounds().Max.Y; y++ {
		for x := want.Bounds().Min.X; x < want.Bounds().Max.X; x++ {
			if got.Img.At(x, y) != want.At(x, y) {
				t.Fatalf("pool-decoded pixel (%d,%d) differs from serial decode", x, y)
			}
		}
	}
}

// TestDecodePoolKittyPassThroughAndPixelDecode pins the worker selection. A valid
// direct Kitty PNG remains the exact CDP string, while a pixel backend decodes JPEG.
func TestDecodePoolKittyPassThroughAndPixelDecode(t *testing.T) {
	t.Run("PNG identity", func(t *testing.T) {
		payload := pngPayload(t)
		e := &ChromiumEngine{
			backend:       backendKitty,
			screencastFmt: screencastFmt{format: page.ScreencastFormatPng},
		}
		rawQ, frames, stop := drivePool(t, e, 1)
		defer stop()
		rawQ.put(seqRawFrame{seq: 1, data: payload})

		got := recvFrame(t, frames)
		if got.Img != nil {
			t.Fatalf("direct Kitty PNG decoded to %T, want no pixel image", got.Img)
		}
		if got.EncodedPNG != payload {
			t.Fatal("direct Kitty PNG payload differs from the CDP string")
		}
	})

	t.Run("pixel backend JPEG", func(t *testing.T) {
		payload := encodeBase64JPEG(t)
		e := &ChromiumEngine{backend: backendSixel}
		rawQ, frames, stop := drivePool(t, e, 1)
		defer stop()
		rawQ.put(seqRawFrame{seq: 1, data: payload})

		got := recvFrame(t, frames)
		if got.Img == nil {
			t.Fatal("pixel backend JPEG frame has no decoded pixels")
		}
		if got.EncodedPNG != "" {
			t.Fatal("pixel backend JPEG frame retained a direct PNG payload")
		}
	})
}

func TestDecodeWorkerRejectsInvalidActiveDirectKittyBeforeDecode(t *testing.T) {
	decodeCalls := atomic.Uint64{}
	ingest := &ingestCounter{on: true, window: ingestWindow}
	ingest.record(time.Now())
	e := &ChromiumEngine{
		backend: backendKitty,
		ingest:  ingest,
		decode: func(string) (*image.RGBA, error) {
			decodeCalls.Add(1)
			return seqImage(1), nil
		},
	}
	if direct, err := e.directKittyPayload("not-a-png"); !direct {
		t.Fatal("active direct Kitty transport was not selected")
	} else if !errors.Is(err, renderkitty.ErrInvalidPNGData) {
		t.Fatalf("directKittyPayload() error = %v, want ErrInvalidPNGData", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := newRawFrameQueue(1)
	out := make(chan decodedFrame, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.runDecodeWorker(ctx, q, out)
	}()
	q.put(seqRawFrame{seq: 1, data: "not-a-png"})
	waitDecodeDrops(t, ingest, 1)
	cancel()
	<-done

	if got := decodeCalls.Load(); got != 0 {
		t.Fatalf("override decoder calls = %d, want 0", got)
	}
	select {
	case got := <-out:
		t.Fatalf("invalid direct payload emitted frame %+v", got)
	default:
	}
	stats := ingest.summary()
	if stats.DecodeDrops != 1 {
		t.Fatalf("decode drops = %d, want 1", stats.DecodeDrops)
	}
	if stats.DecodeFirstError != "engine: direct Kitty PNG validation: kitty: invalid base64 PNG payload" {
		t.Fatalf("first decode error = %q, want direct Kitty validation context", stats.DecodeFirstError)
	}
}

func TestDecodeWorkerCountsEachDecoderFailureOnce(t *testing.T) {
	corruptPNG := base64.StdEncoding.EncodeToString(append(append([]byte(nil), pngSignature...), "broken"...))
	corruptJPEG := base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xdb, 0x00})
	tests := []struct {
		name       string
		engine     func() *ChromiumEngine
		data       string
		wantPrefix string
	}{
		{
			name: "override",
			engine: func() *ChromiumEngine {
				return &ChromiumEngine{backend: backendBlock, decode: func(string) (*image.RGBA, error) {
					return nil, errors.New("override decode failed")
				}}
			},
			data:       "override-payload",
			wantPrefix: "override decode failed",
		},
		{
			name:       "live PNG",
			engine:     func() *ChromiumEngine { return &ChromiumEngine{backend: backendBlock} },
			data:       corruptPNG,
			wantPrefix: "engine: live frame decode: native image decode: png decode:",
		},
		{
			name:       "live JPEG",
			engine:     func() *ChromiumEngine { return &ChromiumEngine{backend: backendSixel} },
			data:       corruptJPEG,
			wantPrefix: "engine: live frame decode: native image decode: jpeg decode:",
		},
		{
			name: "Kitty plane",
			engine: func() *ChromiumEngine {
				return &ChromiumEngine{backend: backendKitty, kittyDecode: true}
			},
			data:       base64.StdEncoding.EncodeToString([]byte("not-an-image")),
			wantPrefix: errUnknownPlaneFormat.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := test.engine()
			e.ingest = &ingestCounter{on: true, window: ingestWindow}
			e.ingest.record(time.Now())
			ctx, cancel := context.WithCancel(context.Background())
			q := newRawFrameQueue(1)
			out := make(chan decodedFrame, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				e.runDecodeWorker(ctx, q, out)
			}()
			q.put(seqRawFrame{seq: 1, data: test.data})
			waitDecodeDrops(t, e.ingest, 1)
			cancel()
			<-done

			stats := e.ingest.summary()
			if stats.DecodeDrops != 1 {
				t.Fatalf("decode drops = %d, want 1", stats.DecodeDrops)
			}
			if !bytes.HasPrefix([]byte(stats.DecodeFirstError), []byte(test.wantPrefix)) {
				t.Fatalf("first decode error = %q, want prefix %q", stats.DecodeFirstError, test.wantPrefix)
			}
			select {
			case frame := <-out:
				t.Fatalf("failed decode emitted frame %+v", frame)
			default:
			}
		})
	}
}

func TestDecodePoolNonDecodeDropsDoNotChangeStats(t *testing.T) {
	t.Run("raw queue pressure", func(t *testing.T) {
		ingest := &ingestCounter{on: true, window: ingestWindow}
		q := newRawFrameQueue(1)
		q.put(seqRawFrame{seq: 1})
		q.put(seqRawFrame{seq: 2})
		if got := ingest.decodeDrops.Load(); got != 0 {
			t.Fatalf("decode drops = %d, want 0", got)
		}
	})

	t.Run("collector stale drop", func(t *testing.T) {
		ingest := &ingestCounter{on: true, window: ingestWindow}
		e := &ChromiumEngine{backend: backendBlock, ingest: ingest}
		ctx, cancel := context.WithCancel(context.Background())
		in := make(chan decodedFrame, 2)
		frames := make(chan render.Frame, 2)
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.runDecodeCollector(ctx, in, frames, &textSync{}, nil, nil)
		}()
		in <- decodedFrame{seq: 2, pending: pendingFrame{img: seqImage(2)}}
		<-frames
		in <- decodedFrame{seq: 1, pending: pendingFrame{img: seqImage(1)}}
		time.Sleep(10 * time.Millisecond)
		cancel()
		<-done
		if got := ingest.decodeDrops.Load(); got != 0 {
			t.Fatalf("decode drops = %d, want 0", got)
		}
	})

	t.Run("worker cancellation", func(t *testing.T) {
		ingest := &ingestCounter{on: true, window: ingestWindow}
		e := &ChromiumEngine{backend: backendBlock, ingest: ingest, decode: seqDecode(nil)}
		ctx, cancel := context.WithCancel(context.Background())
		q := newRawFrameQueue(1)
		out := make(chan decodedFrame)
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.runDecodeWorker(ctx, q, out)
		}()
		q.put(seqRawFrame{seq: 1, data: "1"})
		cancel()
		<-done
		if got := ingest.decodeDrops.Load(); got != 0 {
			t.Fatalf("decode drops = %d, want 0", got)
		}
	})
}

func waitDecodeDrops(t *testing.T, ingest *ingestCounter, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for ingest.decodeDrops.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("decode drops = %d, want %d", ingest.decodeDrops.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
	// Give the worker time to expose an accidental second record for this frame.
	time.Sleep(10 * time.Millisecond)
	if got := ingest.decodeDrops.Load(); got != want {
		t.Fatalf("decode drops = %d after settle, want %d", got, want)
	}
}

// TestDecodePoolSixelDecodesJPEG pins the Sixel worker route for the live JPEG
// q90 capture: the signature sniff sends the payload through the jpegn decode,
// the frame carries decoded pixels rather than a retained payload, and the
// colour lands in RGBA channel order. JPEG is lossy, so the channels compare
// with a tolerance while R must stay the dominant channel.
func TestDecodePoolSixelDecodesJPEG(t *testing.T) {
	payload := encodeBase64JPEG(t)
	e := &ChromiumEngine{backend: backendSixel}
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	rawQ.put(seqRawFrame{seq: 1, data: payload})

	got := recvFrame(t, frames)
	if got.Img == nil {
		t.Fatal("Sixel JPEG frame has no decoded pixels")
	}
	if got.EncodedPNG != "" {
		t.Fatal("Sixel JPEG frame retained a direct payload")
	}
	pixel := imageToRGBA(got.Img).RGBAAt(0, 0)
	const tol = 40
	if absDiff(pixel.R, 200) > tol || absDiff(pixel.G, 10) > tol || absDiff(pixel.B, 20) > tol {
		t.Fatalf("pixel (0,0) = %+v, want approx R=200 G=10 B=20 (RGBA order, tol %d)", pixel, tol)
	}
	if pixel.R <= pixel.G || pixel.R <= pixel.B {
		t.Fatalf("pixel (0,0) = %+v, R must dominate G and B (RGBA order, not BGRA)", pixel)
	}
}

func TestDirectKittyPNGRejectsInvalidRawData(t *testing.T) {
	valid := pngPayload(t)
	raw, err := base64.StdEncoding.DecodeString(valid)
	if err != nil {
		t.Fatalf("decode test PNG: %v", err)
	}
	cases := map[string]string{
		"invalid prefix":   "not-a-png",
		"invalid length":   valid + "A",
		"invalid alphabet": valid[:len(valid)/2] + "!" + valid[len(valid)/2+1:],
		"invalid padding":  valid[:16] + "=" + valid[17:],
		"truncated PNG":    base64.StdEncoding.EncodeToString(raw[:len(raw)-12]),
		"invalid structure": base64.StdEncoding.EncodeToString(func() []byte {
			corrupt := bytes.Clone(raw)
			corrupt[11] = 12 // IHDR must carry exactly 13 data bytes.
			return corrupt
		}()),
		"invalid CRC": base64.StdEncoding.EncodeToString(func() []byte {
			corrupt := bytes.Clone(raw)
			corrupt[29] ^= 1 // First chunk CRC follows signature, header, and IHDR data.
			return corrupt
		}()),
	}
	e := &ChromiumEngine{backend: backendKitty}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if e.directKittyPNG(data) {
				t.Fatal("invalid payload entered the direct Kitty path")
			}
		})
	}
	e.backend = backendSixel
	if e.directKittyPNG(valid) {
		t.Fatal("pixel backend entered the direct Kitty path")
	}
	e.backend = backendKitty
	if !e.directKittyPNG(valid) {
		t.Fatal("valid Kitty PNG missed the direct path")
	}
}

// pngPayload encodes a small gradient RGBA image to a base64 PNG, a stand-in for one
// screencast payload the production decode path accepts.
func pngPayload(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for y := range 6 {
		for x := range 8 {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: uint8(x + y), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestRawFrameQueueDropsOldest proves the bounded queue drops the oldest undecoded
// frame when full and never blocks the producer, so the listener stays free while
// memory stays bounded. It fills past capacity with no consumer, then drains and
// asserts the retained frames are the newest ones in order.
func TestRawFrameQueueDropsOldest(t *testing.T) {
	t.Parallel()
	q := newRawFrameQueue(3)
	for i := 1; i <= 6; i++ {
		q.put(seqRawFrame{seq: uint64(i), data: itoa(i)})
	}
	// Capacity 3, six pushed: the three newest (4,5,6) survive in order.
	var got []uint64
	for {
		select {
		case f := <-q.items:
			got = append(got, f.seq)
		default:
			if want := []uint64{4, 5, 6}; !equalU64(got, want) {
				t.Fatalf("queue retained %v, want the newest %v", got, want)
			}
			return
		}
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandleScreencastFrameLiveAcksAndEnqueues proves the live listener arm acks each
// delivered frame on receive (before any decode) and enqueues the raw frame tagged
// with its arrival sequence, preserving the ack-exactly-once contract.
func TestHandleScreencastFrameLiveAcksAndEnqueues(t *testing.T) {
	t.Parallel()
	acks := newAckQueue()
	e := &ChromiumEngine{acks: acks}
	q := newRawFrameQueue(4)

	e.handleScreencastFrameLive(q, 7, &page.EventScreencastFrame{Data: "payload", SessionID: 9})

	got, ok := acks.pop()
	if !ok {
		t.Fatal("live arm did not ack on receive")
	}
	if got != 9 {
		t.Fatalf("acked SessionID %d, want 9", got)
	}
	select {
	case f := <-q.items:
		if f.seq != 7 || f.data != "payload" {
			t.Fatalf("queued (seq %d, %q), want (7, payload)", f.seq, f.data)
		}
	default:
		t.Fatal("live arm did not enqueue the raw frame")
	}
}

// BenchmarkDecodePoolThroughput shows decode throughput scales with the worker count:
// N frames whose decode each costs a fixed wall time complete in about one frame's
// wall time on N workers, not N frames' worth. It is the design confirmation; the real
// fps lift is verified by the owner on hardware (just fps backend=block, aquarium).
// Run it with, for example: go test -run x -bench DecodePoolThroughput ./internal/engine/
func BenchmarkDecodePoolThroughput(b *testing.B) {
	const perDecode = 5 * time.Millisecond
	for _, workers := range []int{1, 2, 3, 4} {
		b.Run("workers="+itoa(workers), func(b *testing.B) {
			e := &ChromiumEngine{backend: backendBlock, decode: seqDecode(func(int) time.Duration { return perDecode })}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rawQ := newRawFrameQueue(b.N + workers + 1)
			decoded := make(chan decodedFrame, b.N+workers)
			for range workers {
				go e.runDecodeWorker(ctx, rawQ, decoded)
			}
			var done int64
			b.ResetTimer()
			go func() {
				for i := 1; i <= b.N; i++ {
					rawQ.put(seqRawFrame{seq: uint64(i), data: itoa(i)})
				}
			}()
			for range b.N {
				<-decoded
				atomic.AddInt64(&done, 1)
			}
			b.StopTimer()
			// ns/op falls toward perDecode/workers as workers rise: the pool clears the
			// backlog in parallel rather than serially.
		})
	}
}
