package viewer

import (
	"bytes"
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
// with the browser field omitted when empty, the close anchored right, and
// the row exactly cols cells wide.
func TestRenderBarAbout(t *testing.T) {
	const cols = 60
	content, spans, _ := renderBarAbout(cols, []barField{{text: "Nyala dev", priority: 2}, {text: "Chromium 138", priority: 3}})
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("about row width = %d, want %d", w, cols)
	}
	if !strings.Contains(content, "Nyala dev") || !strings.Contains(content, "Chromium 138") {
		t.Fatalf("about row %q missing the versions", content)
	}
	if len(spans) != 1 || spans[0].item != barSpanClose {
		t.Fatalf("about spans = %+v, want the close alone", spans)
	}
	if end := spans[0].col + spans[0].width - 1; end != cols {
		t.Fatalf("close ends at %d, want the corner column %d", end, cols)
	}

	bare, _, _ := renderBarAbout(cols, []barField{{text: "Nyala dev", priority: 2}})
	if strings.Count(bare, barSeparator) != 1 {
		t.Fatalf("about row without a browser kept a field separator: %q", bare)
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
	_, l := renderBar(80, barContent{indicator: "⚃", url: "https://example.org/"})
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

// TestFPSCommandTogglesSegment pins the menu's fps command: activating it shows
// the bar's fps segment on a session the switch left off, arming a meter so the
// badge has rates, and activating it again hides the segment while keeping the
// meter. Each activation closes the menu, so the user sees the effect.
func TestFPSCommandTogglesSegment(t *testing.T) {
	s, _ := modeSession(t, streamTestEngine{currentURL: "https://example.org/"})
	if s.fpsBadgeShown() || s.fps != nil {
		t.Fatal("the badge starts shown on a session with no timing switch")
	}
	showAt := menuIndex(t, false, barCommandShowFPS)

	s.handleKey(keyEvent{act: actionOpenMenu})
	s.menuSel = showAt
	if quit := s.handleKey(keyEvent{b: keyEnter}); quit {
		t.Fatal("the fps command quit")
	}
	if !s.fpsBadgeShown() {
		t.Fatal("the fps command did not show the segment")
	}
	if s.fps == nil {
		t.Fatal("showing the badge did not arm a meter, so it has no rates to report")
	}
	if s.barMode != barModeChrome {
		t.Fatalf("the fps command left the menu open: mode %d", s.barMode)
	}
	if _, ok := s.barLay.segment(barSegmentFPS); !ok {
		t.Fatal("the repaint after showing has no fps span")
	}

	meter := s.fps
	s.handleKey(keyEvent{act: actionOpenMenu})
	s.menuSel = menuIndex(t, true, barCommandHideFPS)
	s.handleKey(keyEvent{b: keyEnter})
	if s.fpsBadgeShown() {
		t.Fatal("the fps command did not hide the segment")
	}
	if s.fps != meter {
		t.Fatal("hiding the badge discarded the meter the quit summary reads")
	}
	if _, ok := s.barLay.segment(barSegmentFPS); ok {
		t.Fatal("the repaint after hiding still has an fps span")
	}
}

// TestFPSCommandLabelReflectsState pins the label: it names the action, so a
// hidden segment offers to show it and a shown segment offers to hide it, and
// both labels are the same width so the menu row keeps its columns.
func TestFPSCommandLabelReflectsState(t *testing.T) {
	if got := barCommandNames(false)[0]; got != barCommandShowFPS {
		t.Fatalf("hidden-state label = %q, want %q", got, barCommandShowFPS)
	}
	if got := barCommandNames(true)[0]; got != barCommandHideFPS {
		t.Fatalf("shown-state label = %q, want %q", got, barCommandHideFPS)
	}
	if a, b := runewidth.StringWidth(barCommandShowFPS), runewidth.StringWidth(barCommandHideFPS); a != b {
		t.Fatalf("label widths differ (%d and %d), so the menu row shifts on a toggle", a, b)
	}

	// The menu row and its spans are identical apart from the one word, so a
	// click lands on the same item in either state.
	shown, shownSpans := renderBarMenu(60, barCommandNames(true))
	hidden, hiddenSpans := renderBarMenu(60, barCommandNames(false))
	if len(shownSpans) != len(hiddenSpans) {
		t.Fatalf("menu spans = %d shown and %d hidden, want the same", len(shownSpans), len(hiddenSpans))
	}
	for i := range shownSpans {
		if shownSpans[i] != hiddenSpans[i] {
			t.Fatalf("menu span %d moved: %+v shown, %+v hidden", i, shownSpans[i], hiddenSpans[i])
		}
	}
	if runewidth.StringWidth(shown) != runewidth.StringWidth(hidden) {
		t.Fatalf("menu row widths differ: %q and %q", shown, hidden)
	}
}

// TestFPSSegmentKeepsMenuAnchored pins the chrome layout across the toggle: the
// ☰ button holds the same column and width with and without the fps segment,
// because it is the last atom of a row that always totals cols and the flexible
// URL region absorbs the difference. It also checks the hit-test in both states,
// so a click still resolves to the segment drawn under it.
func TestFPSSegmentKeepsMenuAnchored(t *testing.T) {
	const cols = 80
	withFPS, withLay := renderBar(cols, barContent{indicator: "⚃", url: "https://example.org/", fps: "↓ 60.0 │ ↑ 60.0 │ ≌ 120"})
	without, withoutLay := renderBar(cols, barContent{indicator: "⚃", url: "https://example.org/"})

	if runewidth.StringWidth(withFPS) != cols || runewidth.StringWidth(without) != cols {
		t.Fatalf("bar rows are %d and %d cells, want %d", runewidth.StringWidth(withFPS), runewidth.StringWidth(without), cols)
	}
	menuWith, ok := withLay.segment(barSegmentMenu)
	if !ok {
		t.Fatal("menu segment missing with the fps segment")
	}
	menuWithout, ok := withoutLay.segment(barSegmentMenu)
	if !ok {
		t.Fatal("menu segment missing without the fps segment")
	}
	if menuWith != menuWithout {
		t.Fatalf("the menu button moved on the toggle: %+v with fps, %+v without", menuWith, menuWithout)
	}

	// Every button left of the URL region also holds its column: only the URL
	// region resizes, and it sits left of the fps segment.
	for _, kind := range []barSegmentKind{
		barSegmentBack, barSegmentForward, barSegmentReload, barSegmentHome, barSegmentIndicator,
	} {
		a, okA := withLay.segment(kind)
		b, okB := withoutLay.segment(kind)
		if !okA || !okB || a != b {
			t.Fatalf("segment %d moved on the toggle: %+v with fps, %+v without", kind, a, b)
		}
	}

	// The URL region absorbs exactly the cells the fps block occupied.
	urlWith, _ := withLay.segment(barSegmentURL)
	urlWithout, _ := withoutLay.segment(barSegmentURL)
	fpsSeg, ok := withLay.segment(barSegmentFPS)
	if !ok {
		t.Fatal("fps segment missing from the fps layout")
	}
	if urlWithout.width <= urlWith.width {
		t.Fatalf("hiding the fps segment did not widen the URL region: %d to %d", urlWith.width, urlWithout.width)
	}
	if _, ok := withoutLay.segment(barSegmentFPS); ok {
		t.Fatal("the fps-free layout still holds an fps span")
	}

	// Hit-testing lands correctly in both states: the fps cells resolve to the
	// fps segment while it is drawn, and to the URL region once it is not.
	if kind, ok := withLay.hit(fpsSeg.col); !ok || kind != barSegmentFPS {
		t.Fatalf("hit on the fps cell = (%d, %v), want the fps segment", kind, ok)
	}
	if kind, ok := withoutLay.hit(menuWithout.col); !ok || kind != barSegmentMenu {
		t.Fatalf("hit on the menu cell without fps = (%d, %v), want the menu", kind, ok)
	}
	if kind, ok := withLay.hit(menuWith.col); !ok || kind != barSegmentMenu {
		t.Fatalf("hit on the menu cell with fps = (%d, %v), want the menu", kind, ok)
	}
}

// TestFPSCommandLeavesMeasurementControls pins that the toggle is a display
// control only: the timing switch still decides whether Stream arms a meter at
// startup, and the switch's own meter is what the quit summary reads, so a
// menu-armed meter cannot add a summary line to a run that did not ask for one.
func TestFPSCommandLeavesMeasurementControls(t *testing.T) {
	if m := newFPSMeter([]string{"PATH=/usr/bin"}); m != nil {
		t.Fatal("the meter armed without the timing switch")
	}
	if m := newFPSMeter([]string{fpsTimingEnv + "1"}); m == nil {
		t.Fatal("the timing switch no longer arms the meter")
	}
	if m := newFPSMeter([]string{fpsTimingEnv}); m != nil {
		t.Fatal("an empty timing switch armed the meter")
	}

	// Stream registers printSummary against the startup meter, so a nil one
	// prints nothing however the session later toggles the badge.
	var out bytes.Buffer
	var absent *fpsMeter
	absent.printSummary(&out)
	if out.Len() != 0 {
		t.Fatalf("a session with no startup meter printed a summary: %q", out.String())
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
			if got := exitTerminal(rows, drawn, false, true, false); !strings.Contains(got, disableBracketedPaste) {
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

// menuIndex returns the menu index of the command labelled name for the given
// fps state, so a test addresses items by label and does not break when the
// registry gains an entry.
func menuIndex(t *testing.T, fpsShown bool, name string) int {
	t.Helper()
	for i, n := range barCommandNames(fpsShown) {
		if n == name {
			return i
		}
	}
	t.Fatalf("menu has no %q command: %v", name, barCommandNames(fpsShown))
	return 0
}

// TestMenuModeKeyboard pins the keyboard menu: Alt+F opens it, Tab and the
// arrows move the highlight with wrap, Enter activates About (the version
// line with the browser product), Esc closes each mode, and Enter on Quit
// quits through the command registry.
func TestMenuModeKeyboard(t *testing.T) {
	s, _ := modeSession(t, streamTestEngine{browserVersion: "Chromium 138.0.7204.49"})
	n := len(barCommandNames(false))

	s.handleKey(keyEvent{act: actionOpenMenu})
	if s.barMode != barModeMenu || s.menuSel != 0 {
		t.Fatalf("Alt+F did not open the menu: mode %d sel %d", s.barMode, s.menuSel)
	}

	s.handleKey(keyEvent{b: keyTab})
	if s.menuSel != 1 {
		t.Fatalf("Tab moved the highlight to %d, want 1", s.menuSel)
	}
	// Right from the last item wraps to the first, and Left from the first wraps
	// back to the last, whatever the registry length is.
	s.menuSel = n - 1
	s.handleKey(keyEvent{scroll: ScrollRight})
	if s.menuSel != 0 {
		t.Fatalf("Right did not wrap the highlight: sel %d", s.menuSel)
	}
	s.handleKey(keyEvent{scroll: ScrollLeft})
	if s.menuSel != n-1 {
		t.Fatalf("Left did not wrap backwards: sel %d, want %d", s.menuSel, n-1)
	}

	// Enter on About swaps to the version line.
	s.menuSel = menuIndex(t, s.fpsBadgeShown(), "About")
	if quit := s.handleKey(keyEvent{b: keyEnter}); quit {
		t.Fatal("About quit")
	}
	if s.barMode != barModeAbout {
		t.Fatalf("About did not open: mode %d", s.barMode)
	}
	out := s.tty.(interface{ String() string }).String()
	if !strings.Contains(out, "Nyala dev") || !strings.Contains(out, "Chromium 138") {
		t.Fatalf("about repaint missing the versions: %q", out)
	}
	s.handleKey(keyEvent{b: keyEsc})
	if s.barMode != barModeChrome {
		t.Fatalf("Esc did not close About: mode %d", s.barMode)
	}

	// Enter on Quit ends the session.
	s.handleKey(keyEvent{act: actionOpenMenu})
	s.menuSel = menuIndex(t, s.fpsBadgeShown(), "Quit")
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
	quitSpan, ok := menuSpan(s.modeSpans, menuIndex(t, s.fpsBadgeShown(), "Quit"))
	if !ok {
		t.Fatal("Quit span missing")
	}
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
