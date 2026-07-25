package viewer

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// urlEditor is the bar's hand-rolled single-line URL editor: a rune slice, a
// rune-index cursor, and a horizontal viewport offset that follows the cursor
// so a URL wider than the bar stays editable. It is pure: the session feeds
// it decoded keys and renders its view, so every operation is testable
// without a terminal.
type urlEditor struct {
	text   []rune
	cursor int // rune index of the insertion point, 0..len(text)
	offset int // rune index of the leftmost visible rune
}

// newURLEditor returns an editor prefilled with text and the cursor at the
// end, the URL edit mode entry state.
func newURLEditor(text string) *urlEditor {
	r := []rune(text)
	return &urlEditor{text: r, cursor: len(r)}
}

// String returns the edited text.
func (e *urlEditor) String() string {
	return string(e.text)
}

// insertRune inserts r at the cursor and advances past it. C0 and C1 control
// characters and DEL are dropped here, the single insertion choke point, so
// neither a typed rune nor a paste can smuggle a control byte the repaint
// would hand to the terminal.
func (e *urlEditor) insertRune(r rune) {
	if isControlRune(r) {
		return
	}
	e.text = append(e.text[:e.cursor], append([]rune{r}, e.text[e.cursor:]...)...)
	e.cursor++
}

// insertText inserts a paste at the cursor as one unit. Newlines and carriage
// returns are stripped, so a multi-line paste cannot smuggle an Enter, and
// other control characters are dropped by insertRune.
func (e *urlEditor) insertText(s string) {
	for _, r := range s {
		e.insertRune(r)
	}
}

// isControlRune reports whether r is a C0 control, DEL, or a C1 control, the
// characters the editor and the bar's URL display refuse: an xterm-family
// terminal treats a C1 byte as an escape introducer, so none may reach a bar
// write.
func isControlRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// backspace deletes the rune before the cursor.
func (e *urlEditor) backspace() {
	if e.cursor == 0 {
		return
	}
	e.text = append(e.text[:e.cursor-1], e.text[e.cursor:]...)
	e.cursor--
}

// deleteForward deletes the rune under the cursor (the Delete key).
func (e *urlEditor) deleteForward() {
	if e.cursor >= len(e.text) {
		return
	}
	e.text = append(e.text[:e.cursor], e.text[e.cursor+1:]...)
}

// moveLeft steps the cursor one rune left.
func (e *urlEditor) moveLeft() {
	if e.cursor > 0 {
		e.cursor--
	}
}

// moveRight steps the cursor one rune right.
func (e *urlEditor) moveRight() {
	if e.cursor < len(e.text) {
		e.cursor++
	}
}

// moveHome moves the cursor to the start (Ctrl+A, Home).
func (e *urlEditor) moveHome() {
	e.cursor = 0
}

// moveEnd moves the cursor to the end (Ctrl+E, End).
func (e *urlEditor) moveEnd() {
	e.cursor = len(e.text)
}

// killToEnd deletes from the cursor to the end (Ctrl+K).
func (e *urlEditor) killToEnd() {
	e.text = e.text[:e.cursor]
}

// clearAll empties the editor (Ctrl+U).
func (e *urlEditor) clearAll() {
	e.text = e.text[:0]
	e.cursor = 0
}

// deleteWord deletes the word before the cursor (Ctrl+W): trailing spaces
// first, then the non-space run, the readline unix-word-rubout rule.
func (e *urlEditor) deleteWord() {
	i := e.cursor
	for i > 0 && e.text[i-1] == ' ' {
		i--
	}
	for i > 0 && e.text[i-1] != ' ' {
		i--
	}
	e.text = append(e.text[:i], e.text[e.cursor:]...)
	e.cursor = i
}

// view renders the editor into a width-cell window: it slides the viewport
// offset so the cursor stays visible (reserving one cell for a cursor at the
// end of the text), then returns the visible text, cell-measured and cut to
// the window, and the cursor's 0-based cell column inside it. A non-positive
// width returns an empty view.
func (e *urlEditor) view(width int) (visible string, cursorCell int) {
	if width < 1 {
		return "", 0
	}
	if e.offset > e.cursor {
		e.offset = e.cursor
	}
	if e.offset < 0 {
		e.offset = 0
	}
	// Slide the window right until the cells from the offset up to the
	// cursor's insertion point fit inside width-1, leaving the cursor cell
	// itself visible.
	for e.offset < e.cursor && cellSpan(e.text[e.offset:e.cursor]) > width-1 {
		e.offset++
	}
	var b strings.Builder
	used := 0
	for _, r := range e.text[e.offset:] {
		w := runewidth.RuneWidth(r)
		if used+w > width {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String(), cellSpan(e.text[e.offset:e.cursor])
}

// placeCursor moves the cursor to the rune boundary under a click at the
// 0-based display cell inside the width-cell text window, walking the visible
// runes by their display width so an East-Asian wide or ambiguous-width glyph
// counts the cells it really occupies. The walk starts at the viewport offset,
// so a scrolled editor maps the cells the user can see, and it stops at the same
// right-edge clip view applies, so a glyph too wide for the last cell is not
// clickable there.
//
// Three edges are pinned. A cell left of the text, which is a click on the ❯
// prompt, clamps to the leftmost visible rune: the prompt is decoration, so the
// nearest text position is the predictable answer. A cell past the last visible
// rune clamps to the boundary after it, which is the end of the text whenever
// the text end is on screen, so clicking the empty run after a URL parks the
// cursor at the end. Every cell of a wide grapheme, its trailing cell included,
// places the cursor before that grapheme: the trailing cell carries background
// only and is not a position of its own, so treating it as one would put the
// cursor somewhere the user cannot see a boundary. Landing on the leading
// boundary keeps one rule for narrow and wide glyphs alike and can never split
// a rune, because the cursor is a rune index throughout.
func (e *urlEditor) placeCursor(cell, width int) {
	if cell < 0 {
		cell = 0
	}
	used := 0
	i := e.offset
	for ; i < len(e.text); i++ {
		w := runewidth.RuneWidth(e.text[i])
		if used+w > width {
			break
		}
		if cell < used+w {
			e.cursor = i
			return
		}
		used += w
	}
	e.cursor = i
}

// cellSpan measures a rune slice in terminal cells.
func cellSpan(rs []rune) int {
	w := 0
	for _, r := range rs {
		w += runewidth.RuneWidth(r)
	}
	return w
}

// commitURL applies the schemeless commit rule to the edited text: an
// allowlisted scheme (http, https, or file, the launch-URL policy) passes
// through unchanged, any other scheme is refused, schemeless text containing
// a dot gains https://, and schemeless text without a dot (or an empty line)
// is refused so the editor stays open rather than navigating somewhere
// surprising. The scheme allowlist is a security boundary: page.Navigate
// executes a committed javascript: URL in the current page's origin (self-XSS
// on an authenticated page), so javascript:, data:, and every other scheme
// never reach it. A search fallback, if it ever arrives, is a launch flag,
// keeping Nyala stateless.
func commitURL(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	if scheme, _, ok := strings.Cut(t, "://"); ok {
		switch strings.ToLower(scheme) {
		case "http", "https", "file":
			return t, true
		}
		return "", false
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "data:") {
		return "", false
	}
	if !strings.Contains(t, ".") {
		return "", false
	}
	return "https://" + t, true
}
