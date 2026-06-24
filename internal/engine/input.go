package engine

import (
	"context"
	"time"

	"github.com/chromedp/chromedp"
)

// Scroll step sizes in CSS pixels for the wheel transport. A line step matches a
// typical wheel notch; a page step is the viewport height, filled in at dispatch
// from the resolved cell geometry.
const wheelLineStepPx = 40

// inputDispatchTimeout bounds one input dispatch. A wedged browser must drop the
// keystroke rather than park the viewer's key goroutine forever, so each dispatch
// runs under this deadline; on expiry the key is dropped and the failure counted.
const inputDispatchTimeout = 2 * time.Second

// inputDispatchFailureThreshold is how many consecutive dispatch failures mark a
// persistent fault worth surfacing. One transient failure is dropped silently; a
// run of failures past this records one capture event so a wedged browser is
// observable in the bundle without spamming the stream.
const inputDispatchFailureThreshold = 5

// dispatchInput runs one input action under a bounded context so a wedged browser
// drops the keystroke instead of parking the caller goroutine. On a persistent
// run of failures it records one capture event (under capture) so the wedge is
// observable; a single transient failure is dropped silently. It never holds e.mu
// across the chromedp.Run.
func (e *ChromiumEngine) dispatchInput(ctx context.Context, action chromedp.Action) {
	if ctx == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, inputDispatchTimeout)
	defer cancel()
	err := chromedp.Run(runCtx, action)
	e.mu.Lock()
	if err != nil {
		e.inputFailures++
	} else {
		e.inputFailures = 0
	}
	failures := e.inputFailures
	e.mu.Unlock()
	if err != nil && failures == inputDispatchFailureThreshold && e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventInputDispatchFailed})
	}
}

// scrollKeyDelta maps a scroll key to a wheel delta in CSS pixels. It returns
// ok=false for any key that is not a scroll key, so the caller routes it to the
// focused-element key path instead. pageStepPx is the viewport height in CSS
// pixels, used for the Page keys; the line keys ignore it. The delta sign follows
// the wheel convention: positive deltaY scrolls the page content up (toward the
// end), so a down key produces a positive deltaY.
func scrollKeyDelta(code string, pageStepPx int) (dx, dy int, ok bool) {
	switch code {
	case "ArrowDown":
		return 0, wheelLineStepPx, true
	case "ArrowUp":
		return 0, -wheelLineStepPx, true
	case "ArrowRight":
		return wheelLineStepPx, 0, true
	case "ArrowLeft":
		return -wheelLineStepPx, 0, true
	case "PageDown":
		return 0, pageStepPx, true
	case "PageUp":
		return 0, -pageStepPx, true
	default:
		return 0, 0, false
	}
}
