package viewer

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
)

// barMode selects what the bottom bar row shows and how it dispatches input.
// The zero value is chrome, the normal button-and-URL row; edit swaps the row
// for the URL editor, menu for the command list, and about for the version
// line. Every mode is exactly one row and repaints through the same bar fold
// in assemble.
type barMode int

const (
	// barModeChrome is the normal bar: buttons, indicator, URL, menu.
	barModeChrome barMode = iota
	// barModeEdit is the inline URL editor (URL click or Ctrl+L).
	barModeEdit
	// barModeMenu is the in-bar command menu (☰ click or Alt+F).
	barModeMenu
	// barModeAbout is the in-bar version line (About in the menu).
	barModeAbout
)

// barCommand is one entry in the bar's command registry: the menu renders the
// names and Enter or a click runs the action. run reports whether the command
// ends the session, riding the same quit path as Ctrl+Q. The future command
// palette reuses this registry.
type barCommand struct {
	name string
	run  func(s *streamSession) (quit bool)
}

// barCommands returns the command registry backing the menu: About swaps the
// bar to the version line and Quit ends the session through the existing
// teardown path.
func barCommands() []barCommand {
	return []barCommand{
		{name: "About", run: func(s *streamSession) bool { s.enterAbout(); return false }},
		{name: "Quit", run: func(*streamSession) bool { return true }},
	}
}

// barCommandNames returns the registry names in menu order.
func barCommandNames() []string {
	cmds := barCommands()
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.name
	}
	return names
}

// barSpanClose is the barSpan item id of the ✕ close button in the menu and
// About rows.
const barSpanClose = -1

// barSpan is one clickable region of a menu or About row: the item index it
// activates (or barSpanClose), its 1-based start column, and its width in
// cells.
type barSpan struct {
	item  int
	col   int
	width int
}

// spanAt returns the item id of the span at the 1-based column and whether
// one is there.
func spanAt(spans []barSpan, col int) (int, bool) {
	for _, s := range spans {
		if col >= s.col && col < s.col+s.width {
			return s.item, true
		}
	}
	return 0, false
}

// closeAnchoredRow pads a left-built row to cols and anchors the ✕ close
// button to the right corner, mirroring the chrome ☰ anchor. It returns the
// row, exactly cols cells wide with no escapes, and the spans extended with
// the close span. A row too narrow for the close button is truncated with the
// ellipsis and keeps only the spans that fit whole.
func closeAnchoredRow(cols int, left string, spans []barSpan) (string, []barSpan) {
	if cols <= 0 {
		return "", nil
	}
	leftW := runewidth.StringWidth(left)
	closeText := " " + barGlyphClose + " "
	closeW := runewidth.StringWidth(closeText)
	pad := cols - leftW - closeW
	if pad < 0 {
		content := runewidth.FillRight(runewidth.Truncate(left, cols, barEllipsis), cols)
		var kept []barSpan
		for _, s := range spans {
			if s.col+s.width-1 <= cols {
				kept = append(kept, s)
			}
		}
		return content, kept
	}
	content := left + strings.Repeat(" ", pad) + closeText
	spans = append(spans, barSpan{item: barSpanClose, col: cols - closeW + 1, width: closeW})
	return content, spans
}

// renderBarMenu lays out the in-bar menu row `About │ Quit │ ✕` for a
// cols-wide terminal: each command name is a padded click target, and the
// close button anchors to the right corner. It returns the plain row content
// and the click spans; the caller bolds the highlighted item with boldSpan.
func renderBarMenu(cols int, names []string) (string, []barSpan) {
	var left strings.Builder
	var spans []barSpan
	col := 1
	for i, n := range names {
		if i > 0 {
			left.WriteString(barSeparator)
			col += runewidth.StringWidth(barSeparator)
		}
		text := " " + n + " "
		w := runewidth.StringWidth(text)
		spans = append(spans, barSpan{item: i, col: col, width: w})
		left.WriteString(text)
		col += w
	}
	return closeAnchoredRow(cols, left.String(), spans)
}

// renderBarAbout lays out the in-bar About row
// `Nyala <version> │ <browser> │ ✕` for a cols-wide terminal. The browser
// segment is omitted when the engine reported no version. Only the
// right-anchored close button is clickable.
func renderBarAbout(cols int, version, browser string) (string, []barSpan) {
	left := " Nyala " + version
	if browser != "" {
		left += " " + barSeparator + " " + browser
	}
	return closeAnchoredRow(cols, left, nil)
}

// renderBarEdit lays out the URL editor row: the ❯ prompt, then the editor's
// viewport-windowed text padded to the row end. It returns the plain row
// content, exactly cols cells wide, the terminal cursor's 1-based column at the
// edit position, and the cells the prompt took. A row too narrow for the prompt
// gives the whole row to the text, reporting a zero prompt width. The caller
// keeps that width so a click maps against the row as drawn rather than a
// recomputed layout that could disagree about the narrow-row case.
func renderBarEdit(cols int, e *urlEditor) (content string, cursorCol, promptW int) {
	if cols <= 0 {
		return "", 1, 0
	}
	prompt := barGlyphPrompt + " "
	pw := runewidth.StringWidth(prompt)
	if cols-pw < 1 {
		prompt, pw = "", 0
	}
	visible, cursorCell := e.view(cols - pw)
	return prompt + runewidth.FillRight(visible, cols-pw), pw + cursorCell + 1, pw
}

// Terminal controls for paste and the cursor. Bracketed paste (mode 2004) is
// on for the whole session, enabled beside the full-screen escapes and
// disabled by exitTerminal on every teardown, so every paste arrives as one
// unit whether it lands in the page or the URL editor. The cursor, hidden
// globally by enterFullScreen, shows only while the editor parks it at the
// edit position.
const (
	enableBracketedPaste  = "\x1b[?2004h"
	disableBracketedPaste = "\x1b[?2004l"
	showCursor            = "\x1b[?25h"
	hideCursor            = "\x1b[?25l"
)

// currentURL returns the engine's last committed URL, falling back to the
// launch URL before the first commit, the string the bar and the editor
// prefill show. The page controls that URL, so control characters are
// stripped here at the display boundary: the bar writes it inside an SGR run,
// and a smuggled C0 or C1 byte would otherwise reach the terminal as an
// escape.
func (s *streamSession) currentURL() string {
	url := s.launchURL
	if s.eng != nil {
		if current := s.eng.CurrentURL(); current != "" {
			url = current
		}
	}
	return stripControls(url)
}

// stripControls removes C0 controls, DEL, and C1 controls from s, returning s
// unchanged (no allocation) when it is already clean.
func stripControls(s string) string {
	if !strings.ContainsFunc(s, isControlRune) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !isControlRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// repaintBar repaints the bar row through the ordered control path; a
// one-row terminal has no bar and repaints nothing.
func (s *streamSession) repaintBar() {
	if b := s.barBytes(); b != nil {
		_ = s.emitControl(b)
	}
}

// enterEdit swaps the bar to URL edit mode: the editor prefills the current
// URL with the cursor at the end and the terminal cursor shows so the repaint
// can park it at the edit position. Bracketed paste is already on for the
// session, so the mode leaves it alone. A one-row terminal has no bar row to
// draw the editor on, so entering would swallow keys in an invisible modal; the
// request is ignored instead.
func (s *streamSession) enterEdit() {
	if s.st.Rows < 2 {
		return
	}
	s.barMode = barModeEdit
	s.pressedSet = false
	s.editor = newURLEditor(s.currentURL())
	var buf bytes.Buffer
	buf.WriteString(showCursor)
	s.appendBar(&buf)
	_ = s.emitControl(buf.Bytes())
}

// leaveEdit restores chrome mode from the editor: the terminal cursor hides
// again. Bracketed paste stays on, so the next paste reaches the page as one
// unit.
func (s *streamSession) leaveEdit() {
	s.barMode = barModeChrome
	s.editor = nil
	var buf bytes.Buffer
	buf.WriteString(hideCursor)
	s.appendBar(&buf)
	_ = s.emitControl(buf.Bytes())
}

// commitEdit applies the schemeless rule to the edited text and navigates. A
// refused line (no scheme and no dot, or empty) keeps the editor open.
func (s *streamSession) commitEdit() {
	url, ok := commitURL(s.editor.String())
	if !ok {
		return
	}
	s.openURL(url)
	s.leaveEdit()
}

// openURL forwards one typed URL to the engine through the same serialised
// worker as keys, so a commit never overtakes an earlier queued key.
func (s *streamSession) openURL(url string) {
	if s.keySender != nil {
		s.keySender.sendOpen(url)
		return
	}
	s.eng.OpenURL(url)
}

// enterMenu swaps the bar to menu mode with the first command highlighted. A
// one-row terminal has no bar row to draw the menu on, so entering would
// swallow keys in an invisible modal; the request is ignored instead.
func (s *streamSession) enterMenu() {
	if s.st.Rows < 2 {
		return
	}
	s.barMode = barModeMenu
	s.pressedSet = false
	s.menuSel = 0
	s.repaintBar()
}

// enterAbout swaps the bar to the About line.
func (s *streamSession) enterAbout() {
	s.barMode = barModeAbout
	s.repaintBar()
}

// leaveMode restores chrome mode from the menu or About line.
func (s *streamSession) leaveMode() {
	s.barMode = barModeChrome
	s.repaintBar()
}

// exitBarModeIfHidden restores chrome mode when a resize left the terminal
// with no bar row: a mode that cannot draw must not keep swallowing keys as
// an invisible modal. Leaving edit mode also hides the cursor again.
func (s *streamSession) exitBarModeIfHidden() {
	if s.st.Rows >= 2 || s.barMode == barModeChrome {
		return
	}
	if s.barMode == barModeEdit {
		s.leaveEdit()
		return
	}
	s.leaveMode()
}

// activateMenu runs the registry command at index i and reports whether it
// quits the session.
func (s *streamSession) activateMenu(i int) bool {
	cmds := barCommands()
	if i < 0 || i >= len(cmds) {
		return false
	}
	return cmds[i].run(s)
}

// handleEditKey routes one key to the URL editor. While editing, every key
// belongs to the editor: printable bytes and assembled runes insert, pastes
// insert as one unit, the readline subset edits, Enter commits through the
// schemeless rule, and Esc cancels. Alt chords and page keys are swallowed so
// nothing reaches CDP; only the quit keys keep their meaning, so a wedged
// session can never trap the user.
func (s *streamSession) handleEditKey(ev keyEvent) (quit bool) {
	e := s.editor
	switch {
	case ev.paste != "":
		e.insertText(ev.paste)
	case ev.r != 0:
		e.insertRune(ev.r)
	case ev.scroll != ScrollNone:
		switch ev.scroll {
		case ScrollLeft:
			e.moveLeft()
		case ScrollRight:
			e.moveRight()
		case ScrollHome:
			e.moveHome()
		case ScrollEnd:
			e.moveEnd()
		}
	case ev.act != actionNone:
		if ev.act == actionDelete {
			e.deleteForward()
		}
	default:
		switch ev.b {
		case keyQuitCtrlQ, keyQuitCtrlC:
			return true
		case keyEnter:
			s.commitEdit()
			return false
		case keyEsc:
			s.leaveEdit()
			return false
		case keyCtrlA:
			e.moveHome()
		case keyCtrlE:
			e.moveEnd()
		case keyCtrlK:
			e.killToEnd()
		case keyCtrlU:
			e.clearAll()
		case keyCtrlW:
			e.deleteWord()
		case keyDel, keyBackspace:
			e.backspace()
		default:
			if printableByte(ev.b) {
				e.insertRune(rune(ev.b))
			}
		}
	}
	s.repaintBar()
	return false
}

// handleMenuKey routes one key in menu mode: Left/Right or Tab move the
// highlight with wrap, Enter activates the highlighted command, and Esc
// closes back to chrome. Everything else except the quit keys is swallowed.
func (s *streamSession) handleMenuKey(ev keyEvent) (quit bool) {
	n := len(barCommands())
	switch {
	case ev.scroll == ScrollLeft:
		s.menuSel = (s.menuSel + n - 1) % n
	case ev.scroll == ScrollRight:
		s.menuSel = (s.menuSel + 1) % n
	case ev.scroll != ScrollNone || ev.act != actionNone || ev.r != 0 || ev.paste != "":
		return false
	case ev.b == keyTab:
		s.menuSel = (s.menuSel + 1) % n
	case ev.b == keyEnter:
		return s.activateMenu(s.menuSel)
	case ev.b == keyEsc:
		s.leaveMode()
		return false
	case ev.b == keyQuitCtrlQ || ev.b == keyQuitCtrlC:
		return true
	default:
		return false
	}
	s.repaintBar()
	return false
}

// handleAboutKey routes one key in About mode: Esc closes back to chrome and
// everything else except the quit keys is swallowed.
func (s *streamSession) handleAboutKey(ev keyEvent) (quit bool) {
	if ev.scroll != ScrollNone || ev.act != actionNone || ev.r != 0 || ev.paste != "" {
		return false
	}
	switch ev.b {
	case keyEsc:
		s.leaveMode()
	case keyQuitCtrlQ, keyQuitCtrlC:
		return true
	}
	return false
}

// onBarRow reports whether a 0-based mouse row lands on the bar row: the
// bottom terminal row, present only when the terminal is at least two rows
// tall.
func (s *streamSession) onBarRow(row int) bool {
	return s.st.Rows >= 2 && row == s.st.Rows-1
}

// handleBarMouse dispatches one chrome-mode bar-row press against the spans of
// the last repaint: the nav buttons reuse the SendNav actions, the indicator
// cycles the backend like Alt+R, the URL region opens the editor, and ☰ opens
// the menu. A press on a button holds it bold until the release, which
// handleMouse clears wherever it lands. Motion and wheel events on the bar are
// dropped: the bar row sits outside the engine grid and holds no draggable
// control.
func (s *streamSession) handleBarMouse(ev MouseEvent) {
	if ev.Action != MouseActionPress {
		return
	}
	kind, ok := s.barLay.hit(ev.Col + 1)
	if !ok {
		return
	}
	switch kind {
	case barSegmentBack:
		s.pressSegment(kind)
		s.forwardNav(NavBack)
	case barSegmentForward:
		s.pressSegment(kind)
		s.forwardNav(NavForward)
	case barSegmentReload:
		s.pressSegment(kind)
		s.forwardNav(NavReload)
	case barSegmentHome:
		s.pressSegment(kind)
		s.forwardNav(NavHome)
	case barSegmentIndicator:
		s.pressSegment(kind)
		s.cycleBackend(false)
	case barSegmentURL:
		s.enterEdit()
	case barSegmentMenu:
		s.enterMenu()
	}
}

// pressSegment records the pressed bar button and repaints it bold; the
// matching release clears it.
func (s *streamSession) pressSegment(kind barSegmentKind) {
	s.pressed, s.pressedSet = kind, true
	s.repaintBar()
}

// appendBarMode renders the active non-chrome bar row into buf: the URL
// editor with the terminal cursor parked at the edit position, or the menu
// or About line with the highlight bolded. The row shares the chrome bar's
// bottom-row reverse-video framing, and the edit-mode cursor park is the last
// write so it lands inside the DEC 2026 bracket after every assembled frame.
func (s *streamSession) appendBarMode(buf *bytes.Buffer) {
	row := s.st.Rows
	switch s.barMode {
	case barModeEdit:
		content, cursorCol, promptW := renderBarEdit(s.st.Cols, s.editor)
		s.editPromptW = promptW
		fmt.Fprintf(buf, "\x1b[%d;1H\x1b[0;7m%s\x1b[0m\x1b[%d;%dH", row, content, row, cursorCol)
	case barModeMenu:
		content, spans := renderBarMenu(s.st.Cols, barCommandNames())
		s.modeSpans = spans
		if sp, ok := menuSpan(spans, s.menuSel); ok {
			content = boldSpan(content, sp.col, sp.width)
		}
		fmt.Fprintf(buf, "\x1b[%d;1H\x1b[0;7m%s\x1b[0m", row, content)
	case barModeAbout:
		browser := ""
		if s.eng != nil {
			browser = s.eng.BrowserVersion()
		}
		content, spans := renderBarAbout(s.st.Cols, s.version, browser)
		s.modeSpans = spans
		fmt.Fprintf(buf, "\x1b[%d;1H\x1b[0;7m%s\x1b[0m", row, content)
	}
}

// menuSpan returns the span for menu item i and whether the layout holds it.
func menuSpan(spans []barSpan, i int) (barSpan, bool) {
	for _, s := range spans {
		if s.item == i {
			return s, true
		}
	}
	return barSpan{}, false
}
