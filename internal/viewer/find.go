package viewer

import (
	"bytes"
	"fmt"

	"github.com/mattn/go-runewidth"
)

// barSpanFindField is the barSpan item id of the find row's query field, the
// click target that places the caret. The count owns no span, so a click there
// does nothing, and the ✕ close keeps the shared barSpanClose id.
const barSpanFindField = -2

// findMinField is the fewest cells the query field keeps before the count and
// the close button are dropped: a field squeezed below it would show a couple of
// characters of the query, which is worse than a narrow row with no count.
const findMinField = 8

// findStallMark suffixes the count when the last dispatch did not land, so the
// reader can tell a stalled step from a step that moved. The count beside it is
// the last one the page reported, which still matches the painted highlights.
const findStallMark = "!"

// renderFindCount returns the bar's compact match count for one search-state
// snapshot: `current/total`, and `0/0` when the page holds no match. A dispatch
// in flight with no counts yet shows the ellipsis, because a new query resets
// the counts and drawing `0/0` would claim a page with no matches before the
// search has answered. A step keeps the counts it steps through, so the row
// holds its reading rather than blinking to a placeholder and back. A dropped
// dispatch keeps the last counts and adds the stall mark: the highlights those
// counts describe are still painted, so the mark says the keypress did not land,
// not that the count is wrong. Every state is a pure function of the snapshot,
// so any number of repaints of one state draw one row.
func renderFindCount(st FindState) string {
	if st.Pending && st.Current == 0 && st.Total == 0 {
		return barEllipsis
	}
	count := fmt.Sprintf("%d/%d", st.Current, st.Total)
	if st.Failed {
		return count + findStallMark
	}
	return count
}

// renderBarFind lays out the find row `⌕ <query> │ <count> │ ✕` for a cols-wide
// terminal: the ⌕ prompt leads, the query field absorbs the width the count and
// the right-anchored close button leave, and the count sits between them. It
// returns the plain row content, exactly cols cells wide with no escapes, the
// terminal cursor's 1-based column at the edit position, the query field's span,
// and the click spans. The caller keeps the field span, so a click maps to a
// text cell against the row as drawn. A row too narrow to hold the count beside
// a usable field degrades to the prompt and the field alone, the same degrade
// the chrome row makes, and the whole field stays clickable.
func renderBarFind(cols int, e *urlEditor, count string) (content string, cursorCol int, field barSpan, spans []barSpan) {
	if cols <= 0 {
		return "", 1, barSpan{}, nil
	}
	prompt := barGlyphFind + " "
	tail := " " + barSeparator + " " + count + " "
	closeText := " " + barGlyphClose + " "
	closeW := runewidth.StringWidth(closeText)
	leftW := cols - runewidth.StringWidth(tail) - closeW
	if leftW < runewidth.StringWidth(prompt)+findMinField {
		content, cursorCol, promptW := fieldRow(cols, prompt, e)
		field = barSpan{item: barSpanFindField, col: promptW + 1, width: cols - promptW}
		return content, cursorCol, field, []barSpan{field}
	}
	left, cursorCol, promptW := fieldRow(leftW, prompt, e)
	field = barSpan{item: barSpanFindField, col: promptW + 1, width: leftW - promptW}
	spans = []barSpan{field, {item: barSpanClose, col: cols - closeW + 1, width: closeW}}
	return left + tail + closeText, cursorCol, field, spans
}

// findState returns the engine's search snapshot, or the zero state when the
// session holds no engine, so a repaint always has counts to draw. The engine
// read takes only its state lock, so the bar never waits on Chromium.
func (s *streamSession) findState() FindState {
	if s.eng == nil {
		return FindState{}
	}
	return s.eng.FindState()
}

// enterFind swaps the bar to find mode: an empty query field with the terminal
// cursor showing so the repaint can park it at the edit position. The field
// starts empty rather than prefilled, because closing find cleared the search,
// so a prefilled query would name matches the page no longer highlights.
// Bracketed paste is already on for the session, so the mode leaves it alone. A
// one-row terminal has no bar row to draw the field on, so entering would
// swallow keys in an invisible modal; the request is ignored instead.
func (s *streamSession) enterFind() {
	if s.st.Rows < 2 {
		return
	}
	s.barMode = barModeFind
	s.pressedSet = false
	s.editor = newURLEditor("")
	s.findQuery = ""
	var buf bytes.Buffer
	buf.WriteString(showCursor)
	s.appendBar(&buf)
	_ = s.emitControl(buf.Bytes())
}

// leaveFind clears the search and restores chrome mode: the highlights, the
// injected stylesheet, and the page-side search state go with the mode, and the
// terminal cursor hides again. The clear leaves the page's DOM focus and scroll
// untouched, so page input works the moment the mode is gone and the page stays
// at the match the reader stopped on. Bracketed paste stays on, so the next
// paste reaches the page as one unit.
func (s *streamSession) leaveFind() {
	s.barMode = barModeChrome
	s.editor = nil
	s.findQuery = ""
	s.forwardFindClear()
	var buf bytes.Buffer
	buf.WriteString(hideCursor)
	s.appendBar(&buf)
	_ = s.emitControl(buf.Bytes())
}

// submitFind runs one Enter or match-step request. A query the engine has not
// searched for yet dispatches a new search, so Enter after typing finds and
// Enter again walks the matches; the search itself selects the first match.
// Search runs on submission rather than on every keystroke: each dispatch
// re-collects and repaints every match in the page, and the queue never
// coalesces, so typing a word would run one whole search per character and only
// the last would matter. A changed query searches even for the previous-match
// request, because a new search lands on the first match and there is nothing
// before it to step back to. An empty field with nothing searched dispatches
// nothing; emptying a searched field and pressing Enter dispatches the empty
// query, which clears the highlights.
func (s *streamSession) submitFind(prev bool) {
	query := s.editor.String()
	switch {
	case query != s.findQuery:
		s.findQuery = query
		s.forwardFind(query)
	case query == "":
	case prev:
		s.forwardFindPrev()
	default:
		s.forwardFindNext()
	}
}

// handleFindKey routes one key to find mode. The mode owns the keyboard the way
// the URL editor does: the shared field path takes the text and edit keys, so
// nothing typed reaches CDP and a paste lands in the query rather than the page.
// Enter searches or steps to the next match and Shift+Enter steps to the
// previous one, read from the CDP modifier bits a kitty key report carried. A
// legacy terminal sends no signal for that chord, so Down and Up step next and
// previous on every terminal, which keeps the previous-match step reachable
// without the kitty keyboard protocol; the horizontal keys stay on the caret and
// the vertical keys on the matches. Esc clears the search and closes the mode,
// so it never reaches the page. Everything else is swallowed, and only the quit
// keys keep their meaning, so a wedged session can never trap the user.
func (s *streamSession) handleFindKey(ev keyEvent) (quit bool) {
	if !editorKey(s.editor, ev) {
		switch {
		case ev.b == keyQuitCtrlQ || ev.b == keyQuitCtrlC:
			return true
		case ev.b == keyEsc:
			s.leaveFind()
			return false
		case ev.b == keyEnter:
			s.submitFind(ev.mod&cdpModShift != 0)
		case ev.scroll == ScrollDown:
			s.submitFind(false)
		case ev.scroll == ScrollUp:
			s.submitFind(true)
		}
	}
	s.repaintBar()
	return false
}

// handleFindMouse dispatches one find-mode press: the ✕ closes the mode, a press
// in the query field places the caret at the clicked cell against the field span
// the last repaint measured, and a press on the count does nothing. A press off
// the bar row closes the mode, like every other bar mode, so one click dismisses
// find and the next reaches the page. Nothing here forwards to CDP.
func (s *streamSession) handleFindMouse(ev MouseEvent, onBar bool) {
	if ev.Action != MouseActionPress {
		return
	}
	if !onBar {
		s.leaveFind()
		return
	}
	item, ok := spanAt(s.modeSpans, ev.Col+1)
	if !ok {
		return
	}
	if item == barSpanClose {
		s.leaveFind()
		return
	}
	s.editor.placeCursor(ev.Col-s.editPromptW, s.editFieldW)
	s.repaintBar()
}

// forwardFind forwards one page search to the engine off the input loop through
// the same serialised worker as keys, so a search never overtakes an earlier
// keystroke and a slow search never freezes quit or draw.
func (s *streamSession) forwardFind(query string) {
	if s.keySender != nil {
		s.keySender.sendFind(query)
		return
	}
	s.eng.Find(query)
}

// forwardFindNext forwards one step to the next match through the same worker.
func (s *streamSession) forwardFindNext() {
	if s.keySender != nil {
		s.keySender.sendFindNext()
		return
	}
	s.eng.FindNext()
}

// forwardFindPrev forwards one step to the previous match through the same
// worker.
func (s *streamSession) forwardFindPrev() {
	if s.keySender != nil {
		s.keySender.sendFindPrev()
		return
	}
	s.eng.FindPrev()
}

// forwardFindClear forwards one search clear through the same worker, so it
// never overtakes a search still waiting to dispatch.
func (s *streamSession) forwardFindClear() {
	if s.keySender != nil {
		s.keySender.sendFindClear()
		return
	}
	s.eng.FindClear()
}
