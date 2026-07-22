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

// wheelTickPx is the CSS-pixel delta one SGR wheel event dispatches: three
// wheelLineStepPx lines, the Charm viewport convention. Wheel events are
// accumulable: high-precision devices emit several per physical tick and each
// one dispatches, so the transport sums a burst instead of debouncing it.
const wheelTickPx = 3 * wheelLineStepPx

// inputDispatchTimeout bounds one input dispatch. A wedged browser must drop the
// keystroke rather than park the viewer's key goroutine forever, so each dispatch
// runs under this deadline; on expiry the key is dropped and the failure counted.
const inputDispatchTimeout = 2 * time.Second

// dispatchInput runs one input action under a bounded context so a wedged browser
// drops the keystroke instead of parking the caller goroutine. It records the
// consecutive failure count for debug inspection and never holds e.mu across
// chromedp.Run.
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
	e.mu.Unlock()
}

// wheelCentre returns the CSS-pixel viewport centre the wheel transport
// dispatches at. DispatchMouseEvent speaks CSS pixels, but cols*cellW and
// rows*cellH are device pixels, so both axes divide by the layout scale Z, the
// same divide pageStepPx applies to its delta; at Z 1 the divide is a no-op.
func wheelCentre(cols, rows, cellW, cellH int, z float64) (cx, cy float64) {
	return float64(cols*cellW) / 2 / z, float64(rows*cellH) / 2 / z
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
