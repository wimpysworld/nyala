package viewer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/wimpysworld/nyala/internal/render"
)

// aboutFieldSep is the three-cell separator the About row writes between two
// fields that are not glued.
const aboutFieldSep = " " + barSeparator + " "

// exampleAboutSnapshot is the source-of-truth run the About row is specified
// against: a Kitty terminal on the shm transport, Sixel off, sextant on,
// truecolour, and a WebGL 2 context from an Intel vendor.
func exampleAboutSnapshot() capabilitySnapshot {
	return capabilitySnapshot{
		Version:        "dev",
		Browser:        "Chrome 150",
		Terminal:       "Kitty 0.4.0",
		Kitty:          capOn,
		Sixel:          capOff,
		Sextant:        capOn,
		KittyTransport: KittyTransportSHM,
		Colour:         capabilityColour{Mode: ColourTrue},
		WebGL:          render.WebGLState{Tier: render.WebGL2, Label: "Intel"},
	}
}

// exampleAboutRow is the row exampleAboutSnapshot renders at its own width,
// built from the bar constants so a glyph change fails here, not silently.
func exampleAboutRow() string {
	fields := []string{
		"Nyala dev",
		"Chrome 150",
		"Kitty 0.4.0",
		"✓ Kitty (shm)",
		"✗ Sixel",
		"✓ Sextant",
		"Colour: True Colour",
		"WebGL ➁" + "  (Intel)",
	}
	return " " + strings.Join(fields, aboutFieldSep) + aboutFieldSep + barGlyphClose + " "
}

// aboutRowBytes enters About on a synchronous session and returns the bytes
// the repaint wrote to the tty.
func aboutRowBytes(t *testing.T, s *streamSession) string {
	t.Helper()
	s.enterAbout()
	if s.barMode != barModeAbout {
		t.Fatalf("enterAbout left mode %d", s.barMode)
	}
	return s.tty.(interface{ String() string }).String()
}

// webGLTestEngine wraps the stream fake with the optional webGLReporter
// capability, so a test can hand the About row a probe result.
type webGLTestEngine struct {
	streamTestEngine
	webgl render.WebGLState
}

func (e webGLTestEngine) WebGL() render.WebGLState { return e.webgl }

// TestRenderBarAboutExampleRow pins the specified row: at exactly its own
// width every field renders in order with no ellipsis, the close span covers
// the last three cells, and the colour pass is handed the three state glyphs
// at their cells.
func TestRenderBarAboutExampleRow(t *testing.T) {
	want := exampleAboutRow()
	cols := runewidth.StringWidth(want)
	if cols != 121 {
		t.Fatalf("example row is %d cells, want 121", cols)
	}

	content, spans, glyphs := renderBarAbout(cols, aboutFields(exampleAboutSnapshot()))
	if content != want {
		t.Fatalf("about row =\n%q\nwant\n%q", content, want)
	}
	if strings.Contains(content, barEllipsis) {
		t.Fatalf("a row that fits carries an ellipsis: %q", content)
	}
	if len(spans) != 1 || spans[0] != (barSpan{item: barSpanClose, col: cols - 2, width: 3}) {
		t.Fatalf("about spans = %+v, want the close over the last three cells", spans)
	}

	// The glyph cells must land on ✓, ✗, and ✓ in the plain row, so the colour
	// pass paints the state glyphs and nothing else.
	if len(glyphs) != 3 {
		t.Fatalf("glyph cells = %+v, want three", glyphs)
	}
	wantGlyphs := []string{aboutGlyphOn, aboutGlyphOff, aboutGlyphOn}
	for i, g := range glyphs {
		if got := runeAtCell(content, g.col); got != wantGlyphs[i] {
			t.Fatalf("glyph %d at cell %d = %q, want %q", i, g.col, got, wantGlyphs[i])
		}
		if g.width != 1 {
			t.Fatalf("glyph %d width = %d, want 1", i, g.width)
		}
	}
}

// runeAtCell returns the rune that starts at the 1-based cell of a plain row,
// or "" when no rune starts there.
func runeAtCell(content string, cell int) string {
	col := 1
	for _, r := range content {
		if col == cell {
			return string(r)
		}
		col += runewidth.RuneWidth(r)
	}
	return ""
}

// TestRenderBarAboutWidthSweep pins the row geometry at every width from 1
// to 200: the row is one line exactly cols cells wide with the full field set
// and with no fields, and an empty field set never earns an ellipsis because
// nothing was dropped.
func TestRenderBarAboutWidthSweep(t *testing.T) {
	full := aboutFields(exampleAboutSnapshot())
	for cols := 1; cols <= 200; cols++ {
		for _, tc := range []struct {
			name   string
			fields []barField
		}{
			{"full", full},
			{"empty", nil},
		} {
			content, _, _ := renderBarAbout(cols, tc.fields)
			if w := runewidth.StringWidth(content); w != cols {
				t.Fatalf("%s row at %d cols is %d cells: %q", tc.name, cols, w, content)
			}
			if strings.ContainsAny(content, "\r\n\x1b") {
				t.Fatalf("%s row at %d cols spans more than one row: %q", tc.name, cols, content)
			}
			if tc.fields == nil && strings.Contains(content, barEllipsis) {
				t.Fatalf("an empty field set at %d cols earned an ellipsis: %q", cols, content)
			}
		}
	}
	if content, spans, glyphs := renderBarAbout(0, full); content != "" || spans != nil || glyphs != nil {
		t.Fatalf("zero-width row = %q %+v %+v, want nothing", content, spans, glyphs)
	}
}

// TestRenderBarAboutTruncationOrder pins whole-field truncation: shrinking the
// width one cell at a time drops fields in priority order, vendor first and
// version last, never mid-field, with exactly one ellipsis once anything is
// gone and none before. The glued vendor never outlives its WebGL tier and
// never follows a separator. At seven cells the row is the ellipsis and the
// close alone, and below that the row still fills the width.
func TestRenderBarAboutTruncationOrder(t *testing.T) {
	fields := aboutFields(exampleAboutSnapshot())
	fullWidth := runewidth.StringWidth(exampleAboutRow())
	closeW := runewidth.StringWidth(aboutClose)

	// dropAt records the widest cols at which each field was absent.
	dropAt := make(map[int]int, len(fields))
	wasPresent := make([]bool, len(fields))
	for i := range wasPresent {
		wasPresent[i] = true
	}
	for cols := fullWidth; cols >= 7; cols-- {
		content, _, _ := renderBarAbout(cols, fields)
		if w := runewidth.StringWidth(content); w != cols {
			t.Fatalf("row at %d cols is %d cells: %q", cols, w, content)
		}

		// Every field is either whole or gone, and the kept set is a prefix
		// by priority: a field is present only when every lower priority is.
		kept := make([]bool, len(fields))
		for i, f := range fields {
			kept[i] = strings.Contains(content, f.text)
		}
		for i, f := range fields {
			if !kept[i] {
				continue
			}
			for j, g := range fields {
				if g.priority < f.priority && !kept[j] {
					t.Fatalf("at %d cols priority %d survived while %d dropped: %q", cols, f.priority, g.priority, content)
				}
			}
		}
		// A dropped field stays dropped as the width keeps shrinking.
		for i := range fields {
			if kept[i] && !wasPresent[i] {
				t.Fatalf("at %d cols field %q came back: %q", cols, fields[i].text, content)
			}
			if !kept[i] && wasPresent[i] {
				dropAt[fields[i].priority] = cols
			}
			wasPresent[i] = kept[i]
		}

		// Rebuild the row from the kept set: one ellipsis field after the last
		// kept field when anything dropped, then padding and the close. A
		// mid-field cut or a second ellipsis cannot match.
		var left strings.Builder
		left.WriteString(" ")
		first := true
		dropped := false
		for i, f := range fields {
			if !kept[i] {
				dropped = true
				continue
			}
			if !first && !f.glued {
				left.WriteString(aboutFieldSep)
			}
			first = false
			left.WriteString(f.text)
		}
		if dropped {
			if !first {
				left.WriteString(aboutFieldSep)
			}
			left.WriteString(barEllipsis)
		}
		pad := cols - runewidth.StringWidth(left.String()) - closeW
		if pad < 0 {
			t.Fatalf("at %d cols the kept fields overflow the row: %q", cols, content)
		}
		if want := left.String() + strings.Repeat(" ", pad) + aboutClose; content != want {
			t.Fatalf("row at %d cols =\n%q\nwant\n%q", cols, content, want)
		}
		if n := strings.Count(content, barEllipsis); (dropped && n != 1) || (!dropped && n != 0) {
			t.Fatalf("row at %d cols has %d ellipses with dropped=%t: %q", cols, n, dropped, content)
		}

		// The glued vendor keeps its tier in front of it, so it never opens a
		// segment after a separator.
		if strings.Contains(content, "(Intel)") && !strings.Contains(content, "WebGL ➁  (Intel)") {
			t.Fatalf("at %d cols the vendor lost its tier: %q", cols, content)
		}
		if strings.Contains(content, barSeparator+" "+"  (Intel)") {
			t.Fatalf("at %d cols the vendor follows a separator: %q", cols, content)
		}
	}

	// Vendor first, then priorities 9 down to 2, each no later than the next.
	order := []int{10, 9, 8, 7, 6, 5, 4, 3, 2}
	for _, p := range order {
		if _, ok := dropAt[p]; !ok {
			t.Fatalf("priority %d never dropped by 7 cols: %v", p, dropAt)
		}
	}
	for i := 1; i < len(order); i++ {
		if dropAt[order[i-1]] < dropAt[order[i]] {
			t.Fatalf("priority %d dropped at %d cols, after priority %d at %d", order[i-1], dropAt[order[i-1]], order[i], dropAt[order[i]])
		}
	}
	if dropAt[10] <= dropAt[9] {
		t.Fatalf("vendor dropped at %d cols, not before its tier at %d", dropAt[10], dropAt[9])
	}

	content, _, _ := renderBarAbout(7, fields)
	if want := " " + barEllipsis + aboutClose; content != want {
		t.Fatalf("row at 7 cols = %q, want %q", content, want)
	}
	for cols := 6; cols >= 1; cols-- {
		content, _, _ := renderBarAbout(cols, fields)
		if w := runewidth.StringWidth(content); w != cols {
			t.Fatalf("row at %d cols is %d cells: %q", cols, w, content)
		}
		if cols >= 3 && !strings.Contains(content, barGlyphClose) {
			t.Fatalf("row at %d cols dropped the close before the ellipsis: %q", cols, content)
		}
	}
}

// TestRenderBarAboutCloseSpan pins the close click target at every width: the
// span is the ✕ cell and its two neighbours at the row end whenever the ✕ is
// drawn, and a row too narrow to draw it reports no span at all.
func TestRenderBarAboutCloseSpan(t *testing.T) {
	fields := aboutFields(exampleAboutSnapshot())
	for cols := 1; cols <= 200; cols++ {
		content, spans, _ := renderBarAbout(cols, fields)
		visible := strings.Contains(content, barGlyphClose)
		if !visible {
			if len(spans) != 0 {
				t.Fatalf("at %d cols the ✕ is not drawn but spans = %+v", cols, spans)
			}
			continue
		}
		if len(spans) != 1 || spans[0] != (barSpan{item: barSpanClose, col: cols - 2, width: 3}) {
			t.Fatalf("at %d cols spans = %+v, want the close over the last three cells", cols, spans)
		}
		if got := runeAtCell(content, cols-1); got != barGlyphClose {
			t.Fatalf("at %d cols the ✕ is at %q, not the span's middle cell: %q", cols, got, content)
		}
	}
}

// TestHandleMouseAboutClose pins About-mode mouse dispatch: opening About
// from the menu lays out the close span, a click on it returns to chrome, a
// padding cell keeps About open, and no press reaches the page.
func TestHandleMouseAboutClose(t *testing.T) {
	var forwarded []MouseEvent
	s, st := modeSession(t, streamTestEngine{
		browserVersion: "Chromium 150.0.1",
		sendMouse:      func(ev MouseEvent) { forwarded = append(forwarded, ev) },
	})
	barRow := st.Rows - 1

	s.handleKey(keyEvent{act: actionOpenMenu})
	s.menuSel = menuIndex(t, s.fpsBadgeShown(), "About")
	s.handleKey(keyEvent{b: keyEnter})
	if s.barMode != barModeAbout {
		t.Fatalf("About did not open from the menu: mode %d", s.barMode)
	}
	closeSpan, ok := menuSpan(s.modeSpans, barSpanClose)
	if !ok {
		t.Fatal("About laid out no close span")
	}
	if closeSpan.col+closeSpan.width-1 != st.Cols {
		t.Fatalf("close span %+v does not end at the corner column %d", closeSpan, st.Cols)
	}

	// A padding cell left of the close does nothing.
	pad := MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: closeSpan.col - 4, Row: barRow}
	if s.handleMouse(pad); s.barMode != barModeAbout {
		t.Fatalf("padding click changed the mode: %d", s.barMode)
	}

	for _, col := range []int{closeSpan.col - 1, closeSpan.col, closeSpan.col + 1} {
		s.enterAbout()
		if quit := s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: col, Row: barRow}); quit {
			t.Fatalf("close click at column %d quit", col)
		}
		if s.barMode != barModeChrome {
			t.Fatalf("close click at column %d did not leave About: mode %d", col, s.barMode)
		}
	}
	if len(forwarded) != 0 {
		t.Fatalf("an About press reached the page: %+v", forwarded)
	}
}

// TestAboutModeOwnsPageInput pins that About swallows page input: printable
// bytes, runes, pastes, scroll keys, and Delete reach nothing while About is
// open. Esc closes it, a page-row press closes it without forwarding, and page
// input flows again once it is closed.
func TestAboutModeOwnsPageInput(t *testing.T) {
	var sent, text, backends []string
	var forwarded []MouseEvent
	s, _ := modeSession(t, streamTestEngine{
		sendKey:    func(code string) { sent = append(sent, code) },
		sendText:   func(t string) { text = append(text, t) },
		setBackend: func(name string) { backends = append(backends, name) },
		sendMouse:  func(ev MouseEvent) { forwarded = append(forwarded, ev) },
	})
	s.enterAbout()
	for _, ev := range []keyEvent{
		{b: 'a'},
		{r: 'é'},
		{paste: "https://example.org/"},
		{scroll: ScrollUp},
		{scroll: ScrollHome},
		{act: actionDelete},
		{b: keyTab},
		{b: keyEnter},
	} {
		if quit := s.handleKey(ev); quit {
			t.Fatalf("key %+v quit About", ev)
		}
		if s.barMode != barModeAbout {
			t.Fatalf("key %+v left About: mode %d", ev, s.barMode)
		}
	}
	if len(sent) != 0 || len(text) != 0 || len(backends) != 0 {
		t.Fatalf("About leaked input: keys %v, text %q, backends %v", sent, text, backends)
	}

	s.handleKey(keyEvent{b: keyEsc})
	if s.barMode != barModeChrome {
		t.Fatalf("Esc did not close About: mode %d", s.barMode)
	}
	s.handleKey(keyEvent{b: keyEsc})
	if len(sent) != 1 || sent[0] != "Escape" {
		t.Fatalf("chrome Esc = %v, want [Escape] once About closed", sent)
	}

	// A page-row press closes About without reaching the page; the next one
	// is ordinary page input again.
	s.enterAbout()
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 10, Row: 5})
	if s.barMode != barModeChrome || len(forwarded) != 0 {
		t.Fatalf("page click during About: mode %d, forwarded %+v", s.barMode, forwarded)
	}
	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 10, Row: 5})
	if len(forwarded) != 1 || forwarded[0].Row != 5 {
		t.Fatalf("page press after About = %+v, want one forwarded press", forwarded)
	}
}

// TestAboutColourCycleRefreshesOpenRow pins the live About label to the Alt+C
// cycle. Each palette capability uses the same three modes and its own compact
// labels, while About stays open for every repaint.
func TestAboutColourCycleRefreshesOpenRow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		adaptive   bool
		wantLabels []string
	}{
		{"static", false, []string{"Colour: 240 Static", "Colour: 24 Shades", "Colour: True Colour"}},
		{"adaptive", true, []string{"Colour: 240 Adaptive", "Colour: 240 Shades", "Colour: True Colour"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := modeSession(t, streamTestEngine{})
			st.SetEnabled([]Backend{BackendBlock})
			st.staticPaletteSelected = !tc.adaptive
			done := make(chan struct{})
			defer close(done)
			s.ctl = newControl(s.eng, done)
			s.enterAbout()

			out := s.tty.(interface {
				Reset()
				String() string
			})
			for i, want := range tc.wantLabels {
				out.Reset()
				s.handleKey(keyEvent{act: actionCycleColour})
				if s.barMode != barModeAbout {
					t.Fatalf("cycle %d left About: mode %d", i, s.barMode)
				}
				if row := out.String(); !strings.Contains(row, want) {
					t.Fatalf("cycle %d About row %q missing %q", i, row, want)
				}
			}
		})
	}
}

// TestAboutPaletteToggleRefreshesOpenRow checks both palette-policy directions
// in the RGB and grey modes. About stays open and shows each new label in the
// same repaint that clears the old view.
func TestAboutPaletteToggleRefreshesOpenRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     ColourMode
		adaptive string
		static   string
	}{
		{"RGB", ColourXTerm256, "Colour: 240 Adaptive", "Colour: 240 Static"},
		{"grey", ColourXTermGrey, "Colour: 240 Shades", "Colour: 24 Shades"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := modeSession(t, streamTestEngine{})
			st.SetEnabled([]Backend{BackendBlock})
			st.Backend = BackendBlock
			st.Colour = tc.mode
			st.paletteQueryOK = true
			done := make(chan struct{})
			defer close(done)
			s.ctl = newControl(s.eng, done)
			s.enterAbout()

			out := s.tty.(interface {
				Reset()
				String() string
			})
			if row := out.String(); !strings.Contains(row, tc.adaptive) {
				t.Fatalf("initial About row %q missing %q", row, tc.adaptive)
			}

			out.Reset()
			s.handleKey(keyEvent{act: actionTogglePalette})
			if s.barMode != barModeAbout || !st.staticPaletteSelected {
				t.Fatalf("static toggle changed About mode %d or kept adaptive", s.barMode)
			}
			if row := out.String(); !strings.Contains(row, tc.static) {
				t.Fatalf("static About row %q missing %q", row, tc.static)
			}

			out.Reset()
			s.handleKey(keyEvent{act: actionTogglePalette})
			if s.barMode != barModeAbout || st.staticPaletteSelected {
				t.Fatalf("adaptive toggle changed About mode %d or kept static", s.barMode)
			}
			if row := out.String(); !strings.Contains(row, tc.adaptive) {
				t.Fatalf("adaptive About row %q missing %q", row, tc.adaptive)
			}
		})
	}
}

// TestAboutPaletteToggleNoopKeepsOpenRow checks each unsupported context.
// Alt+P leaves About and its current row unchanged when State rejects the toggle.
func TestAboutPaletteToggleNoopKeepsOpenRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*State)
	}{
		{"without OSC 4", func(st *State) { st.paletteQueryOK = false }},
		{"outside Block", func(st *State) { st.Backend = BackendSixel }},
		{"in truecolour", func(st *State) { st.Colour = ColourTrue }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := modeSession(t, streamTestEngine{})
			st.Backend = BackendBlock
			st.Colour = ColourXTerm256
			st.paletteQueryOK = true
			tc.prepare(st)
			s.enterAbout()
			out := s.tty.(*bytes.Buffer)
			out.Reset()

			s.handleKey(keyEvent{act: actionTogglePalette})

			if s.barMode != barModeAbout || st.staticPaletteSelected || out.Len() != 0 {
				t.Fatalf("unsupported Alt+P changed About: mode %d, static %t, output %q",
					s.barMode, st.staticPaletteSelected, out.String())
			}
		})
	}
}

// TestAboutBarGlyphColourByMode pins the colour hint against the colour
// ladder: the greyscale mode writes no green or red SGR and the ✓ and ✗ still
// reach the row, and every other mode wraps each glyph in its colour and then
// restores the bar style.
func TestAboutBarGlyphColourByMode(t *testing.T) {
	const green, red = "\x1b[32m", "\x1b[31m"
	for _, tc := range []struct {
		name   string
		mode   ColourMode
		colour bool
	}{
		{"greyscale", ColourXTermGrey, false},
		{"truecolour", ColourTrue, true},
		{"xterm256", ColourXTerm256, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := modeSession(t, streamTestEngine{browserVersion: "Chrome 150.0.1"})
			st.Colour = tc.mode
			st.SetEnabled([]Backend{BackendKitty, BackendBlock})
			st.sextantOK = true
			out := aboutRowBytes(t, s)

			if !strings.Contains(out, aboutGlyphOn) || !strings.Contains(out, aboutGlyphOff) {
				t.Fatalf("%s About row lost a state glyph: %q", tc.name, out)
			}
			restore := "\x1b[0m" + st.barStyle()
			hasGreen := strings.Contains(out, green+aboutGlyphOn+restore)
			hasRed := strings.Contains(out, red+aboutGlyphOff+restore)
			if tc.colour && (!hasGreen || !hasRed) {
				t.Fatalf("%s About row did not colour and restore both glyphs: %q", tc.name, out)
			}
			if !tc.colour && (strings.Contains(out, green) || strings.Contains(out, red)) {
				t.Fatalf("%s About row wrote a colour SGR: %q", tc.name, out)
			}
			// The bar style is restored after each coloured glyph, so a bare
			// SGR 39 never leaves the bar on the terminal default foreground.
			if strings.Contains(out, "\x1b[39m") {
				t.Fatalf("%s About row reset the foreground to default: %q", tc.name, out)
			}
		})
	}
}

// TestColourGlyphs pins the colour pass on plain text: each ✓ cell gains
// green, each ✗ cell red, both followed by the restore, cells are counted by
// display width so a wide rune before a glyph does not shift it, and an empty
// glyph list returns the content untouched.
func TestColourGlyphs(t *testing.T) {
	const restore = "\x1b[0m" + barStyleReverse
	content := "漢" + aboutGlyphOn + " x " + aboutGlyphOff
	glyphs := []barSegment{{col: 3, width: 1}, {col: 7, width: 1}}
	want := "漢\x1b[32m" + aboutGlyphOn + restore + " x " + "\x1b[31m" + aboutGlyphOff + restore
	if got := colourGlyphs(content, glyphs, restore); got != want {
		t.Fatalf("colourGlyphs = %q, want %q", got, want)
	}
	if got := colourGlyphs(content, nil, restore); got != content {
		t.Fatalf("colourGlyphs with no glyphs = %q, want the content unchanged", got)
	}
}

// TestAboutFieldsWebGL pins the WebGL field: ? only for the unknown tier, ✗
// when no context could be created, ➀ and ➁ for the two versions, an
// out-of-range tier reads as unknown, and the glued vendor field appears only
// with a non-empty label.
func TestAboutFieldsWebGL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state render.WebGLState
		want  string
		label bool
	}{
		{"unknown", render.WebGLState{Tier: render.WebGLUnknown}, "WebGL " + aboutGlyphUnknown, false},
		{"unavailable", render.WebGLState{Tier: render.WebGLUnavailable}, "WebGL " + aboutGlyphOff, false},
		{"webgl1", render.WebGLState{Tier: render.WebGL1}, "WebGL " + aboutGlyphWebGL1, false},
		{"webgl2", render.WebGLState{Tier: render.WebGL2}, "WebGL " + aboutGlyphWebGL2, false},
		{"out of range", render.WebGLState{Tier: render.WebGLTier(99)}, "WebGL " + aboutGlyphUnknown, false},
		{"webgl2 with vendor", render.WebGLState{Tier: render.WebGL2, Label: "Intel"}, "WebGL " + aboutGlyphWebGL2, true},
		{"unavailable with vendor", render.WebGLState{Tier: render.WebGLUnavailable, Label: "SwiftShader"}, "WebGL " + aboutGlyphOff, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := exampleAboutSnapshot()
			snap.WebGL = tc.state
			fields := aboutFields(snap)
			tier := fields[len(fields)-1]
			if tc.label {
				vendor := tier
				tier = fields[len(fields)-2]
				if want := "  (" + tc.state.Label + ")"; vendor.text != want || !vendor.glued || vendor.priority != 10 {
					t.Fatalf("vendor field = %+v, want %q glued at priority 10", vendor, want)
				}
			}
			if tier.text != tc.want || tier.glued || tier.priority != 9 {
				t.Fatalf("tier field = %+v, want %q unglued at priority 9", tier, tc.want)
			}
			if !tc.label {
				for _, f := range fields {
					if f.glued {
						t.Fatalf("a glued field appeared with no label: %+v", f)
					}
				}
			}
		})
	}
}

// TestAboutFieldsOrderAndOptionalFields pins the field order and priorities:
// the version is priority 2 and never drops first, the browser and terminal
// fields are omitted when empty rather than rendered blank, and the priorities
// rise strictly along the row so the drop order is the reverse of the visual
// order.
func TestAboutFieldsOrderAndOptionalFields(t *testing.T) {
	full := aboutFields(exampleAboutSnapshot())
	wantTexts := []string{
		"Nyala dev", "Chrome 150", "Kitty 0.4.0", "✓ Kitty (shm)", "✗ Sixel",
		"✓ Sextant", "Colour: True Colour", "WebGL ➁", "  (Intel)",
	}
	if len(full) != len(wantTexts) {
		t.Fatalf("aboutFields = %d fields, want %d", len(full), len(wantTexts))
	}
	for i, f := range full {
		if f.text != wantTexts[i] {
			t.Fatalf("field %d = %q, want %q", i, f.text, wantTexts[i])
		}
		if f.priority != i+2 {
			t.Fatalf("field %q priority = %d, want %d", f.text, f.priority, i+2)
		}
		if f.glued != (i == len(full)-1) {
			t.Fatalf("field %q glued = %t", f.text, f.glued)
		}
	}

	snap := exampleAboutSnapshot()
	snap.Browser, snap.Terminal = "", ""
	bare := aboutFields(snap)
	if len(bare) != len(full)-2 {
		t.Fatalf("aboutFields without browser and terminal = %d fields, want %d", len(bare), len(full)-2)
	}
	for _, f := range bare {
		if f.priority == 3 || f.priority == 4 {
			t.Fatalf("an empty optional field was kept: %+v", f)
		}
		if strings.TrimSpace(f.text) == "" {
			t.Fatalf("a blank field was kept: %+v", f)
		}
	}
}

// TestStateGlyph pins the state glyph map: on is ✓, off is ✗, the zero value
// is ?, and an out-of-range value reads as the decided off state.
func TestStateGlyph(t *testing.T) {
	for _, tc := range []struct {
		state capabilityState
		want  string
	}{
		{capUnknown, aboutGlyphUnknown},
		{capOn, aboutGlyphOn},
		{capOff, aboutGlyphOff},
		{capabilityState(99), aboutGlyphOff},
	} {
		if got := stateGlyph(tc.state); got != tc.want {
			t.Fatalf("stateGlyph(%d) = %q, want %q", tc.state, got, tc.want)
		}
	}
	if got := kittyField(capOn, KittyTransportDirect); got != aboutGlyphOn+" Kitty (direct)" {
		t.Fatalf("kittyField on = %q", got)
	}
	if got := kittyField(capOff, KittyTransportSHM); got != aboutGlyphOff+" Kitty" {
		t.Fatalf("kittyField off = %q, want no transport", got)
	}
}

// TestAboutRowStripsControls pins the display boundary for every text the
// row shows: a terminal identity, a browser product, and a WebGL label that
// smuggle ESC, CSI, C0, DEL, and C1 bytes reach the terminal without them.
// The greyscale mode skips the colour pass, so the bytes between the bar
// opener and the reset are the plain row and can be checked rune by rune.
func TestAboutRowStripsControls(t *testing.T) {
	const smuggled = "\x1b]0;x\x07\u009b31m\x01\x7f\x9b"
	st := NewState(160, 24)
	st.Colour = ColourXTermGrey
	st.probe.TermID = "kitty" + smuggled + " 0.4.0"
	eng := webGLTestEngine{
		streamTestEngine: streamTestEngine{browserVersion: "Chrome" + smuggled + " 150.0.1"},
		webgl:            render.WebGLState{Tier: render.WebGL2, Label: "Intel" + smuggled},
	}
	s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, version: "dev" + smuggled}
	out := aboutRowBytes(t, s)

	prefix := "\x1b[24;1H" + st.barStyle()
	if !strings.HasPrefix(out, prefix) || !strings.HasSuffix(out, "\x1b[0m") {
		t.Fatalf("About row framing = %q", out)
	}
	row := strings.TrimSuffix(strings.TrimPrefix(out, prefix), "\x1b[0m")
	if strings.ContainsFunc(row, isControlRune) {
		t.Fatalf("a control rune reached the About row: %q", row)
	}
	// Only the control bytes go: the printable remainder of each smuggled
	// sequence stays as text, and the raw 0x9b byte, which is not UTF-8,
	// becomes the replacement rune rather than a bare CSI byte.
	if strings.Contains(out, "\x9b") {
		t.Fatalf("a raw C1 byte reached the About row: %q", out)
	}
	const remainder = "]0;x31m\uFFFD"
	for _, want := range []string{
		"Nyala dev" + remainder,
		"Chrome" + remainder + " 150",
		"Kitty" + remainder + " 0.4.0",
		"WebGL ➁  (Intel" + remainder + ")",
	} {
		if !strings.Contains(row, want) {
			t.Fatalf("About row %q lost %q after stripping", row, want)
		}
	}
}

// TestAboutSnapshotWebGLReporter pins the optional engine capability: a fake
// without WebGL yields the unknown tier and the ? glyph, a fake that reports
// yields its tier and label, and a nil engine yields unknown with no browser.
func TestAboutSnapshotWebGLReporter(t *testing.T) {
	st := NewState(160, 24)

	plain := &streamSession{eng: streamTestEngine{browserVersion: "Chrome 150.0.1"}, st: &st, tty: &bytes.Buffer{}, version: "dev"}
	snap := plain.aboutSnapshot()
	if snap.WebGL.Tier != render.WebGLUnknown || snap.WebGL.Label != "" {
		t.Fatalf("plain fake WebGL = %+v, want unknown", snap.WebGL)
	}
	if snap.Browser != "Chrome 150" || snap.Version != "dev" {
		t.Fatalf("plain fake snapshot = %+v", snap)
	}
	if out := aboutRowBytes(t, plain); !strings.Contains(out, "WebGL "+aboutGlyphUnknown) {
		t.Fatalf("plain fake About row = %q, want WebGL ?", out)
	}

	reporting := &streamSession{
		eng: webGLTestEngine{
			streamTestEngine: streamTestEngine{browserVersion: "Chrome 150.0.1"},
			webgl:            render.WebGLState{Tier: render.WebGL1, Label: "Mesa"},
		},
		st: &st, tty: &bytes.Buffer{}, version: "dev",
	}
	snap = reporting.aboutSnapshot()
	if snap.WebGL != (render.WebGLState{Tier: render.WebGL1, Label: "Mesa"}) {
		t.Fatalf("reporting fake WebGL = %+v, want the reported state", snap.WebGL)
	}
	if out := aboutRowBytes(t, reporting); !strings.Contains(out, "WebGL "+aboutGlyphWebGL1+"  (Mesa)") {
		t.Fatalf("reporting fake About row = %q, want WebGL ➀  (Mesa)", out)
	}

	none := &streamSession{st: &st, tty: &bytes.Buffer{}, version: "dev"}
	snap = none.aboutSnapshot()
	if snap.WebGL.Tier != render.WebGLUnknown || snap.Browser != "" {
		t.Fatalf("nil engine snapshot = %+v, want unknown WebGL and no browser", snap)
	}
}
