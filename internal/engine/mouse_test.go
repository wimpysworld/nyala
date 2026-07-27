package engine

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
)

// TestCellCentreCSSAtNonIntegerZ pins the Z-aware cell-to-CSS mapping: the
// dispatch position is ((col+0.5)*cellW)/Z, the device cell centre divided by
// the layout scale, using the fractional CSS cell snapshotCellPx derives. At
// a 9x18 cell on a 120x40 block grid Z = 1080/1024, non-integer, so a missing
// divide would land the click visibly off target.
func TestCellCentreCSSAtNonIntegerZ(t *testing.T) {
	e := New("")
	e.SetCellPx(9, 18)
	e.Resize(120, 40)

	e.mu.Lock()
	cellW, cellH := e.snapshotCellPx()
	e.mu.Unlock()
	x, y := cellCentreCSS(10, 5, 120, 40, cellW, cellH)

	z := 1080.0 / 1024.0
	if wantX := 10.5 * 9.0 / z; math.Abs(x-wantX) > 1e-9 {
		t.Errorf("cellCentreCSS x = %g, want %g ((col+0.5)*cellW/Z)", x, wantX)
	}
	if wantY := 5.5 * 18.0 / z; math.Abs(y-wantY) > 1e-9 {
		t.Errorf("cellCentreCSS y = %g, want %g ((row+0.5)*cellH/Z)", y, wantY)
	}
}

// TestCellCentreCSSClampsToGrid asserts an out-of-grid report lands on the
// nearest edge cell: oversized coordinates clamp to the last column and row,
// negative ones to the first.
func TestCellCentreCSSClampsToGrid(t *testing.T) {
	x, y := cellCentreCSS(999, 999, 120, 40, 8, 16)
	if wantX := 119.5 * 8.0; x != wantX {
		t.Errorf("oversized col x = %g, want %g (last column centre)", x, wantX)
	}
	if wantY := 39.5 * 16.0; y != wantY {
		t.Errorf("oversized row y = %g, want %g (last row centre)", y, wantY)
	}
	x, y = cellCentreCSS(-3, -1, 120, 40, 8, 16)
	if x != 0.5*8.0 || y != 0.5*16.0 {
		t.Errorf("negative cell = (%g, %g), want first cell centre (4, 8)", x, y)
	}
}

// TestWheelPositionSelectsPointerOrCentre asserts the wheel transport
// dispatches at the pointer cell centre for SGR wheels and stays at the
// viewport centre for keyboard scrolling and recovery replay.
func TestWheelPositionSelectsPointerOrCentre(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(120, 40)

	e.mu.Lock()
	px, py := e.wheelPositionLocked(true, 3, 4)
	cx, cy := e.wheelPositionLocked(false, 3, 4)
	cellW, cellH := e.snapshotCellPx()
	wantCx, wantCy := wheelCentre(e.cols, e.rows, e.cellW, e.cellH, e.layoutScale())
	e.mu.Unlock()

	if wantX, wantY := 3.5*cellW, 4.5*cellH; px != wantX || py != wantY {
		t.Errorf("pointer wheel position = (%g, %g), want cell centre (%g, %g)", px, py, wantX, wantY)
	}
	if cx != wantCx || cy != wantCy {
		t.Errorf("centre wheel position = (%g, %g), want wheelCentre (%g, %g)", cx, cy, wantCx, wantCy)
	}
}

// TestSendMouseWheelAtPointer asserts one SGR wheel event dispatches three
// lines (120 CSS px) through the one wheel transport with the sign following
// the direction, horizontal steps on the X axis, and Block's whole-cell
// vertical quantisation still applied to pointer wheels. With ctx nil the
// held offsets observe the transport without a browser.
func TestSendMouseWheelAtPointer(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(120, 40) // Z = 1, block cell height 16 CSS px

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDY: 1, Col: 3, Row: 4})
	e.mu.Lock()
	target, snapped := e.scrollYTarget, e.scrollY
	e.mu.Unlock()
	if target != wheelTickPx {
		t.Errorf("wheel down scrollYTarget = %d, want %d (3 lines)", target, wheelTickPx)
	}
	if want := 128; snapped != want { // round(120/16) = 8 cells = 128 px
		t.Errorf("wheel down scrollY = %d, want %d (whole-cell snap)", snapped, want)
	}

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDY: -1, Col: 3, Row: 4})
	e.mu.Lock()
	target, snapped = e.scrollYTarget, e.scrollY
	e.mu.Unlock()
	if target != 0 || snapped != 0 {
		t.Errorf("wheel up returned (target, scrollY) = (%d, %d), want (0, 0)", target, snapped)
	}

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDX: 1, Col: 3, Row: 4})
	e.mu.Lock()
	sx := e.scrollX
	e.mu.Unlock()
	if sx != wheelTickPx {
		t.Errorf("wheel right scrollX = %d, want %d", sx, wheelTickPx)
	}
	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDX: -1, Col: 3, Row: 4})
	e.mu.Lock()
	sx = e.scrollX
	e.mu.Unlock()
	if sx != 0 {
		t.Errorf("wheel left scrollX = %d, want 0", sx)
	}
}

// TestWheelParams pins the CDP mouseWheel dispatch shape: the event type, the
// signed deltas on each axis, the CSS position, and the modifier bits. The
// modifier assertion is the load-bearing one: the wheel path used to build its
// dispatch without modifiers, so a Shift or Ctrl wheel reached the page bare and
// a listener reading shiftKey saw nothing.
func TestWheelParams(t *testing.T) {
	p := wheelParams(12.5, 34.75, wheelTickPx, 0, cdpModShiftBits)
	if p.Type != input.MouseWheel {
		t.Errorf("wheel type = %q, want %q", p.Type, input.MouseWheel)
	}
	if p.DeltaX != wheelTickPx || p.DeltaY != 0 {
		t.Errorf("horizontal wheel deltas = (%g, %g), want (%d, 0)", p.DeltaX, p.DeltaY, wheelTickPx)
	}
	if p.X != 12.5 || p.Y != 34.75 {
		t.Errorf("wheel position = (%g, %g), want (12.5, 34.75)", p.X, p.Y)
	}
	if p.Modifiers != cdpModShiftBits {
		t.Errorf("wheel modifiers = %d, want %d (Shift)", p.Modifiers, cdpModShiftBits)
	}

	left := wheelParams(0, 0, -wheelTickPx, 0, 0)
	if left.DeltaX != -wheelTickPx || left.DeltaY != 0 {
		t.Errorf("wheel-left deltas = (%g, %g), want (%d, 0)", left.DeltaX, left.DeltaY, -wheelTickPx)
	}
	down := wheelParams(0, 0, 0, wheelTickPx, cdpModCtrlBits)
	if down.DeltaY != wheelTickPx || down.DeltaX != 0 {
		t.Errorf("wheel-down deltas = (%g, %g), want (0, %d)", down.DeltaX, down.DeltaY, wheelTickPx)
	}
	if down.Modifiers != cdpModCtrlBits {
		t.Errorf("wheel-down modifiers = %d, want %d (Ctrl)", down.Modifiers, cdpModCtrlBits)
	}
}

// TestSendMouseHorizontalWheelKeepsVertical asserts a horizontal report moves
// the held horizontal offset by one fixed 120 CSS px step per report, signed by
// direction, and leaves the vertical offset and its whole-cell snap target
// untouched, so the vertical path is unchanged by a sideways scroll. With ctx
// nil the held offsets observe the transport without a browser.
func TestSendMouseHorizontalWheelKeepsVertical(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(120, 40)

	for i := 1; i <= 3; i++ {
		e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDX: 1, Mod: cdpModShiftBits, Col: 3, Row: 4})
		e.mu.Lock()
		sx, sy, target := e.scrollX, e.scrollY, e.scrollYTarget
		e.mu.Unlock()
		if want := i * wheelTickPx; sx != want {
			t.Fatalf("after %d wheel-right reports scrollX = %d, want %d (120 px each)", i, sx, want)
		}
		if sy != 0 || target != 0 {
			t.Fatalf("wheel-right moved the vertical offset to (%d, %d), want (0, 0)", sy, target)
		}
	}

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDX: -1, Col: 3, Row: 4})
	e.mu.Lock()
	sx, sy := e.scrollX, e.scrollY
	e.mu.Unlock()
	if want := 2 * wheelTickPx; sx != want {
		t.Fatalf("wheel-left scrollX = %d, want %d (one step back)", sx, want)
	}
	if sy != 0 {
		t.Fatalf("wheel-left moved the vertical offset to %d, want 0", sy)
	}
}

// TestNextClickCountSynthesis drives the pure clickCount synthesis with
// explicit timestamps: presses chain within the 400 ms window on the same
// cell and button, cap at 3, and reset on movement, window expiry, or a
// different button.
func TestNextClickCountSynthesis(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var st clickState
	var n int

	step := func(button MouseButton, col, row int, at time.Time, want int, why string) {
		t.Helper()
		n, st = nextClickCount(st, button, col, row, at)
		if n != want {
			t.Fatalf("%s: clickCount = %d, want %d", why, n, want)
		}
	}

	step(MouseButtonLeft, 5, 5, t0, 1, "first press")
	step(MouseButtonLeft, 5, 5, t0.Add(200*time.Millisecond), 2, "double within window")
	step(MouseButtonLeft, 5, 5, t0.Add(400*time.Millisecond), 3, "triple at window edge")
	step(MouseButtonLeft, 5, 5, t0.Add(600*time.Millisecond), 3, "fourth press caps at 3")
	step(MouseButtonLeft, 6, 5, t0.Add(700*time.Millisecond), 1, "movement to another cell resets")
	step(MouseButtonLeft, 6, 5, t0.Add(1200*time.Millisecond), 1, "window expiry resets")
	step(MouseButtonRight, 6, 5, t0.Add(1300*time.Millisecond), 1, "different button resets")
}

// TestReleaseClickCount asserts a release reports the matching press chain's
// count and falls back to 1 for a button with no chain, so CDP never sees a
// zero clickCount on release.
func TestReleaseClickCount(t *testing.T) {
	st := clickState{count: 2, button: MouseButtonLeft, col: 5, row: 5, at: time.Unix(1000, 0)}
	if got := releaseClickCount(st, MouseButtonLeft); got != 2 {
		t.Errorf("matching release clickCount = %d, want 2", got)
	}
	if got := releaseClickCount(st, MouseButtonRight); got != 1 {
		t.Errorf("unmatched release clickCount = %d, want 1", got)
	}
	if got := releaseClickCount(clickState{}, MouseButtonLeft); got != 1 {
		t.Errorf("no-chain release clickCount = %d, want 1", got)
	}
}

// TestMouseParams pins the CDP dispatch shape: the event type, the named
// button, the pressed-buttons mask (Left=1, Right=2, Middle=4; a release
// holds none), the clickCount, the modifier bits, and the CSS position.
func TestMouseParams(t *testing.T) {
	p := mouseParams(MouseActionPress, MouseButtonRight, 10, 2, 100.5, 50.25)
	if p.Type != input.MousePressed {
		t.Errorf("press type = %q, want %q", p.Type, input.MousePressed)
	}
	if p.Button != input.Right {
		t.Errorf("press button = %q, want %q", p.Button, input.Right)
	}
	if p.Buttons != 2 {
		t.Errorf("press buttons mask = %d, want 2 (Right)", p.Buttons)
	}
	if p.ClickCount != 2 {
		t.Errorf("press clickCount = %d, want 2", p.ClickCount)
	}
	if p.Modifiers != 10 {
		t.Errorf("press modifiers = %d, want 10 (Alt|Shift)", p.Modifiers)
	}
	if p.X != 100.5 || p.Y != 50.25 {
		t.Errorf("press position = (%g, %g), want (100.5, 50.25)", p.X, p.Y)
	}

	r := mouseParams(MouseActionRelease, MouseButtonMiddle, 0, 3, 1, 2)
	if r.Type != input.MouseReleased {
		t.Errorf("release type = %q, want %q", r.Type, input.MouseReleased)
	}
	if r.Button != input.Middle {
		t.Errorf("release button = %q, want %q", r.Button, input.Middle)
	}
	if r.Buttons != 0 {
		t.Errorf("release buttons mask = %d, want 0 (no held button)", r.Buttons)
	}
	if r.ClickCount != 3 {
		t.Errorf("release clickCount = %d, want 3", r.ClickCount)
	}
}

// TestMouseParamsMove pins the move dispatch shape: a move reports the
// mouseMoved type, repeats whatever button it carries in both the name and the
// pressed-buttons mask (a held button reads as a drag of that button, and the
// no-button value reads as a hover with an empty mask), preserves the modifier
// bits, and carries clickCount 0 because it starts no click. Modifier
// preservation is pinned deliberately: a synthetic move that lost the modifier
// bits would break shift-extend drag-selection.
func TestMouseParamsMove(t *testing.T) {
	cases := []struct {
		button MouseButton
		want   input.MouseButton
		mask   int64
	}{
		{MouseButtonLeft, input.Left, 1},
		{MouseButtonRight, input.Right, 2},
		{MouseButtonMiddle, input.Middle, 4},
		{MouseButtonNone, input.None, 0},
	}
	for _, c := range cases {
		p := mouseParams(MouseActionMove, c.button, cdpModShiftBits, 0, 12.5, 34.75)
		if p.Type != input.MouseMoved {
			t.Errorf("move type = %q, want %q", p.Type, input.MouseMoved)
		}
		if p.Button != c.want {
			t.Errorf("move button = %q, want %q", p.Button, c.want)
		}
		if p.Buttons != c.mask {
			t.Errorf("move buttons mask = %d, want %d (the button the move carries)", p.Buttons, c.mask)
		}
		if p.Modifiers != cdpModShiftBits {
			t.Errorf("move modifiers = %d, want %d", p.Modifiers, cdpModShiftBits)
		}
		if p.ClickCount != 0 {
			t.Errorf("move clickCount = %d, want 0", p.ClickCount)
		}
		if p.X != 12.5 || p.Y != 34.75 {
			t.Errorf("move position = (%g, %g), want (12.5, 34.75)", p.X, p.Y)
		}
	}
}

// TestMouseParamsHoverWireShape pins what a hover move puts on the wire, which
// the typed field assertions above cannot see. Two things matter and only one
// of them is visible in Go. The button name goes out in full as "none", the
// value CDP documents as that field's default, because input.None is the
// non-empty string "none" and neither omitempty nor omitzero drops it. The
// pressed-buttons mask, by contrast, does not appear at all: cdproto tags the
// field omitempty,omitzero, so the empty mask a hover carries is dropped rather
// than sent as 0. The omission is the dispatch we want, not a bug. CDP
// documents the field's absent value as None=0, which is exactly the empty mask
// mouseParams built, so Chromium reads the move as a hover either way. A test
// that demanded a literal "buttons":0 would fail on a correct dispatch.
func TestMouseParamsHoverWireShape(t *testing.T) {
	raw, err := json.Marshal(mouseParams(MouseActionMove, MouseButtonNone, 0, 0, 12.5, 34.75))
	if err != nil {
		t.Fatalf("marshal hover params: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal hover params: %v", err)
	}
	if got := string(fields["button"]); got != `"none"` {
		t.Errorf("hover button field = %s, want \"none\" (the CDP default name)", got)
	}
	if _, ok := fields["buttons"]; ok {
		t.Errorf("hover carried a buttons field %s, want it omitted (CDP reads the absent field as None=0)", fields["buttons"])
	}
}

// cdpModShiftBits and cdpModCtrlBits are the CDP Shift and Ctrl modifier bits,
// spelled here so the dispatch tests assert against the protocol values rather
// than bare literals.
const (
	cdpModCtrlBits  = 2
	cdpModShiftBits = 8
)

// TestSendMouseMoveKeepsClickChain asserts a drag move leaves the click chain
// alone: two presses on one cell with a move between them still report a
// double click, so dragging inside a double-click gesture cannot break word
// selection. With no browser context the dispatch is skipped and the held
// state alone moves, so it runs browser-free.
func TestSendMouseMoveKeepsClickChain(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(120, 40)

	e.SendMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 2, Row: 3})
	e.SendMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 5, Row: 9})
	e.SendMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 2, Row: 3})

	e.mu.Lock()
	got := e.click
	e.mu.Unlock()
	if got.count != 2 || got.col != 2 || got.row != 3 {
		t.Errorf("click chain = %+v, want count 2 on the press cell (2, 3)", got)
	}
}

// TestSendMousePressTracksChain asserts a press without a browser context
// still advances the click chain under mu, so the first dispatch after a
// rebuild carries the right count.
func TestSendMousePressTracksChain(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(120, 40)

	e.SendMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 2, Row: 3})
	e.SendMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 2, Row: 3})
	e.mu.Lock()
	got := e.click
	e.mu.Unlock()
	if got.count != 2 || got.button != MouseButtonLeft || got.col != 2 || got.row != 3 {
		t.Errorf("click chain = %+v, want count 2 on left at (2, 3)", got)
	}
}
