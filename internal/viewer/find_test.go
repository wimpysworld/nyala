package viewer

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

// findRecordingEngine records the forwarded search operations and keys on one
// channel, so a keySender test can assert search ordering against typing.
type findRecordingEngine struct {
	*fakeEngine
	got   chan string
	state FindState
}

func (e *findRecordingEngine) SendKey(code string) { e.got <- "key:" + code }

func (e *findRecordingEngine) Find(query string) { e.got <- "find:" + query }

func (e *findRecordingEngine) FindNext() { e.got <- "next" }

func (e *findRecordingEngine) FindPrev() { e.got <- "prev" }

func (e *findRecordingEngine) FindClear() { e.got <- "clear" }

func (e *findRecordingEngine) FindState() FindState { return e.state }

// TestKeySenderOrdersFindWithKeys asserts the shared worker forwards every
// search operation off the input loop and in submission order, so a search, its
// steps, and page keys reach the engine as the user produced them and a wedged
// browser search never blocks quit or draw.
func TestKeySenderOrdersFindWithKeys(t *testing.T) {
	eng := &findRecordingEngine{fakeEngine: &fakeEngine{}, got: make(chan string, 6)}
	ks := newKeySender(eng)
	t.Cleanup(ks.close)

	ks.sendFind("needle")
	ks.sendFindNext()
	ks.send("ArrowDown")
	ks.sendFindPrev()
	ks.sendFindClear()

	want := []string{"find:needle", "next", "key:ArrowDown", "prev", "clear"}
	deadline := time.After(2 * time.Second)
	for i, w := range want {
		select {
		case got := <-eng.got:
			if got != w {
				t.Fatalf("forwarded event %d = %q, want %q", i, got, w)
			}
		case <-deadline:
			t.Fatalf("keySender forwarded %d events, want %d", i, len(want))
		}
	}
}

// TestFindStateReadsWithoutDispatch asserts the state read is a plain snapshot:
// it forwards nothing to the engine, so a repaint can read the counts as often
// as it likes while a search is still in flight.
func TestFindStateReadsWithoutDispatch(t *testing.T) {
	eng := &findRecordingEngine{
		fakeEngine: &fakeEngine{},
		got:        make(chan string, 1),
		state:      FindState{Query: "needle", Total: 4, Current: 2, Pending: true},
	}

	got := eng.FindState()

	if got.Total != 4 || got.Current != 2 || !got.Pending {
		t.Fatalf("find state = %+v, want the second of four matches with a search in flight", got)
	}
	select {
	case ev := <-eng.got:
		t.Fatalf("reading the search state forwarded %q to the engine", ev)
	default:
	}
}

// findCalls records what a find-mode session forwarded over the Engine seam, so
// a test reads the calls in order rather than counting them hook by hook. state
// is the snapshot the bar draws its count from; a test moves it to stand for the
// engine landing a result.
type findCalls struct {
	ops   []string
	page  []string
	mouse []MouseEvent
	state FindState
}

// findSession returns a browser-free session wired to a findCalls recorder.
func findSession(t *testing.T, c *findCalls) (*streamSession, *State) {
	t.Helper()
	return modeSession(t, streamTestEngine{
		currentURL: "https://example.org/",
		find:       func(q string) { c.ops = append(c.ops, "find:"+q) },
		findNext:   func() { c.ops = append(c.ops, "next") },
		findPrev:   func() { c.ops = append(c.ops, "prev") },
		findClear:  func() { c.ops = append(c.ops, "clear") },
		findState:  &c.state,
		sendKey:    func(code string) { c.page = append(c.page, "key:"+code) },
		sendText:   func(text string) { c.page = append(c.page, "text:"+text) },
		sendPaste:  func(text string) { c.page = append(c.page, "paste:"+text) },
		sendMouse:  func(ev MouseEvent) { c.mouse = append(c.mouse, ev) },
		setBackend: func(name string) { c.page = append(c.page, "backend:"+name) },
	})
}

// TestRenderFindCount pins the count text for every search state: a landed
// result reads current/total, a page with no match reads 0/0, a fresh query in
// flight reads the ellipsis rather than claiming no matches, a step in flight
// keeps the counts it steps through, and a dropped dispatch keeps the last
// counts and adds the stall mark. Each state is a pure function of the snapshot,
// so a repaint of one state always draws one text.
func TestRenderFindCount(t *testing.T) {
	cases := []struct {
		name  string
		state FindState
		want  string
	}{
		{"landed", FindState{Query: "a", Total: 12, Current: 2}, "2/12"},
		{"no matches", FindState{Query: "a"}, "0/0"},
		{"fresh query in flight", FindState{Query: "a", Pending: true}, barEllipsis},
		{"step in flight", FindState{Query: "a", Total: 12, Current: 2, Pending: true}, "2/12"},
		{"stalled", FindState{Query: "a", Total: 12, Current: 2, Failed: true}, "2/12" + findStallMark},
		{"stalled with no result", FindState{Query: "a", Failed: true}, "0/0" + findStallMark},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderFindCount(c.state); got != c.want {
				t.Fatalf("renderFindCount(%+v) = %q, want %q", c.state, got, c.want)
			}
			if again := renderFindCount(c.state); again != c.want {
				t.Fatalf("a second repaint of one state drew %q, want %q", again, c.want)
			}
		})
	}
}

// TestRenderBarFindLayout pins the find row: the ⌕ prompt leads, the query
// follows, the count sits between the field and the right-anchored ✕, the row is
// exactly cols cells wide in every count state, the cursor column tracks the
// edit position, and the spans resolve a field click while the count cells
// resolve to nothing.
func TestRenderBarFindLayout(t *testing.T) {
	const cols = 40
	e := newURLEditor("needle")
	content, cursor, field, spans := renderBarFind(cols, e, "2/12")
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("find row width = %d, want %d", w, cols)
	}
	if !strings.HasPrefix(content, barGlyphFind+" needle") {
		t.Fatalf("find row = %q, want the prompt then the query", content)
	}
	for _, want := range []string{"2/12", barSeparator, barGlyphClose} {
		t.Run("holds "+want, func(t *testing.T) {
			if !strings.Contains(content, want) {
				t.Fatalf("find row %q missing %q", content, want)
			}
		})
	}
	promptW := runewidth.StringWidth(barGlyphFind + " ")
	if field.col != promptW+1 {
		t.Fatalf("field starts at column %d, want %d", field.col, promptW+1)
	}
	if want := promptW + len("needle") + 1; cursor != want {
		t.Fatalf("cursor column = %d, want %d", cursor, want)
	}
	if len(spans) != 2 || spans[0].item != barSpanFindField || spans[1].item != barSpanClose {
		t.Fatalf("find spans = %+v, want the field and the close", spans)
	}
	if end := spans[1].col + spans[1].width - 1; end != cols {
		t.Fatalf("close ends at %d, want the corner column %d", end, cols)
	}
	if item, ok := spanAt(spans, field.col+2); !ok || item != barSpanFindField {
		t.Fatalf("spanAt(a query cell) = (%d, %v), want the field", item, ok)
	}
	if _, ok := spanAt(spans, field.col+field.width+2); ok {
		t.Fatal("spanAt hit the count cells, which own no click target")
	}

	// Every count state keeps the row exactly cols cells wide, so a changed
	// count can never leave the row short or wrap it.
	for _, count := range []string{"0/0", barEllipsis, "2/12" + findStallMark, "2000/2000"} {
		row, _, _, _ := renderBarFind(cols, newURLEditor("needle"), count)
		if w := runewidth.StringWidth(row); w != cols {
			t.Fatalf("find row width with count %q = %d, want %d", count, w, cols)
		}
	}
}

// TestRenderBarFindNarrowRow pins the narrow-row degrade: a row too short to
// carry the count beside a usable field gives every cell to the prompt and the
// field, keeps its exact width, and leaves the whole field clickable, so the
// caret still places where a recomputed layout could be two cells out.
func TestRenderBarFindNarrowRow(t *testing.T) {
	for _, cols := range []int{1, 6, 12} {
		content, _, field, spans := renderBarFind(cols, newURLEditor("needle"), "2/12")
		if w := runewidth.StringWidth(content); w != cols {
			t.Fatalf("narrow find row (%d cols) width = %d", cols, w)
		}
		if strings.Contains(content, barGlyphClose) {
			t.Fatalf("narrow find row (%d cols) kept the close button: %q", cols, content)
		}
		if len(spans) != 1 || spans[0].item != barSpanFindField {
			t.Fatalf("narrow find spans = %+v, want the field alone", spans)
		}
		if field.col+field.width-1 != cols {
			t.Fatalf("narrow field span %+v does not reach the row end %d", field, cols)
		}
	}
}

// TestFindModeLifecycle pins the find-mode dispatch: Ctrl+F opens the field and
// shows the cursor without touching the session paste mode, typing edits the
// query and reaches no page, Enter searches the typed query, Enter again steps
// to the next match, Shift+Enter steps back, Down and Up step on a terminal with
// no modifier signal, retyping searches again rather than stepping, and Esc
// clears the search and restores chrome.
func TestFindModeLifecycle(t *testing.T) {
	var c findCalls
	s, st := findSession(t, &c)

	if quit := s.handleKey(keyEvent{b: keyCtrlF}); quit {
		t.Fatal("Ctrl+F quit")
	}
	if s.barMode != barModeFind || s.editor == nil {
		t.Fatalf("Ctrl+F did not enter find mode: mode %d", s.barMode)
	}
	if s.editor.String() != "" {
		t.Fatalf("find field prefill = %q, want empty", s.editor.String())
	}
	out := s.tty.(interface{ String() string }).String()
	if !strings.Contains(out, showCursor) {
		t.Fatalf("find entry escapes missing from %q", out)
	}
	if strings.Contains(out, enableBracketedPaste) {
		t.Fatalf("find entry toggled the session paste mode: %q", out)
	}

	// Every key belongs to the field: an Alt chord cycles nothing, and typing
	// reaches the query rather than the page.
	startBackend := st.Backend
	s.handleKey(keyEvent{act: actionCycleBackend})
	for _, b := range []byte("needle") {
		s.handleKey(keyEvent{b: b})
	}
	if st.Backend != startBackend {
		t.Fatalf("find mode leaked an Alt chord: backend %v", st.Backend)
	}
	if len(c.page) != 0 {
		t.Fatalf("find mode leaked input to the page: %v", c.page)
	}
	if s.editor.String() != "needle" {
		t.Fatalf("query = %q, want needle", s.editor.String())
	}
	if len(c.ops) != 0 {
		t.Fatalf("typing dispatched %v, want nothing until submission", c.ops)
	}

	s.handleKey(keyEvent{b: keyEnter})
	s.handleKey(keyEvent{b: keyEnter})
	s.handleKey(keyEvent{b: keyEnter, mod: cdpModShift})
	s.handleKey(keyEvent{scroll: ScrollDown})
	s.handleKey(keyEvent{scroll: ScrollUp})
	want := []string{"find:needle", "next", "prev", "next", "prev"}
	if strings.Join(c.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatched %v, want %v", c.ops, want)
	}

	// Editing the query makes the next submission a new search, not a step, so
	// the counts the bar shows can never belong to a query nobody searched for.
	s.handleKey(keyEvent{b: keyBackspace})
	s.handleKey(keyEvent{b: keyEnter})
	if got := c.ops[len(c.ops)-1]; got != "find:needl" {
		t.Fatalf("submission after an edit = %q, want a new search", got)
	}

	if quit := s.handleKey(keyEvent{b: keyEsc}); quit {
		t.Fatal("Esc quit")
	}
	if s.barMode != barModeChrome || s.editor != nil {
		t.Fatalf("Esc did not restore chrome: mode %d", s.barMode)
	}
	if got := c.ops[len(c.ops)-1]; got != "clear" {
		t.Fatalf("Esc dispatched %q last, want clear", got)
	}
	if len(c.page) != 0 {
		t.Fatalf("find mode leaked %v to the page, Esc included", c.page)
	}
	out = s.tty.(interface{ String() string }).String()
	if !strings.Contains(out, hideCursor) {
		t.Fatalf("find exit escapes missing from %q", out)
	}
	if strings.Contains(out, disableBracketedPaste) {
		t.Fatalf("find exit turned the session paste mode off: %q", out)
	}

	// Page input is back: a printable byte types into the page again.
	s.handleKey(keyEvent{b: 'x'})
	if len(c.page) != 1 || c.page[0] != "text:x" {
		t.Fatalf("page input after find = %v, want [text:x]", c.page)
	}
}

// TestFindModeEmptySubmissions pins the two empty-query cases: Enter with
// nothing typed dispatches nothing, and emptying a searched query and pressing
// Enter dispatches the empty query, which clears the highlights.
func TestFindModeEmptySubmissions(t *testing.T) {
	var c findCalls
	s, _ := findSession(t, &c)
	s.handleKey(keyEvent{b: keyCtrlF})

	s.handleKey(keyEvent{b: keyEnter})
	if len(c.ops) != 0 {
		t.Fatalf("Enter on an empty field dispatched %v", c.ops)
	}

	for _, b := range []byte("ne") {
		s.handleKey(keyEvent{b: b})
	}
	s.handleKey(keyEvent{b: keyEnter})
	s.handleKey(keyEvent{b: keyCtrlU})
	s.handleKey(keyEvent{b: keyEnter})
	want := []string{"find:ne", "find:"}
	if strings.Join(c.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatched %v, want %v", c.ops, want)
	}
}

// TestFindModePasteFillsField pins the paste route: a session-wide bracketed
// paste lands in the query field as one unit rather than the page, its newlines
// are stripped so it cannot smuggle a submission, and the search still waits for
// Enter.
func TestFindModePasteFillsField(t *testing.T) {
	var c findCalls
	s, _ := findSession(t, &c)
	s.handleKey(keyEvent{b: keyCtrlF})

	s.handleKey(keyEvent{paste: "nee\ndle"})
	if s.editor.String() != "needle" {
		t.Fatalf("query after the paste = %q, want needle", s.editor.String())
	}
	if len(c.page) != 0 || len(c.ops) != 0 {
		t.Fatalf("the paste reached the page (%v) or searched early (%v)", c.page, c.ops)
	}

	s.handleKey(keyEvent{b: keyEnter})
	if len(c.ops) != 1 || c.ops[0] != "find:needle" {
		t.Fatalf("dispatched %v, want [find:needle]", c.ops)
	}
}

// TestFindModeMouse pins the find-row mouse routing: a press in the query field
// places the caret at the clicked cell against the field span the last repaint
// measured, a press on the ✕ closes the mode and clears the search, a press on
// the page region closes it too, no press reaches CDP, and none arms the page
// pointer capture.
func TestFindModeMouse(t *testing.T) {
	const barRow = 23
	promptW := runewidth.StringWidth(barGlyphFind + " ")

	t.Run("field press places the caret", func(t *testing.T) {
		var c findCalls
		s, _ := findSession(t, &c)
		s.handleKey(keyEvent{b: keyCtrlF})
		for _, b := range []byte("needle") {
			s.handleKey(keyEvent{b: b})
		}
		if s.editPromptW != promptW || s.editFieldW < findMinField {
			t.Fatalf("recorded field = prompt %d, width %d", s.editPromptW, s.editFieldW)
		}

		s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: promptW + 2, Row: barRow})

		if s.barMode != barModeFind {
			t.Fatalf("the click closed find mode: mode %d", s.barMode)
		}
		if s.editor.cursor != 2 {
			t.Fatalf("caret = %d, want 2", s.editor.cursor)
		}
		if len(c.mouse) != 0 || len(c.ops) != 0 {
			t.Fatalf("a field click forwarded %v / %v", c.mouse, c.ops)
		}
		if s.dragOnPage {
			t.Fatal("a find press armed the page pointer capture")
		}
	})

	t.Run("close press clears and closes", func(t *testing.T) {
		var c findCalls
		s, st := findSession(t, &c)
		s.handleKey(keyEvent{b: keyCtrlF})
		closeSpan, ok := menuSpan(s.modeSpans, barSpanClose)
		if !ok {
			t.Fatalf("find row spans %+v hold no close button", s.modeSpans)
		}
		if end := closeSpan.col + closeSpan.width - 1; end != st.Cols {
			t.Fatalf("close ends at %d, want the corner column %d", end, st.Cols)
		}

		s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: closeSpan.col - 1, Row: barRow})

		if s.barMode != barModeChrome || s.editor != nil {
			t.Fatalf("the ✕ did not close find mode: mode %d", s.barMode)
		}
		if len(c.ops) != 1 || c.ops[0] != "clear" {
			t.Fatalf("the ✕ dispatched %v, want [clear]", c.ops)
		}
		if len(c.mouse) != 0 {
			t.Fatalf("the ✕ press reached the page: %+v", c.mouse)
		}
	})

	t.Run("page press closes and drops the capture", func(t *testing.T) {
		var c findCalls
		s, _ := findSession(t, &c)
		// A page press before find opens arms the pointer capture, and find mode
		// then swallows the release that would clear it. The first mouse event the
		// mode sees must drop the capture, or a stale capture would keep feeding
		// the page motion for the rest of the session.
		s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 4, Row: 2})
		s.handleKey(keyEvent{b: keyCtrlF})
		s.handleMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 6, Row: 2})
		if s.dragOnPage {
			t.Fatal("find mode kept the page pointer capture")
		}
		if s.barMode != barModeFind {
			t.Fatalf("a motion report closed find mode: mode %d", s.barMode)
		}

		s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 4, Row: 2})

		if s.barMode != barModeChrome {
			t.Fatalf("a page press left find mode open: mode %d", s.barMode)
		}
		if len(c.ops) != 1 || c.ops[0] != "clear" {
			t.Fatalf("a page press dispatched %v, want [clear]", c.ops)
		}
		// Neither the swallowed motion nor the press that dismissed the mode
		// reaches the page.
		if len(c.mouse) != 1 {
			t.Fatalf("page mouse events = %+v, want the pre-find press alone", c.mouse)
		}
	})
}

// TestFindModeRowDrawsCount pins the drawn row: the bar fold writes the find
// field with the engine's count and parks the terminal cursor at the edit
// position, and a landed count replaces the in-flight ellipsis on the next
// repaint without the row changing width.
func TestFindModeRowDrawsCount(t *testing.T) {
	var c findCalls
	c.state = FindState{Query: "needle", Pending: true}
	s, st := findSession(t, &c)
	s.handleKey(keyEvent{b: keyCtrlF})

	pending := string(s.barBytes())
	if !strings.Contains(pending, barEllipsis) {
		t.Fatalf("in-flight find row %q missing the pending count", pending)
	}
	if !strings.HasPrefix(pending, "\x1b[24;1H\x1b[0;7m") {
		t.Fatalf("find row prefix = %q, want bottom-row CUP and reverse video", pending)
	}
	if !strings.HasSuffix(pending, "\x1b[24;3H") {
		t.Fatalf("find row suffix = %q, want the cursor parked at the edit position", pending)
	}

	c.state = FindState{Query: "needle", Total: 12, Current: 1}
	landed := string(s.barBytes())
	if !strings.Contains(landed, "1/12") {
		t.Fatalf("landed find row %q missing the count", landed)
	}
	if s.editFieldW+s.editPromptW >= st.Cols {
		t.Fatalf("field %d and prompt %d leave no room for the count in %d cols", s.editFieldW, s.editPromptW, st.Cols)
	}
}

// TestFindModeExitsWhenHidden pins the resize escape hatch: a terminal left with
// no bar row cannot draw the field, so the mode closes and clears rather than
// swallowing keys as an invisible modal.
func TestFindModeExitsWhenHidden(t *testing.T) {
	var c findCalls
	s, st := findSession(t, &c)
	s.handleKey(keyEvent{b: keyCtrlF})

	st.Resize(80, 1)
	s.exitBarModeIfHidden()

	if s.barMode != barModeChrome || s.editor != nil {
		t.Fatalf("a hidden bar kept find mode open: mode %d", s.barMode)
	}
	if len(c.ops) != 1 || c.ops[0] != "clear" {
		t.Fatalf("the hidden-bar exit dispatched %v, want [clear]", c.ops)
	}

	// A one-row terminal refuses to open the mode at all.
	s.handleKey(keyEvent{b: keyCtrlF})
	if s.barMode != barModeChrome {
		t.Fatalf("Ctrl+F opened an undrawable find mode: mode %d", s.barMode)
	}
}
