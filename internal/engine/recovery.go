package engine

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// startSession builds one live browser session from the engine's held state and
// starts the screencast and frame loop. It launches the allocator and context,
// registers a crash watcher, starts the stream and consuming frame loop, then
// dispatches navigation to the held URL without waiting for full page load. The
// returned channel is closed when the page reports Target.targetCrashed, so the
// supervisor can rebuild. All session state (URL, scroll, backend, cell px) is
// read from the engine, never from a dead context, so a rebuild resumes where the
// crash left off.
func (e *ChromiumEngine) startSession() (crashed chan struct{}, err error) {
	timer := newLoadTimer(e.loadSpans)
	if err = e.launch(e.browserFlag); err != nil {
		return nil, err
	}
	timer.mark(labelProcessSpawned)
	e.mu.Lock()
	ctx := e.ctx
	url := e.url
	frames := e.frames
	scrollX, scrollY := e.scrollX, e.scrollY
	cx := float64(e.cols*e.cellW) / 2
	cy := float64(e.rows*e.cellH) / 2
	e.mu.Unlock()

	crashed = make(chan struct{})
	e.watchCrash(crashed)
	e.watchPaint()
	// The frame sink differs by path. Capture keeps the ordered single-frame slot and
	// its single decode worker (runFrameLoop), so the corpus stays complete and in
	// order. The live path feeds a bounded, drop-oldest queue to a small pool of decode
	// workers, so several frames decode in parallel and the block-backend frame rate is
	// no longer capped by a single ~28 ms decode. A collector re-serialises the pool by
	// arrival sequence, so the live screen stays monotonic.
	live := !e.captureOn
	var slot *frameSlot
	var rawQ *rawFrameQueue
	workers := 1
	if live {
		workers = decodeWorkerCount()
		// Bound the queue to one buffered frame per worker plus one in flight, so the
		// pool stays fed while memory stays bounded; a full queue drops the oldest
		// undecoded frame.
		rawQ = newRawFrameQueue(workers + 1)
	} else {
		slot = newFrameSlot()
	}
	// acks is the buffered ack channel the ack pump drains. It is created before
	// startScreencast so the listener's first enqueue lands, and the pump runs off
	// the event goroutine to avoid the chromedp #920 deadlock. The buffer is sized
	// to a few in-flight frames so a transient pump stall never blocks the listener
	// or the slot's displaced-frame ack.
	acks := make(chan int64, ackBufferDepth)
	ts := &textSync{}
	// reSnap, reflow, and scroll clock the snapshot goroutine off the page, not off
	// frame delivery: reSnap fires an immediate re-snapshot on resize or backend
	// switch (restartStream), reflow carries the debounced networkAlmostIdle late
	// reflow (watchPaint), and scroll carries the debounced scroll-offset change the
	// frame loop detects. Each is capacity 1 latest-wins, so a burst coalesces to one
	// wake. All are goroutine-scoped to this session, so ctx cancellation stops the
	// goroutines and the channels need no teardown reset beyond clearing the fields.
	reSnap := make(chan struct{}, 1)
	reflow := make(chan struct{}, 1)
	scroll := make(chan struct{}, 1)
	// reEmit carries the held-frame re-emit poke from the snapshot goroutine to the
	// frame loop, so a static page repaints with text once the snapshot publishes.
	// Capacity 1 latest-wins, so a burst of publishes coalesces to one re-emit.
	reEmit := make(chan struct{}, 1)
	// motion carries frame-arrival instants to the adaptive-format controller. It is
	// created and the controller started only on the live path; under capture it stays
	// nil so the format never switches (capture forces PNG). Buffered so the listener's
	// non-blocking poke never drops under a normal cadence.
	var motion chan time.Time
	if !e.captureOn {
		motion = make(chan time.Time, 8)
	}
	e.mu.Lock()
	e.slot = slot
	e.rawQ = rawQ
	e.decodeWorkers = workers
	e.acks = acks
	e.text = ts
	e.reSnap = reSnap
	e.reflow = reflow
	e.scroll = scroll
	e.reEmit = reEmit
	e.motion = motion
	e.timing = timer
	e.screencastFmt = screencastFmt{}
	e.screencastStarted = false
	e.mu.Unlock()
	// Register the frame listener once, on this fresh session context, before the
	// first startScreencast. restartStream re-issues startScreencast on the same
	// context, so registering here (not in startScreencast) keeps a backend switch
	// or resize from leaking a second listener.
	e.registerScreencastListener(ctx, slot, rawQ)
	// Reset the stuck-state watchdog for this session, so a rebuild does not read the
	// previous session's receive timestamp or SessionID as a live stall.
	e.recvNano.Store(0)
	e.lastSession.Store(0)
	e.stallPoked.Store(false)
	if err = e.startInitialScreencast(ctx); err != nil {
		e.teardownSession()
		return nil, err
	}
	timer.mark(labelScreencastStarted)
	go e.ackPump(ctx, acks)
	go e.runScreencastWatchdog(ctx)
	// The adaptive-format controller runs only on the live path; under capture motion
	// is nil, so the format stays the deterministic PNG the corpus needs.
	if motion != nil {
		go e.runScreencastFormat(ctx, motion)
	}
	go e.runTextSync(ctx, ts, reSnap, reflow, scroll, reEmit)
	if live {
		// Live: a pool of decode workers feeds one collector that re-serialises them by
		// arrival sequence and owns the compose, the held-frame re-emit, the scroll
		// tracking, and the frame send. The decoded channel is buffered to the worker
		// count so a finished worker never blocks handing off to the collector.
		decoded := make(chan decodedFrame, workers)
		for range workers {
			go e.runDecodeWorker(ctx, rawQ, decoded)
		}
		go e.runDecodeCollector(ctx, decoded, frames, ts, scroll, reEmit)
	} else {
		// Capture: the single ordered decode worker, unchanged, so the corpus stays
		// complete and in order.
		go e.runFrameLoop(ctx, slot, frames, ts, scroll, reEmit)
	}

	// Navigate and the held-scroll restore run on one ordered path: navigateAsync
	// drives the load waiter, then restores the held offset only after the load
	// signal, so document-exists happens-before scroll-restore. A fresh navigate
	// lands at the top, so a non-zero held offset is re-applied as one mouseWheel
	// at the viewport centre by the held deltas.
	go e.navigateAsync(ctx, url, scrollRestore{x: scrollX, y: scrollY, cx: cx, cy: cy})
	return crashed, nil
}

// scrollRestore is the held scroll offset re-applied after a navigate completes,
// expressed as the wheel deltas and the viewport-centre coordinates the engine
// dispatches one mouseWheel from. A zero offset restores nothing.
type scrollRestore struct {
	x, y   int     // held scroll deltas in CSS pixels
	cx, cy float64 // viewport-centre coordinates for the mouseWheel dispatch
}

// navigateAsync drives navigation through chromedp's full responseAction waiter
// (chromedp.Navigate) on this goroutine, after the screencast and frame loop are
// live. The waiter learns the loaderID from the "init" lifecycle event, buffers
// pre-loaderID events, matches the top frame, and finishes on the matched load
// signal; lifecycle events are enabled per session so it can match. Running it
// here, off the launch path, keeps startup non-blocking so the raw-mode viewer
// keeps Ctrl-C live during launch (Decision 1). The waiter's completion is the
// bounded timeout backstop for the paint latch: watchPaint fires markPainted on
// the firstContentfulPaint lifecycle event, and this navigate-return call is the
// backstop so a page that never fires that event still closes the latch. A
// navigate errorText renders the browser's own error page rather than tearing the
// session down (Decision 1), so the supervisor never rebuilds on a failed URL.
func (e *ChromiumEngine) navigateAsync(ctx context.Context, url string, restore scrollRestore) {
	err := chromedp.Run(ctx, chromedp.Navigate(url))
	if e.recorder != nil {
		result := "ok"
		if err != nil {
			result = err.Error()
		}
		e.recorder.Record(captureEvent{Type: captureEventNavigate, URL: url, Result: result})
		e.fillSession(ctx, url, result)
	}
	// Restore the held scroll offset only after the load signal, so the document
	// exists before the wheel lands. This sequences navigate then scroll-restore on
	// one ordered path and closes the navigate-vs-scroll race.
	if restore.x != 0 || restore.y != 0 {
		wheel := input.DispatchMouseEvent(input.MouseWheel, restore.cx, restore.cy).
			WithDeltaX(float64(restore.x)).
			WithDeltaY(float64(restore.y))
		_ = chromedp.Run(ctx, wheel)
	}
	// Backstop the paint latch on navigate-return, whether the page loaded or the
	// browser rendered an error page, so a page that never fires firstContentfulPaint
	// still closes it. paintedOnce makes this a no-op once watchPaint has fired, and
	// markPainted is a no-op off the capture path.
	e.markPainted()
}

// fillSession assembles the cdp-session.json bytes from held engine state and
// stores them on the recorder when it is an exported *Recorder, so the viewer
// reads them after Navigate. It runs only on the capture path. It reads the
// capture and surface size under mu, then builds and marshals outside the lock, so
// it never holds mu across the assemble. The launch flags derive from proxyFlags,
// which carries only the credential-free Server, so the proxy user and password
// never reach the file. The browser version (Browser.getVersion) and target id
// (the live chromedp target) are queried from the live ctx, which exists here on
// the navigate path; a query error leaves the field empty rather than failing the
// bundle.
func (e *ChromiumEngine) fillSession(ctx context.Context, navURL, navResult string) {
	rec, ok := e.recorder.(*Recorder)
	if !ok {
		return
	}
	e.mu.Lock()
	capW, capH := e.captureW, e.captureH
	maxW, maxH := e.screencastCap(e.cols, e.rows)
	scale := e.layoutScale()
	e.mu.Unlock()
	version := browserVersion(ctx)
	targetID := liveTargetID(ctx)
	session := buildCDPSession(version, e.browserFlag, targetID, navURL, navResult, capW, capH, maxW, maxH, rec.droppedCount(), scale, e.proxy)
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return
	}
	rec.setSession(data)
}

// browserVersion returns the product version string Browser.getVersion reports,
// or "" on any error. It runs through chromedp.Run so the action gets the target
// executor context.
func browserVersion(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	var product string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		_, p, _, _, _, gerr := browser.GetVersion().Do(runCtx)
		product = p
		return gerr
	}))
	if err != nil {
		return ""
	}
	return product
}

// liveTargetID returns the CDP target id of the driven page from the live
// chromedp context, or "" when no target is attached.
func liveTargetID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	c := chromedp.FromContext(ctx)
	if c == nil || c.Target == nil {
		return ""
	}
	return c.Target.TargetID.String()
}

// finishStartup marks the supervisor as live after startup has a session. If
// Close won the race while startup was blocked, it cancels the new session and
// closes the lifecycle channels that no supervisor will own.
func (e *ChromiumEngine) finishStartup() error {
	e.mu.Lock()
	closing := e.closing
	frames := e.frames
	supDone := e.supDone
	if channelClosed(closing) {
		e.supervised = false
		e.mu.Unlock()
		e.teardownSession()
		if frames != nil {
			close(frames)
		}
		if supDone != nil {
			close(supDone)
		}
		return context.Canceled
	}
	e.supervised = true
	e.mu.Unlock()
	return nil
}

// channelClosed reports whether ch has already been closed without blocking.
func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// watchCrash registers a ListenTarget handler that closes crashed on the first
// Target.targetCrashed event. A sync.Once guards the close because the listener
// may fire more than once, matching the rest of the file's once idiom. The
// supervisor selects on crashed to trigger a rebuild.
func (e *ChromiumEngine) watchCrash(crashed chan struct{}) {
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	var once sync.Once
	chromedp.ListenTarget(ctx, func(ev any) {
		if _, ok := ev.(*target.EventTargetCrashed); ok {
			once.Do(func() {
				if e.recorder != nil {
					e.recorder.Record(captureEvent{Type: captureEventTargetCrashed})
				}
				close(crashed)
			})
		}
	})
}

// watchPaint registers a ListenTarget handler that watches the Page frame and
// lifecycle event streams plus the Runtime binding stream. On a Page.frameNavigated
// whose frame has no parent it records the main frame id, the gate for the paint arm.
// On a "firstContentfulPaint" lifecycle event it fires markPainted, the primary
// paint-gate fire source the snapshot goroutine reads as its first-snapshot clock,
// but only when the event's frame is the main frame; a subframe (iframe) painting
// first must not close the latch early. On that same main-frame paint, off the
// capture path, it also pokes the reflow channel so the crisp overlay decodes as
// soon as the page paints rather than waiting on networkAlmostIdle; capture keeps
// the painted latch and takes no poke. On "networkAlmostIdle" it pokes the reflow
// channel for a debounced late re-snapshot, catching a reflow that lands without
// navigation or resize (web fonts loading, async content). On a Runtime.bindingCalled
// for the layout-shift binding it pokes the same reflow channel, so a layout shift
// that lands after the last trigger (a late web-font swap, an image loading and
// reshaping a card) re-decodes the text and the overlay tracks the final layout; the
// injected observer already thresholds the shift score, and the reflow debounce and
// 1s cap rate-limit it. SetLifecycleEventsEnabled
// is on per session (launch), so the lifecycle events are delivered. paintedOnce
// latches the paint close, so a repeat event or the navigate-return backstop is a
// no-op; the reflow poke is latest-wins and rate-capped on the snapshot goroutine,
// so a chatty page cannot drive it. The listener attaches before navigation,
// mirroring watchCrash, so no event is missed. It reads e.reflow at event time, not
// at registration, because startSession sets the channel after this attaches. The
// main frame's own frameNavigated fires before its firstContentfulPaint in practice;
// if a paint event ever beats it the arm drops, and the navigate-return backstop
// still closes the latch.
func (e *ChromiumEngine) watchPaint() {
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	// mainFrame is closure-local and touched only from this listener. chromedp
	// dispatches every ListenTarget callback for a target sequentially from one
	// event goroutine, so the read and write need no extra locking.
	var mainFrame cdp.FrameID
	chromedp.ListenTarget(ctx, func(ev any) {
		switch t := ev.(type) {
		case *page.EventFrameNavigated:
			if t.Frame != nil && t.Frame.ParentID == "" {
				mainFrame = t.Frame.ID
			}
		case *page.EventLifecycleEvent:
			switch t.Name {
			case "firstContentfulPaint":
				if mainFrame != "" && t.FrameID == mainFrame {
					e.markPainted()
					// Production has no other snapshot trigger at first paint: the painted
					// latch is capture-only and the first publishSnapshot raced navigation
					// before the page painted, so on a heavy page text would otherwise wait
					// on networkAlmostIdle, seconds away. Poke the same debounced reflow the
					// networkAlmostIdle and layout-shift arms use, so the crisp overlay
					// decodes as soon as the page paints. Capture drives its first snapshot
					// off the painted latch and the textReady settle hold, so gating on
					// captureOn leaves capture's settle timing unchanged.
					if !e.captureOn {
						e.pokeReflow()
					}
				}
			case "networkAlmostIdle":
				e.pokeReflow()
			}
		case *runtime.EventBindingCalled:
			if isLayoutShiftBinding(t.Name) {
				e.recordLayoutShift()
				e.pokeReflow()
			}
		}
	})
}

// isLayoutShiftBinding reports whether a Runtime.bindingCalled name is the
// layout-shift binding, so the watchPaint arm pokes the reflow channel only for the
// engine's own binding and ignores any other page-registered binding. It is the pure
// match the listener runs, factored out so it is unit-tested without a browser.
func isLayoutShiftBinding(name string) bool {
	return name == layoutShiftBinding
}

// pokeReflow signals the snapshot goroutine for a debounced late re-snapshot. The
// send is latest-wins (select-with-default on a capacity-1 channel), so a burst of
// networkAlmostIdle events collapses to one pending wake and the event goroutine
// never blocks. It reads e.reflow under mu because watchPaint may fire before
// startSession sets the channel and teardownSession clears it; a nil channel drops
// the poke, which the no-backstop trigger tolerates.
func (e *ChromiumEngine) pokeReflow() {
	e.mu.Lock()
	reflow := e.reflow
	e.mu.Unlock()
	if reflow == nil {
		return
	}
	select {
	case reflow <- struct{}{}:
	default:
	}
}

// teardownSession tears down the live session without closing the frames
// channel, so it is safe on both a crash rebuild and Close. It drains the capture
// slot (releasing the frame loop parked on it; the held raw frame is already acked)
// then cancels the context, which stops the screencast, force-kills the managed
// Chromium, and releases the frame loop's send select. The live decode pool takes no
// drain: its workers and collector park on ctx.Done, so cancel alone releases them.
// It clears the per-session fields so a stale context is never reused.
func (e *ChromiumEngine) teardownSession() {
	e.mu.Lock()
	slot := e.slot
	cancel := e.cancel
	e.ctx, e.cancel, e.slot, e.acks, e.text = nil, nil, nil, nil, nil
	e.rawQ = nil
	e.decodeWorkers = 0
	e.reSnap, e.reflow, e.scroll, e.reEmit = nil, nil, nil, nil
	e.motion = nil
	e.screencastFmt = screencastFmt{}
	e.screencastStarted = false
	e.windowID = 0
	e.fontForcingID = ""
	e.layoutShiftID = ""
	e.mu.Unlock()
	if slot != nil {
		slot.drain()
	}
	if cancel != nil {
		cancel()
	}
}

// supervise owns the engine lifetime and is the single owner of the session
// fields, so no teardown races with Close. It waits for a crash, a
// lost-connection context cancellation, or Close. On Close it tears the live
// session down and closes frames, the single point that closes it. On a crash or
// a lost connection it tears the dead session down and rebuilds from held state,
// re-navigating to the held URL so frames resume. A rebuild that fails to
// relaunch ends the supervisor; frames closes and the consumer stops. Close has
// priority over a rebuild: when a crash or context cancellation races Close, the
// supervisor re-checks closing after the select and tears down rather than
// relaunching Chromium during shutdown.
func (e *ChromiumEngine) supervise(crashed chan struct{}) {
	defer close(e.supDone)
	defer close(e.frames)
	for {
		e.mu.Lock()
		ctx := e.ctx
		e.mu.Unlock()
		if ctx == nil {
			return
		}
		select {
		case <-e.closing:
			e.teardownSession()
			return
		case <-crashed:
		case <-ctx.Done():
		}
		// A concurrent Close always wins over a rebuild. The select picks a ready
		// case at random, so a crash or context cancellation racing Close can land
		// here with closing already set; re-check it and tear down instead of
		// relaunching Chromium during shutdown.
		if channelClosed(e.closing) {
			e.teardownSession()
			return
		}
		next, ok := e.rebuild()
		if !ok {
			return
		}
		crashed = next
	}
}

// rebuild tears down the dead session and starts a fresh one from held state. It
// returns the new session's crash channel and ok=true on success, or ok=false
// when the relaunch fails, in which case the supervisor stops.
func (e *ChromiumEngine) rebuild() (crashed chan struct{}, ok bool) {
	e.teardownSession()
	next, err := e.startSession()
	if err != nil {
		return nil, false
	}
	return next, true
}
