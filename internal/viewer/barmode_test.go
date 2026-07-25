package viewer

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// TestRenderBarMenuLayout pins the menu row: the command names in order as
// padded click targets, the ✕ close anchored to the right corner, the row
// exactly cols cells wide, and spanAt resolving clicks to items.
func TestRenderBarMenuLayout(t *testing.T) {
	const cols = 40
	content, spans := renderBarMenu(cols, []string{"About", "Quit"})
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("menu row width = %d, want %d", w, cols)
	}
	for _, want := range []string{"About", "Quit", barGlyphClose, barSeparator} {
		if !strings.Contains(content, want) {
			t.Fatalf("menu row %q missing %q", content, want)
		}
	}
	if len(spans) != 3 {
		t.Fatalf("menu spans = %d, want two items and the close", len(spans))
	}
	if spans[0].item != 0 || spans[1].item != 1 || spans[2].item != barSpanClose {
		t.Fatalf("menu span ids = %+v, want 0, 1, close", spans)
	}
	if end := spans[2].col + spans[2].width - 1; end != cols {
		t.Fatalf("close ends at %d, want the corner column %d", end, cols)
	}

	if item, ok := spanAt(spans, spans[1].col); !ok || item != 1 {
		t.Fatalf("spanAt(Quit col) = (%d, %v), want (1, true)", item, ok)
	}
	if _, ok := spanAt(spans, spans[0].col+spans[0].width); ok {
		t.Fatal("spanAt hit the separator cell between items")
	}
}

// TestRenderBarAbout pins the About row: `Nyala <version> │ <browser> │ ✕`
// with the browser segment omitted when empty, the close anchored right, and
// the row exactly cols cells wide.
func TestRenderBarAbout(t *testing.T) {
	const cols = 60
	content, spans := renderBarAbout(cols, "dev", "Chromium 138.0.7204.49")
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("about row width = %d, want %d", w, cols)
	}
	if !strings.Contains(content, "Nyala dev") || !strings.Contains(content, "Chromium 138.0.7204.49") {
		t.Fatalf("about row %q missing the versions", content)
	}
	if len(spans) != 1 || spans[0].item != barSpanClose {
		t.Fatalf("about spans = %+v, want the close alone", spans)
	}
	if end := spans[0].col + spans[0].width - 1; end != cols {
		t.Fatalf("close ends at %d, want the corner column %d", end, cols)
	}

	bare, _ := renderBarAbout(cols, "dev", "")
	if strings.Contains(bare, barSeparator) {
		t.Fatalf("about row without a browser kept a separator: %q", bare)
	}
}

// TestRenderBarEdit pins the editor row: the ❯ prompt leads, the text
// follows inside the viewport window, the row is exactly cols cells wide,
// the returned cursor column tracks the edit position after the prompt, and the
// reported prompt width is the cells the prompt actually took, which a click
// maps against.
func TestRenderBarEdit(t *testing.T) {
	const cols = 20
	e := newURLEditor("a.io")
	content, cursor, promptW := renderBarEdit(cols, e)
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("edit row width = %d, want %d", w, cols)
	}
	if !strings.HasPrefix(content, barGlyphPrompt+" a.io") {
		t.Fatalf("edit row = %q, want the prompt then the text", content)
	}
	wantPrompt := runewidth.StringWidth(barGlyphPrompt + " ")
	if promptW != wantPrompt {
		t.Fatalf("prompt width = %d, want %d", promptW, wantPrompt)
	}
	if want := wantPrompt + 4 + 1; cursor != want {
		t.Fatalf("cursor column = %d, want %d", cursor, want)
	}

	// A row too narrow for the prompt gives every cell to the text and reports a
	// zero prompt width, so a click is not offset past a prompt that never drew.
	narrow, _, narrowPrompt := renderBarEdit(1, newURLEditor("a"))
	if narrowPrompt != 0 {
		t.Fatalf("narrow-row prompt width = %d, want 0", narrowPrompt)
	}
	if runewidth.StringWidth(narrow) != 1 {
		t.Fatalf("narrow edit row = %q, want one cell", narrow)
	}
}

// TestBoldSpan pins the press-feedback styling: the span's cells gain SGR
// bold with a matching reset, an out-of-row span leaves the content
// untouched, and a span running to the row end still closes its bold.
func TestBoldSpan(t *testing.T) {
	if got := boldSpan("abcdef", 2, 3); got != "a\x1b[1mbcd\x1b[22mef" {
		t.Fatalf("boldSpan mid = %q", got)
	}
	if got := boldSpan("abc", 1, 3); got != "\x1b[1mabc\x1b[22m" {
		t.Fatalf("boldSpan whole = %q", got)
	}
	if got := boldSpan("abc", 9, 3); got != "abc" {
		t.Fatalf("boldSpan outside = %q, want unchanged", got)
	}
}

// TestBarLayoutHit pins the chrome hit-test: each button span resolves to its
// kind, the URL region resolves anywhere inside it, and a separator cell
// resolves to nothing.
func TestBarLayoutHit(t *testing.T) {
	_, l := renderBar(80, barContent{indicator: "⬔", url: "https://example.org/"})
	for _, kind := range []barSegmentKind{
		barSegmentBack, barSegmentForward, barSegmentReload, barSegmentHome,
		barSegmentIndicator, barSegmentURL, barSegmentMenu,
	} {
		seg, ok := l.segment(kind)
		if !ok {
			t.Fatalf("segment %d missing", kind)
		}
		for _, col := range []int{seg.col, seg.col + seg.width - 1} {
			if got, ok := l.hit(col); !ok || got != kind {
				t.Fatalf("hit(%d) = (%d, %v), want (%d, true)", col, got, ok, kind)
			}
		}
	}
	back, _ := l.segment(barSegmentBack)
	if _, ok := l.hit(back.col + back.width); ok {
		t.Fatal("hit resolved the separator cell after the back button")
	}
}

// modeSession builds a synchronous-dispatch session for the bar-mode tests:
// no keySender, so engine calls run inline, and the tty is a plain buffer.
func modeSession(t *testing.T, eng streamTestEngine) (*streamSession, *State) {
	t.Helper()
	st := NewState(80, 24)
	s := dispatchSession(eng, &st)
	s.version = "dev"
	return s, &st
}

// TestEditModeLifecycle pins the URL editor lifecycle: Ctrl+L enters edit
// mode prefilled with the current URL and enables bracketed paste and the
// cursor, every key routes to the editor (Alt chords included, so nothing
// reaches CDP), Enter applies the schemeless rule and navigates, and a
// refused commit keeps the editor open.
func TestEditModeLifecycle(t *testing.T) {
	var opened []string
	var sent []string
	var backends []string
	s, st := modeSession(t, streamTestEngine{
		currentURL: "https://example.org/",
		openURL:    func(u string) { opened = append(opened, u) },
		sendKey:    func(code string) { sent = append(sent, code) },
		setBackend: func(name string) { backends = append(backends, name) },
	})

	if quit := s.handleKey(keyEvent{b: keyCtrlL}); quit {
		t.Fatal("Ctrl+L quit")
	}
	if s.barMode != barModeEdit || s.editor == nil {
		t.Fatalf("Ctrl+L did not enter edit mode: mode %d", s.barMode)
	}
	if s.editor.String() != "https://example.org/" {
		t.Fatalf("editor prefill = %q, want the current URL", s.editor.String())
	}
	out := s.tty.(interface{ String() string }).String()
	if !strings.Contains(out, showCursor) {
		t.Fatalf("edit entry escapes missing from %q", out)
	}
	// Bracketed paste belongs to the session, not the mode: entering must not
	// re-enable it, so leaving cannot turn the page's paste route off.
	if strings.Contains(out, enableBracketedPaste) {
		t.Fatalf("edit entry toggled the session paste mode: %q", out)
	}

	// All input routes to the editor: an Alt chord cycles nothing and a
	// scroll key forwards nothing.
	startBackend := st.Backend
	s.handleKey(keyEvent{act: actionCycleBackend})
	s.handleKey(keyEvent{scroll: ScrollUp})
	if st.Backend != startBackend || len(backends) != 0 || len(sent) != 0 {
		t.Fatalf("edit mode leaked input: backends %v, keys %v", backends, sent)
	}

	// Retype a schemeless host and commit: the rule prepends https://.
	s.handleKey(keyEvent{b: keyCtrlU})
	for _, b := range []byte("a.io/x") {
		s.handleKey(keyEvent{b: b})
	}
	if quit := s.handleKey(keyEvent{b: keyEnter}); quit {
		t.Fatal("commit quit")
	}
	if len(opened) != 1 || opened[0] != "https://a.io/x" {
		t.Fatalf("OpenURL calls = %v, want [https://a.io/x]", opened)
	}
	if s.barMode != barModeChrome || s.editor != nil {
		t.Fatalf("commit did not return to chrome: mode %d", s.barMode)
	}
}

// TestEditModeRefusesDotlessCommit pins the schemeless refusal: a dotless
// line does not navigate and the editor stays open; Esc then cancels without
// navigating and restores the cursor and paste modes.
func TestEditModeRefusesDotlessCommit(t *testing.T) {
	var opened []string
	s, _ := modeSession(t, streamTestEngine{
		openURL: func(u string) { opened = append(opened, u) },
	})
	s.handleKey(keyEvent{b: keyCtrlL})
	s.handleKey(keyEvent{b: keyCtrlU})
	for _, b := range []byte("nodots") {
		s.handleKey(keyEvent{b: b})
	}
	s.handleKey(keyEvent{b: keyEnter})
	if len(opened) != 0 || s.barMode != barModeEdit {
		t.Fatalf("dotless commit navigated (%v) or left edit mode (%d)", opened, s.barMode)
	}

	s.handleKey(keyEvent{b: keyEsc})
	if s.barMode != barModeChrome || len(opened) != 0 {
		t.Fatalf("Esc did not cancel cleanly: mode %d, opened %v", s.barMode, opened)
	}
	out := s.tty.(interface{ String() string }).String()
	if !strings.Contains(out, hideCursor) {
		t.Fatalf("edit exit escapes missing from %q", out)
	}
	if strings.Contains(out, disableBracketedPaste) {
		t.Fatalf("edit exit turned the session paste mode off: %q", out)
	}
}

// TestEditModeClickPlacesCursor pins the click routing in edit mode: a bar-row
// press places the caret at the clicked cell against the prompt width the last
// repaint measured, the editor stays open, and no mouse event reaches the page.
// The prompt cells and the cell after the text are pinned too, so the whole row
// is predictable.
func TestEditModeClickPlacesCursor(t *testing.T) {
	barRow := 23
	promptW := runewidth.StringWidth(barGlyphPrompt + " ")
	cases := []struct {
		name string
		col  int
		want int
	}{
		{"prompt cell", 0, 0},
		{"first text cell", promptW, 0},
		{"third text cell", promptW + 2, 2},
		{"cell after the text", promptW + 4, 4},
		{"far past the text", promptW + 40, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mouse []MouseEvent
			s, _ := modeSession(t, streamTestEngine{
				currentURL: "a.io",
				sendMouse:  func(ev MouseEvent) { mouse = append(mouse, ev) },
			})
			s.handleKey(keyEvent{b: keyCtrlL})
			if s.barMode != barModeEdit {
				t.Fatalf("Ctrl+L did not open the editor: mode %d", s.barMode)
			}
			if s.editPromptW != promptW {
				t.Fatalf("recorded prompt width = %d, want %d", s.editPromptW, promptW)
			}

			s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: c.col, Row: barRow})

			if s.barMode != barModeEdit {
				t.Fatalf("the click closed the editor: mode %d", s.barMode)
			}
			if s.editor.cursor != c.want {
				t.Fatalf("click at column %d = cursor %d, want %d", c.col, s.editor.cursor, c.want)
			}
			if len(mouse) != 0 {
				t.Fatalf("an editor click reached the page: %+v", mouse)
			}
			if s.dragOnPage {
				t.Fatal("an editor press armed the page pointer capture")
			}
		})
	}
}

// TestEditModePageClickStillCancels pins what a click outside the bar row does
// while the editor is open: it cancels the editor without navigating, and page
// input keeps working once the editor has closed.
func TestEditModePageClickStillCancels(t *testing.T) {
	var mouse []MouseEvent
	var opened []string
	s, _ := modeSession(t, streamTestEngine{
		currentURL: "a.io",
		openURL:    func(u string) { opened = append(opened, u) },
		sendMouse:  func(ev MouseEvent) { mouse = append(mouse, ev) },
	})
	s.handleKey(keyEvent{b: keyCtrlL})

	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 10, Row: 5})
	if s.barMode != barModeChrome || s.editor != nil {
		t.Fatalf("a page click did not cancel the editor: mode %d", s.barMode)
	}
	if len(opened) != 0 {
		t.Fatalf("cancelling navigated: %v", opened)
	}
	if len(mouse) != 0 {
		t.Fatalf("the cancelling click also reached the page: %+v", mouse)
	}

	// The next page press is ordinary page input again.
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 10, Row: 5})
	if len(mouse) != 1 || mouse[0].Action != MouseActionPress {
		t.Fatalf("page press after the cancel = %+v, want one forwarded press", mouse)
	}
}

// TestBracketedPasteIsSessionWide pins the paste mode as session state, not
// mode state: the enable and disable are the DEC 2004 pair, and every teardown
// emits the disable whether or not a frame drew, so a normal quit, a signal,
// and a startup error all leave the shell with paste markers off.
func TestBracketedPasteIsSessionWide(t *testing.T) {
	if enableBracketedPaste != "\x1b[?2004h" || disableBracketedPaste != "\x1b[?2004l" {
		t.Fatalf("paste mode pair = %q/%q, want ?2004h/?2004l",
			enableBracketedPaste, disableBracketedPaste)
	}
	for _, drawn := range []bool{true, false} {
		for _, rows := range []int{24, 1, 0} {
			if got := exitTerminal(rows, drawn); !strings.Contains(got, disableBracketedPaste) {
				t.Fatalf("exitTerminal(%d, %t) = %q, want it to contain %q",
					rows, drawn, got, disableBracketedPaste)
			}
		}
	}
}

// TestEditModePasteAndRunes pins the editor's paste and rune input: a
// bracketed paste inserts as one unit with newlines stripped, and an
// assembled UTF-8 rune inserts at the cursor.
func TestEditModePasteAndRunes(t *testing.T) {
	s, _ := modeSession(t, streamTestEngine{})
	s.handleKey(keyEvent{b: keyCtrlL})
	s.handleKey(keyEvent{b: keyCtrlU})
	s.handleKey(keyEvent{paste: "exam\nple.org"})
	s.handleKey(keyEvent{r: 'é'})
	if got := s.editor.String(); got != "example.orgé" {
		t.Fatalf("editor text = %q, want the paste and the rune inserted", got)
	}
}

// TestMenuModeKeyboard pins the keyboard menu: Alt+F opens it, Tab and the
// arrows move the highlight with wrap, Enter activates About (the version
// line with the browser product), Esc closes each mode, and Enter on Quit
// quits through the command registry.
func TestMenuModeKeyboard(t *testing.T) {
	s, _ := modeSession(t, streamTestEngine{browserVersion: "Chromium 138.0.7204.49"})

	s.handleKey(keyEvent{act: actionOpenMenu})
	if s.barMode != barModeMenu || s.menuSel != 0 {
		t.Fatalf("Alt+F did not open the menu: mode %d sel %d", s.barMode, s.menuSel)
	}

	s.handleKey(keyEvent{b: keyTab})
	if s.menuSel != 1 {
		t.Fatalf("Tab moved the highlight to %d, want 1", s.menuSel)
	}
	s.handleKey(keyEvent{scroll: ScrollRight})
	if s.menuSel != 0 {
		t.Fatalf("Right did not wrap the highlight: sel %d", s.menuSel)
	}
	s.handleKey(keyEvent{scroll: ScrollLeft})
	if s.menuSel != 1 {
		t.Fatalf("Left did not wrap backwards: sel %d", s.menuSel)
	}

	// Enter on About swaps to the version line.
	s.menuSel = 0
	if quit := s.handleKey(keyEvent{b: keyEnter}); quit {
		t.Fatal("About quit")
	}
	if s.barMode != barModeAbout {
		t.Fatalf("About did not open: mode %d", s.barMode)
	}
	out := s.tty.(interface{ String() string }).String()
	if !strings.Contains(out, "Nyala dev") || !strings.Contains(out, "Chromium 138.0.7204.49") {
		t.Fatalf("about repaint missing the versions: %q", out)
	}
	s.handleKey(keyEvent{b: keyEsc})
	if s.barMode != barModeChrome {
		t.Fatalf("Esc did not close About: mode %d", s.barMode)
	}

	// Enter on Quit ends the session.
	s.handleKey(keyEvent{act: actionOpenMenu})
	s.handleKey(keyEvent{b: keyTab})
	if quit := s.handleKey(keyEvent{b: keyEnter}); !quit {
		t.Fatal("Quit did not quit")
	}
}

// TestHandleMouseBarChrome pins chrome bar-row dispatch: a press on each nav
// button forwards its SendNav action and holds the button bold until the
// release, the URL region opens the editor, ☰ opens the menu, and a page-row
// event still forwards to the engine.
func TestHandleMouseBarChrome(t *testing.T) {
	var navs []NavAction
	var forwarded []MouseEvent
	s, st := modeSession(t, streamTestEngine{
		sendNav:   func(a NavAction) { navs = append(navs, a) },
		sendMouse: func(ev MouseEvent) { forwarded = append(forwarded, ev) },
	})
	barRow := st.Rows - 1 // 0-based bar row
	s.repaintBar()        // lay out the chrome spans

	press := func(kind barSegmentKind) MouseEvent {
		t.Helper()
		seg, ok := s.barLay.segment(kind)
		if !ok {
			t.Fatalf("segment %d missing", kind)
		}
		return MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: seg.col - 1, Row: barRow}
	}

	wantNavs := []NavAction{NavBack, NavForward, NavReload, NavHome}
	for i, kind := range []barSegmentKind{barSegmentBack, barSegmentForward, barSegmentReload, barSegmentHome} {
		if quit := s.handleMouse(press(kind)); quit {
			t.Fatalf("nav press %d quit", kind)
		}
		if len(navs) != i+1 || navs[i] != wantNavs[i] {
			t.Fatalf("navs = %v, want %v", navs, wantNavs[:i+1])
		}
		if !s.pressedSet || s.pressed != kind {
			t.Fatalf("press feedback not set for %d", kind)
		}
		s.handleMouse(MouseEvent{Action: MouseActionRelease, Button: MouseButtonLeft, Col: 0, Row: barRow})
		if s.pressedSet {
			t.Fatal("release did not clear the press feedback")
		}
	}

	// The URL region enters edit mode.
	s.handleMouse(press(barSegmentURL))
	if s.barMode != barModeEdit {
		t.Fatalf("URL click did not enter edit mode: mode %d", s.barMode)
	}
	// A page-region press cancels the editor and never reaches the engine.
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 5, Row: 5})
	if s.barMode != barModeChrome || len(forwarded) != 0 {
		t.Fatalf("page click during edit: mode %d, forwarded %v", s.barMode, forwarded)
	}

	// ☰ opens the menu.
	s.repaintBar()
	s.handleMouse(press(barSegmentMenu))
	if s.barMode != barModeMenu {
		t.Fatalf("menu click did not enter menu mode: mode %d", s.barMode)
	}
	s.handleKey(keyEvent{b: keyEsc})

	// A page-row event in chrome forwards to the engine.
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 5, Row: 5})
	if len(forwarded) != 1 || forwarded[0].Row != 5 {
		t.Fatalf("page press forwards = %v, want the row-5 press", forwarded)
	}
}

// TestHandleMouseIndicatorCycles pins the backend-indicator click: it cycles
// the backend like Alt+R.
func TestHandleMouseIndicatorCycles(t *testing.T) {
	st := NewState(80, 24)
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	eng := streamTestEngine{}
	done := make(chan struct{})
	defer close(done)
	s := dispatchSession(eng, &st)
	s.ctl = newControl(eng, done)
	s.repaintBar()

	seg, ok := s.barLay.segment(barSegmentIndicator)
	if !ok {
		t.Fatal("indicator segment missing")
	}
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: seg.col - 1, Row: st.Rows - 1})
	if st.Backend != BackendBlock {
		t.Fatalf("indicator click did not cycle the backend: %v", st.Backend)
	}
}

// indicatorSession builds a synchronous-dispatch session over the enabled
// backends with the bar drawn once, and returns the session, the state, the
// indicator's 0-based column, and the forwarded mouse events, so an indicator
// test can click the segment and assert nothing reached the page.
func indicatorSession(t *testing.T, enabled []Backend) (*streamSession, *State, int, *[]MouseEvent) {
	t.Helper()
	st := NewState(80, 24)
	st.SetEnabled(enabled)
	var forwarded []MouseEvent
	eng := streamTestEngine{sendMouse: func(ev MouseEvent) { forwarded = append(forwarded, ev) }}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	s := dispatchSession(eng, &st)
	s.ctl = newControl(eng, done)
	s.repaintBar()

	seg, ok := s.barLay.segment(barSegmentIndicator)
	if !ok {
		t.Fatal("indicator segment missing")
	}
	return s, &st, seg.col - 1, &forwarded
}

// TestHandleMouseIndicatorRightClickCyclesReverse pins the right-click on the
// backend indicator: it steps the same enabled cycle backwards like
// Alt+Shift+R, reaching every enabled backend in reverse order and wrapping at
// the boundary, and it never forwards to the page, so no context-menu event
// reaches Chromium (a right press on the page row still does).
func TestHandleMouseIndicatorRightClickCyclesReverse(t *testing.T) {
	enabled := []Backend{BackendKitty, BackendSixel, BackendBlock}
	s, st, col, forwarded := indicatorSession(t, enabled)
	barRow := st.Rows - 1

	// Starting on the first enabled backend, reverse steps wrap to the last and
	// walk the list backwards, visiting every enabled backend.
	want := []Backend{BackendBlock, BackendSixel, BackendKitty}
	for i, w := range want {
		s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonRight, Col: col, Row: barRow})
		if st.Backend != w {
			t.Fatalf("reverse step %d = %v, want %v", i, st.Backend, w)
		}
		// The matching release must not cycle again.
		s.handleMouse(MouseEvent{Action: MouseActionRelease, Button: MouseButtonRight, Col: col, Row: barRow})
		if st.Backend != w {
			t.Fatalf("the release after reverse step %d cycled again to %v", i, st.Backend)
		}
	}
	if len(*forwarded) != 0 {
		t.Fatalf("an indicator right-click reached the page: %+v", *forwarded)
	}

	// The contrast: a right press on the page region does forward, where
	// Chromium turns it into the page's context-menu event.
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonRight, Col: 10, Row: 5})
	if len(*forwarded) != 1 || (*forwarded)[0].Button != MouseButtonRight {
		t.Fatalf("page right press = %+v, want one forwarded right press", *forwarded)
	}
}

// TestHandleMouseIndicatorSkipsUnavailable pins the reverse cycle against the
// enabled list: a backend the terminal cannot run is not in the list, so the
// reverse cycle steps straight past it rather than landing on it.
func TestHandleMouseIndicatorSkipsUnavailable(t *testing.T) {
	// Sixel is unavailable here, so the enabled cycle is Kitty and Block only.
	s, st, col, _ := indicatorSession(t, []Backend{BackendKitty, BackendBlock})
	barRow := st.Rows - 1

	for i, w := range []Backend{BackendBlock, BackendKitty, BackendBlock} {
		s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonRight, Col: col, Row: barRow})
		if st.Backend != w {
			t.Fatalf("reverse step %d = %v, want %v (Sixel is not enabled)", i, st.Backend, w)
		}
		if st.Backend == BackendSixel {
			t.Fatal("the reverse cycle landed on an unavailable backend")
		}
	}
}

// TestHandleMouseIndicatorLeftAndMiddleCycleForward pins that only the right
// button reverses: a left press steps forwards as before, and a middle press
// takes the same forward step, because no bar control has a secondary action and
// a click on a button is an activation whichever button it is.
func TestHandleMouseIndicatorLeftAndMiddleCycleForward(t *testing.T) {
	for _, button := range []MouseButton{MouseButtonLeft, MouseButtonMiddle} {
		s, st, col, _ := indicatorSession(t, []Backend{BackendKitty, BackendSixel, BackendBlock})
		barRow := st.Rows - 1

		for i, w := range []Backend{BackendSixel, BackendBlock, BackendKitty} {
			s.handleMouse(MouseEvent{Action: MouseActionPress, Button: button, Col: col, Row: barRow})
			if st.Backend != w {
				t.Fatalf("button %d forward step %d = %v, want %v", button, i, st.Backend, w)
			}
		}
	}
}

// TestHandleMouseMenuSpans pins menu-mode mouse dispatch: a click on Quit
// activates it, ✕ closes back to chrome, a padding cell on the bar row does
// nothing, and a page-region click closes the menu without forwarding.
func TestHandleMouseMenuSpans(t *testing.T) {
	var forwarded []MouseEvent
	s, st := modeSession(t, streamTestEngine{
		sendMouse: func(ev MouseEvent) { forwarded = append(forwarded, ev) },
	})
	barRow := st.Rows - 1

	s.handleKey(keyEvent{act: actionOpenMenu})
	closeSpan, ok := menuSpan(s.modeSpans, barSpanClose)
	if !ok {
		t.Fatal("close span missing")
	}
	if quit := s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: closeSpan.col - 1, Row: barRow}); quit {
		t.Fatal("close click quit")
	}
	if s.barMode != barModeChrome {
		t.Fatalf("close click did not close the menu: mode %d", s.barMode)
	}

	s.handleKey(keyEvent{act: actionOpenMenu})
	quitSpan, _ := menuSpan(s.modeSpans, 1)
	pad := MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: quitSpan.col + quitSpan.width + 2, Row: barRow}
	if s.handleMouse(pad); s.barMode != barModeMenu {
		t.Fatalf("padding click changed the mode: %d", s.barMode)
	}
	if quit := s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: quitSpan.col - 1, Row: barRow}); !quit {
		t.Fatal("Quit click did not quit")
	}

	s.barMode = barModeMenu
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 5, Row: 3})
	if s.barMode != barModeChrome || len(forwarded) != 0 {
		t.Fatalf("page click during menu: mode %d, forwarded %v", s.barMode, forwarded)
	}
}

// TestHandleMouseSideButtons pins the side buttons: a back or forward press
// on any row dispatches its history navigation, the release is inert, and
// neither forwards to the engine as a pointer event.
func TestHandleMouseSideButtons(t *testing.T) {
	var navs []NavAction
	var forwarded []MouseEvent
	s, _ := modeSession(t, streamTestEngine{
		sendNav:   func(a NavAction) { navs = append(navs, a) },
		sendMouse: func(ev MouseEvent) { forwarded = append(forwarded, ev) },
	})

	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonBack, Col: 10, Row: 5})
	s.handleMouse(MouseEvent{Action: MouseActionRelease, Button: MouseButtonBack, Col: 10, Row: 5})
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonForward, Col: 10, Row: 5})
	if len(navs) != 2 || navs[0] != NavBack || navs[1] != NavForward {
		t.Fatalf("side-button navs = %v, want [back forward]", navs)
	}
	if len(forwarded) != 0 {
		t.Fatalf("side buttons forwarded to the engine: %v", forwarded)
	}
}

// TestBarModesRefuseOneRowTerminal pins the invisible-modal guard: on a
// one-row terminal the bar cannot draw (appendBar emits nothing below two
// rows), so Ctrl+L and Alt+F must not enter a mode that would swallow keys
// with no visible feedback.
func TestBarModesRefuseOneRowTerminal(t *testing.T) {
	st := NewState(80, 1)
	s := dispatchSession(streamTestEngine{currentURL: "https://example.org/"}, &st)

	s.handleKey(keyEvent{b: keyCtrlL})
	if s.barMode != barModeChrome || s.editor != nil {
		t.Fatalf("Ctrl+L entered edit mode on a one-row terminal: mode %d", s.barMode)
	}
	s.handleKey(keyEvent{act: actionOpenMenu})
	if s.barMode != barModeChrome {
		t.Fatalf("Alt+F entered menu mode on a one-row terminal: mode %d", s.barMode)
	}
	if out := s.tty.(interface{ String() string }).String(); strings.Contains(out, showCursor) {
		t.Fatalf("refused edit entry still wrote terminal escapes: %q", out)
	}
}

// TestExitBarModeIfHidden pins the resize half of the invisible-modal guard:
// a shrink below two rows closes an open bar mode, and edit mode also hides the
// cursor again.
func TestExitBarModeIfHidden(t *testing.T) {
	s, st := modeSession(t, streamTestEngine{currentURL: "https://example.org/"})
	s.handleKey(keyEvent{b: keyCtrlL})
	if s.barMode != barModeEdit {
		t.Fatalf("Ctrl+L did not enter edit mode: %d", s.barMode)
	}

	st.Resize(80, 1)
	s.exitBarModeIfHidden()
	if s.barMode != barModeChrome || s.editor != nil {
		t.Fatalf("shrink kept the invisible edit mode open: mode %d", s.barMode)
	}
	out := s.tty.(interface{ String() string }).String()
	if !strings.Contains(out, hideCursor) {
		t.Fatalf("edit exit escapes missing from %q", out)
	}

	// The menu closes the same way, and a tall terminal keeps its mode.
	st.Resize(80, 24)
	s.handleKey(keyEvent{act: actionOpenMenu})
	st.Resize(80, 1)
	s.exitBarModeIfHidden()
	if s.barMode != barModeChrome {
		t.Fatalf("shrink kept the invisible menu open: mode %d", s.barMode)
	}
	st.Resize(80, 24)
	s.handleKey(keyEvent{act: actionOpenMenu})
	s.exitBarModeIfHidden()
	if s.barMode != barModeMenu {
		t.Fatalf("a tall terminal lost its menu mode: %d", s.barMode)
	}
}

// TestCurrentURLStripsControls pins the display boundary: a page-controlled
// URL with smuggled C0 or C1 bytes repaints without them, so the bar never
// hands the terminal an escape introducer inside its SGR run.
func TestCurrentURLStripsControls(t *testing.T) {
	s, _ := modeSession(t, streamTestEngine{currentURL: "https://e.org/\x1b]0;x\x07pq"})
	if got := s.currentURL(); got != "https://e.org/]0;xpq" {
		t.Fatalf("currentURL = %q, want control bytes stripped", got)
	}
}
