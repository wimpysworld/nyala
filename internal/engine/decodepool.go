package engine

import (
	"context"
	"fmt"
	"image"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
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
// or unparseable. It is read once per live session.
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
	seq      uint64                        // strictly increasing arrival sequence
	data     string                        // base64 screencast payload, decoded by a worker
	meta     *page.ScreencastFrameMetadata // scroll offset and page scale for the sync
	afterFCP bool                          // received after main-frame first contentful paint
}

// decodedFrame is one decoded screencast frame a worker hands to the collector,
// still tagged with its arrival sequence so the collector can drop a stale one.
type decodedFrame struct {
	seq     uint64
	pending pendingFrame
}

// directKittyPayload separates the active direct transport from payload validity.
// The worker uses this distinction to reject corrupt direct PNG data before any
// override or live pixel decoder can accept it.
func (e *ChromiumEngine) directKittyPayload(data string) (bool, error) {
	e.mu.Lock()
	direct := e.backend.UsesDirectPayload() && !e.kittyDecode
	e.mu.Unlock()
	if !direct {
		return false, nil
	}
	if !renderkitty.ValidDirectPNGData(data) {
		return true, fmt.Errorf("engine: direct Kitty PNG validation: %w", renderkitty.ErrInvalidPNGData)
	}
	return true, nil
}

// SetKittyDecode selects the kitty-shm decode mode. When on, frames that would
// travel as direct Kitty PNG payloads instead decode into RGBA planes
// (render.Frame.Img plus a Release hook) so the viewer can copy raw pixels
// into a shared-memory object at write time. The viewer sets the mode before
// the initial stream start and before each backend change. The next stream
// start then selects JPEG q90 for shm or PNG for direct transport. The viewer
// must call Frame.Release exactly once per plane frame at its drop and
// post-paint points (Release is nil-safe, and a JPEG plane carries none).
func (e *ChromiumEngine) SetKittyDecode(on bool) {
	e.mu.Lock()
	e.kittyDecode = on
	e.mu.Unlock()
}

// kittyPlaneMode reports whether the worker decodes payloads into RGBA planes
// for the kitty-shm transport: the active backend takes direct payloads and
// the viewer selected shm decode. It reads both values under one lock.
func (e *ChromiumEngine) kittyPlaneMode() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kittyDecode && e.backend.UsesDirectPayload()
}

// framePlane is one pooled RGBA plane. Its release closure is built once per
// entry, so releasing and reusing a plane allocates nothing per frame.
type framePlane struct {
	img      image.RGBA
	release  func()
	released atomic.Bool
}

// planePool is the bounded recycler for kitty-shm RGBA planes. It is a
// buffered channel: get pops a free plane or grows a new one, put returns a
// plane and drops it for the GC when the pool is already full, so the retained
// set never exceeds the configured depth.
type planePool struct {
	items chan *framePlane
}

// newPlanePool returns a pool retaining at most depth planes.
func newPlanePool(depth int) *planePool {
	if depth < 1 {
		depth = 1
	}
	return &planePool{items: make(chan *framePlane, depth)}
}

// get returns a plane shaped w by h, reusing a pooled backing array when its
// capacity suffices. The plane is tightly packed RGBA (stride 4*w).
func (p *planePool) get(w, h int) *framePlane {
	var fp *framePlane
	select {
	case fp = <-p.items:
	default:
		fp = &framePlane{}
		fp.release = func() { p.put(fp) }
	}
	need := 4 * w * h
	if cap(fp.img.Pix) < need {
		fp.img.Pix = make([]byte, need)
	}
	fp.img.Pix = fp.img.Pix[:need]
	fp.img.Stride = 4 * w
	fp.img.Rect = image.Rect(0, 0, w, h)
	fp.released.Store(false)
	return fp
}

// put returns a plane to the pool. The released flag makes the release hook
// exactly-once: a second call is a no-op, so a lifecycle bug can never hand
// the same plane to two frames.
func (p *planePool) put(fp *framePlane) {
	if fp.released.Swap(true) {
		return
	}
	select {
	case p.items <- fp:
	default: // pool at its ceiling: drop the plane, the GC reclaims it
	}
}

// livePlanePool returns the process-wide kitty-shm plane pool, created on
// first use. Its depth is the in-flight plane ceiling, derived from the
// decode-pool constants: one plane per decode worker, the raw-queue depth
// (workers+1, headroom only, since queued frames are still encoded), the
// frames channel buffer, plus one pending and one drawing at the viewer.
var livePlanePool = sync.OnceValue(func() *planePool {
	workers := decodeWorkerCount()
	return newPlanePool(workers + (workers + 1) + frameChannelBuffer + 2)
})

// rawFrameQueue is the bounded, drop-oldest feed from the listener goroutine to the
// live decode pool. Several decode workers pull from it in parallel, so it holds a
// small backlog. When full it drops the OLDEST undecoded frame, never the
// newest and never blocking the listener, so Chromium's ack-on-receive credit and the
// 60 fps arrival stay unchanged while memory stays bounded. The draw path draws the
// newest published frame, so dropping an old undecoded frame under load only skips a
// frame that would have been superseded anyway.
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
// the retry always has room. A frame dropped here is already acked on receive, and it
// is still encoded, so it holds no pooled plane and needs no release.
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
// cancelled during session teardown.
func (e *ChromiumEngine) runDecodeWorker(ctx context.Context, q *rawFrameQueue, out chan<- decodedFrame) {
	if ctx == nil || q == nil || out == nil {
		return
	}
	e.mu.Lock()
	decode := e.decode
	e.mu.Unlock()
	// A test-swapped e.decode keeps the old decoded-pixel hook. The production nil
	// case selects direct Kitty PNG or decodeLiveFrame per payload below.
	// Time the decode only when the ingest counter is armed for the
	// NYALA_FPS_TIMING decode budget.
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
			pending := pendingFrame{meta: raw.meta, afterFCP: raw.afterFCP}
			if e.kittyPlaneMode() {
				// kitty-shm mode: decode into an RGBA plane the viewer copies
				// into shared memory, sniffing the payload signature so both
				// supported formats convert. A pooled PNG plane's release hook is
				// single-owner and travels with the frame; a JPEG plane carries
				// none. A failed decode drops the frame, like the pixel decodes
				// below (it was acked on receive).
				img, release, derr := e.decodeKittyPlane(raw.data)
				if derr != nil {
					e.ingest.recordDecodeDrop(derr)
					continue
				}
				pending.img = img
				pending.release = release
			} else {
				direct, derr := e.directKittyPayload(raw.data)
				if derr != nil {
					e.ingest.recordDecodeDrop(derr)
					continue
				}
				switch {
				case direct:
					// Strings are immutable and the event owns this value, so retaining
					// it in the held frame gives re-emits and queued draws stable bytes.
					pending.png = raw.data
				case decode != nil:
					img, decodeErr := decode(raw.data)
					if decodeErr != nil {
						e.ingest.recordDecodeDrop(decodeErr)
						continue
					}
					pending.img = img
				default:
					img, decodeErr := e.decodeLiveFrame(raw.data)
					if decodeErr != nil {
						e.ingest.recordDecodeDrop(decodeErr)
						continue
					}
					pending.img = img
				}
			}
			if ingestArmed {
				e.ingest.recordDecode(msSince(start))
			}
			select {
			case out <- decodedFrame{seq: raw.seq, pending: pending}:
			case <-ctx.Done():
				// Teardown drop point: the frame never reaches the collector,
				// so return its pooled plane (nil-safe off the kitty-shm path).
				pending.releasePlane()
				return
			}
		}
	}
}

// runDecodePipeline joins the collector and every decode worker before marking the
// session pipeline done. This makes decode failures which finish during cancellation
// visible before teardown returns and its caller reads the session summary.
func (e *ChromiumEngine) runDecodePipeline(ctx context.Context, q *rawFrameQueue, decoded chan decodedFrame, frames chan render.Frame, ts *textSync, scroll chan<- struct{}, reEmit <-chan struct{}, workerCount int, done chan<- struct{}) {
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			e.runDecodeWorker(ctx, q, decoded)
		}()
	}
	e.runDecodeCollector(ctx, decoded, frames, ts, scroll, reEmit)
	workers.Wait()
	close(done)
}

// captureGeom returns the capture geometry the engine currently expects: the
// screencast cap for the held grid, cell pair, and backend, read under mu. The
// collector tags its held re-emit bitmap with it, so a bitmap captured for an
// older grid is dropped rather than recomposed onto a resized one.
func (e *ChromiumEngine) captureGeom() (w, h int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.screencastCap(e.cols, e.rows)
}

// runDecodeCollector is the single serialising stage of the live decode pool. The
// workers decode in parallel and finish out of order, so this stage restores order and
// owns everything the frame loop kept serial: the monotonic publish gate, the held
// bitmap for re-emit, the scroll-offset tracking, the composed-overlay hold, and the
// frame send, preserving ordered publication after parallel decode.
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
	// heldGeomW, heldGeomH tag the held bitmap with the capture geometry the
	// engine expected when the collector took it. A resize or backend switch moves
	// that geometry before the first fresh frame lands, and the restart's snapshot
	// poke sends a re-emit in exactly that window; re-pushing then recomposes the
	// old-geometry bitmap onto the already-resized grid (the transient
	// misalignment after a zoom). The re-emit arm compares the tag against the
	// current geometry and drops a stale hold instead.
	var heldGeomW, heldGeomH int
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
			if w, h := e.captureGeom(); w != heldGeomW || h != heldGeomH {
				// The grid moved since this bitmap arrived: drop the stale hold so the
				// next fresh frame repaints instead of recomposing old-geometry pixels
				// onto the new grid. A held frame never carries a pooled plane (the
				// plane path clears the hold), so there is nothing to release.
				held, haveHeld = pendingFrame{}, false
				continue
			}
			if !e.emitFrame(ctx, frames, ts, held, &lastComposed) {
				return
			}
			timer.markFrame()
		case d := <-in:
			// Monotonic publish gate: drop a stale out-of-order decode, returning
			// its pooled plane (nil-safe off the kitty-shm path).
			if d.seq <= lastPublishedSeq {
				d.pending.releasePlane()
				continue
			}
			lastPublishedSeq = d.seq
			pending := d.pending
			if pending.release != nil {
				// A pooled plane is single-owner: the viewer releases it after
				// paint or on drop, so the collector must not retain it for
				// re-emit. Kitty ignores the text overlay, so losing the re-emit
				// repaint costs nothing on the kitty-shm path; clearing the hold
				// also stops a poke from re-pushing an older non-plane frame.
				held, haveHeld = pendingFrame{}, false
			} else {
				held, haveHeld = pending, true
				heldGeomW, heldGeomH = e.captureGeom()
			}
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
