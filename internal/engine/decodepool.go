package engine

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
	renderkitty "github.com/wimpysworld/nyala/internal/render/kitty"
)

// decodeWorkersEnv overrides the live decode-worker count. It follows the NYALA_*
// diagnostic-switch convention, so the owner can sweep the pool size on hardware
// (for example NYALA_DECODE_WORKERS=2 while watching just fps backend=block). Unset
// or unparseable falls back to defaultDecodeWorkers.
const decodeWorkersEnv = "NYALA_DECODE_WORKERS"

// maxDecodeWorkers is the hard cap on the manual env override, so a stray large value
// cannot spawn an unbounded pool and starve the render, encode, and browser of cores.
const maxDecodeWorkers = 16

// maxDefaultDecodeWorkers caps the core-adaptive default. Four workers sustain the
// measured Block and Sixel pipelines while bounding the decoded-frame working set. A
// manual decodeWorkersEnv override can still select up to maxDecodeWorkers.
const maxDefaultDecodeWorkers = 4

// minDecodeWorkers is the floor default: two workers lock output at 60 on a strong CPU
// (the jpegn SIMD decode roughly halves a screencast decode, ~29 ms on a large block
// frame, so two clear the ~17 ms 60 fps gap with headroom) and oversubscribe nothing.
const minDecodeWorkers = 2

// defaultDecodeWorkers is the live decode-pool size when the env override is unset. It
// reads the visible cores once (GOMAXPROCS is cgroup-aware from Go 1.26, so it reflects a
// container's real quota) and derives the pool from adaptiveDecodeWorkers.
func defaultDecodeWorkers() int {
	return adaptiveDecodeWorkers(runtime.GOMAXPROCS(0))
}

// adaptiveDecodeWorkers derives the pool as min(maxDefaultDecodeWorkers,
// max(minDecodeWorkers, procs/2)). The decode workers take at most half the cores and
// leave capacity for rendering, terminal encoding, engine goroutines, and Chromium.
// The floor keeps a low-core box from using only one worker; the ceiling bounds the
// decoded-frame working set on a many-core box.
func adaptiveDecodeWorkers(procs int) int {
	n := procs / 2
	if n < minDecodeWorkers {
		return minDecodeWorkers
	}
	if n > maxDefaultDecodeWorkers {
		return maxDefaultDecodeWorkers
	}
	return n
}

// decodeWorkerCount reads the live decode-pool size from NYALA_DECODE_WORKERS, clamped to
// [1, maxDecodeWorkers], falling back to the core-adaptive defaultDecodeWorkers when unset
// or unparseable. It is read once per session on the live path only; capture keeps its
// single ordered worker.
func decodeWorkerCount() int {
	if v := os.Getenv(decodeWorkersEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			if n > maxDecodeWorkers {
				n = maxDecodeWorkers
			}
			return n
		}
	}
	return defaultDecodeWorkers()
}

// seqRawFrame is one undecoded screencast frame tagged with its arrival sequence
// number. The listener stamps seq in strict arrival order (a single-goroutine
// counter), so the collector can gate out a stale decode: a worker that finishes an
// older frame after a newer one has already published is dropped by comparing seq.
// It carries no ack id: the frame is acked on receive, before it enters the queue.
type seqRawFrame struct {
	seq  uint64                        // strictly increasing arrival sequence
	data string                        // base64 screencast payload, decoded by a worker
	meta *page.ScreencastFrameMetadata // scroll offset and page scale for the sync
}

// decodedFrame is one decoded screencast frame a worker hands to the collector,
// still tagged with its arrival sequence so the collector can drop a stale one.
type decodedFrame struct {
	seq     uint64
	pending pendingFrame
}

// directKittyPNG reports whether data can bypass the pixel decode and travel to a
// live Kitty consumer unchanged. It reads the backend at worker time, while the
// payload signature guards a format-switch race. A later backend switch is safe:
// the viewer decodes the retained payload when the active consumer is not Kitty.
func (e *ChromiumEngine) directKittyPNG(data string) bool {
	e.mu.Lock()
	kitty := e.backend == "kitty" && !e.captureOn
	e.mu.Unlock()
	return kitty && renderkitty.ValidDirectPNGData(data)
}

// rawFrameQueue is the bounded, drop-oldest feed from the listener goroutine to the
// live decode pool. It is the multi-consumer counterpart of frameSlot: several
// decode workers pull from it in parallel, so it holds a small backlog rather than a
// single latest-wins slot. When full it drops the OLDEST undecoded frame, never the
// newest and never blocking the listener, so Chromium's ack-on-receive credit and the
// 60 fps arrival stay unchanged while memory stays bounded. The draw path draws the
// newest published frame, so dropping an old undecoded frame under load only skips a
// frame that would have been superseded anyway. Capture does not use it; it keeps the
// ordered frameSlot.
type rawFrameQueue struct {
	items chan seqRawFrame
}

// newRawFrameQueue returns a queue bounded to depth undecoded frames.
func newRawFrameQueue(depth int) *rawFrameQueue {
	if depth < 1 {
		depth = 1
	}
	return &rawFrameQueue{items: make(chan seqRawFrame, depth)}
}

// put enqueues the newest frame, dropping the oldest when the queue is full. It never
// blocks the caller (the listener goroutine): a full queue drops one oldest frame and
// retries the non-blocking send. The listener is the only producer, so after a drop
// the retry always has room. A frame dropped here is already acked on receive.
func (q *rawFrameQueue) put(f seqRawFrame) {
	for {
		select {
		case q.items <- f:
			return
		default:
		}
		select {
		case <-q.items: // drop the oldest undecoded frame to make room
		default:
		}
	}
}

// runDecodeWorker is one worker of the live decode pool. It pulls raw frames from the
// shared queue, decodes each to a decoder-native bitmap with its own decode call (no shared
// destination buffer, so parallel workers never race a bitmap), and hands the decoded
// frame to the collector still tagged with its arrival sequence. The decode is the
// ~28 ms wall the pool parallelises; the collector serialises the cheap compose and
// publish. A corrupt frame fails to decode and is dropped (it was already acked on
// receive). It records the per-frame decode time into the ingest counter when armed,
// so in_decode_ms still measures per-frame decode under the pool. It exits when ctx is
// cancelled (session teardown). Capture never runs it: capture keeps runFrameLoop.
func (e *ChromiumEngine) runDecodeWorker(ctx context.Context, q *rawFrameQueue, out chan<- decodedFrame) {
	if ctx == nil || q == nil || out == nil {
		return
	}
	e.mu.Lock()
	decode := e.decode
	e.mu.Unlock()
	// A test-swapped e.decode keeps the old decoded-pixel hook. The production nil
	// case selects direct Kitty PNG or decodeLiveFrame per payload below.
	// Time the decode only when the ingest counter is armed. On the live path
	// captureOn is false, so the per-frame decodeMS the bundle records is never needed
	// here; the only clock read is the NYALA_FPS_TIMING decode budget.
	ingestArmed := e.ingest.armed()
	for {
		select {
		case <-ctx.Done():
			return
		case raw := <-q.items:
			var start time.Time
			if ingestArmed {
				start = time.Now()
			}
			pending := pendingFrame{meta: raw.meta}
			switch {
			case decode != nil:
				img, derr := decode(raw.data)
				if derr != nil {
					continue
				}
				pending.img = img
			case e.directKittyPNG(raw.data):
				// Strings are immutable and the event owns this value, so retaining it in
				// the held frame gives re-emits and queued draws stable bytes.
				pending.png = raw.data
			default:
				img, derr := e.decodeLiveFrame(raw.data)
				if derr != nil {
					continue
				}
				pending.img = img
			}
			if ingestArmed {
				e.ingest.recordDecode(msSince(start))
			}
			select {
			case out <- decodedFrame{seq: raw.seq, pending: pending}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// runDecodeCollector is the single serialising stage of the live decode pool. The
// workers decode in parallel and finish out of order, so this stage restores order and
// owns everything the frame loop kept serial: the monotonic publish gate, the held
// bitmap for re-emit, the scroll-offset tracking, the composed-overlay hold, and the
// frame send. It mirrors runFrameLoop's non-decode body exactly, so the live block
// path behaves as before apart from the parallel decode ahead of it.
//
// Monotonic ordering: each decoded frame carries its arrival sequence. A frame whose
// seq is not newer than the last published seq is dropped, so a worker that finished an
// older frame after a newer one already drew never replaces it on screen. The screen
// stays monotonic in time. Because only this one goroutine sends on the frames channel,
// the ordering, backpressure, and scroll tracking are identical to the single-worker
// frame loop; only the decode ran in parallel upstream.
//
// It also parks on the re-emit poke the snapshot goroutine sends after each publish and
// re-pushes its held bitmap recomposed with the fresh text, so a static page still
// repaints with text. It exits when ctx is cancelled.
func (e *ChromiumEngine) runDecodeCollector(ctx context.Context, in <-chan decodedFrame, frames chan render.Frame, ts *textSync, scroll chan<- struct{}, reEmit <-chan struct{}) {
	if ctx == nil || in == nil || frames == nil {
		return
	}
	// Read the per-session load timer once on the goroutine start ordering, so no
	// per-frame lock and no rebuild race with startSession's write. A nil timer is a
	// valid disabled timer.
	e.mu.Lock()
	timer := e.timing
	e.mu.Unlock()
	var held pendingFrame
	var haveHeld bool
	// lastScroll tracks the scroll offset of the last published frame so a change pokes
	// a re-snapshot. lastComposed holds the previous frame's composed overlay for one
	// frame when a bitmap's generation or scroll does not match the latest runs. Both
	// live on this goroutine, so the tracking is lock-free.
	var lastScroll scrollOffset
	var haveScroll bool
	var lastComposed []render.GlyphRun
	// lastPublishedSeq is the arrival sequence of the last frame the collector drew. A
	// decoded frame at or below it is stale (a slower worker finished an older frame
	// after a newer one drew) and is dropped, so the screen never steps backward in time.
	var lastPublishedSeq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-reEmit:
			// Text published with no new frame in flight: re-push the held bitmap so the
			// viewer repaints with the fresh text. Before the first frame there is nothing
			// to re-push, so the poke is a no-op; the first frame carries it.
			if !haveHeld {
				continue
			}
			if !e.emitFrame(ctx, frames, ts, held, &lastComposed) {
				return
			}
			timer.markFrame()
		case d := <-in:
			// Monotonic publish gate: drop a stale out-of-order decode.
			if d.seq <= lastPublishedSeq {
				continue
			}
			lastPublishedSeq = d.seq
			pending := d.pending
			held, haveHeld = pending, true
			off, offOK := frameScrollOffset(pending)
			if offOK {
				if !haveScroll || off != lastScroll {
					if haveScroll {
						pokeScroll(scroll)
					}
					lastScroll, haveScroll = off, true
				}
			}
			if !e.emitFrame(ctx, frames, ts, pending, &lastComposed) {
				return
			}
			timer.markFrame()
		}
	}
}
