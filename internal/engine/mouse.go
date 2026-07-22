package engine

import (
	"time"

	"github.com/chromedp/cdproto/input"
)

// MouseAction classifies a forwarded mouse event: a button press, a button
// release, or one wheel step.
type MouseAction int

const (
	// MouseActionPress is a button press.
	MouseActionPress MouseAction = iota
	// MouseActionRelease is a button release.
	MouseActionRelease
	// MouseActionWheel is one wheel step at the pointer cell.
	MouseActionWheel
)

// MouseButton identifies the pointer button of a press or release.
type MouseButton int

const (
	// MouseButtonLeft is the primary button.
	MouseButtonLeft MouseButton = iota
	// MouseButtonMiddle is the wheel button.
	MouseButtonMiddle
	// MouseButtonRight is the secondary button; Chromium turns a right press
	// into the page's context-menu event.
	MouseButtonRight
)

// MouseEvent is a terminal-neutral mouse event forwarded to the page. The
// viewer forwards terminal-native cells; the engine owns the cell-to-CSS
// conversion and clamps the cell to its grid.
type MouseEvent struct {
	// Action is press, release, or wheel.
	Action MouseAction
	// Button is the pressed or released button; ignored for wheel events.
	Button MouseButton
	// Mod is the CDP modifier bitmask (Alt=1, Ctrl=2, Meta=4, Shift=8).
	Mod int
	// Col and Row are the 0-based terminal cell of the pointer.
	Col int
	Row int
	// WheelDX and WheelDY carry the unit wheel step direction (-1, 0, or +1
	// per axis) for MouseActionWheel; up and left are negative.
	WheelDX int
	WheelDY int
}

// clickChainWindow bounds a double or triple click: a press later than this
// after the previous press starts a new chain.
const clickChainWindow = 400 * time.Millisecond

// clickChainMax caps the synthesised clickCount. Chromium uses 2 for word
// select and 3 for line select; higher counts add nothing.
const clickChainMax = 3

// clickState is the press-chain state clickCount synthesis carries between
// presses: the chain length, the button, the press cell, and the press time.
// It lives on the engine under e.mu beside the scroll state.
type clickState struct {
	count    int
	button   MouseButton
	col, row int
	at       time.Time
}

// nextClickCount synthesises the CDP clickCount for a press at now and
// returns the updated chain state. A press chains (count+1, capped at
// clickChainMax) only when the same button lands on the same cell within
// clickChainWindow of the previous press; integer cell coordinates make the
// same-cell test the sub-cell movement tolerance. Anything else starts a new
// chain at 1. It is pure, so tests drive explicit timestamps.
func nextClickCount(prev clickState, button MouseButton, col, row int, now time.Time) (int, clickState) {
	count := 1
	if prev.count > 0 && button == prev.button && col == prev.col && row == prev.row &&
		now.Sub(prev.at) <= clickChainWindow {
		count = min(prev.count+1, clickChainMax)
	}
	return count, clickState{count: count, button: button, col: col, row: row, at: now}
}

// releaseClickCount returns the clickCount a release dispatches: the chain
// count of the matching press, or 1 when no press chain matches, so a release
// never reports the zero count CDP treats as no click.
func releaseClickCount(prev clickState, button MouseButton) int {
	if prev.count > 0 && button == prev.button {
		return prev.count
	}
	return 1
}

// cellCentreCSS maps a 0-based terminal cell to the CSS-pixel centre of that
// cell, ((col+0.5)*cellW, (row+0.5)*cellH), where cellW and cellH are the
// CSS-pixel cell size snapshotCellPx derives: the device cell divided by the
// layout scale Z, so the mapping is the Z-aware (col+0.5)*cellW/Z form. The
// cell clamps to the cols x rows grid first, so a report outside the grid
// lands on the nearest edge cell.
func cellCentreCSS(col, row, cols, rows int, cellW, cellH float64) (x, y float64) {
	col = min(max(col, 0), max(cols-1, 0))
	row = min(max(row, 0), max(rows-1, 0))
	return (float64(col) + 0.5) * cellW, (float64(row) + 0.5) * cellH
}

// cdpButton maps a MouseButton to the CDP button name.
func cdpButton(b MouseButton) input.MouseButton {
	switch b {
	case MouseButtonMiddle:
		return input.Middle
	case MouseButtonRight:
		return input.Right
	default:
		return input.Left
	}
}

// cdpButtonMask maps a MouseButton to its CDP pressed-buttons mask bit:
// Left=1, Right=2, Middle=4.
func cdpButtonMask(b MouseButton) int64 {
	switch b {
	case MouseButtonMiddle:
		return 4
	case MouseButtonRight:
		return 2
	default:
		return 1
	}
}

// mouseParams builds one CDP mouse press or release dispatch at the CSS
// position x, y: the named button, the pressed-buttons mask (a release
// reports no held button), the CDP modifier bits, and the synthesised
// clickCount.
func mouseParams(action MouseAction, button MouseButton, mod, clickCount int, x, y float64) *input.DispatchMouseEventParams {
	mt, buttons := input.MousePressed, cdpButtonMask(button)
	if action == MouseActionRelease {
		mt, buttons = input.MouseReleased, 0
	}
	return input.DispatchMouseEvent(mt, x, y).
		WithButton(cdpButton(button)).
		WithButtons(buttons).
		WithClickCount(int64(clickCount)).
		WithModifiers(input.Modifier(mod))
}

// SendMouse forwards one mouse event to the page. A wheel step scrolls
// through the one wheel transport at the pointer cell, so Block's whole-cell
// vertical quantisation and the crash-recovery scroll offset keep working. A
// press or release dispatches a CDP mouse event at the cell centre with a
// synthesised clickCount, through the same bounded dispatch seam as keys, so
// a wedged browser drops the event instead of parking the caller.
func (e *ChromiumEngine) SendMouse(ev MouseEvent) {
	if ev.Action == MouseActionWheel {
		e.dispatchWheelAt(ev.WheelDX*wheelTickPx, ev.WheelDY*wheelTickPx, ev.Col, ev.Row)
		return
	}
	e.mu.Lock()
	ctx := e.ctx
	cellW, cellH := e.snapshotCellPx()
	x, y := cellCentreCSS(ev.Col, ev.Row, e.cols, e.rows, cellW, cellH)
	var count int
	if ev.Action == MouseActionPress {
		count, e.click = nextClickCount(e.click, ev.Button, ev.Col, ev.Row, time.Now())
	} else {
		count = releaseClickCount(e.click, ev.Button)
	}
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	e.dispatchInput(ctx, mouseParams(ev.Action, ev.Button, ev.Mod, count, x, y))
}
