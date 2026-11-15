package viewer

import "testing"

// TestURLEditorPrefill pins the entry state: the editor holds the current URL
// with the cursor at the end.
func TestURLEditorPrefill(t *testing.T) {
	e := newURLEditor("https://a.io/")
	if e.String() != "https://a.io/" || e.cursor != len([]rune("https://a.io/")) {
		t.Fatalf("prefill = %q cursor %d, want the text with the cursor at the end", e.String(), e.cursor)
	}
}

// TestURLEditorOperations pins the readline subset: insert, backspace,
// forward delete, start/end moves, kill to end, clear, and delete word.
func TestURLEditorOperations(t *testing.T) {
	cases := []struct {
		name string
		edit func(e *urlEditor)
		want string
	}{
		{"insert at end", func(e *urlEditor) { e.insertRune('!') }, "one two!"},
		{"insert mid", func(e *urlEditor) { e.moveLeft(); e.insertRune('X') }, "one twXo"},
		{"backspace", func(e *urlEditor) { e.backspace() }, "one tw"},
		{"backspace at start is inert", func(e *urlEditor) { e.moveHome(); e.backspace() }, "one two"},
		{"delete under cursor", func(e *urlEditor) { e.moveHome(); e.deleteForward() }, "ne two"},
		{"delete at end is inert", func(e *urlEditor) { e.deleteForward() }, "one two"},
		{"kill to end", func(e *urlEditor) { e.moveHome(); e.moveRight(); e.killToEnd() }, "o"},
		{"clear", func(e *urlEditor) { e.clearAll() }, ""},
		{"delete word", func(e *urlEditor) { e.deleteWord() }, "one "},
		{"delete word skips trailing spaces", func(e *urlEditor) { e.insertText("  "); e.deleteWord() }, "one "},
		{"delete word at start is inert", func(e *urlEditor) { e.moveHome(); e.deleteWord() }, "one two"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newURLEditor("one two")
			c.edit(e)
			if got := e.String(); got != c.want {
				t.Fatalf("text = %q, want %q", got, c.want)
			}
		})
	}
}

// TestURLEditorCursorMoves pins the cursor motion bounds: left and right stop
// at the ends, and home/end jump to them.
func TestURLEditorCursorMoves(t *testing.T) {
	e := newURLEditor("ab")
	e.moveRight()
	if e.cursor != 2 {
		t.Fatalf("right at end moved the cursor to %d", e.cursor)
	}
	e.moveHome()
	e.moveLeft()
	if e.cursor != 0 {
		t.Fatalf("left at start moved the cursor to %d", e.cursor)
	}
	e.moveEnd()
	if e.cursor != 2 {
		t.Fatalf("end moved the cursor to %d, want 2", e.cursor)
	}
}

// TestURLEditorInsertTextStripsNewlines pins the paste rule: a paste inserts
// as one unit with newlines, carriage returns, and other control characters
// stripped, so a multi-line paste cannot smuggle an Enter.
func TestURLEditorInsertTextStripsNewlines(t *testing.T) {
	e := newURLEditor("")
	e.insertText("one\ntwo\rthree\x1b[Z")
	if got := e.String(); got != "onetwothree[Z" {
		t.Fatalf("pasted text = %q, want control characters stripped", got)
	}
}

// TestURLEditorInsertRuneDropsControls pins the single insertion choke point:
// C0, DEL, and C1 runes are dropped whether typed or pasted, so an xterm-family
// terminal never receives a C1 escape introducer from the repainted row.
func TestURLEditorInsertRuneDropsControls(t *testing.T) {
	e := newURLEditor("")
	for _, r := range []rune{0x07, 0x7f, 0x80, 0x9b, 0x9f} {
		e.insertRune(r)
	}
	e.insertRune('a')
	e.insertText("b\u009bc")
	if got := e.String(); got != "abc" {
		t.Fatalf("editor text = %q, want control runes dropped", got)
	}
}

// TestURLEditorViewFollowsCursor pins the horizontal viewport: the window
// slides right so the cursor stays visible on a long URL, slides back left
// when the cursor returns to the start, and reports the cursor cell inside
// the window.
func TestURLEditorViewFollowsCursor(t *testing.T) {
	e := newURLEditor("0123456789")

	// Cursor at the end of ten runes in a five-cell window: the window shows
	// the tail with one cell reserved for the cursor.
	visible, cell := e.view(5)
	if visible != "6789" || cell != 4 {
		t.Fatalf("view(5) at end = (%q, %d), want (%q, 4)", visible, cell, "6789")
	}

	e.moveHome()
	visible, cell = e.view(5)
	if visible != "01234" || cell != 0 {
		t.Fatalf("view(5) at start = (%q, %d), want (%q, 0)", visible, cell, "01234")
	}

	// A short text fits whole.
	s := newURLEditor("ab")
	visible, cell = s.view(5)
	if visible != "ab" || cell != 2 {
		t.Fatalf("short view = (%q, %d), want (%q, 2)", visible, cell, "ab")
	}

	if visible, cell = s.view(0); visible != "" || cell != 0 {
		t.Fatalf("zero-width view = (%q, %d), want empty", visible, cell)
	}
}

// TestURLEditorViewWideRunes pins cell-measured windowing: a double-width
// rune counts two cells in both the visible cut and the cursor column.
func TestURLEditorViewWideRunes(t *testing.T) {
	e := newURLEditor("字字字")
	visible, cell := e.view(4)
	if visible != "字" || cell != 2 {
		t.Fatalf("wide view = (%q, %d), want one wide rune and the cursor at cell 2", visible, cell)
	}
}

// TestURLEditorPlaceCursor pins the click-to-caret mapping on a narrow-glyph
// URL: the first cell lands at the start, an interior cell lands on that rune,
// the cell after the last rune and every cell past it land at the end, and a
// negative cell (a click on the ❯ prompt) clamps to the start rather than
// moving nothing.
func TestURLEditorPlaceCursor(t *testing.T) {
	cases := []struct {
		name string
		cell int
		want int
	}{
		{"prompt clamps to start", -2, 0},
		{"first cell", 0, 0},
		{"middle cell", 3, 3},
		{"last rune", 6, 6},
		{"cell after the text", 7, 7},
		{"beyond the text", 40, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newURLEditor("a.io/pq") // seven narrow runes
			e.placeCursor(c.cell, 40)
			if e.cursor != c.want {
				t.Fatalf("placeCursor(%d) cursor = %d, want %d", c.cell, e.cursor, c.want)
			}
			if e.cursor > len(e.text) {
				t.Fatalf("cursor %d ran past the text length %d", e.cursor, len(e.text))
			}
		})
	}
}

// TestURLEditorPlaceCursorWideGraphemes pins the wide-glyph rule: both cells of
// a double-width rune place the cursor before that rune, because the trailing
// cell carries background only and is not a caret position of its own. The
// cursor stays a rune index throughout, so no mapping can land inside a
// character.
func TestURLEditorPlaceCursorWideGraphemes(t *testing.T) {
	const text = "日本語" // three runes, two cells each, six cells total
	cases := []struct {
		name string
		cell int
		want int
	}{
		{"leading cell of the first rune", 0, 0},
		{"trailing cell of the first rune", 1, 0},
		{"leading cell of the second rune", 2, 1},
		{"trailing cell of the second rune", 3, 1},
		{"leading cell of the third rune", 4, 2},
		{"trailing cell of the third rune", 5, 2},
		{"cell after the last rune", 6, 3},
		{"beyond the text", 20, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newURLEditor(text)
			e.placeCursor(c.cell, 20)
			if e.cursor != c.want {
				t.Fatalf("placeCursor(%d) cursor = %d, want %d", c.cell, e.cursor, c.want)
			}
			// The cursor is a rune index, so slicing at it always yields whole
			// characters: the text either side must still be valid UTF-8 and
			// recombine to the original.
			left, right := string(e.text[:e.cursor]), string(e.text[e.cursor:])
			if left+right != text {
				t.Fatalf("cursor split the text into %q + %q, want %q", left, right, text)
			}
		})
	}
}

// TestURLEditorPlaceCursorScrolledViewport pins the mapping against a scrolled
// editor: cells map from the leftmost visible rune, not from the start of the
// text, and the viewport does not move under the click because the placed cursor
// is already inside the window.
func TestURLEditorPlaceCursorScrolledViewport(t *testing.T) {
	e := newURLEditor("0123456789")
	// The window shows the tail "6789" with the cursor at the end.
	if visible, _ := e.view(5); visible != "6789" {
		t.Fatalf("view(5) = %q, want the scrolled tail", visible)
	}
	offset := e.offset

	e.placeCursor(0, 5)
	if e.cursor != offset {
		t.Fatalf("first visible cell = cursor %d, want the viewport offset %d", e.cursor, offset)
	}
	e.placeCursor(2, 5)
	if want := offset + 2; e.cursor != want {
		t.Fatalf("third visible cell = cursor %d, want %d", e.cursor, want)
	}
	if e.offset != offset {
		t.Fatalf("the viewport moved to %d on a click inside it, want %d", e.offset, offset)
	}
	// The view still shows the same window and reports the clicked cell, so the
	// caret the user sees is where they clicked.
	visible, cell := e.view(5)
	if visible != "6789" || cell != 2 {
		t.Fatalf("view after the click = (%q, %d), want (%q, 2)", visible, cell, "6789")
	}
}

// TestURLEditorPlaceCursorClipsAtWindowEdge pins the right-edge clip: a wide
// rune that view drops because it cannot fit the last cell is not clickable
// there, so a click on that blank cell lands on the boundary before it and never
// inside the dropped rune.
func TestURLEditorPlaceCursorClipsAtWindowEdge(t *testing.T) {
	e := newURLEditor("a字")
	e.moveHome()
	// A three-cell window fits "a" and then has one cell left, too narrow for
	// the double-width rune, so view drops it.
	if visible, _ := e.view(2); visible != "a" {
		t.Fatalf("view(2) = %q, want the wide rune clipped", visible)
	}
	e.placeCursor(1, 2)
	if e.cursor != 1 {
		t.Fatalf("click on the clipped cell = cursor %d, want 1 (before the dropped rune)", e.cursor)
	}
}

// TestCommitURL pins the commit rule: an allowlisted scheme (http, https,
// file) passes through, any other scheme is refused (javascript: would
// execute in the committed page's origin, a self-XSS), a dotted schemeless
// host gains https://, and dotless or empty text is refused so the editor
// stays open.
func TestCommitURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"scheme passes", "https://example.org/", "https://example.org/", true},
		{"http scheme passes", "http://example.org/", "http://example.org/", true},
		{"scheme case-insensitive", "HTTPS://example.org/", "HTTPS://example.org/", true},
		{"file scheme passes", "file:///tmp/page.html", "file:///tmp/page.html", true},
		{"dotted host gains https", "example.org/page", "https://example.org/page", true},
		{"surrounding space trimmed", "  example.org  ", "https://example.org", true},
		{"dotless refused", "example", "", false},
		{"empty refused", "", "", false},
		{"spaces refused", "   ", "", false},
		{"javascript authority form refused", "javascript://x%0aalert(1)", "", false},
		{"javascript opaque form refused", "javascript:document.cookie", "", false},
		{"javascript case refused", "JavaScript:alert(1)", "", false},
		{"data scheme refused", "data:text/html,<script>1</script>", "", false},
		{"vbscript scheme refused", "vbscript:msgbox(1.0)", "", false},
		{"chrome scheme refused", "chrome://settings", "", false},
		{"embedded https does not launder", "javascript:fetch('https://evil/')", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := commitURL(c.in)
			if ok != c.ok || got != c.want {
				t.Fatalf("commitURL(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}
