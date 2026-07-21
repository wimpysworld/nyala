package engine

import (
	"context"
	"sync"

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
	timer := newLoadTimer()
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
	// Feed a bounded, drop-oldest queue to a small pool of decode
	// workers, so several frames decode in parallel and the block-backend frame rate is
	// no longer capped by a single ~28 ms decode. A collector re-serialises the pool by
	// arrival sequence, so the live screen stays monotonic.
	workers := decodeWorkerCount()
	rawQ := newRawFrameQueue(workers + 1)
	// acks is the unbounded FIFO the ack pump drains. The pump starts before
	// StartScreencast, so the first delivered frame can release Chromium's in-flight
	// credit at once without running a CDP command on the event goroutine.
	acks := newAckQueue()
	ackPumpDone := make(chan struct{})
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
	e.mu.Lock()
	e.rawQ = rawQ
	e.acks = acks
	e.ackPumpDone = ackPumpDone
	e.text = ts
	e.reSnap = reSnap
	e.reflow = reflow
	e.scroll = scroll
	e.reEmit = reEmit
	e.timing = timer
	e.screencastFmt = screencastFmt{}
	e.screencastStarted = false
	e.mu.Unlock()
	go func() {
		defer close(ackPumpDone)
		e.ackPump(ctx, acks)
	}()
	// Register the frame listener once, on this fresh session context, before the
	// first startScreencast. restartStream re-issues startScreencast on the same
	// context, so registering here (not in startScreencast) keeps a backend switch
	// or resize from leaking a second listener.
	e.registerScreencastListener(ctx, rawQ)
	if err = e.startInitialScreencast(ctx); err != nil {
		e.teardownSession()
		return nil, err
	}
	timer.mark(labelScreencastStarted)
	go e.runTextSync(ctx, ts, reSnap, reflow, scroll, reEmit)
	{
		// Live: a pool of decode workers feeds one collector that re-serialises them by
		// arrival sequence and owns the compose, the held-frame re-emit, the scroll
		// tracking, and the frame send. The decoded channel is buffered to the worker
		// count so a finished worker never blocks handing off to the collector.
		decoded := make(chan decodedFrame, workers)
		publisherDone := make(chan struct{})
		e.mu.Lock()
		e.framePublisherDone = publisherDone
		e.mu.Unlock()
		for range workers {
			go e.runDecodeWorker(ctx, rawQ, decoded)
		}
		go func() {
			defer close(publisherDone)
			e.runDecodeCollector(ctx, decoded, frames, ts, scroll, reEmit)
		}()
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
// keeps Ctrl-C live during launch. The waiter's completion is the
// bounded timeout backstop for the paint latch: watchPaint fires markPainted on
// the firstContentfulPaint lifecycle event, and this navigate-return call is the
// backstop so a page that never fires that event still closes the latch. A
// navigate errorText renders the browser's own error page rather than tearing the
// session down, so the supervisor never rebuilds on a failed URL.
func (e *ChromiumEngine) navigateAsync(ctx context.Context, url string, restore scrollRestore) {
	_ = chromedp.Run(ctx, chromedp.Navigate(url))
	// Restore the held scroll offset only after the load signal, so the document
	// exists before the wheel lands. This sequences navigate then scroll-restore on
	// one ordered path and closes the navigate-vs-scroll race.
	if restore.x != 0 || restore.y != 0 {
		wheel := input.DispatchMouseEvent(input.MouseWheel, restore.cx, restore.cy).
			WithDeltaX(float64(restore.x)).
			WithDeltaY(float64(restore.y))
		_ = chromedp.Run(ctx, wheel)
	}
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
// first must not close the latch early. On that same main-frame paint it pokes the
// reflow channel so the crisp overlay decodes promptly. On "networkAlmostIdle" it pokes the reflow
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
					e.pokeReflow()
				}
			case "networkAlmostIdle":
				e.pokeReflow()
			}
		case *runtime.EventBindingCalled:
			if isLayoutShiftBinding(t.Name) {
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
// channel, so it is safe on both a crash rebuild and Close. It cancels the context,
// which stops the screencast, force-kills the managed
// Chromium, and releases the publisher's send select. It then waits for the session's
// sole frame publisher and drains frames buffered before cancellation won its selects.
// This prevents an old-session frame surviving recovery. Decode workers need no join
// because their decoded channel is session-local and only the joined collector can
// reach frames. It clears the per-session fields so a stale context is never reused.
func (e *ChromiumEngine) teardownSession() {
	e.mu.Lock()
	cancel := e.cancel
	acks := e.acks
	ackPumpDone := e.ackPumpDone
	publisherDone := e.framePublisherDone
	frames := e.frames
	e.ctx, e.cancel, e.acks, e.text = nil, nil, nil, nil
	e.ackPumpDone = nil
	e.rawQ = nil
	e.reSnap, e.reflow, e.scroll, e.reEmit = nil, nil, nil, nil
	e.framePublisherDone = nil
	e.screencastFmt = screencastFmt{}
	e.screencastStarted = false
	e.windowID = 0
	e.fontForcingID = ""
	e.layoutShiftID = ""
	e.mu.Unlock()
	acks.stop()
	if cancel != nil {
		cancel()
	}
	if ackPumpDone != nil {
		<-ackPumpDone
	}
	if publisherDone != nil {
		<-publisherDone
		for {
			select {
			case _, ok := <-frames:
				if !ok {
					return
				}
			default:
				return
			}
		}
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
