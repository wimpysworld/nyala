package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
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
	// MouseActionMove is pointer motion. Button carries either the held button,
	// which the dispatch reports as the move's pressed button so the page sees a
	// drag of that button, or MouseButtonNone, which the dispatch reports with an
	// empty pressed-buttons mask so the page sees a hover.
	MouseActionMove
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
	// MouseButtonNone is the no-button value a hover move carries. It dispatches
	// as the CDP none button with an empty pressed-buttons mask, so Chromium
	// reads the move as a hover rather than a drag. It is appended last on
	// purpose: MouseButtonLeft is the iota zero value, so a zero MouseEvent still
	// means the primary button.
	MouseButtonNone
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

// cdpButton maps a MouseButton to the CDP button name. MouseButtonNone needs
// its own arm rather than the default one, which answers Left: without it the
// no-button value a hover carries would forge a left button on the wire.
func cdpButton(b MouseButton) input.MouseButton {
	switch b {
	case MouseButtonMiddle:
		return input.Middle
	case MouseButtonRight:
		return input.Right
	case MouseButtonNone:
		return input.None
	default:
		return input.Left
	}
}

// cdpButtonMask maps a MouseButton to its CDP pressed-buttons mask bit:
// Left=1, Right=2, Middle=4, and MouseButtonNone holds none at 0. The no-button
// value needs its own arm for the same reason cdpButton does: the default arm
// answers 1, so a hover would arrive carrying a held left button.
func cdpButtonMask(b MouseButton) int64 {
	switch b {
	case MouseButtonMiddle:
		return 4
	case MouseButtonRight:
		return 2
	case MouseButtonNone:
		return 0
	default:
		return 1
	}
}

// mouseParams builds one CDP mouse press, release, or move dispatch at the CSS
// position x, y: the named button, the pressed-buttons mask (a release
// reports no held button), the CDP modifier bits, and the synthesised
// clickCount. A move reports whatever button it carries in both the button
// name and the mask, so a held button reads as a drag of that button and
// MouseButtonNone reads as a hover with an empty mask, and it carries
// clickCount 0 because it starts no click. On the wire a hover sends
// button "none", the value CDP documents as the field's default, and no
// buttons field at all: the cdproto field is tagged omitempty,omitzero, so the
// empty mask is dropped, and CDP reads the missing field as None=0.
func mouseParams(action MouseAction, button MouseButton, mod, clickCount int, x, y float64) *input.DispatchMouseEventParams {
	mt, buttons := input.MousePressed, cdpButtonMask(button)
	switch action {
	case MouseActionRelease:
		mt, buttons = input.MouseReleased, 0
	case MouseActionMove:
		mt = input.MouseMoved
	}
	return input.DispatchMouseEvent(mt, x, y).
		WithButton(cdpButton(button)).
		WithButtons(buttons).
		WithClickCount(int64(clickCount)).
		WithModifiers(input.Modifier(mod))
}

// wheelParams builds one CDP mouseWheel dispatch at the CSS position x, y with
// the signed scroll deltas and the CDP modifier bits. The delta signs follow
// the DOM wheel convention CDP speaks: positive deltaY scrolls the content
// towards the document end and positive deltaX towards its right edge, so a
// wheel-right report carries a positive deltaX and increases scrollLeft, the
// same way the gesture behaves in a desktop browser.
func wheelParams(x, y float64, dx, dy, mod int) *input.DispatchMouseEventParams {
	return input.DispatchMouseEvent(input.MouseWheel, x, y).
		WithDeltaX(float64(dx)).
		WithDeltaY(float64(dy)).
		WithModifiers(input.Modifier(mod))
}

// hoverReadsCursor reports whether a mouse dispatch also reads the computed
// cursor at the pointer: only a hover move, a move with no held button, does.
// A press, release, wheel, or drag never evaluates, so the input path stays a
// pure event send for every gesture that changes page state.
func hoverReadsCursor(action MouseAction, button MouseButton) bool {
	return action == MouseActionMove && button == MouseButtonNone
}

// pointerCursorExpr builds the one-expression computed-cursor read at the CSS
// position x, y. It reads only the cursor property, never the full style, and
// the arrow-function guard answers the empty string when elementFromPoint
// finds no element, because getComputedStyle(null) would throw.
func pointerCursorExpr(x, y float64) string {
	return fmt.Sprintf(
		"(el => el ? getComputedStyle(el).cursor ?? '' : '')(document.elementFromPoint(%g, %g))",
		x, y)
}

// cursorCtxSource picks the execution context source for one hover cursor
// read: the held layout-shift context wins because it already exists, the
// cached cursor fallback comes next, and a zero pair asks the read to create
// a fresh isolated world. It is pure, so the test drives every combination.
func cursorCtxSource(held, cached runtime.ExecutionContextID) (ctxID runtime.ExecutionContextID, create bool) {
	if held != 0 {
		return held, false
	}
	if cached != 0 {
		return cached, false
	}
	return 0, true
}

// isContextLostErr reports whether an evaluate error names a destroyed
// execution context, the "Cannot find context" answer Chromium gives after a
// navigation invalidates isolated worlds.
func isContextLostErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Cannot find context")
}

// mainFrameID returns the committed main frame id of the live session, empty
// before the first commit or when no session is live.
func (e *ChromiumEngine) mainFrameID() cdp.FrameID {
	e.mu.Lock()
	lc := e.lifecycle
	e.mu.Unlock()
	if lc == nil {
		return ""
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.mainFrame
}

// cursorReadContext resolves the execution context one hover cursor read
// evaluates in, per cursorCtxSource. When neither the held layout-shift
// context nor the cached fallback exists, it creates an isolated world on the
// main frame under the layoutShiftWorld name, so a document carries at most
// one extra world, and caches the id for later hovers. A missing main frame
// or a create failure answers 0, which skips the read.
func (e *ChromiumEngine) cursorReadContext(c context.Context, held runtime.ExecutionContextID) runtime.ExecutionContextID {
	e.mu.Lock()
	cached := e.cursorCtxID
	e.mu.Unlock()
	ctxID, create := cursorCtxSource(held, cached)
	if !create {
		return ctxID
	}
	frameID := e.mainFrameID()
	if frameID == "" {
		return 0
	}
	created, err := page.CreateIsolatedWorld(frameID).WithWorldName(layoutShiftWorld).Do(c)
	if err != nil {
		return 0
	}
	e.mu.Lock()
	e.cursorCtxID = created
	e.mu.Unlock()
	return created
}

// clearCursorCtx drops the cached cursor context when it still matches ctxID,
// so the next hover recreates the world, and never clobbers a newer id.
func (e *ChromiumEngine) clearCursorCtx(ctxID runtime.ExecutionContextID) {
	e.mu.Lock()
	if e.cursorCtxID == ctxID {
		e.cursorCtxID = 0
	}
	e.mu.Unlock()
}

// pointerCursorReadAction returns the chromedp action a hover dispatch appends:
// it evaluates pointerCursorExpr in an isolated world through
// walkEvaluateParams, so the page cannot observe the read, and records the
// value PointerCursor reports. heldCtxID is the layout-shift context the Block
// backend holds, 0 elsewhere; cursorReadContext then falls back to the cached
// cursor world or creates one. Every failure - a missing context, an evaluate
// error, a thrown expression, a missing value, or a decode error - returns nil
// and leaves the stored value unchanged, so a flaky read never counts as an
// input failure. A context-lost error additionally drops the cached fallback,
// so the next hover recreates the world the navigation destroyed.
func (e *ChromiumEngine) pointerCursorReadAction(heldCtxID runtime.ExecutionContextID, x, y float64) chromedp.Action {
	return chromedp.ActionFunc(func(c context.Context) error {
		ctxID := e.cursorReadContext(c, heldCtxID)
		if ctxID == 0 {
			return nil
		}
		res, exc, err := walkEvaluateParams(pointerCursorExpr(x, y), ctxID).Do(c)
		if isContextLostErr(err) {
			e.clearCursorCtx(ctxID)
			return nil
		}
		if err != nil || exc != nil || res == nil || len(res.Value) == 0 {
			return nil
		}
		var cursor string
		if json.Unmarshal(res.Value, &cursor) != nil {
			return nil
		}
		e.recordPointerCursor(cursor)
		return nil
	})
}

// recordPointerCursor stores the raw computed cursor value PointerCursor
// reports.
func (e *ChromiumEngine) recordPointerCursor(cursor string) {
	e.mu.Lock()
	e.pointerCursor = cursor
	e.mu.Unlock()
}

// PointerCursor returns the raw computed cursor value of the last successful
// hover read. It is a snapshot getter beside CurrentTitle and LoadState: it
// never waits on the browser, and it is empty before the first read.
func (e *ChromiumEngine) PointerCursor() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pointerCursor
}

// SendMouse forwards one mouse event to the page. A wheel step scrolls
// through the one wheel transport at the pointer cell, so Block's whole-cell
// vertical quantisation and the crash-recovery scroll offset keep working. A
// press, release, or move dispatches a CDP mouse event at the cell centre, a
// press and release with a synthesised clickCount and a move with none, through
// the same bounded dispatch seam as keys, so a wedged browser drops the event
// instead of parking the caller. A move carrying a held button reads as a drag
// and one carrying MouseButtonNone as a hover. Either way it leaves the click
// chain untouched, so moving between two presses cannot break a double click.
// A hover additionally reads the computed cursor at the point for
// PointerCursor, in the same bounded dispatch.
func (e *ChromiumEngine) SendMouse(ev MouseEvent) {
	if ev.Action == MouseActionWheel {
		// Both axes take the same fixed step: an SGR report carries a direction,
		// not a magnitude, so there is nothing finer to scale.
		e.dispatchWheelAt(ev.WheelDX*wheelTickPx, ev.WheelDY*wheelTickPx, ev.Mod, ev.Col, ev.Row)
		return
	}
	e.mu.Lock()
	ctx := e.ctx
	ctxID := e.layoutShiftCtxID
	cellW, cellH := e.snapshotCellPx()
	x, y := cellCentreCSS(ev.Col, ev.Row, e.cols, e.rows, cellW, cellH)
	var count int
	switch ev.Action {
	case MouseActionPress:
		count, e.click = nextClickCount(e.click, ev.Button, ev.Col, ev.Row, time.Now())
	case MouseActionRelease:
		count = releaseClickCount(e.click, ev.Button)
	}
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	action := chromedp.Action(mouseParams(ev.Action, ev.Button, ev.Mod, count, x, y))
	if hoverReadsCursor(ev.Action, ev.Button) {
		// The cursor read rides the same bounded dispatch as the move, so it adds
		// no goroutine and a wedged browser drops the pair together. A zero held
		// context id falls back to the cursor read's own isolated world.
		action = chromedp.Tasks{action, e.pointerCursorReadAction(ctxID, x, y)}
	}
	e.dispatchInput(ctx, action)
}
