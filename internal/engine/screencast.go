package engine

import (
	"context"
	"fmt"
	"image"
	"math"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render"
)

// msSince returns the milliseconds elapsed since start as a wall-clock duration.
// It is never negative because time.Since uses the monotonic clock.
func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

// pendingFrame is one decoded screencast frame the worker produces off the
// listener goroutine and hands to compose. It carries no ack id: the frame was
// already acknowledged on receipt, before decode.
type pendingFrame struct {
	img      image.Image                   // decoded bitmap in its decoder-native layout
	png      string                        // owned base64 PNG for direct live Kitty transmission
	release  func()                        // returns the pooled kitty-shm PNG plane img wraps; nil elsewhere, including GC-owned JPEG planes
	meta     *page.ScreencastFrameMetadata // scroll offset and page scale for the sync
	afterFCP bool                          // received after main-frame first contentful paint
}

// releasePlane returns the frame's pooled kitty-shm plane at an engine-side
// drop point. It is nil-safe: frames off the kitty-shm path carry no release
// hook and are unaffected.
func (p pendingFrame) releasePlane() {
	if p.release != nil {
		p.release()
	}
}

// handleScreencastFrameLive acknowledges a frame on receipt and queues it for
// parallel decode, tagged with its arrival sequence for ordered publication.
func (e *ChromiumEngine) handleScreencastFrameLive(q *rawFrameQueue, seq uint64, f *page.EventScreencastFrame) {
	afterFCP := e.afterMainFrameFCP()
	e.receiveScreencastFrame(f, afterFCP)
	q.put(seqRawFrame{seq: seq, data: f.Data, meta: f.Metadata, afterFCP: afterFCP})
}

// receiveScreencastFrame counts true ingest before queue coalescing and
// acknowledges the frame. The
// ack lands on receive, before any decode, so Chrome's in-flight credit is released at
// once and decoupled from decode and any downstream stall. It runs on the chromedp
// ListenTarget event goroutine, so it does no PNG decode and no chromedp.Run: the ack
// is enqueued for ackPump to run off this goroutine (the #920-safe off-goroutine ack).
func (e *ChromiumEngine) receiveScreencastFrame(f *page.EventScreencastFrame, afterFCP ...bool) {
	// Count true ingest for every delivered frame, before any coalescing or drop.
	// Gated behind the armed check so an off run pays one bool check and no clock read.
	count := len(afterFCP) == 0 || afterFCP[0]
	if count && e.ingest.armed() {
		e.ingest.record(time.Now())
	}
	// Ack on receive, before any decode. The enqueue is #920-safe: ackPump runs the
	// chromedp.Run off this event goroutine. Whether this frame is later decoded or
	// dropped, it is acked exactly once, here.
	e.ackFrame(f.SessionID)
}

// registerScreencastListener registers the session listener that acknowledges each
// frame on receipt and feeds the live decode queue with an arrival sequence.
func (e *ChromiumEngine) registerScreencastListener(ctx context.Context, rawQ *rawFrameQueue) {
	if ctx == nil || rawQ == nil {
		return
	}
	var seq uint64
	chromedp.ListenTarget(ctx, func(ev any) {
		if f, ok := ev.(*page.EventScreencastFrame); ok {
			seq++
			e.handleScreencastFrameLive(rawQ, seq, f)
		}
	})
}

// screencastFmt is the current screencast wire format and its JPEG quality. quality
// applies only to the JPEG format; the PNG format is lossless and ignores it. The
// engine holds it under e.mu and updates it only after StartScreencast succeeds.
type screencastFmt struct {
	format  page.ScreencastFormat // page.ScreencastFormatPng or ...Jpeg
	quality int                   // JPEG quality (0-100); ignored for PNG
}

// startScreencast starts or restarts the screencast at the active frame dimensions
// in fmtSel. The frame listener is
// registered once per session by registerScreencastListener, so this issues only the
// CDP StartScreencast command; restartStream re-issues it without leaking a second
// listener. It sets the JPEG quality only for Block's JPEG format. The stream
// couples to the consume rate because Chrome withholds frames until acked.
func (e *ChromiumEngine) startScreencast(ctx context.Context, capW, capH int, fmtSel screencastFmt) error {
	if ctx == nil {
		return fmt.Errorf("engine: start screencast: no browser context")
	}
	if fmtSel.format == "" {
		fmtSel.format = page.ScreencastFormatPng
	}
	action := page.StartScreencast().
		WithFormat(fmtSel.format).
		WithMaxWidth(int64(capW)).
		WithMaxHeight(int64(capH))
	if fmtSel.format == page.ScreencastFormatJpeg {
		action = action.WithQuality(int64(fmtSel.quality))
	}
	return chromedp.Run(ctx, action)
}

// stopScreencast, runStartScreencast, and runApplySizing expose the three stream
// control operations to deterministic unit tests. Production uses their CDP paths.
func (e *ChromiumEngine) stopScreencast(ctx context.Context) error {
	if e.stopScreencastOp != nil {
		return e.stopScreencastOp(ctx)
	}
	return chromedp.Run(ctx, page.StopScreencast())
}

func (e *ChromiumEngine) runStartScreencast(ctx context.Context, capW, capH int, fmtSel screencastFmt) error {
	if e.startScreencastOp != nil {
		return e.startScreencastOp(ctx, capW, capH, fmtSel)
	}
	return e.startScreencast(ctx, capW, capH, fmtSel)
}

func (e *ChromiumEngine) runApplySizing(ctx context.Context, windowID browser.WindowID, surfW, surfH int, z float64) error {
	if e.applySizingOp != nil {
		return e.applySizingOp(ctx, windowID, surfW, surfH, z)
	}
	return applySizing(ctx, windowID, surfW, surfH, z)
}

// screencastTransition describes one lock-owned stream start or restart.
type screencastTransition struct {
	stop              bool
	resize            bool
	skipUnchangedSize bool
}

// sameScreencastSession compares the cancellation channel that identifies one
// browser session. Context wrappers for the same session share it; rebuilt sessions
// do not.
func sameScreencastSession(active, expected context.Context) bool {
	if active == nil || expected == nil || active.Done() == nil {
		return false
	}
	return active.Done() == expected.Done()
}

// transitionScreencastLocked performs one initial start or restart while the caller
// holds screencastCtl. It validates the expected session before any I/O. A stop
// transition also requires a successfully started
// stream. It records the selected format and started state only after StartScreencast
// succeeds, and clears both after a confirmed stop.
func (e *ChromiumEngine) transitionScreencastLocked(ctx context.Context, change screencastTransition) error {
	e.mu.Lock()
	if !sameScreencastSession(e.ctx, ctx) || !e.streamingLocked() {
		e.mu.Unlock()
		return nil
	}
	if change.stop && !e.screencastStarted {
		e.mu.Unlock()
		return nil
	}
	fmtSel := e.initialScreencastFormatLocked()
	windowID := e.windowID
	surfW, surfH := e.surfaceSize(e.cols, e.rows)
	capW, capH := e.screencastCap(e.cols, e.rows)
	z := e.layoutScale()
	needsSizing := surfW != e.sizedSurfaceW || surfH != e.sizedSurfaceH || z != e.sizedScale
	e.mu.Unlock()

	if change.stop {
		if err := e.stopScreencast(ctx); err != nil {
			return fmt.Errorf("engine: stop screencast: %w", err)
		}
		e.mu.Lock()
		if sameScreencastSession(e.ctx, ctx) {
			e.screencastFmt = screencastFmt{}
			e.screencastStarted = false
		}
		e.mu.Unlock()
	}
	if change.resize && (!change.skipUnchangedSize || needsSizing) {
		if err := e.runApplySizing(ctx, windowID, surfW, surfH, z); err != nil {
			return fmt.Errorf("engine: resize viewport: %w", err)
		}
		e.mu.Lock()
		if sameScreencastSession(e.ctx, ctx) {
			e.sizedSurfaceW, e.sizedSurfaceH, e.sizedScale = surfW, surfH, z
		}
		e.mu.Unlock()
	}
	if err := e.runStartScreencast(ctx, capW, capH, fmtSel); err != nil {
		return fmt.Errorf("engine: start screencast: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !sameScreencastSession(e.ctx, ctx) {
		return context.Canceled
	}
	e.screencastFmt = fmtSel
	e.screencastStarted = true
	return nil
}

// recoverScreencastTransition cancels the failed active session. The supervisor
// observes the cancellation and rebuilds it from the engine's held backend, size,
// URL, and scroll state.
func (e *ChromiumEngine) recoverScreencastTransition(ctx context.Context, err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	var cancel context.CancelFunc
	if sameScreencastSession(e.ctx, ctx) {
		cancel = e.cancel
	}
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// startInitialScreencast serialises the first StartScreencast with resize, backend,
// and restarts. A pre-start resize or backend change can win the lock,
// update held state, and return without a stop; this transition then picks that state
// and issues the session's single initial StartScreencast.
func (e *ChromiumEngine) startInitialScreencast(ctx context.Context) error {
	if e.beforeInitialLock != nil {
		e.beforeInitialLock()
	}
	e.screencastCtl.Lock()
	defer e.screencastCtl.Unlock()
	return e.transitionScreencastLocked(ctx, screencastTransition{resize: true, skipUnchangedSize: true})
}

// initialScreencastFormatLocked selects the wire format for the next
// StartScreencast, read under the caller's e.mu hold. JPEG q90 serves the
// backends that decode every frame: Block and Sixel by their capture format,
// and Kitty in shm mode, where the plane decode converts either format and
// JPEG halves the payload and decodes about four times faster than PNG on
// real content. PNG serves direct Kitty, whose pass-through needs the PNG
// bytes.
func (e *ChromiumEngine) initialScreencastFormatLocked() screencastFmt {
	if e.backend.CaptureFormat() == render.CaptureJPEG || (e.kittyDecode && e.backend.UsesDirectPayload()) {
		return screencastFmt{format: page.ScreencastFormatJpeg, quality: blockScreencastQuality}
	}
	return screencastFmt{format: page.ScreencastFormatPng}
}

// ackQueue is a session-scoped, unbounded FIFO between the screencast listener and
// the acknowledgement pump. push holds its mutex only while appending, so a slow
// CDP acknowledgement command cannot park the listener. ready coalesces wake-ups;
// the queue itself retains every SessionID in order.
type ackQueue struct {
	mu      sync.Mutex
	pending []int64
	head    int
	stopped bool
	ready   chan struct{}
}

func newAckQueue() *ackQueue {
	return &ackQueue{ready: make(chan struct{}, 1)}
}

func (q *ackQueue) push(ack int64) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	q.pending = append(q.pending, ack)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return true
}

func (q *ackQueue) pop() (int64, bool) {
	if q == nil {
		return 0, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.head == len(q.pending) {
		return 0, false
	}
	ack := q.pending[q.head]
	q.head++
	if q.head == len(q.pending) {
		q.pending = q.pending[:0]
		q.head = 0
	}
	return ack, true
}

func (q *ackQueue) stop() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.stopped = true
	q.pending = nil
	q.head = 0
	q.mu.Unlock()
}

// ackFrame enqueues one frame's SessionID onto the pump queue for ackPump to run
// as page.ScreencastFrameAck off the ListenTarget event goroutine. The listener calls
// it once per delivered frame.
// It never runs the ack inline because chromedp.Run on the event goroutine causes
// the chromedp #920 self-deadlock.
// The unbounded FIFO has no full state, so every live-session enqueue is retained in
// arrival order. A nil or stopped queue means the session is already torn down.
func (e *ChromiumEngine) ackFrame(ack int64) {
	e.mu.Lock()
	acks := e.acks
	e.mu.Unlock()
	acks.push(ack)
}

// runScreencastFrameAck runs the production CDP acknowledgement or its test hook.
func (e *ChromiumEngine) runScreencastFrameAck(ctx context.Context, ack int64) error {
	if e.ackScreencastOp != nil {
		return e.ackScreencastOp(ctx, ack)
	}
	return chromedp.Run(ctx, page.ScreencastFrameAck(ack))
}

// ackPump drains the ack queue and runs page.ScreencastFrameAck on the live
// context, off the ListenTarget event goroutine. The listener enqueues SessionID
// values and this single goroutine runs the chromedp.Run that would deadlock on
// the event goroutine (chromedp #920). It exits when ctx is cancelled, and session
// teardown joins it before releasing the queue.
func (e *ChromiumEngine) ackPump(ctx context.Context, acks *ackQueue) {
	if ctx == nil || acks == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if ack, ok := acks.pop(); ok {
			// Time the acknowledgement round trip only when NYALA_FPS_TIMING armed
			// the ingest counter, so an off run reads no clock here.
			var start time.Time
			if e.ingest.armed() {
				start = time.Now()
			}
			err := e.runScreencastFrameAck(ctx, ack)
			if !start.IsZero() {
				e.ingest.recordAck(float64(time.Since(start)) / float64(time.Millisecond))
			}
			if err != nil {
				// An acknowledgement command failure loses Chromium's screencast
				// credit. Cancel this session so the supervisor rebuilds it; never
				// retry the popped SessionID and risk a duplicate acknowledgement.
				e.recoverScreencastTransition(ctx, err)
				return
			}
			continue
		}
		select {
		case <-acks.ready:
		case <-ctx.Done():
			return
		}
	}
}

// overlayState is the text-sync state the snapshot goroutine threads across its
// iterations. It holds the last-matched runs and the viewport-anchored boxes they
// were walked against, the wall-clock start of the current snapshot-failure run,
// and the wall-clock time of the last debounced re-snapshot. The snapshot
// goroutine owns it, so the hold is single-goroutine and lock-free; the snapshot
// read runs outside any lock. Carrying the boxes here rather than returning them
// separately keeps the snapshot function type the test hook implements unchanged.
type overlayState struct {
	lastText       []render.GlyphRun // last text matched to a bitmap
	lastFixed      []walkFixedBox    // viewport-anchored boxes lastText was walked against, for the error-hold tag
	lastScroll     scrollOffset      // DOM scroll offset lastText was decoded at, for the error-hold tag
	lastGen        int64             // layout generation lastText was decoded at, for the error-hold tag
	failingSince   time.Time         // start of the current snapshot-failure run, zero when not failing
	lastReSnap     time.Time         // wall-clock time of the last networkAlmostIdle re-snapshot, zero before the first
	lastScrollSnap time.Time         // wall-clock time of the last mid-scroll re-snapshot, zero before the first
}

// textSync publishes the latest decoded overlay runs from the snapshot goroutine
// to the frame-emit goroutine. The snapshot goroutine stores the freshly decoded
// runs; composeFrame reads the latest set and folds it onto the frame. Decoupling
// the DOM snapshot from the frame loop this way keeps snapshot latency from
// withholding acks or stalling frame emission (the snapshot no longer runs on the
// frame-emit goroutine). The held runs and boxes are read-only once published, so
// the reader shares both slices without copying.
type textSync struct {
	mu   sync.Mutex
	runs []render.GlyphRun
	// fixed carries the viewport-anchored boxes the walk reported alongside the runs.
	// A box holds its screen position while the page scrolls, so the compose-time
	// scroll shift needs the boxes to tell anchored text from flow text and to
	// re-test occlusion on a shifted frame. They are published with the runs because
	// they are only meaningful against the set they were walked with.
	fixed []walkFixedBox
	// scrollY tags the runs with the DOM vertical scroll offset (CSS pixels) they were
	// decoded at, so composeFrame pairs a bitmap to this set only when the bitmap's own
	// scroll matches. Vertical scroll is the only axis that reflows rows, so the pair
	// gates on Y alone.
	scrollY float64
	// gen is the layout generation the runs were decoded at, read in the same
	// sequence as the snapshot. composeFrame pairs a bitmap to this set only when the
	// bitmap's inferred generation matches gen, so a run set never composes over a
	// reflowed bitmap of a different generation.
	gen int64
}

// store publishes the latest decoded runs, the viewport-anchored boxes they were
// walked against, the DOM vertical scroll offset the runs were decoded at, and the
// layout generation they were decoded at. The caller must not write to either
// slice afterwards: both are read-only once published.
func (t *textSync) store(runs []render.GlyphRun, fixed []walkFixedBox, scrollY float64, gen int64) {
	t.mu.Lock()
	t.runs = runs
	t.fixed = fixed
	t.scrollY = scrollY
	t.gen = gen
	t.mu.Unlock()
}

// latest returns the most recently published runs, the viewport-anchored boxes
// they were walked against, the DOM vertical scroll offset, and the layout
// generation the runs were decoded at, or nil and zeroes before the first snapshot
// lands. The returned slices are shared, not copied, so the caller only reads them.
func (t *textSync) latest() ([]render.GlyphRun, []walkFixedBox, float64, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs, t.fixed, t.scrollY, t.gen
}

// noSnapshotBackoff is the idle pause the snapshot goroutine takes between
// iterations when the active backend takes no overlay (the graphics backends), so
// it does not spin while there is no text to decode.
const noSnapshotBackoff = 50 * time.Millisecond

// reSnapshotMinInterval caps the debounced networkAlmostIdle re-snapshot to at
// most one extra snapshot per second per session. A late reflow that lands inside
// this window keeps the navigation and resize clocked text rather than forcing a
// fresh layout read, so a chatty page cannot drive the snapshot goroutine.
const reSnapshotMinInterval = time.Second

// scrollCadence is the minimum gap between mid-scroll re-snapshots, the leading
// throttle on the scroll trigger. During a continuous scroll the overlay
// re-snapshots at most once per scrollCadence (about 6-7 per second at 150 ms), so
// the crisp text tracks the page as it scrolls rather than freezing until the
// scroll settles. 150 ms balances overlay tracking against the per-snapshot cost: an
// in-page text walk forces a synchronous layout read, so a tighter cadence would tax
// the page during a fast scroll for little extra legibility, and a looser one would
// let the text lag the bitmap visibly. The trailing holdStalenessLimit debounce
// lands one final snapshot on settle, so the overlay always ends on the resting
// scroll position regardless of the cadence phase.
const scrollCadence = 150 * time.Millisecond

// runTextSync runs the DOM snapshot loop on its own goroutine, off the frame-emit
// goroutine, so snapshot latency never stalls frame emission or withholds acks. It
// is independent of frame delivery: it takes the first snapshot as soon as the
// block backend is active, then re-snapshots only on a real trigger. So a static
// page that produces no further frames still publishes its text, and the frame
// loop folds the published runs onto every
// frame as sticky state. The re-snapshot triggers arrive on three channels: reSnap
// fires an immediate re-snapshot on resize or backend switch (restartStream),
// reflow carries the networkAlmostIdle late-reflow pokes, which this loop debounces
// to one trailing snapshot per holdStalenessLimit quiet window and caps to one per
// reSnapshotMinInterval, and scroll carries the scroll-offset-change pokes the frame
// loop sends, which this loop handles as a leading throttle plus a trailing
// debounce: each poke re-snapshots at once when at least scrollCadence has elapsed
// since the last scroll snapshot, so the overlay tracks the page during a continuous
// scroll (about 6-7 snapshots per second at 150 ms), and every poke also re-arms the
// holdStalenessLimit trailing debounce so one final snapshot lands on settle. The
// scroll path is not capped by the 1s reflow cap; scrollCadence is its only throttle.
// Each snapshot decodes the
// runs at the snapshot's own scroll, routes them through the bounded holdDecision,
// and publishes the emitted set to ts, then pokes reEmit so the frame loop re-paints
// its held bitmap with the new text on a static page that delivers no new frame. It
// owns the overlayState, so the hold and the re-snapshot timestamp are
// single-goroutine and lock-free. It runs only the block path's snapshot; on a
// graphics backend it publishes nil and backs off, since those backends ignore the
// overlay. It exits when ctx is cancelled (session teardown).
func (e *ChromiumEngine) runTextSync(ctx context.Context, ts *textSync, reSnap, reflow, scroll <-chan struct{}, reEmit chan<- struct{}) {
	if ctx == nil || ts == nil {
		return
	}
	snapshot := e.snapshot
	if snapshot == nil {
		snapshot = e.snapshotRuns
	}
	// Read the per-session load timer once here, on the goroutine start ordering, so
	// no rebuild race with startSession's write. A nil timer is a valid disabled timer.
	e.mu.Lock()
	timer := e.timing
	lifecycle := e.lifecycle
	e.mu.Unlock()
	var state overlayState
	// lastCalCellW/H track the (cellW, cellH) pair the last calibration corrected
	// for. The reSnap arm skips recalibration when the pair is unchanged, so a
	// non-resize poke does not re-measure. Goroutine-local like state: the snapshot
	// goroutine owns it, so the guard is lock-free.
	var lastCalCellW, lastCalCellH float64
	if !e.waitForFirstSnapshot(ctx, ts) {
		return
	}
	if lifecycle != nil {
		select {
		case <-lifecycle.fcp:
		case <-ctx.Done():
			return
		}
	}
	// Reconcile font forcing to the active backend before the first publish. On the
	// block path this is the no-op the launch injection already covered; on a
	// graphics-to-block cycle it registers the script and injects the live style.
	// waitForFirstSnapshot only returns for the block backend, so a graphics start
	// never reaches here.
	e.syncFontForcing(ctx)
	// Publish the session's first snapshot as soon as the block backend is active.
	// Publish before calibrating so the first composed frame can show text without waiting for the
	// calibration base (document.fonts.load plus probe and reinject evals). The seed
	// advance is already ~cellW (advanceRatio measured from the shipped TTF), so seed
	// placement is near-correct; the calibrated re-publish below trims the residual.
	// markText fires here, the first-text milestone. Program order on this single
	// goroutine is the whole ordering contract: no channel, no sync.Once, no new goroutine.
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	timer.markText()
	pokeReEmit(reEmit)
	// Calibrate against the forced font metrics, then re-publish so the overlay maps
	// against the corrected size. This trails the first publish, so production first
	// paint shows near-calibrated text for one reflow cycle before this re-publish
	// lands the final placement.
	e.calibrateFonts(ctx)
	e.mu.Lock()
	lastCalCellW, lastCalCellH = e.snapshotCellPx()
	e.mu.Unlock()
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	pokeReEmit(reEmit)
	// debounce arms a trailing timer per reflow quiet window; scrollDebounce does the
	// same for scroll. A nil channel means none is pending; each timer is armed on its
	// poke and cleared on fire.
	var debounce *time.Timer
	var debounceC <-chan time.Time
	var scrollDebounce *time.Timer
	var scrollDebounceC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-reSnap:
			// Resize or backend switch: reconcile font forcing first, then re-snapshot at
			// once, no debounce, no cap. A block-to-graphics cycle removes the forcing; a
			// graphics-to-block cycle re-registers it and injects the live style. A fresh
			// enable (disabled before, enabled after) resets the calibration guard to zero
			// so the recalibrate below reruns, since snapshotCellPx is backend-independent
			// and would otherwise wrongly skip after a cycle back to block at the same grid.
			wasForcing := e.fontForcingActive()
			if e.syncFontForcing(ctx) && !wasForcing {
				lastCalCellW, lastCalCellH = 0, 0
			}
			// Recalibrate first when the cell size changed, since the seed size is
			// derived from cellW; skip when the pair is unchanged so a backend switch
			// at the same grid does not re-measure. Only the block backend forces the
			// font, so a graphics backend takes no calibration.
			e.mu.Lock()
			cellW, cellH := e.snapshotCellPx()
			e.mu.Unlock()
			if e.textOverlayBackend() && (cellW != lastCalCellW || cellH != lastCalCellH) {
				e.calibrateFonts(ctx)
				lastCalCellW, lastCalCellH = cellW, cellH
			}
			state = e.publishSnapshot(ctx, snapshot, ts, state)
			pokeReEmit(reEmit)
		case <-reflow:
			// Late reflow: arm or reset the trailing debounce timer so a burst of
			// networkAlmostIdle events collapses to one trailing snapshot.
			debounce = armDebounce(debounce, holdStalenessLimit)
			debounceC = debounce.C
		case <-debounceC:
			debounceC = nil
			state = e.reSnapshotCapped(ctx, snapshot, ts, state)
			pokeReEmit(reEmit)
		case <-scroll:
			// Scroll: leading throttle plus trailing debounce. Snapshot now when the
			// cadence allows, so the overlay tracks mid-scroll, and always re-arm the
			// trailing timer so a final snapshot lands once the scroll settles.
			state = e.scrollSnap(ctx, snapshot, ts, state, reEmit)
			scrollDebounce = armDebounce(scrollDebounce, holdStalenessLimit)
			scrollDebounceC = scrollDebounce.C
		case <-scrollDebounceC:
			scrollDebounceC = nil
			// Settle snapshot: the scroll stopped, so land the final scroll position.
			// Uncapped by the 1s reflow cap; the cadence throttled the mid-scroll fires.
			state = e.publishSnapshot(ctx, snapshot, ts, state)
			state.lastScrollSnap = time.Now()
			pokeReEmit(reEmit)
		}
	}
}

// waitForFirstSnapshot blocks until the active backend takes the overlay. A
// graphics backend publishes nil and backs off rather than reading layout it
// ignores. It returns false when ctx is cancelled, so
// the caller exits without snapshotting.
func (e *ChromiumEngine) waitForFirstSnapshot(ctx context.Context, ts *textSync) bool {
	for !e.textOverlayBackend() {
		ts.store(nil, nil, 0, 0)
		if sleepCtx(ctx, noSnapshotBackoff) {
			return false
		}
	}
	return ctx.Err() == nil
}

// reSnapshotCapped runs one late-reflow re-snapshot if the rate cap allows, then
// records the time. The cap holds it to one per reSnapshotMinInterval, so a chatty
// page cannot drive the snapshot goroutine; a capped poke keeps the navigation and
// resize clocked text. It recalibrates the font before the publish, because a late
// web-font swap raises a layout shift and is the reflow this path fires on: the
// one-shot launch calibration measured the fallback face, so a font that lands after
// paint would leave a stale font-size and drift the origins the walk snaps. The
// recalibrate rides the same rate cap, so a chatty page cannot re-measure per reflow.
// It returns the updated hold state.
func (e *ChromiumEngine) reSnapshotCapped(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState), ts *textSync, state overlayState) overlayState {
	now := time.Now()
	if !state.lastReSnap.IsZero() && now.Sub(state.lastReSnap) < reSnapshotMinInterval {
		return state
	}
	if e.textOverlayBackend() {
		e.calibrateFonts(ctx)
	}
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	state.lastReSnap = now
	return state
}

// scrollSnapDue reports whether a mid-scroll re-snapshot is due: the first scroll
// poke (lastScrollSnap zero) always fires, and a later poke fires once cadence has
// elapsed since the last scroll snapshot. It is the pure leading-throttle decision,
// factored out so it is unit-tested without a wall-clock sleep.
func scrollSnapDue(last, now time.Time, cadence time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= cadence
}

// scrollSnap runs one mid-scroll re-snapshot when the leading throttle allows, then
// records the time, so a continuous scroll re-snapshots at most once per
// scrollCadence. A throttled poke leaves the state unchanged; the trailing debounce
// still lands the settle snapshot. It returns the updated hold state and pokes
// reEmit on a fire so the held frame repaints with the new scroll's text. Each
// snapshot decodes runs at its own whole-cell scroll, so the overlay tracks the page
// as it scrolls.
func (e *ChromiumEngine) scrollSnap(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState), ts *textSync, state overlayState, reEmit chan<- struct{}) overlayState {
	now := time.Now()
	if !scrollSnapDue(state.lastScrollSnap, now, scrollCadence) {
		return state
	}
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	state.lastScrollSnap = now
	pokeReEmit(reEmit)
	return state
}

// pokeReEmit signals the frame loop to re-push its held bitmap recomposed with the
// freshly published text. The send is latest-wins on a capacity-1 channel, so it
// never blocks and a burst of publishes collapses to one re-emit. A nil channel (no
// session) drops the poke.
func pokeReEmit(reEmit chan<- struct{}) {
	if reEmit == nil {
		return
	}
	select {
	case reEmit <- struct{}{}:
	default:
	}
}

// armDebounce starts or resets a trailing timer to fire after d, draining a stale
// fire so a reset never leaves a spurious tick on the channel. It returns the live
// timer so the caller threads it across iterations.
func armDebounce(t *time.Timer, d time.Duration) *time.Timer {
	if t == nil {
		return time.NewTimer(d)
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
	return t
}

// publishSnapshot runs one snapshot and publishes its runs, viewport-anchored
// boxes, scroll, and layout generation to ts. It returns the updated hold state. It
// is the single snapshot-and-publish step the first-after-paint and re-snapshot
// triggers share, so the publish stays in one place. A graphics backend takes no
// overlay, so it publishes nil and skips the layout read rather than snapshot the
// full viewport it ignores.
func (e *ChromiumEngine) publishSnapshot(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState), ts *textSync, state overlayState) overlayState {
	if !e.textOverlayBackend() {
		ts.store(nil, nil, 0, 0)
		return state
	}
	runs, scroll, gen, next := snapshot(ctx, state)
	// snapshotRuns rebuilds the hold fields and drops the cadence timestamps, so carry
	// them forward here: the re-snapshot caps live in runTextSync, not the snapshot.
	next.lastReSnap = state.lastReSnap
	next.lastScrollSnap = state.lastScrollSnap
	// Tag the published runs with the DOM scroll and the layout generation they were
	// decoded at, so composeFrame can pair them to a same-generation bitmap at the same
	// scroll. The boxes ride the returned state rather than the return values, since
	// the snapshot function type is a test hook that must not change, and snapshotRuns
	// pairs them to the emitted runs there.
	ts.store(runs, next.lastFixed, scroll.Y, gen)
	return next
}

// sleepCtx sleeps for d or returns true early when ctx is cancelled, so the
// snapshot goroutine's idle back-off never outlives the session.
func sleepCtx(ctx context.Context, d time.Duration) (cancelled bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-ctx.Done():
		return true
	}
}

// inferFrameGen infers the layout generation of a consumed screencast bitmap. The
// screencast metadata carries no generation token, so it is inferred from the latest
// published snapshot. Scroll never bumps the layout-generation counter (only a reflow
// does), so a bitmap shares the latest published snapshot's generation whether or not
// its scroll matches; composeFrame gates on scroll separately, so a scroll-only
// difference shifts the runs onto the bitmap's scroll rather than holding the overlay.
// A reflow bumps the counter and the snapshot goroutine re-snapshots
// on the same layout-shift signal, so the latest published generation tracks the
// current layout. It returns the latest published generation, and whether a snapshot
// has published (haveTS): before the first publish there is no generation to pair, so
// ok is false and the compose draws the bitmap alone. tsGen is the latest published
// set's generation.
//
// Bound: the inference lags a reflow by at most one re-snapshot. In the window between
// a reflow bumping the page counter and the snapshot goroutine publishing the
// re-decoded runs at the new generation, a bitmap already painted at the new layout is
// inferred at the old published generation. The runs published at the old generation
// then compose over it for that window. It closes on the next publish, which the
// layout-shift trigger fires promptly, so the drift is bounded to the re-snapshot
// latency rather than persisting.
func inferFrameGen(tsGen int64, haveTS bool) (gen int64, ok bool) {
	if !haveTS {
		return 0, false
	}
	return tsGen, true
}

// frameScrollOffset reads the page scroll offset from a consumed frame's
// screencast metadata in CSS pixels, returning ok=false when the frame carries no
// metadata. The listener sets meta on every frame, so ok is true in production;
// the false branch only guards a metadata-less test frame.
func frameScrollOffset(p pendingFrame) (scrollOffset, bool) {
	if p.meta == nil {
		return scrollOffset{}, false
	}
	return scrollOffset{X: p.meta.ScrollOffsetX, Y: p.meta.ScrollOffsetY}, true
}

// pokeScroll signals the snapshot goroutine that the page scroll offset changed,
// so it re-snapshots the text at the new scroll once the scroll settles. The send
// is latest-wins on a capacity-1 channel, so a burst of scroll frames collapses to
// one wake and the frame loop never blocks. A nil channel drops the poke.
func pokeScroll(scroll chan<- struct{}) {
	if scroll == nil {
		return
	}
	select {
	case scroll <- struct{}{}:
	default:
	}
}

// emitFrame composes one pending bitmap with the latest published text and pushes
// it on the frames channel, selecting on ctx so the send cannot leak when the
// consumer stops. It returns false when ctx is cancelled mid-send, so the caller
// exits the loop.
func (e *ChromiumEngine) emitFrame(ctx context.Context, frames chan render.Frame, ts *textSync, pending pendingFrame, lastComposed *[]render.GlyphRun) bool {
	frame := e.composeFrame(pending, ts, lastComposed)
	select {
	case frames <- frame:
		return true
	case <-ctx.Done():
		// Send-failure drop point: the frame never reaches the viewer, so
		// return its pooled plane (nil-safe off the kitty-shm path).
		pending.releasePlane()
		return false
	}
}

// composeFrame turns one consumed screencast bitmap into a synced render.Frame.
// It reads the latest overlay runs the snapshot goroutine published (textSync),
// rather than running the DOM snapshot inline, so snapshot latency never stalls
// the frame loop. The graphics backends draw the bitmap alone and ignore
// Frame.Text; runTextSync publishes nil for them, so this folds nil.
//
// It pairs the run set to the bitmap by layout generation and scroll. It infers the
// bitmap's generation from the latest published snapshot (inferFrameGen). When the
// bitmap's inferred generation matches the run set's generation and the bitmap's own
// scroll equals the scroll the runs were decoded at, it composes the set as-is: the
// walk placed each run on-screen at that scroll, so no shift is needed. When the
// generation matches but the bitmap has scrolled past the snapshot, it shifts the runs
// onto the bitmap's whole-cell scroll (shiftRunsToBitmapScroll) so the overlay tracks
// the moving page. It hands that shift the viewport-anchored boxes the run set was
// walked against, so a run anchored inside a box is held in place rather than shifted,
// and a shifted run is re-tested against the opaque boxes it has moved under. Only a
// genuinely different generation (a reflow) or an uninferable generation holds the
// previous frame's composed overlay for one frame
// rather than compose a stale set onto a freshly reflowed bitmap. lastComposed carries
// the held set across frames; the frame loop owns it. Before the first publish (nil
// run set) it composes nil, the bitmap alone.
func (e *ChromiumEngine) composeFrame(pending pendingFrame, ts *textSync, lastComposed *[]render.GlyphRun) render.Frame {
	var runs []render.GlyphRun
	var fixed []walkFixedBox
	var snapScrollY float64
	var tsGen int64
	var haveTS bool
	if ts != nil {
		runs, fixed, snapScrollY, tsGen = ts.latest()
		haveTS = runs != nil
	}
	e.mu.Lock()
	_, cellCSSH := e.snapshotCellPx()
	rows := e.rows
	e.mu.Unlock()
	bitmapScrollY := snapScrollY
	if pending.meta != nil {
		bitmapScrollY = pending.meta.ScrollOffsetY
	}
	frameGen, frameGenOK := inferFrameGen(tsGen, haveTS)
	var composed []render.GlyphRun
	switch {
	case !haveTS:
		// No valid run set to compose: none has published yet, or the snapshot error
		// hold expired to nil (holdDecision dropped the stale text past its staleness
		// bound). Draw the bitmap alone, so the staleness bound keeps its authority and
		// the overlay never shows text the page no longer matches.
		composed = nil
	case frameGenOK && frameGen == tsGen && bitmapScrollY == snapScrollY:
		// The bitmap and the run set share a generation and scroll: compose the runs
		// as-is, since the walk placed each on-screen at that scroll. The generation and
		// scroll guards pair the two so a run set never composes onto a bitmap of a
		// different generation or scroll.
		composed = runs
	case frameGenOK && frameGen == tsGen:
		// Same generation, but the bitmap scrolled past the snapshot: shift the runs onto
		// the bitmap's scroll so the overlay stays glued to the moving page rather than
		// freezing at the snapshot's scroll until the settle re-snapshot lands. Scroll is
		// whole-cell (dispatchWheel snaps the DOM scroll to a multiple of cellH), so the
		// row shift is an exact integer and the copy never aliases fractional rows. The
		// boxes the set was walked against ride along, so an anchored run holds its
		// screen position and a shifted run is re-tested against the opaque boxes.
		composed = shiftRunsToBitmapScroll(runs, fixed, bitmapScrollY, snapScrollY, cellCSSH, rows)
	default:
		// A genuinely different generation (a reflow bumped the counter): hold the
		// previous composed overlay one frame rather than compose a stale set onto a
		// freshly reflowed bitmap. The re-snapshot on the layout-shift signal converges
		// the two so the pairing lands on the next frame.
		if lastComposed != nil {
			composed = *lastComposed
		}
	}
	if lastComposed != nil {
		*lastComposed = composed
	}
	// A held bitmap re-emitted after the lifecycle event is also an after-FCP
	// emission. This matters on static pages where the paint frame reaches the
	// screencast listener just before the corresponding lifecycle event and no new
	// image arrives afterwards.
	afterFCP := pending.afterFCP || e.afterMainFrameFCP()
	frame := render.Frame{Img: pending.img, EncodedPNG: pending.png, Text: composed, AfterFCP: afterFCP, Release: pending.release}
	if e.diagnosticFrameMeta && pending.meta != nil {
		e.mu.Lock()
		serial := e.diagnosticCalibrationSerial
		succeeded := e.diagnosticCalibrationSucceeded
		fontSize := e.diagnosticCalibratedFontSize
		e.mu.Unlock()
		frame.Diagnostics = &render.FrameDiagnostics{
			Meta:                 screencastMeta(pending.meta),
			CalibrationSerial:    serial,
			CalibrationSucceeded: succeeded,
			CalibratedFontSize:   fontSize,
		}
	}
	return frame
}

// shiftRunsToBitmapScroll shifts the published runs vertically onto the bitmap's
// own scroll, so the overlay tracks the moving page while the bitmap scrolls past
// the snapshot the runs were decoded at. A run's box Y is the on-screen top with the
// snapshot scroll already subtracted, so a bitmap scrolled dy past the snapshot moves
// the run up by dy: the box Y shifts by -dy and the cell row by -round(dy/cellCSSH).
// Scroll is whole-cell, so dy/cellCSSH is an integer and the row shift is exact.
// Columns are untouched: this is the vertical-scroll axis only. A run whose shifted
// row falls outside the grid is dropped.
//
// fixed carries the viewport-anchored boxes the walk reported beside the runs, and
// each run is tested against them twice, in this order. A run whose pre-shift box sits
// inside a box (runInsideAnyFixedBox) is that box's own text: it holds its screen
// position while the page scrolls under it, so it is kept exactly as it is. Every
// other run shifts, then drops when an opaque box covers its post-shift box
// (runCoveredByOpaqueFixedBox), because the walk hit-tested that text as visible at
// the snapshot's scroll and it has since moved beneath the box. The order is
// load-bearing: containment compares the pre-shift box, since the run and the boxes
// were both measured in the snapshot's screen space, and coverage compares the
// post-shift box, since a viewport-anchored box holds its screen rect while the page
// moves under it.
//
// Two limits here are deliberate. position: sticky is not tracked, because telling a
// stuck element from an unstuck one needs a second layout measurement the mid-scroll
// path cannot afford, and treating an unstuck one as anchored would cull text that is
// visible. A fixed box with no text inside it is never discovered, because collection
// rides the text walk. Both fail towards text bleeding through, the safe polarity, and
// both self-correct on the settle walk once the scroll stops.
//
// It returns the runs untouched (no copy) on a zero delta, the production fast path;
// otherwise it returns a shifted copy and never mutates runs, which textSync shares
// across composes. A nil or empty box list costs one length check per run and
// reproduces the plain shift.
func shiftRunsToBitmapScroll(runs []render.GlyphRun, fixed []walkFixedBox, bitmapScrollY, snapScrollY, cellCSSH float64, rows int) []render.GlyphRun {
	dy := bitmapScrollY - snapScrollY
	if len(runs) == 0 || dy == 0 {
		return runs
	}
	var rowShift int
	if cellCSSH > 0 {
		rowShift = int(math.Round(dy / cellCSSH))
	}
	// Hoisted so a page with no viewport-anchored boxes pays one bool test per run on
	// the frame loop rather than two helper calls.
	hasFixed := len(fixed) > 0
	shifted := make([]render.GlyphRun, 0, len(runs))
	for _, run := range runs {
		// Pre-shift: the run and the boxes share the snapshot's screen space, so this is
		// the only space the containment test is valid in. An anchored run keeps its row
		// and its Y, because the page moved and the box did not.
		if hasFixed && runInsideAnyFixedBox(run, fixed) {
			shifted = append(shifted, run)
			continue
		}
		run.Row -= rowShift
		run.Y -= dy
		if run.Row < 0 || (rows > 0 && run.Row >= rows) {
			continue
		}
		// Post-shift: the box holds its screen rect, so the cover test asks where the run
		// has moved to, not where the walk found it.
		if hasFixed && runCoveredByOpaqueFixedBox(run, fixed) {
			continue
		}
		shifted = append(shifted, run)
	}
	return shifted
}

// fixedBoxEpsilon is the per-edge tolerance the containment test allows, in CSS
// pixels. A line box is measured from getClientRects and a viewport-anchored box from
// getBoundingClientRect, so a sub-pixel edge can put a run marginally outside the
// container that owns it. Half a pixel absorbs that without reaching a neighbouring
// element.
const fixedBoxEpsilon = 0.5

// runInsideAnyFixedBox reports whether the run's on-screen box sits wholly inside one
// of the viewport-anchored boxes, within fixedBoxEpsilon per edge. Such a run is the
// box's own text, so a page scroll leaves it where it is and the compose-time shift
// must hold it rather than move it. Every box counts here, opaque or not: a
// transparent viewport-anchored container still anchors the text inside it.
func runInsideAnyFixedBox(run render.GlyphRun, fixed []walkFixedBox) bool {
	for _, box := range fixed {
		if run.X >= box.Left-fixedBoxEpsilon && run.X+run.W <= box.Right+fixedBoxEpsilon &&
			run.Y >= box.Top-fixedBoxEpsilon && run.Y+run.H <= box.Bottom+fixedBoxEpsilon {
			return true
		}
	}
	return false
}

// runCoveredByOpaqueFixedBox reports whether an opaque viewport-anchored box covers
// every one of three samples taken across the run's vertical centre: the left edge
// plus an inset, the horizontal centre, and the right edge less the same inset, with
// the inset min(1, width/4). It mirrors the in-page walk's own occlusion sampling, so
// this mid-scroll approximation and the walk's cull agree on the same line box. One
// uncovered sample keeps the whole run, so a partly covered line keeps all its text.
// Samples may fall in different boxes; only the opaque ones can cover.
func runCoveredByOpaqueFixedBox(run render.GlyphRun, fixed []walkFixedBox) bool {
	l, r := run.X, run.X+run.W
	cy := run.Y + run.H/2
	inset := math.Min(1, (r-l)/4)
	for _, x := range [3]float64{l + inset, (l + r) / 2, r - inset} {
		covered := false
		for _, box := range fixed {
			if box.Opaque && x >= box.Left && x <= box.Right && cy >= box.Top && cy <= box.Bottom {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// snapshotRuns runs one in-page text walk, which returns per-line runs already
// snapped to integer cells against the forced cell, the viewport-anchored boxes the
// runs were walked against, the DOM scroll offset the walk read at, and the layout
// generation, all in one Runtime.evaluate. It decodes the walk result to glyph runs
// (trusting the page's integer origins, keeping only the grid right-edge clip), then
// routes the result through the bounded holdDecision: a successful read emits the
// fresh runs, a failing read holds the last-matched runs for at most
// holdStalenessLimit before dropping to nil. It returns the emitted runs, the scroll
// offset, the layout generation, and the updated state, whose lastFixed field
// carries the boxes the emitted runs were walked against: the walk's own boxes on a
// success, the held set's boxes on an error hold, and nil once the hold expires. The
// boxes travel on the state rather than the return values so the snapshot function
// type stays the one the test hook implements. It runs on the snapshot goroutine;
// the walk chromedp.Run stays outside any lock.
func (e *ChromiumEngine) snapshotRuns(ctx context.Context, state overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState) {
	if ctx == nil {
		emit, last, failing := holdDecision(context.Canceled, nil, state.lastText, state.failingSince, time.Now())
		fixed := state.lastFixed
		if last == nil {
			fixed = nil
		}
		return emit, state.lastScroll, state.lastGen, overlayState{lastText: last, lastFixed: fixed, lastScroll: state.lastScroll, lastGen: state.lastGen, failingSince: failing}
	}
	e.mu.Lock()
	cellW, cellH := e.snapshotCellPx()
	cols := e.cols
	e.mu.Unlock()
	result, err := e.textWalk(ctx, cellW, cellH)
	var decoded []render.GlyphRun
	gen := state.lastGen
	var scroll scrollOffset
	var fixed []walkFixedBox
	if err == nil {
		// The walk returns runs already placed on-screen (getClientRects subtracts the
		// scroll) and snapped to integer cells against the forced cell in the page, so
		// the origin snap and the layout advance share one unit and a long run cannot
		// drift across x. The tagged scroll, generation, and viewport-anchored boxes come
		// from the same evaluate, so they are internally consistent with the runs.
		gen = result.Gen
		scroll = scrollOffset{X: result.ScrollX, Y: result.ScrollY}
		fixed = result.Fixed
		decoded = decodeWalkRuns(result, cellW, cols)
	}
	emit, last, failing := holdDecision(err, decoded, state.lastText, state.failingSince, time.Now())
	// Tag the emitted runs with the scroll, generation, and viewport-anchored boxes
	// they were decoded at: a fresh success carries the new snapshot scroll,
	// generation, and boxes, an error hold re-emits the last-matched runs so it
	// carries the scroll, generation, and boxes those were decoded at
	// (state.lastScroll, state.lastGen, state.lastFixed). The runs and their scroll,
	// generation, and boxes travel together so composeFrame shifts and pairs the right
	// way. A hold that expires past its staleness bound drops the runs, so it drops
	// the boxes with them rather than leave boxes with no set to test.
	emitScroll := scroll
	emitGen := gen
	emitFixed := fixed
	if err != nil {
		emitScroll = state.lastScroll
		emitGen = state.lastGen
		emitFixed = state.lastFixed
	}
	if last == nil {
		emitFixed = nil
	}
	return emit, emitScroll, emitGen, overlayState{lastText: last, lastFixed: emitFixed, lastScroll: emitScroll, lastGen: emitGen, failingSince: failing}
}

// screencastMeta copies the CDP screencast frame metadata into a neutral
// render.ScreencastMeta so diagnostics can read scroll and device geometry without
// the render seam importing the page type. The timestamp is
// the frame swap time as float64 seconds since the Unix epoch, or 0 when absent.
func screencastMeta(meta *page.ScreencastFrameMetadata) render.ScreencastMeta {
	var ts float64
	if meta.Timestamp != nil {
		t := time.Time(*meta.Timestamp)
		ts = float64(t.UnixNano()) / float64(time.Second)
	}
	return render.ScreencastMeta{
		OffsetTop:       meta.OffsetTop,
		PageScaleFactor: meta.PageScaleFactor,
		DeviceWidth:     meta.DeviceWidth,
		DeviceHeight:    meta.DeviceHeight,
		ScrollOffsetX:   meta.ScrollOffsetX,
		ScrollOffsetY:   meta.ScrollOffsetY,
		Timestamp:       ts,
	}
}

// restartStreamLocked stops the current screencast and starts a fresh one at the
// recomputed surface size while the caller holds screencastCtl. Resize and SetBackend
// use it so their state change and restart stay atomic.
func (e *ChromiumEngine) restartStreamLocked() error {
	e.mu.Lock()
	ctx := e.ctx
	reSnap := e.reSnap
	e.mu.Unlock()
	if ctx == nil {
		return nil
	}
	// Poke the snapshot goroutine for an immediate re-snapshot: a resize or backend
	// switch changes the layout viewport, so the prior text no longer maps to the
	// new grid. The send is latest-wins so it never blocks. A graphics backend
	// publishes nil through the same poke.
	if reSnap != nil {
		select {
		case reSnap <- struct{}{}:
		default:
		}
	}
	err := e.transitionScreencastLocked(ctx, screencastTransition{
		stop:   true,
		resize: true,
	})
	e.recoverScreencastTransition(ctx, err)
	return err
}
