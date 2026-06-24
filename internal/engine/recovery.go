package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
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
	if err = e.launch(e.browserFlag); err != nil {
		return nil, err
	}
	e.mu.Lock()
	ctx := e.ctx
	url := e.url
	frames := e.frames
	scrollX, scrollY := e.scrollX, e.scrollY
	cx := float64(e.cols*e.cellW) / 2
	cy := float64(e.rows*e.cellH) / 2
	// The screencast cap is the real pixel surface size, so Chromium emits real
	// pixels; the engine downscales the block frame to the sub-cell captureSize
	// client-side.
	capW, capH := e.surfaceSize(e.cols, e.rows)
	e.mu.Unlock()

	crashed = make(chan struct{})
	e.watchCrash(crashed)
	slot := newFrameSlot()
	// acks is the buffered ack channel the ack pump drains. It is created before
	// startScreencast so the listener's first enqueue lands, and the pump runs off
	// the event goroutine to avoid the chromedp #920 deadlock. The buffer is sized
	// to a few in-flight frames so a transient pump stall never blocks the listener
	// or the slot's displaced-frame ack.
	acks := make(chan int64, ackBufferDepth)
	ts := &textSync{}
	// snapTrigger gates the snapshot goroutine to about one snapshot per delivered
	// frame: the frame loop pokes it (latest-wins, non-blocking) after each consumed
	// frame, and runTextSync wakes once per poke. Capacity 1 coalesces a burst to one
	// wake; on a static page with no frames the snapshot goroutine blocks on it and
	// does zero snapshots. It is goroutine-scoped to this session, so ctx cancellation
	// stops both goroutines and the channel needs no teardown reset.
	snapTrigger := make(chan struct{}, 1)
	e.mu.Lock()
	e.slot = slot
	e.acks = acks
	e.text = ts
	e.mu.Unlock()
	if err = e.startScreencast(ctx, slot, capW, capH); err != nil {
		e.teardownSession()
		return nil, fmt.Errorf("engine: start screencast: %w", err)
	}
	go e.ackPump(ctx, acks)
	go e.runTextSync(ctx, ts, snapTrigger)
	go e.runFrameLoop(ctx, slot, frames, ts, snapTrigger)

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
// load signal: it closes the capture paint latch. A navigate errorText renders
// the browser's own error page rather than tearing the session down (Decision 1),
// so the supervisor never rebuilds on a failed URL.
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
	// The load signal fires the capture paint latch, whether the page loaded or the
	// browser rendered an error page. markPainted is a no-op off the capture path.
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
	maxW, maxH := e.surfaceSize(e.cols, e.rows)
	e.mu.Unlock()
	version := browserVersion(ctx)
	targetID := liveTargetID(ctx)
	session := buildCDPSession(version, e.browserFlag, targetID, navURL, navResult, capW, capH, maxW, maxH, rec.droppedCount(), e.proxy)
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

// teardownSession tears down the live session without closing the frames
// channel, so it is safe on both a crash rebuild and Close. It drains the slot
// (acking any held frame and releasing the frame loop parked on it) then cancels
// the context, which stops the screencast, force-kills the managed Chromium, and
// releases the frame loop's send select. It clears the per-session fields so a
// stale context is never reused.
func (e *ChromiumEngine) teardownSession() {
	e.mu.Lock()
	slot := e.slot
	cancel := e.cancel
	e.ctx, e.cancel, e.slot, e.acks, e.text = nil, nil, nil, nil, nil
	e.windowID = 0
	e.mu.Unlock()
	if slot != nil {
		slot.drain(e.ackFrame)
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
