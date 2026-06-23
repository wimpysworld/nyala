package engine

// Scroll step sizes in CSS pixels for the wheel transport. A line step matches a
// typical wheel notch; a page step is the viewport height, filled in at dispatch
// from the resolved cell geometry.
const wheelLineStepPx = 40

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
