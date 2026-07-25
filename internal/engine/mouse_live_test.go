//go:build live

package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// liveDragPage is a self-contained data: URL with three drag targets at known
// CSS positions, so a test can aim a press at a cell and know what it hits at
// cell 8x16 and Z 1: a range input on row 0 spanning columns 0 to 49, a
// paragraph whose first line sits on row 6, and an HTML5 draggable box on rows
// 10 and 11. The box records its own pointer and drag events, so a test can
// tell motion arriving from a drag gesture starting.
const liveDragPage = "data:text/html," +
	"<html><body style='margin:0;font:16px monospace'>" +
	"<input type='range' id='r' min='0' max='100' value='0' " +
	"style='position:absolute;left:0px;top:0px;width:400px;height:16px'>" +
	"<p id='t' style='position:absolute;left:0px;top:96px;width:600px;margin:0'>" +
	"alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima</p>" +
	"<div id='d' draggable='true' " +
	"style='position:absolute;left:0px;top:160px;width:200px;height:32px;background:silver'>dragme</div>" +
	"<script>window.events=[];" +
	"['dragstart','dragend','mousedown','mousemove','mouseup'].forEach(function(n){" +
	"document.getElementById('d').addEventListener(n,function(){window.events.push(n)})});" +
	"</script></body></html>"

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
