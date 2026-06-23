package engine

import (
	"context"
	"fmt"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
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
	capW, capH := e.captureW, e.captureH
	e.mu.Unlock()

	crashed = make(chan struct{})
	e.watchCrash(crashed)
	slot := newFrameSlot()
	e.mu.Lock()
	e.slot = slot
	e.mu.Unlock()
	if err = e.startScreencast(ctx, slot, capW, capH); err != nil {
		e.teardownSession()
		return nil, fmt.Errorf("engine: start screencast: %w", err)
	}
	go e.runFrameLoop(ctx, slot, frames)

	go e.navigateAsync(ctx, url)
	// Restore the held scroll offset after a rebuild so the page resumes where the
	// crash left off. A fresh navigate lands at the top, so a non-zero held offset
	// is re-applied as one mouseWheel at the viewport centre by the held deltas.
	if scrollX != 0 || scrollY != 0 {
		wheel := input.DispatchMouseEvent(input.MouseWheel, cx, cy).
			WithDeltaX(float64(scrollX)).
			WithDeltaY(float64(scrollY))
		_ = chromedp.Run(ctx, wheel)
	}
	return crashed, nil
}

// navigateAsync dispatches Page.navigate after the screencast and frame loop are
// live. Navigation errors tear down the session so the supervisor can rebuild or
// close the frame channel; they do not trap the raw-mode viewer before its key
// reader starts.
func (e *ChromiumEngine) navigateAsync(ctx context.Context, url string) {
	if err := dispatchNavigate(ctx, url); err != nil {
		e.teardownSession()
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

// dispatchNavigate sends Page.navigate through chromedp.Run so the action gets
// the target executor context, but avoids chromedp.Navigate's load-wait wrapper.
func dispatchNavigate(ctx context.Context, url string) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, errorText, _, err := page.Navigate(url).Do(ctx)
		if err != nil {
			return err
		}
		if errorText != "" {
			return fmt.Errorf("page load error %s", errorText)
		}
		return nil
	}))
}

// watchCrash registers a ListenTarget handler that closes crashed on the first
// Target.targetCrashed event. closeOnce guards the close because the listener
// may fire more than once. The supervisor selects on crashed to trigger a
// rebuild.
func (e *ChromiumEngine) watchCrash(crashed chan struct{}) {
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	var once boolFlag
	chromedp.ListenTarget(ctx, func(ev any) {
		if _, ok := ev.(*target.EventTargetCrashed); ok && once.set() {
			close(crashed)
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
	e.ctx, e.cancel, e.slot = nil, nil, nil
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
// relaunch ends the supervisor; frames closes and the consumer stops.
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

// boolFlag is a one-shot flag used to close a channel exactly once from a
// listener that may fire repeatedly. It is not safe for concurrent set calls,
// which is fine because ListenTarget delivers events to one handler serially.
type boolFlag struct{ done bool }

// set reports whether this is the first call, returning true once.
func (b *boolFlag) set() bool {
	if b.done {
		return false
	}
	b.done = true
	return true
}
