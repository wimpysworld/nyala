//go:build live

package engine

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// clickCells sends count whole clicks at one cell through the normal SendMouse
// seam, so Chromium's own click-count handling sees a double or triple click: the
// press chain needs the same button on the same cell inside clickChainWindow, and
// each dispatch is synchronous, so the pairs arrive in order well inside it.
func clickCells(e *ChromiumEngine, count, col, row int) {
	for range count {
		e.SendMouse(MouseEvent{Action: MouseActionPress, Col: col, Row: row})
		e.SendMouse(MouseEvent{Action: MouseActionRelease, Col: col, Row: row})
	}
}

// waitForEngineSelection polls the engine's selection read until want accepts it,
// so an assertion tolerates the renderer completing the selection just after the
// gesture dispatch returns. It returns the last text read, so a failure reports
// what the page really held.
func waitForEngineSelection(t *testing.T, e *ChromiumEngine, want func(string) bool) (got string, ok bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got = e.Selection()
		if want(got) {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLiveSelectionFromDrag proves the copy read returns what a drag selected:
// the words the pointer crossed, read back through the same seam the clipboard
// write consumes.
func TestLiveSelectionFromDrag(t *testing.T) {
	e, _ := liveEngineOn(t, liveDragPage)

	dragCells(e, MouseButtonLeft, 0,
		[2]int{0, 6}, [2]int{8, 6}, [2]int{20, 6}, [2]int{34, 6}, [2]int{34, 6})

	got, ok := waitForEngineSelection(t, e, func(s string) bool { return strings.Contains(s, "delta") })
	if !ok {
		t.Fatalf("selection read = %q after a text drag, want the dragged words", got)
	}
	// The press lands at the centre of column 0, inside the first character, so
	// the caret can snap past it: assert the words the drag crossed.
	for _, word := range []string{"bravo", "charlie", "delta"} {
		if !strings.Contains(got, word) {
			t.Fatalf("selection read = %q, want it to carry the dragged word %q", got, word)
		}
	}
}

// TestLiveSelectionFromDoubleClick proves a double click selects the word under
// the pointer and the copy read returns that word alone. The word select comes
// from Chromium's own click-count handling on the existing dispatch, so this pins
// the count reaching the browser as much as the read.
func TestLiveSelectionFromDoubleClick(t *testing.T) {
	e, _ := liveEngineOn(t, liveDragPage)

	// Column 8 of row 6 sits inside "bravo", the second word of the paragraph.
	clickCells(e, 2, 8, 6)

	got, ok := waitForEngineSelection(t, e, func(s string) bool { return strings.TrimSpace(s) != "" })
	if !ok {
		t.Fatalf("selection read = %q after a double click, want the word under the pointer", got)
	}
	if strings.TrimSpace(got) != "bravo" {
		t.Fatalf("selection read = %q after a double click, want the single word %q", got, "bravo")
	}
}

// TestLiveSelectionFromTripleClick proves a triple click extends the selection to
// the whole line the double click's word sits on, so the copy read returns the
// line rather than the word.
func TestLiveSelectionFromTripleClick(t *testing.T) {
	e, _ := liveEngineOn(t, liveDragPage)

	clickCells(e, 3, 8, 6)

	got, ok := waitForEngineSelection(t, e, func(s string) bool { return strings.Contains(s, "alpha") })
	if !ok {
		t.Fatalf("selection read = %q after a triple click, want the whole line", got)
	}
	for _, word := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(got, word) {
			t.Fatalf("selection read = %q after a triple click, want it to carry %q", got, word)
		}
	}
	if len(strings.TrimSpace(got)) <= len("bravo") {
		t.Fatalf("selection read = %q, want more than the double click's single word", got)
	}
}

// TestLiveSelectionEmptyWithNoSelection proves a copy on a page with nothing
// selected has nothing to write: the read reports the empty string, which is the
// signal the viewer treats as no clipboard write at all.
func TestLiveSelectionEmptyWithNoSelection(t *testing.T) {
	e, _ := liveEngineOn(t, liveDragPage)

	if got := e.Selection(); got != "" {
		t.Fatalf("selection read = %q on a page with no selection, want the empty string", got)
	}

	// A plain click places a caret and selects nothing, so it still has nothing to
	// copy: a collapsed selection must not read as text.
	clickCells(e, 1, 8, 6)

	if got := e.Selection(); got != "" {
		t.Fatalf("selection read = %q after a plain click, want the empty string", got)
	}
}

// TestLiveSelectionReadsTextControl proves a selection inside a focused text
// control is copyable. A control keeps its selection in an inner editor the
// document selection cannot see, so without the control branch a user selecting
// in a form field would copy nothing.
func TestLiveSelectionReadsTextControl(t *testing.T) {
	e, ctx := liveEngineOn(t, liveFindPage)

	evalVoid(t, ctx, "var q=document.getElementById('q');q.value='alpha bravo';q.focus();q.setSelectionRange(6,11)")

	got, ok := waitForEngineSelection(t, e, func(s string) bool { return s != "" })
	if !ok {
		t.Fatalf("selection read = %q for a text-control selection, want the selected characters", got)
	}
	if got != "bravo" {
		t.Fatalf("selection read = %q, want the control's selected text %q", got, "bravo")
	}
}

// TestLiveSelectionRefusesOversizedSelection proves the read refuses a selection
// past the copy bound at the source: nothing crosses the CDP wire, and the caller
// sees the same empty result as no selection, so an over-long copy leaves the
// clipboard untouched rather than pasting a truncated fragment later.
func TestLiveSelectionRefusesOversizedSelection(t *testing.T) {
	e, ctx := liveEngineOn(t, liveDragPage)

	evalVoid(t, ctx, "var big=document.createElement('p');"+
		"big.textContent='x'.repeat("+strconv.Itoa(selectionMaxBytes+1)+");"+
		"document.body.appendChild(big);"+
		"window.getSelection().selectAllChildren(big)")

	// The page really holds the selection; only the read refuses it.
	if got := evalString(t, ctx, "String(window.getSelection().toString().length)"); got != strconv.Itoa(selectionMaxBytes+1) {
		t.Fatalf("page selection length = %q, want %d characters selected", got, selectionMaxBytes+1)
	}
	if got := e.Selection(); got != "" {
		t.Fatalf("selection read returned %d bytes for an oversized selection, want the empty string", len(got))
	}
}
