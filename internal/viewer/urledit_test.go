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

// TestCommitURL pins the schemeless commit rule: a scheme passes through,
// a dotted schemeless host gains https://, and dotless or empty text is
// refused so the editor stays open.
func TestCommitURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"scheme passes", "https://example.org/", "https://example.org/", true},
		{"file scheme passes", "file:///tmp/page.html", "file:///tmp/page.html", true},
		{"dotted host gains https", "example.org/page", "https://example.org/page", true},
		{"surrounding space trimmed", "  example.org  ", "https://example.org", true},
		{"dotless refused", "example", "", false},
		{"empty refused", "", "", false},
		{"spaces refused", "   ", "", false},
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
