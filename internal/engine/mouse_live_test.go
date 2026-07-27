//go:build live

package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// liveDragPage is a self-contained data: URL with three drag targets at known
// CSS positions, so a test can aim a press at a cell and know what it hits at
// cell 8x16 and Z 1: a range input on row 0 spanning columns 0 to 49, a
// paragraph whose first line sits on row 6, and an HTML5 draggable box on rows
// 10 and 11. The box records its own pointer and drag events, so a test can
// tell motion arriving from a drag gesture starting. The document records the
// target of every move on top of that, so a test that crosses the slider and
// the paragraph can prove its motion reached them.
const liveDragPage = "data:text/html," +
	"<html><body style='margin:0;font:16px monospace'>" +
	"<input type='range' id='r' min='0' max='100' value='0' " +
	"style='position:absolute;left:0px;top:0px;width:400px;height:16px'>" +
	"<p id='t' style='position:absolute;left:0px;top:96px;width:600px;margin:0'>" +
	"alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima</p>" +
	"<div id='d' draggable='true' " +
	"style='position:absolute;left:0px;top:160px;width:200px;height:32px;background:silver'>dragme</div>" +
	"<script>window.events=[];window.moves=[];" +
	"['dragstart','dragend','mousedown','mousemove','mouseup'].forEach(function(n){" +
	"document.getElementById('d').addEventListener(n,function(){window.events.push(n)})});" +
	"document.addEventListener('mousemove',function(e){window.moves.push(e.target.id||'body')});" +
	"</script></body></html>"

// liveHoverPage is a self-contained data: URL carrying a hover-triggered
// navigation menu and a hover swatch at known CSS positions, so a test can aim
// at a cell and know what it hits at cell 8x16 and Z 1: the menu trigger sits on
// row 0 spanning columns 0 to 19, its submenu covers rows 1 to 3 once CSS
// reveals it, and the swatch covers rows 6 and 7. CSS alone does the reveal
// (`%23menu:hover %23submenu`), because CSS :hover is the thing under test and a
// script reveal would prove nothing about it. Two authoring traps are avoided
// here, and both make a working hover look broken. The `#` characters are
// percent-encoded, because a raw one would start the URL fragment and cut the
// page short. The submenu's `display:none` sits in the stylesheet rather than
// an inline style attribute, because an inline declaration outranks every
// selector: the hover rule would match and still lose the cascade, leaving the
// submenu hidden. The page records the pointer events it sees, so a failure says
// whether motion arrived at all or arrived and CSS ignored it.
const liveHoverPage = "data:text/html," +
	"<html><head><style>" +
	"%23submenu{display:none;position:absolute;left:0px;top:16px;width:160px;height:48px}" +
	"%23menu:hover %23submenu{display:block}" +
	"%23swatch{background:navy}" +
	"%23swatch:hover{background:red}" +
	"</style></head><body style='margin:0;font:16px monospace'>" +
	"<div id='menu' style='position:absolute;left:0px;top:0px;width:160px;height:16px'>menu" +
	"<div id='submenu'>one two three</div>" +
	"</div>" +
	"<div id='swatch' style='position:absolute;left:0px;top:96px;width:160px;height:32px'>swatch</div>" +
	"<script>window.hoverEvents=[];" +
	"['mouseover','mousemove','mouseout'].forEach(function(n){" +
	"document.addEventListener(n,function(e){window.hoverEvents.push(n+':'+(e.target.id||'body'))})});" +
	"</script></body></html>"

// liveScrollPage is a self-contained data: URL that scrolls on both axes, so a
// horizontal wheel test can prove the page moved sideways and not down. It
// records the modifier flags of each wheel event, which is how the test sees a
// modifier survive the dispatch.
const liveScrollPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='width:4000px;height:4000px;background:linear-gradient(to right,white,black)'></div>" +
	"<script>window.wheels=[];" +
	"window.addEventListener('wheel',function(e){" +
	"window.wheels.push([e.deltaX,e.deltaY,e.shiftKey,e.ctrlKey].join(':'))});" +
	"</script></body></html>"

// TestLiveHorizontalWheelScrollsSideways proves a horizontal report scrolls the
// real page on the X axis in the desktop-browser direction and leaves the Y axis
// alone: a wheel-right report increases scrollX, a wheel-left report brings it
// back, and scrollY never moves. It also pins the sign at the DOM boundary, so
// the deltaX convention is asserted against Chromium rather than assumed.
func TestLiveHorizontalWheelScrollsSideways(t *testing.T) {
	e, ctx := liveEngineOn(t, liveScrollPage)

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDX: 1, Col: 20, Row: 10})
	right, ok := waitForScrollX(t, ctx, func(x float64) bool { return x > 0 })
	if !ok {
		t.Fatalf("window.scrollX = %g after a wheel-right report, want a scroll towards the right edge", right)
	}
	if y := evalFloat(t, ctx, "window.scrollY"); y != 0 {
		t.Fatalf("window.scrollY = %g after a horizontal report, want 0 (vertical untouched)", y)
	}
	if got := evalString(t, ctx, "window.wheels[0]"); got != "120:0:false:false" {
		t.Fatalf("page wheel event = %q, want deltaX 120 and deltaY 0", got)
	}

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDX: -1, Col: 20, Row: 10})
	left, ok := waitForScrollX(t, ctx, func(x float64) bool { return x < right })
	if !ok {
		t.Fatalf("window.scrollX = %g after a wheel-left report, want less than %g", left, right)
	}
	if y := evalFloat(t, ctx, "window.scrollY"); y != 0 {
		t.Fatalf("window.scrollY = %g after two horizontal reports, want 0", y)
	}
}

// TestLiveWheelKeepsModifiers proves the wheel dispatch carries the report's
// modifier bits to the page: a Shift wheel arrives with shiftKey set, which a
// page reading the flag depends on. It also checks the CSS layout width across a
// Ctrl wheel, because ctrlKey is the browser zoom gesture and a zoom would move
// the layout out from under the cell geometry the whole renderer is sized to.
func TestLiveWheelKeepsModifiers(t *testing.T) {
	e, ctx := liveEngineOn(t, liveScrollPage)
	before := evalFloat(t, ctx, "window.innerWidth")

	// Assert the flags, not the vertical magnitude: on the Block backend the
	// dispatched deltaY is the whole-cell snapped delta (120 px snaps to 128 at a
	// 16 px cell, and the next report closes the remainder), which is the
	// documented vertical behaviour this issue leaves alone.
	flags := func(i int) string {
		return fmt.Sprintf("(window.wheels[%d]||'').split(':').slice(2).join(':')", i)
	}
	axis := func(i int) string {
		return fmt.Sprintf("(window.wheels[%d]||'').split(':')[0]", i)
	}

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDY: 1, Mod: cdpModShiftBits, Col: 20, Row: 10})
	if got, ok := waitForString(t, ctx, flags(0), "true:false"); !ok {
		t.Fatalf("page wheel flags = %q, want shiftKey set and ctrlKey clear", got)
	}
	if got := evalString(t, ctx, axis(0)); got != "0" {
		t.Fatalf("vertical wheel deltaX = %q, want 0", got)
	}

	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDY: 1, Mod: cdpModCtrlBits, Col: 20, Row: 10})
	if got, ok := waitForString(t, ctx, flags(1), "false:true"); !ok {
		t.Fatalf("page wheel flags = %q, want ctrlKey set and shiftKey clear", got)
	}
	if after := evalFloat(t, ctx, "window.innerWidth"); after != before {
		t.Fatalf("CSS layout width moved from %g to %g across a Ctrl wheel: the page zoomed", before, after)
	}
}

// waitForScrollX polls the page's horizontal scroll offset until want reports
// it settled, so the assertion tolerates the renderer applying the scroll just
// after the dispatch returns. It returns the last offset read.
func waitForScrollX(t *testing.T, ctx context.Context, want func(float64) bool) (got float64, ok bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got = evalFloat(t, ctx, "window.scrollX")
		if want(got) {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dragCells sends one whole drag gesture through the normal SendMouse seam: a
// press on the first cell, a move to each cell after it, then a release on the
// last. Each dispatch is synchronous, so the page sees the gesture in order.
func dragCells(e *ChromiumEngine, button MouseButton, mod int, cells ...[2]int) {
	for i, c := range cells {
		action := MouseActionMove
		switch i {
		case 0:
			action = MouseActionPress
		case len(cells) - 1:
			action = MouseActionRelease
		}
		e.SendMouse(MouseEvent{Action: action, Button: button, Mod: mod, Col: c[0], Row: c[1]})
	}
}

// TestLiveDragMovesRangeInput proves a press-move-release drag reaches a real
// control: the range input's value tracks the pointer across the drag. The
// slider spans 400 CSS px over columns 0 to 49, so releasing near column 37
// (x = 300) must land the value near 75. A press alone would set the value once
// and never track, so a value that follows the moves is the motion evidence.
func TestLiveDragMovesRangeInput(t *testing.T) {
	e, ctx := liveEngineOn(t, liveDragPage)

	if start := evalFloat(t, ctx, "Number(document.getElementById('r').value)"); start != 0 {
		t.Fatalf("range starts at %g, want 0", start)
	}

	dragCells(e, MouseButtonLeft, 0,
		[2]int{2, 0}, [2]int{12, 0}, [2]int{25, 0}, [2]int{37, 0}, [2]int{37, 0})

	got := evalFloat(t, ctx, "Number(document.getElementById('r').value)")
	if got < 60 || got > 90 {
		t.Fatalf("range value = %g after a drag to x=300 of 400, want near 75", got)
	}
}

// TestLiveDragSelectsPageText proves text drag-selection works, the case an
// HTML5 draggable element cannot stand in for: pressing on the paragraph and
// moving across it must leave a non-empty DOM selection carrying the dragged
// words.
func TestLiveDragSelectsPageText(t *testing.T) {
	e, ctx := liveEngineOn(t, liveDragPage)

	dragCells(e, MouseButtonLeft, 0,
		[2]int{0, 6}, [2]int{8, 6}, [2]int{20, 6}, [2]int{34, 6}, [2]int{34, 6})

	got, ok := waitForSelection(t, ctx)
	if !ok {
		t.Fatalf("selection = %q after a text drag, want the dragged words", got)
	}
	// The press lands at the centre of column 0, inside the first character, so
	// the caret can snap past it: assert the words the drag crossed, not the
	// exact first letter.
	for _, word := range []string{"bravo", "charlie", "delta"} {
		if !strings.Contains(got, word) {
			t.Fatalf("selection = %q, want it to carry the dragged word %q", got, word)
		}
	}
}

// TestLiveDragOverDraggableElement covers an HTML5 draggable element on its
// own, because a page can bind the drag events instead of reading the pointer.
// The forwarded motion reaches the element as mousedown then mousemove, and
// Chromium promotes the held-button motion into its own drag gesture, so
// dragstart fires from the pointer path alone. No mouseup follows: the drag
// gesture consumes the release. Completing a drop across applications is a
// different mechanism (CDP drag interception) and stays out of scope.
func TestLiveDragOverDraggableElement(t *testing.T) {
	e, ctx := liveEngineOn(t, liveDragPage)

	dragCells(e, MouseButtonLeft, 0,
		[2]int{2, 10}, [2]int{8, 10}, [2]int{16, 10}, [2]int{16, 10})

	// Deduplicate: a drag emits many mousemove reports and only the order of
	// the first of each kind is the contract.
	const seen = "window.events.filter(function(n,i,a){return a.indexOf(n)===i}).join(',')"
	if got, ok := waitForString(t, ctx, seen+".startsWith('mousedown,mousemove,dragstart').toString()", "true"); !ok {
		t.Fatalf("draggable element saw %q (startsWith = %q), want mousedown, then mousemove, then dragstart",
			evalString(t, ctx, seen), got)
	}
}

// hoverCells sends buttonless pointer motion through the normal SendMouse seam:
// one MouseActionMove carrying MouseButtonNone per cell, in the order given.
// Each dispatch is synchronous, so the page sees the motion in that order.
func hoverCells(e *ChromiumEngine, cells ...[2]int) {
	for _, c := range cells {
		e.SendMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonNone, Col: c[0], Row: c[1]})
	}
}

// TestLiveHoverRevealsMenu proves CSS :hover responds to forwarded buttonless
// motion on a real page, the acceptance criterion hover support exists for:
// motion over the trigger reveals a submenu that only a CSS :hover rule can
// show, motion away hides it again, and the swatch under the second position
// paints its hover colour. The hide half is the point of the second move: it
// proves the hover tracks the pointer rather than latching on the first cell it
// lands on.
func TestLiveHoverRevealsMenu(t *testing.T) {
	e, ctx := liveEngineOn(t, liveHoverPage)

	const display = "getComputedStyle(document.getElementById('submenu')).display"
	const seen = "window.hoverEvents.join(',')"
	if got := evalString(t, ctx, display); got != "none" {
		t.Fatalf("submenu display = %q before any motion, want none", got)
	}

	hoverCells(e, [2]int{2, 0}, [2]int{8, 0})
	if got, ok := waitForString(t, ctx, display, "block"); !ok {
		t.Fatalf("submenu display = %q after buttonless motion over the trigger, want block (page saw %q)",
			got, evalString(t, ctx, seen))
	}

	// Row 7 is 32 px clear of the revealed submenu and lands inside the swatch.
	hoverCells(e, [2]int{5, 7})
	if got, ok := waitForString(t, ctx, display, "none"); !ok {
		t.Fatalf("submenu display = %q after motion away from the trigger, want none: the hover latched instead of tracking the pointer (page saw %q)",
			got, evalString(t, ctx, seen))
	}
	const swatch = "getComputedStyle(document.getElementById('swatch')).backgroundColor"
	if got, ok := waitForString(t, ctx, swatch, "rgb(255, 0, 0)"); !ok {
		t.Fatalf("swatch background = %q under the pointer, want the hover colour rgb(255, 0, 0) (page saw %q)",
			got, evalString(t, ctx, seen))
	}
}

// TestLiveHoverDoesNotDragOrClick is the regression MouseButtonNone exists to
// prevent: buttonless motion that reached Chromium carrying a held button would
// drag whatever it crossed. Motion across the range input and the paragraph must
// leave the slider value at 0 and the DOM selection empty. The recorded move
// targets are the arrival evidence, so the test cannot pass by dispatching
// nothing: they name the elements the motion crossed.
func TestLiveHoverDoesNotDragOrClick(t *testing.T) {
	e, ctx := liveEngineOn(t, liveDragPage)

	hoverCells(e,
		[2]int{2, 0}, [2]int{12, 0}, [2]int{25, 0}, [2]int{37, 0},
		[2]int{0, 6}, [2]int{8, 6}, [2]int{20, 6}, [2]int{34, 6})

	const crossed = "(window.moves.includes('r')&&window.moves.includes('t')).toString()"
	if got, ok := waitForString(t, ctx, crossed, "true"); !ok {
		t.Fatalf("page saw moves over %q (both slider and paragraph = %q), want the motion to reach both, or the rest of this test proves nothing",
			evalString(t, ctx, "window.moves.join(',')"), got)
	}
	if got := evalFloat(t, ctx, "Number(document.getElementById('r').value)"); got != 0 {
		t.Fatalf("range value = %g after buttonless motion across the slider, want 0: the hover carried a held button and dragged it", got)
	}
	if got := evalString(t, ctx, "window.getSelection().toString()"); got != "" {
		t.Fatalf("selection = %q after buttonless motion across the paragraph, want empty: the hover carried a held button and selected text", got)
	}
}

// waitForSelection polls the page's DOM selection until it is non-empty, so the
// assertion tolerates the renderer completing the selection just after the
// release dispatch returns. It returns the last value read.
func waitForSelection(t *testing.T, ctx context.Context) (got string, ok bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got = evalString(t, ctx, "window.getSelection().toString()")
		if got != "" {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
