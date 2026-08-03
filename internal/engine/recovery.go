package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// gracefulCloseTimeout gives healthy local Chromium time to flush state while
// keeping a stuck close below one second.
const gracefulCloseTimeout = 500 * time.Millisecond

// startSession builds one live browser session from the engine's held state and
// starts the screencast and frame loop. It launches the allocator and context,
// registers a crash watcher, starts the stream and consuming frame loop, then
// dispatches navigation to the held URL without waiting for full page load. The
// returned channel is closed when the page reports Target.targetCrashed, so the
// supervisor can rebuild. All session state (scroll, backend, cell px) is read
// from the engine, never from a dead context. A rebuild navigates to the held
// launch URL (Navigate records it once and OpenURL never rewrites it), keeping
// the backend and cell state; the held scroll offset replays only when the
// crashed page was that launch URL, after its first contentful paint.
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
	closing := e.closing
	scrollX, scrollY := e.scrollX, e.scrollY
	if e.currentURL != "" && e.currentURL != url {
		// The rebuild target is the launch URL, not the crashed page, so a
		// stale offset from another document must not replay onto it.
		scrollX, scrollY = 0, 0
		e.scrollX, e.scrollY, e.scrollYTarget = 0, 0, 0
	}
	cx, cy := wheelCentre(e.cols, e.rows, e.cellW, e.cellH, e.layoutScale())
	e.mu.Unlock()
	// Cache the browser product and user agent once per engine; a rebuild reuses
	// them, and a failure degrades to an empty version string and no UA override.
	e.fetchBrowserVersion(ctx)
	if err := e.applyUserAgentOverride(ctx); err != nil {
		e.teardownSession()
		return nil, err
	}

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
	// reflow (watchLifecycle), and scroll carries the debounced scroll-offset change the
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
	lifecycle := newSessionLifecycle()
	e.mu.Lock()
	e.rawQ = rawQ
	e.acks = acks
	e.ackPumpDone = ackPumpDone
	e.text = ts
	e.reSnap = reSnap
	e.reflow = reflow
	e.scroll = scroll
	e.reEmit = reEmit
	e.lifecycle = lifecycle
	e.timing = timer
	e.screencastFmt = screencastFmt{}
	e.screencastStarted = false
	e.mu.Unlock()

	// Every listener, queue, worker, collector, and acknowledgement path is live
	// before navigation. Page.navigate can commit and paint immediately, so no
	// event or first post-commit frame may race plumbing construction.
	crashed = make(chan struct{})
	e.watchCrash(crashed)
	e.watchLifecycle(ctx, lifecycle)
	go func() {
		defer close(ackPumpDone)
		e.ackPump(ctx, acks)
	}()
	// Register the frame listener once, on this fresh session context, before the
	// first startScreencast. restartStream re-issues startScreencast on the same
	// context, so registering here (not in startScreencast) keeps a backend switch
	// or resize from leaking a second listener.
	e.registerScreencastListener(ctx, rawQ)
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

	// Dispatch raw Page.navigate and wait only for its matching main-frame commit.
	// Capture starts after that commit, so about:blank can never enter the raw queue
	// or clear the viewer's loading cue. Full load and held subresources do not gate
	// progressive image delivery. The navGuard context aborts the unbounded
	// commit and readiness waits when the renderer crashes or Close begins:
	// on the supervised rebuild path this code runs on the supervisor
	// goroutine, and a navigation that never commits (a hung proxy, a
	// captive portal, a second renderer crash) would otherwise park the
	// supervisor forever and deadlock Close on supDone.
	navCtx, stopGuard := navGuard(ctx, crashed, closing)
	err = e.navigateAndStartCapture(ctx, navCtx, lifecycle, url)
	stopGuard()
	if err != nil {
		e.teardownSession()
		return nil, err
	}
	timer.mark(labelScreencastStarted)
	// Text sync starts only after the committed document exists. Its first
	// snapshot separately waits for this main frame's first contentful paint.
	go e.runTextSync(ctx, ts, reSnap, reflow, scroll, reEmit)
	if scrollX != 0 || scrollY != 0 {
		// Replay the held offset only after the main frame's first
		// contentful paint: at commit the document has no height yet, so
		// Chromium would clamp the wheel to zero and lose the position. The
		// goroutine is session-scoped; teardown cancels ctx and ends it.
		go func() {
			select {
			case <-lifecycle.fcp:
			case <-ctx.Done():
				return
			}
			wheel := input.DispatchMouseEvent(input.MouseWheel, cx, cy).
				WithDeltaX(float64(scrollX)).
				WithDeltaY(float64(scrollY))
			_ = chromedp.Run(ctx, wheel)
		}()
	}
	return crashed, nil
}

// navGuard derives a context for the startup navigation waits that is
// cancelled when the session crashes or the engine starts closing, so
// waitForCommit and the capture-readiness wait can never park the caller
// forever. The returned stop func releases the watcher; call it as soon as the
// guarded waits finish.
func navGuard(ctx context.Context, crashed, closing <-chan struct{}) (context.Context, func()) {
	navCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer cancel()
		select {
		case <-crashed:
		case <-closing:
		case <-stopped:
		}
	}()
	var once sync.Once
	return navCtx, func() { once.Do(func() { close(stopped) }) }
}

// navigateAndStartCapture is the startup ordering seam: prepared plumbing first,
// raw navigation second, matching commit third, and initial capture last. ctx is
// the session context; waitCtx bounds the navigation and readiness waits (the
// navGuard cancels it on a crash or Close) and must never reach the screencast
// start, whose session-identity check compares Done channels and would treat a
// derived context as a foreign session and silently skip the start.
func (e *ChromiumEngine) navigateAndStartCapture(ctx, waitCtx context.Context, lifecycle *sessionLifecycle, url string) error {
	if err := e.navigateToCommit(waitCtx, lifecycle, url); err != nil {
		return err
	}
	return e.startCaptureAfterCommit(ctx, waitCtx, lifecycle)
}

// startCaptureAfterCommit starts capture at the navigation commit. Chromium can
// briefly reject StartScreencast with "Not attached to an active page" while the
// committed renderer becomes active. That specific state waits for the matching
// main frame's DOMContentLoaded lifecycle event and retries once. This is an
// event-driven readiness retry, not a load wait or timer; a held subresource does
// not delay it. The readiness wait selects on waitCtx so a crash or Close
// releases it; the screencast starts take the session ctx, which carries the
// identity transitionScreencastLocked validates.
func (e *ChromiumEngine) startCaptureAfterCommit(ctx, waitCtx context.Context, lifecycle *sessionLifecycle) error {
	err := e.startInitialScreencast(ctx)
	if !pageNotActive(err) {
		return err
	}
	select {
	case <-lifecycle.domReady:
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
	return e.startInitialScreencast(ctx)
}

func pageNotActive(err error) bool {
	var cdpErr *cdproto.Error
	return errors.As(err, &cdpErr) && cdpErr.Code == -32000 && cdpErr.Message == "Not attached to an active page"
}

// navigateToCommit dispatches raw Page.navigate and waits for the matching
// main-frame EventFrameNavigated. It does not wait for DOMContentLoaded, load, or
// network idle. ErrorText remains renderable: Chromium commits its error document
// under the returned frame and loader, so it follows the same commit gate.
func (e *ChromiumEngine) navigateToCommit(ctx context.Context, lifecycle *sessionLifecycle, url string) error {
	frameID, loaderID, isDownload, err := e.runNavigate(ctx, url)
	if err != nil {
		return fmt.Errorf("engine: navigate: %w", err)
	}
	if isDownload {
		return fmt.Errorf("engine: navigate: target became a download")
	}
	if _, err := lifecycle.waitForCommit(ctx, frameID, loaderID); err != nil {
		return fmt.Errorf("engine: wait for navigation commit: %w", err)
	}
	return nil
}

// runNavigate executes raw Page.navigate or the deterministic test hook. The
// CDP errorText result is dropped: Chromium commits its error document under
// the returned frame and loader, so rendering needs only those.
func (e *ChromiumEngine) runNavigate(ctx context.Context, url string) (frameID cdp.FrameID, loaderID cdp.LoaderID, isDownload bool, err error) {
	if e.navigateOp != nil {
		return e.navigateOp(ctx, url)
	}
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		frameID, loaderID, _, isDownload, err = page.Navigate(url).Do(ctx)
		return err
	}))
	return
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

// navigationCommit is the identity of one committed top-frame document.
type navigationCommit struct {
	frameID  cdp.FrameID
	loaderID cdp.LoaderID
}

// sessionLifecycle retains navigation commits in an unbounded FIFO and latches
// main-frame first contentful paint. The listener can therefore run before the
// Page.navigate command without blocking or losing a commit that arrives before
// the command response supplies its frame and loader identifiers.
type sessionLifecycle struct {
	mu         sync.Mutex
	commits    []navigationCommit
	head       int
	ready      chan struct{}
	mainFrame  cdp.FrameID
	mainLoader cdp.LoaderID
	fcp        chan struct{}
	fcpOnce    sync.Once
	afterFCP   atomic.Bool
	domReady   chan struct{}
	domOnce    sync.Once
}

func newSessionLifecycle() *sessionLifecycle {
	return &sessionLifecycle{ready: make(chan struct{}, 1), fcp: make(chan struct{}), domReady: make(chan struct{})}
}

func (s *sessionLifecycle) recordDOMContentLoaded(frameID cdp.FrameID, loaderID cdp.LoaderID) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	mainFrame := s.mainFrame
	mainLoader := s.mainLoader
	s.mu.Unlock()
	if mainFrame == "" || frameID != mainFrame || (loaderID != "" && mainLoader != "" && loaderID != mainLoader) {
		return false
	}
	s.domOnce.Do(func() { close(s.domReady) })
	return true
}

func (s *sessionLifecycle) recordCommit(frame *cdp.Frame) {
	if s == nil || frame == nil || frame.ParentID != "" {
		return
	}
	s.mu.Lock()
	s.mainFrame = frame.ID
	s.mainLoader = frame.LoaderID
	s.commits = append(s.commits, navigationCommit{frameID: frame.ID, loaderID: frame.LoaderID})
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func (s *sessionLifecycle) recordFCP(frameID cdp.FrameID, loaderID ...cdp.LoaderID) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	mainFrame := s.mainFrame
	mainLoader := s.mainLoader
	s.mu.Unlock()
	eventLoader := cdp.LoaderID("")
	if len(loaderID) > 0 {
		eventLoader = loaderID[0]
	}
	if mainFrame == "" || frameID != mainFrame || (eventLoader != "" && mainLoader != "" && eventLoader != mainLoader) {
		return false
	}
	s.afterFCP.Store(true)
	s.fcpOnce.Do(func() { close(s.fcp) })
	return true
}

// isMainFrame reports whether frameID is the recorded main frame, so the
// lifecycle watcher applies within-document URL updates from the top frame
// only and ignores subframe events. It is false before the first commit.
func (s *sessionLifecycle) isMainFrame(frameID cdp.FrameID) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mainFrame != "" && frameID == s.mainFrame
}

func (s *sessionLifecycle) popMatching(frameID cdp.FrameID, loaderID cdp.LoaderID) (navigationCommit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.head < len(s.commits) {
		commit := s.commits[s.head]
		s.head++
		if (frameID == "" || commit.frameID == frameID) && (loaderID == "" || commit.loaderID == loaderID) {
			if s.head == len(s.commits) {
				s.commits = s.commits[:0]
				s.head = 0
			}
			return commit, true
		}
	}
	s.commits = s.commits[:0]
	s.head = 0
	return navigationCommit{}, false
}

func (s *sessionLifecycle) waitForCommit(ctx context.Context, frameID cdp.FrameID, loaderID cdp.LoaderID) (navigationCommit, error) {
	if s == nil {
		return navigationCommit{}, fmt.Errorf("no lifecycle tracker")
	}
	for {
		if commit, ok := s.popMatching(frameID, loaderID); ok {
			return commit, nil
		}
		select {
		case <-s.ready:
		case <-ctx.Done():
			return navigationCommit{}, ctx.Err()
		}
	}
}

// afterMainFrameFCP reports the listener-ordered FCP state for a newly received
// screencast frame. Tests without session lifecycle retain the historical true
// default, while every live session installs a tracker before capture can start.
func (e *ChromiumEngine) afterMainFrameFCP() bool {
	e.mu.Lock()
	lifecycle := e.lifecycle
	e.mu.Unlock()
	return lifecycle == nil || lifecycle.afterFCP.Load()
}

// watchLifecycle records top-frame commits, latches the matching main-frame FCP,
// and retains the existing late-reflow and layout-shift triggers. It also
// records the committed main-frame URL for CurrentURL, kept fresh across SPA
// and fragment moves by Page.navigatedWithinDocument, and drops the page-search
// state on a main-frame commit, because the new document carries neither the
// highlights nor the page-side match list. It attaches before raw navigation and
// before screencast start.
func (e *ChromiumEngine) watchLifecycle(ctx context.Context, lifecycle *sessionLifecycle) {
	chromedp.ListenTarget(ctx, func(ev any) {
		switch t := ev.(type) {
		case *page.EventFrameNavigated:
			lifecycle.recordCommit(t.Frame)
			if t.Frame != nil && t.Frame.ParentID == "" {
				e.recordCurrentURL(t.Frame.URL)
				// The new document carries neither the search highlights nor
				// the page-side search state, so the held counts go with it.
				e.resetFind()
			}
		case *page.EventNavigatedWithinDocument:
			if lifecycle.isMainFrame(t.FrameID) {
				e.recordCurrentURL(t.URL)
			}
		case *page.EventLifecycleEvent:
			switch t.Name {
			case "DOMContentLoaded":
				lifecycle.recordDOMContentLoaded(t.FrameID, t.LoaderID)
			case "firstContentfulPaint":
				if lifecycle.recordFCP(t.FrameID, t.LoaderID) {
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
// layout-shift binding, so the lifecycle watcher pokes the reflow channel only for the
// engine's own binding and ignores any other page-registered binding. It is the pure
// match the listener runs, factored out so it is unit-tested without a browser.
func isLayoutShiftBinding(name string) bool {
	return name == layoutShiftBinding
}

// pokeReflow signals the snapshot goroutine for a debounced late re-snapshot. The
// send is latest-wins (select-with-default on a capacity-1 channel), so a burst of
// networkAlmostIdle events collapses to one pending wake and the event goroutine
// never blocks. It reads e.reflow under mu because watchLifecycle may fire before
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
// channel, so normal Close, error cleanup, and crash rebuild share one path. For
// a live session it asks chromedp.Cancel to send Browser.close, bounded by
// gracefulCloseTimeout. A close error, timeout, or dead session selects raw
// cancellation, so browser or CDP failure cannot block exit or crash rebuild.
// It then waits for the session's sole frame publisher and drains frames buffered
// before cancellation won its selects.
// This prevents an old-session frame surviving recovery. Decode workers need no join
// because their decoded channel is session-local and only the joined collector can
// reach frames. It clears the per-session fields so a stale context is never reused.
func (e *ChromiumEngine) teardownSession() {
	e.mu.Lock()
	ctx := e.ctx
	closeBrowser := e.closeBrowserOp
	cancel := e.cancel
	closeTimeout := e.closeTimeout
	acks := e.acks
	ackPumpDone := e.ackPumpDone
	publisherDone := e.framePublisherDone
	frames := e.frames
	e.ctx, e.closeBrowserOp, e.cancel, e.acks, e.text = nil, nil, nil, nil, nil
	e.ackPumpDone = nil
	e.rawQ = nil
	e.reSnap, e.reflow, e.scroll, e.reEmit = nil, nil, nil, nil
	e.lifecycle = nil
	e.framePublisherDone = nil
	e.screencastFmt = screencastFmt{}
	e.screencastStarted = false
	e.sizedSurfaceW, e.sizedSurfaceH, e.sizedScale = 0, 0, 0
	e.windowID = 0
	e.fontForcingID = ""
	e.forcedCellW, e.forcedCellH = 0, 0
	e.layoutShiftID = ""
	e.mu.Unlock()
	acks.stop()
	if closeTimeout <= 0 {
		closeTimeout = gracefulCloseTimeout
	}
	if ctx != nil && ctx.Err() == nil && closeBrowser != nil {
		// chromedp.Cancel cancels ctx before it returns. Keep that cancellation
		// separate from the deadline that bounds the graceful close attempt.
		closeCtx, stopClose := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		closeDone := make(chan error, 1)
		go func() {
			closeDone <- closeBrowser(closeCtx)
		}()
		var closeFailed bool
		select {
		case err := <-closeDone:
			closeFailed = err != nil || errors.Is(closeCtx.Err(), context.DeadlineExceeded)
		case <-closeCtx.Done():
			closeFailed = true
		}
		stopClose()
		if closeFailed && cancel != nil {
			cancel()
		}
	} else if cancel != nil {
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
	startSession := e.startSession
	if e.startSessionOp != nil {
		startSession = e.startSessionOp
	}
	next, err := startSession()
	if err != nil {
		return nil, false
	}
	return next, true
}
