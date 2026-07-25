//go:build live

package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// liveInputPage is a self-contained data: URL for the input tests: one text
// field, one multi-line text area for the vertical caret keys, a body tall
// enough to scroll, and a keydown recorder so a key with no visible effect is
// still observable. A data: URL needs no network and no local file, so the test
// stays hermetic once a Chromium resolves.
const liveInputPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<input id='f' style='width:400px'>" +
	"<textarea id='t' rows='4' cols='40'></textarea>" +
	"<div style='height:400vh;background:linear-gradient(white,black)'></div>" +
	"<script>window.pressed=[];" +
	"document.addEventListener('keydown',function(e){window.pressed.push(e.key)});" +
	"</script></body></html>"

// liveInputEngine launches a real Chromium on liveInputPage and returns the
// engine and its session context once the first frame proves the page laid out,
// so an input dispatch lands on a live document.
func liveInputEngine(t *testing.T) (*ChromiumEngine, context.Context) {
	t.Helper()
	return liveEngineOn(t, liveInputPage)
}

// liveEngineOn launches a real Chromium on page at cell 8x16 over an 80x24
// grid, so the layout scale Z is 1 and a cell maps to CSS pixels as
// ((col+0.5)*8, (row+0.5)*16): a test can aim a press at a known element. It
// returns once the first frame proves the page laid out. It skips when no
// Chromium resolves and closes the engine on cleanup.
func liveEngineOn(t *testing.T, page string) (*ChromiumEngine, context.Context) {
	t.Helper()
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(80, 24)
	if err := e.Navigate(page); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	select {
	case _, ok := <-e.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
	case <-deadline.C:
		t.Fatal("no synced frame within the deadline")
	}

	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		t.Fatal("engine context is nil after navigate")
	}
	return e, ctx
}

// focusField focuses the page's text field, the state the typed-text tests
// assert against.
func focusField(t *testing.T, ctx context.Context) {
	t.Helper()
	evalVoid(t, ctx, "document.getElementById('f').focus()")
}

// blurField moves focus off the text field to the body, the state the space
// scroll test asserts against: no editable element holds focus.
func blurField(t *testing.T, ctx context.Context) {
	t.Helper()
	evalVoid(t, ctx, "document.getElementById('f').blur();document.body.focus()")
}

// evalVoid runs one expression in the page for its side effect.
func evalVoid(t *testing.T, ctx context.Context, expr string) {
	t.Helper()
	var ignored []byte
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ignored)); err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
}

// evalString reads one string expression from the page.
func evalString(t *testing.T, ctx context.Context, expr string) string {
	t.Helper()
	var got string
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &got)); err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	return got
}

// evalFloat reads one numeric expression from the page.
func evalFloat(t *testing.T, ctx context.Context, expr string) float64 {
	t.Helper()
	var got float64
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &got)); err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	return got
}

// waitForString polls a string expression until it equals want, so a test
// tolerates the renderer applying an input event just after the CDP dispatch
// returns. It returns the last value read, so a failing test reports what the
// page actually held.
func waitForString(t *testing.T, ctx context.Context, expr, want string) (got string, ok bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got = evalString(t, ctx, expr)
		if got == want {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForScroll polls the page's own scroll offset until it moves off zero, so
// the space scroll test observes the browser's default action rather than the
// engine's held offset. It returns the last offset read.
func waitForScroll(t *testing.T, ctx context.Context) (got float64, ok bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got = evalFloat(t, ctx, "window.scrollY")
		if got > 0 {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLiveTextTypesIntoFocusedField proves a printable character reaches a
// focused field and lands in its value, the base case of the typed-text path.
// It also proves the page sees each character as a real key press: a keydown
// listener records every one, which a page's own shortcut handler or in-page
// editor depends on.
func TestLiveTextTypesIntoFocusedField(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)

	for _, s := range []string{"n", "y", "a", "l", "a"} {
		e.SendText(s)
	}

	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", "nyala"); !ok {
		t.Fatalf("field value = %q, want %q", got, "nyala")
	}
	if got, ok := waitForString(t, ctx, "window.pressed.join('')", "nyala"); !ok {
		t.Fatalf("page keydown log = %q, want each typed key", got)
	}
}

// TestLiveMultiByteTextReachesFocusedField proves multi-byte text reaches a
// focused field intact: two-byte, three-byte, and four-byte UTF-8 characters
// insert atomically rather than arriving as broken bytes.
func TestLiveMultiByteTextReachesFocusedField(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)

	const want = "héllo 日本 🙂"
	e.SendText(want)

	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", want); !ok {
		t.Fatalf("field value = %q, want %q", got, want)
	}
}

// TestLiveSpaceTypesIntoFocusedField proves Space types a space when a field
// holds focus, the typing half of the browser's own space arbitration.
func TestLiveSpaceTypesIntoFocusedField(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)

	e.SendText("a")
	e.SendText(" ")
	e.SendText("b")

	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", "a b"); !ok {
		t.Fatalf("field value = %q, want %q", got, "a b")
	}
}

// TestLiveSpaceScrollsWithoutFocus proves the other half of the arbitration:
// with no editable element focused, Space runs Chromium's own scroll default
// action and the page scroll offset moves. The viewer keeps no focus state, so
// this behaviour has to come from the browser. It also pins the key the page
// saw, so a stroke that scrolled without reporting a Space keydown fails here.
func TestLiveSpaceScrollsWithoutFocus(t *testing.T) {
	e, ctx := liveInputEngine(t)
	blurField(t, ctx)
	if before := evalFloat(t, ctx, "window.scrollY"); before != 0 {
		t.Fatalf("page starts scrolled at %g, want 0", before)
	}

	e.SendText(" ")

	got, ok := waitForScroll(t, ctx)
	t.Logf("window.scrollY after Space = %g; keydown log = %q", got,
		evalString(t, ctx, "window.pressed.join(',')"))
	if !ok {
		t.Fatalf("window.scrollY = %g after Space, want a scroll off the top", got)
	}
	if pressed := evalString(t, ctx, "window.pressed.join(',')"); pressed != " " {
		t.Fatalf("page keydown log = %q, want the Space key alone", pressed)
	}
}

// TestLiveBackspaceEditsFocusedField proves the Backspace stroke edits a
// focused field: the character before the caret is removed.
func TestLiveBackspaceEditsFocusedField(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)

	e.SendText("ab")
	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", "ab"); !ok {
		t.Fatalf("field value = %q before Backspace, want %q", got, "ab")
	}

	e.SendKey(Key{Code: "Backspace"})

	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", "a"); !ok {
		t.Fatalf("field value = %q after Backspace, want %q", got, "a")
	}
	if pressed := evalString(t, ctx, "window.pressed.includes('Backspace').toString()"); pressed != "true" {
		t.Fatal("the page saw no Backspace keydown")
	}
}

// TestLivePasteInsertsIntoFocusedField proves an ASCII paste lands in a focused
// field as one unit. The page's keydown log must stay empty: a paste inserts
// atomically and is never synthesised as key presses, so a pasted newline
// cannot submit a form the way a typed Enter would.
func TestLivePasteInsertsIntoFocusedField(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)

	const want = "pasted text"
	e.SendPaste(want)

	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", want); !ok {
		t.Fatalf("field value = %q, want %q", got, want)
	}
	if pressed := evalString(t, ctx, "window.pressed.join(',')"); pressed != "" {
		t.Fatalf("paste raised keydown events %q, want none", pressed)
	}
}

// TestLiveMultiBytePasteInsertsIntact proves a multi-byte paste reaches a
// focused field unbroken: two-byte, three-byte, and four-byte UTF-8 characters
// survive the insert as one unit.
func TestLiveMultiBytePasteInsertsIntact(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)

	const want = "héllo 日本 🙂 ok"
	e.SendPaste(want)

	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", want); !ok {
		t.Fatalf("field value = %q, want %q", got, want)
	}
}

// TestLiveSingleCharacterPasteIsNotAKeyPress proves the paste seam stays off the
// typed-character stroke even for a one-character payload. A pasted character
// inserts with no keydown, and a pasted space with nothing focused must not
// scroll: a typed space would, so a scroll here would prove the paste took the
// key path.
func TestLiveSingleCharacterPasteIsNotAKeyPress(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)

	e.SendPaste("x")

	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", "x"); !ok {
		t.Fatalf("field value = %q, want %q", got, "x")
	}
	if pressed := evalString(t, ctx, "window.pressed.join(',')"); pressed != "" {
		t.Fatalf("single-character paste raised keydown events %q, want none", pressed)
	}

	blurField(t, ctx)
	e.SendPaste(" ")
	// Settle: a space key press scrolls within a frame, so a still-zero offset
	// after the field read below is evidence of no default action, not of a
	// race. The field read is a round trip to the renderer after the paste.
	if got := evalFloat(t, ctx, "window.scrollY"); got != 0 {
		t.Fatalf("window.scrollY = %g after a pasted space, want 0 (a paste runs no key default action)", got)
	}
	if pressed := evalString(t, ctx, "window.pressed.join(',')"); pressed != "" {
		t.Fatalf("pasted space raised keydown events %q, want none", pressed)
	}
}

// TestLiveEscapeReachesPage proves Escape reaches the page as a key stroke: the
// document's keydown listener records it. Escape has no visible default action
// on a plain page, so page-observable key state is the assertion.
func TestLiveEscapeReachesPage(t *testing.T) {
	e, ctx := liveInputEngine(t)

	e.SendKey(Key{Code: "Escape"})

	if got, ok := waitForString(t, ctx, "window.pressed.join(',')", "Escape"); !ok {
		t.Fatalf("page keydown log = %q, want Escape", got)
	}
}

// caretIndex reads the caret offset of a focused text control, the observable a
// caret-key test asserts on.
func caretIndex(t *testing.T, ctx context.Context, id string) float64 {
	t.Helper()
	return evalFloat(t, ctx, "document.getElementById('"+id+"').selectionStart")
}

// heldScroll reads the engine's own scroll offset, so a test can assert it
// matches the offset the page settled at after a key-driven scroll.
func heldScroll(e *ChromiumEngine) (x, y int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.scrollX, e.scrollY
}

// TestLiveArrowKeysMoveCaretInFocusedField proves Left and Right reach a
// focused field and move its caret rather than scrolling the page. The page
// must stay at the top: a caret key that scrolled a focused field's page would
// be the shipped bug this replaces.
func TestLiveArrowKeysMoveCaretInFocusedField(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)
	e.SendText("nyala")
	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", "nyala"); !ok {
		t.Fatalf("field value = %q, want %q", got, "nyala")
	}

	e.SendKey(Key{Code: "ArrowLeft"})
	e.SendKey(Key{Code: "ArrowLeft"})
	if got := caretIndex(t, ctx, "f"); got != 3 {
		t.Fatalf("caret after two ArrowLeft = %g, want 3", got)
	}

	e.SendKey(Key{Code: "ArrowRight"})
	if got := caretIndex(t, ctx, "f"); got != 4 {
		t.Fatalf("caret after ArrowRight = %g, want 4", got)
	}

	if got := evalFloat(t, ctx, "window.scrollY"); got != 0 {
		t.Fatalf("window.scrollY = %g, want 0 (a caret key in a focused field must not scroll)", got)
	}
	if _, y := heldScroll(e); y != 0 {
		t.Fatalf("held scroll offset = %d, want 0", y)
	}
}

// TestLiveHomeEndMoveCaretInFocusedField proves Home and End move the caret to
// the line ends of a focused field instead of scrolling to the document edges.
func TestLiveHomeEndMoveCaretInFocusedField(t *testing.T) {
	e, ctx := liveInputEngine(t)
	focusField(t, ctx)
	e.SendText("nyala")
	if got, ok := waitForString(t, ctx, "document.getElementById('f').value", "nyala"); !ok {
		t.Fatalf("field value = %q, want %q", got, "nyala")
	}

	e.SendKey(Key{Code: "Home"})
	if got := caretIndex(t, ctx, "f"); got != 0 {
		t.Fatalf("caret after Home = %g, want 0 (line start)", got)
	}
	if got := evalFloat(t, ctx, "window.scrollY"); got != 0 {
		t.Fatalf("window.scrollY = %g after Home in a focused field, want 0", got)
	}

	e.SendKey(Key{Code: "End"})
	if got := caretIndex(t, ctx, "f"); got != 5 {
		t.Fatalf("caret after End = %g, want 5 (line end)", got)
	}
	if got := evalFloat(t, ctx, "window.scrollY"); got != 0 {
		t.Fatalf("window.scrollY = %g after End in a focused field, want 0 (the line end, not the document end)", got)
	}
}

// TestLiveVerticalArrowsMoveCaretInTextArea proves Up and Down move the caret
// between the lines of a focused multi-line control rather than scrolling the
// page, the half of the bug where the page moved under the reader instead.
func TestLiveVerticalArrowsMoveCaretInTextArea(t *testing.T) {
	e, ctx := liveInputEngine(t)
	evalVoid(t, ctx, "var a=document.getElementById('t');a.value='alpha\\nbravo';a.focus();a.setSelectionRange(11,11)")

	e.SendKey(Key{Code: "ArrowUp"})
	if got := caretIndex(t, ctx, "t"); got != 5 {
		t.Fatalf("caret after ArrowUp = %g, want 5 (same column, previous line)", got)
	}

	e.SendKey(Key{Code: "ArrowDown"})
	if got := caretIndex(t, ctx, "t"); got != 11 {
		t.Fatalf("caret after ArrowDown = %g, want 11 (same column, next line)", got)
	}

	if got := evalFloat(t, ctx, "window.scrollY"); got != 0 {
		t.Fatalf("window.scrollY = %g, want 0 (a caret key in a focused control must not scroll)", got)
	}
}

// TestLiveCaretKeysScrollWithoutFocus proves the other half of the browser's
// arbitration: with nothing focused, the caret keys still scroll the page, and
// the engine's held offset matches the offset the page settled at, so the wheel
// transport and crash recovery keep working from where the page sits.
func TestLiveCaretKeysScrollWithoutFocus(t *testing.T) {
	e, ctx := liveInputEngine(t)
	blurField(t, ctx)
	if before := evalFloat(t, ctx, "window.scrollY"); before != 0 {
		t.Fatalf("page starts scrolled at %g, want 0", before)
	}

	e.SendKey(Key{Code: "ArrowDown"})
	afterArrow := evalFloat(t, ctx, "window.scrollY")
	if afterArrow <= 0 {
		t.Fatalf("window.scrollY = %g after ArrowDown, want a scroll off the top", afterArrow)
	}
	if _, held := heldScroll(e); float64(held) != afterArrow {
		t.Fatalf("held scroll offset = %d, want %g (the page's own offset)", held, afterArrow)
	}

	e.SendKey(Key{Code: "PageDown"})
	afterPage := evalFloat(t, ctx, "window.scrollY")
	if afterPage <= afterArrow {
		t.Fatalf("window.scrollY = %g after PageDown, want more than the line step %g", afterPage, afterArrow)
	}
	if _, held := heldScroll(e); float64(held) != afterPage {
		t.Fatalf("held scroll offset = %d, want %g (the page's own offset)", held, afterPage)
	}

	e.SendKey(Key{Code: "End"})
	afterEnd := evalFloat(t, ctx, "window.scrollY")
	bottom := evalFloat(t, ctx, "document.documentElement.scrollHeight-window.innerHeight")
	if afterEnd < bottom-1 {
		t.Fatalf("window.scrollY = %g after End, want the document bottom %g", afterEnd, bottom)
	}
	if _, held := heldScroll(e); float64(held) != afterEnd {
		t.Fatalf("held scroll offset = %d, want %g (the page's own offset at the bottom)", held, afterEnd)
	}

	e.SendKey(Key{Code: "Home"})
	if got := evalFloat(t, ctx, "window.scrollY"); got != 0 {
		t.Fatalf("window.scrollY = %g after Home, want 0 (the document top)", got)
	}
	if _, held := heldScroll(e); held != 0 {
		t.Fatalf("held scroll offset = %d after Home, want 0", held)
	}
}

// TestLiveKeyScrollSnapsToCellPitch proves a key-driven scroll on the block
// backend leaves the page on the cell grid, the invariant the crisp text
// overlay depends on, and that the engine holds the same snapped offset.
func TestLiveKeyScrollSnapsToCellPitch(t *testing.T) {
	e, ctx := liveInputEngine(t)
	blurField(t, ctx)

	e.mu.Lock()
	_, cellH := e.snapshotCellPx()
	e.mu.Unlock()
	if cellH <= 0 {
		t.Fatalf("snap cell height = %g, want the block cell pitch", cellH)
	}

	e.SendKey(Key{Code: "ArrowDown"})

	got := evalFloat(t, ctx, "window.scrollY")
	if cells := got / cellH; cells != math.Trunc(cells) {
		t.Fatalf("window.scrollY = %g, want a whole multiple of the %g px cell pitch", got, cellH)
	}
	if _, held := heldScroll(e); float64(held) != got {
		t.Fatalf("held scroll offset = %d, want %g (the snapped page offset)", held, got)
	}
}
