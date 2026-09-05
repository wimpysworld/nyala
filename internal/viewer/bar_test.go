package viewer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestEngineRows pins the pure engine-grid row decision: the bar reserves the
// bottom terminal row on every backend, and a grid below two rows never
// shrinks, so a one-row terminal keeps the full grid and draws no bar.
func TestEngineRows(t *testing.T) {
	cases := []struct {
		name string
		rows int
		want int
	}{
		{"normal grid reserves bottom row", 24, 23},
		{"two rows reserve to one", 2, 1},
		{"one row never shrinks", 1, 1},
		{"zero rows pass through", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineRows(tc.rows); got != tc.want {
				t.Fatalf("engineRows(%d) = %d, want %d", tc.rows, got, tc.want)
			}
		})
	}
}

// TestSpinnerGlyph pins the braille spinner cycle: ten frames in order,
// wrapping at the end.
func TestSpinnerGlyph(t *testing.T) {
	want := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	for i, w := range want {
		if got := spinnerGlyph(i); got != w {
			t.Fatalf("spinnerGlyph(%d) = %q, want %q", i, got, w)
		}
	}
	if got := spinnerGlyph(len(want)); got != want[0] {
		t.Fatalf("spinnerGlyph wraps to %q, want %q", got, want[0])
	}
}

// TestBackendGlyph pins the indicator glyphs: the Block glyph tracks the
// live scheme, Sixel and Kitty ignore it.
func TestBackendGlyph(t *testing.T) {
	cases := []struct {
		name    string
		backend Backend
		scheme  block.Scheme
		want    string
	}{
		{"block quadrant", BackendBlock, block.SchemeQuadrant, "⬒"},
		{"block sextant", BackendBlock, block.SchemeSextant, "⬢"},
		{"sixel", BackendSixel, block.SchemeQuadrant, "🅢"},
		{"kitty", BackendKitty, block.SchemeSextant, "🅺"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backendGlyph(tc.backend, tc.scheme); got != tc.want {
				t.Fatalf("backendGlyph(%v, %v) = %q, want %q", tc.backend, tc.scheme, got, tc.want)
			}
		})
	}
}

func TestColourBadge(t *testing.T) {
	cases := []struct {
		name    string
		backend Backend
		colour  ColourMode
		static  bool
		want    string
	}{
		{"Kitty", BackendKitty, ColourXTermGrey, true, "RGB "},
		{"Sixel", BackendSixel, ColourTrue, true, "256a"},
		{"Block true colour", BackendBlock, ColourTrue, false, "RGB "},
		{"Block 240 static", BackendBlock, ColourXTerm256, true, "240s"},
		{"Block 240 adaptive", BackendBlock, ColourXTerm256, false, "240a"},
		{"Block 24 shades", BackendBlock, ColourXTermGrey, true, "24g "},
		{"Block 240 shades", BackendBlock, ColourXTermGrey, false, "240g"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(80, 24)
			st.Backend, st.Colour, st.staticPaletteSelected = tc.backend, tc.colour, tc.static
			if got := colourBadge(&st); got != tc.want {
				t.Fatalf("colourBadge() = %q, want %q", got, tc.want)
			}
			if got := runewidth.StringWidth(colourBadge(&st)); got != 4 {
				t.Fatalf("colourBadge() width = %d, want 4", got)
			}
		})
	}
}

// TestRenderBarLayout pins the full-chrome segment model: the navigation
// buttons, URL, render indicator, and anchored menu. It also checks that spans
// use cells, stay ordered, and fit a row that is exactly cols cells wide.
func TestRenderBarLayout(t *testing.T) {
	const cols = 80
	content, l := renderBar(cols, barContent{indicator: "⬒", colour: "240a", url: "https://example.org/"})
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("bar content width = %d, want %d", w, cols)
	}
	wantOrder := []barSegmentKind{
		barSegmentBack, barSegmentForward, barSegmentReload, barSegmentHome,
		barSegmentURL, barSegmentIndicator, barSegmentColour, barSegmentMenu,
	}
	if len(l.segments) != len(wantOrder) {
		t.Fatalf("segments = %d, want %d", len(l.segments), len(wantOrder))
	}
	prevEnd := 0
	for i, seg := range l.segments {
		if seg.kind != wantOrder[i] {
			t.Fatalf("segment %d kind = %d, want %d", i, seg.kind, wantOrder[i])
		}
		if seg.col <= prevEnd {
			t.Fatalf("segment %d at col %d overlaps the previous end %d", i, seg.col, prevEnd)
		}
		prevEnd = seg.col + seg.width - 1
	}

	// Navigation and menu click targets pad to three cells. The render target
	// adds one explicit trailing field cell and therefore spans four cells.
	glyphs := map[barSegmentKind]string{
		barSegmentBack:    barGlyphBack,
		barSegmentForward: barGlyphForward,
		barSegmentReload:  barGlyphReload,
		barSegmentHome:    barGlyphHome,
		barSegmentMenu:    barGlyphMenu,
	}
	for kind, glyph := range glyphs {
		seg, ok := l.segment(kind)
		if !ok {
			t.Fatalf("segment %d missing", kind)
		}
		if want := runewidth.StringWidth(glyph) + 2; seg.width != want || seg.width < 3 {
			t.Fatalf("segment %d width = %d, want the measured %d-cell click target", kind, seg.width, want)
		}
	}
	indicator, ok := l.segment(barSegmentIndicator)
	if !ok || indicator.width != 4 {
		t.Fatalf("render indicator = %+v, want a four-cell click target", indicator)
	}
	colour, ok := l.segment(barSegmentColour)
	if !ok || colour.width != 6 {
		t.Fatalf("colour badge = %+v, want a six-cell click target", colour)
	}

	// The menu anchors to the bottom-right corner behind its own separator.
	menu, _ := l.segment(barSegmentMenu)
	if menu.col+menu.width-1 != cols {
		t.Fatalf("menu ends at %d, want the corner column %d", menu.col+menu.width-1, cols)
	}
	if !strings.Contains(content, barSeparator+" "+barGlyphMenu) {
		t.Fatalf("bar content %q missing the separator before ☰", content)
	}
	if !strings.Contains(content, "│ ⬒  │ 240a │ ☰") {
		t.Fatalf("bar content %q has the wrong status order", content)
	}

	// No fps segment without the meter, and never a variation selector.
	if _, ok := l.segment(barSegmentFPS); ok {
		t.Fatal("fps segment present without the meter")
	}
	if strings.ContainsRune(content, '\ufe0f') {
		t.Fatalf("bar content carries U+FE0F: %q", content)
	}
}

func TestRenderBarStatusGroupWidthIsStable(t *testing.T) {
	type position struct {
		indicator barSegment
		menu      barSegment
	}
	var want position
	for i, tc := range []struct {
		glyph string
		badge string
	}{
		{"🅺", "RGB "},
		{"🅢", "256a"},
		{"⬢", "240s"},
		{"⬒", "24g "},
	} {
		if got := runewidth.StringWidth(tc.glyph); got != 1 {
			t.Fatalf("render glyph %q width = %d, want 1", tc.glyph, got)
		}
		content, layout := renderBar(80, barContent{indicator: tc.glyph, colour: tc.badge, url: "https://example.org/"})
		indicator, _ := layout.segment(barSegmentIndicator)
		menu, _ := layout.segment(barSegmentMenu)
		got := position{indicator: indicator, menu: menu}
		if i == 0 {
			want = got
		} else if got != want {
			t.Fatalf("status position for %q and %q = %+v, want %+v", tc.glyph, tc.badge, got, want)
		}
		if runewidth.StringWidth(content) != 80 {
			t.Fatalf("row for %q and %q has the wrong width", tc.glyph, tc.badge)
		}
		if !strings.Contains(content, "│ "+tc.glyph+"  │") {
			t.Fatalf("row for %q lacks its trailing render-field cell: %q", tc.glyph, content)
		}
	}
}

// TestRenderBarFPSSegment pins the fps placement: the segment exists exactly
// when the meter text does and sits before the anchored status group. The URL
// region absorbs the width change, so the status group never moves.
func TestRenderBarFPSSegment(t *testing.T) {
	const cols = 80
	fps := "↓ 24.0 " + barSeparator + " ↑ 12.5 " + barSeparator + " ≌ 42"
	_, without := renderBar(cols, barContent{indicator: "🅢", colour: "256a", url: "https://example.org/"})
	content, with := renderBar(cols, barContent{indicator: "🅢", colour: "256a", url: "https://example.org/", fps: fps})
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("bar content width = %d, want %d", w, cols)
	}
	if !strings.Contains(content, fps) {
		t.Fatalf("bar content %q missing fps text %q", content, fps)
	}
	if !strings.Contains(content, fps+" │ 🅢  │ 256a │ "+barGlyphMenu) {
		t.Fatalf("bar content %q missing the separator between fps and ☰", content)
	}

	seg, ok := with.segment(barSegmentFPS)
	if !ok {
		t.Fatal("fps segment missing with the meter armed")
	}
	if seg.width != runewidth.StringWidth(fps) {
		t.Fatalf("fps width = %d, want measured %d", seg.width, runewidth.StringWidth(fps))
	}
	menuWith, _ := with.segment(barSegmentMenu)
	menuWithout, _ := without.segment(barSegmentMenu)
	if menuWith != menuWithout {
		t.Fatalf("menu moved when the fps segment appeared: %+v vs %+v", menuWith, menuWithout)
	}
	indicator, _ := with.segment(barSegmentIndicator)
	if seg.col+seg.width+2 != indicator.col {
		t.Fatalf("fps segment ends at %d, want two filler cells before the indicator at %d", seg.col+seg.width, indicator.col)
	}
	urlWith, _ := with.segment(barSegmentURL)
	urlWithout, _ := without.segment(barSegmentURL)
	if urlWith.width >= urlWithout.width {
		t.Fatalf("URL region did not absorb the fps width: %d vs %d", urlWith.width, urlWithout.width)
	}
}

// TestRenderBarTruncatesURL pins the URL truncation: a URL wider than the
// region ends with the ellipsis, a fitting URL stays whole, and the row width
// never grows.
func TestRenderBarTruncatesURL(t *testing.T) {
	const cols = 48
	long := "https://example.org/" + strings.Repeat("segment/", 12)
	content, _ := renderBar(cols, barContent{indicator: "🅺", colour: "RGB ", url: long})
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("bar content width = %d, want %d", w, cols)
	}
	if !strings.Contains(content, barEllipsis) {
		t.Fatalf("truncated URL lost its ellipsis: %q", content)
	}
	if strings.Contains(content, long) {
		t.Fatalf("over-wide URL was not truncated: %q", content)
	}

	short := "https://a.io/"
	content, _ = renderBar(cols, barContent{indicator: "🅺", colour: "RGB ", url: short})
	if !strings.Contains(content, short) || strings.Contains(content, barEllipsis) {
		t.Fatalf("fitting URL was truncated: %q", content)
	}
}

// TestRenderBarNarrowFallsBackToURL pins the narrow-terminal degradation: a
// row too narrow for the chrome carries the URL region alone, still exactly
// cols cells wide.
func TestRenderBarNarrowFallsBackToURL(t *testing.T) {
	const cols = 16
	content, l := renderBar(cols, barContent{indicator: "⬒", colour: "240a", url: "https://example.org/page"})
	if w := runewidth.StringWidth(content); w != cols {
		t.Fatalf("bar content width = %d, want %d", w, cols)
	}
	if len(l.segments) != 1 || l.segments[0].kind != barSegmentURL {
		t.Fatalf("narrow layout = %+v, want the URL region alone", l.segments)
	}
	if l.segments[0].col != 1 || l.segments[0].width != cols {
		t.Fatalf("narrow URL span = %+v, want the whole row", l.segments[0])
	}
}

// TestAppendBarSessionGlue pins the session-side bar draw: the bar addresses
// the bottom terminal row in reverse video, resets the SGR after, shows the
// launch URL before the engine records a commit, the spinner before the
// first frame, and the backend glyph after. A one-row terminal draws no bar.
func TestAppendBarSessionGlue(t *testing.T) {
	st := NewState(60, 4)
	st.Backend = BackendKitty
	s := &streamSession{st: &st, launchURL: "https://launch.example/"}

	var buf bytes.Buffer
	s.appendBar(&buf)
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b[4;1H\x1b[0;7m") {
		t.Fatalf("bar prefix = %q, want bottom-row CUP and reverse video", out)
	}
	if !strings.HasSuffix(out, "\x1b[0m") {
		t.Fatalf("bar suffix = %q, want an SGR reset", out)
	}
	if !strings.Contains(out, "https://launch.example/") {
		t.Fatalf("bar %q missing the launch URL", out)
	}
	if !strings.Contains(out, spinnerGlyph(0)) {
		t.Fatalf("pre-frame bar %q missing the spinner", out)
	}

	s.firstDrawn.Store(true)
	buf.Reset()
	s.appendBar(&buf)
	if !strings.Contains(buf.String(), "│ 🅺  │ RGB  │ ☰") {
		t.Fatalf("post-frame bar %q missing the Kitty indicator", buf.String())
	}

	st.Resize(60, 1)
	if b := s.barBytes(); b != nil {
		t.Fatalf("one-row terminal drew a bar: %q", b)
	}
}

func TestChromeBadgeRefreshesForColourAndPaletteChanges(t *testing.T) {
	st := NewState(80, 24)
	st.Backend = BackendBlock
	st.paletteQueryOK = true
	eng := streamTestEngine{}
	done := make(chan struct{})
	defer close(done)
	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: &st, tty: out, ctl: newControl(eng, done)}
	s.firstDrawn.Store(true)

	s.cycleColour(false)
	if !strings.Contains(out.String(), "│ ⬒  │ 240a │ ☰") {
		t.Fatalf("Alt+C bar %q missing the adaptive badge", out.String())
	}

	out.Reset()
	s.togglePalettePolicy()
	if !strings.Contains(out.String(), "│ ⬒  │ 240s │ ☰") {
		t.Fatalf("Alt+P bar %q missing the static badge", out.String())
	}
}

// TestAppendBarBoldsPressedButton pins the press feedback: a pressed button
// draws bold until its release, and an unpressed bar (the ⬢ sextant
// indicator included, a distinct glyph rather than a shade) carries no bold.
func TestAppendBarBoldsPressedButton(t *testing.T) {
	st := NewState(60, 4)
	st.Backend = BackendBlock
	st.Scheme = block.SchemeSextant
	s := &streamSession{st: &st, launchURL: "https://a.io/"}
	s.firstDrawn.Store(true)

	var buf bytes.Buffer
	s.appendBar(&buf)
	if strings.Contains(buf.String(), "\x1b[1m") {
		t.Fatalf("unpressed bar drew bold: %q", buf.String())
	}

	s.pressed, s.pressedSet = barSegmentBack, true
	buf.Reset()
	s.appendBar(&buf)
	if !strings.Contains(buf.String(), "\x1b[1m ← \x1b[22m") {
		t.Fatalf("pressed button not bold: %q", buf.String())
	}

	s.pressedSet = false
	buf.Reset()
	s.appendBar(&buf)
	if strings.Contains(buf.String(), "\x1b[1m") {
		t.Fatalf("released bar kept bold: %q", buf.String())
	}
}

// TestResolveBarStyle pins the bar's colour decision: a detected dark terminal
// gets the dark grey bar, a detected light one the light grey bar, and every
// unresolved case keeps reverse video.
func TestResolveBarStyle(t *testing.T) {
	cases := []struct {
		name string
		dark bool
		ok   bool
		want string
	}{
		{"dark terminal", true, true, barStyleDark},
		{"light terminal", false, true, barStyleLight},
		{"terminal stayed silent", false, false, barStyleReverse},
		{"unreadable reply cannot claim dark", true, false, barStyleReverse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveBarStyle(c.dark, c.ok); got != c.want {
				t.Fatalf("resolveBarStyle(%v, %v) = %q, want %q", c.dark, c.ok, got, c.want)
			}
		})
	}
}

// TestBarStyleFallsBackToReverse asserts a State that never ran the handshake
// still draws a legible bar: the unresolved opener reads as reverse video, so
// diagnostics and tests that build a State directly are never left writing a
// bar with no style at all.
func TestBarStyleFallsBackToReverse(t *testing.T) {
	st := NewState(60, 4)
	if got := st.barStyle(); got != barStyleReverse {
		t.Fatalf("pre-detection barStyle = %q, want reverse video", got)
	}
	st.barSGR = barStyleDark
	if got := st.barStyle(); got != barStyleDark {
		t.Fatalf("resolved barStyle = %q, want the dark bar", got)
	}
}

// TestBarStylesUseTheBase16Palette asserts every bar style stays inside the
// base 16 ANSI colours, so the bar follows the user's theme. A 256-palette or
// truecolour selector here would pin a shade the theme cannot move.
func TestBarStylesUseTheBase16Palette(t *testing.T) {
	for _, style := range []string{barStyleReverse, barStyleDark, barStyleLight} {
		if strings.Contains(style, "38;5;") || strings.Contains(style, "48;5;") {
			t.Fatalf("bar style %q selects a 256-palette entry", style)
		}
		if strings.Contains(style, "38;2;") || strings.Contains(style, "48;2;") {
			t.Fatalf("bar style %q selects a truecolour value", style)
		}
	}
}

// TestBarRowsDrawTheResolvedStyle asserts every bar mode frames its row with
// the style barStyle resolved, not a hard-coded reverse video, so one
// detection result reaches the chrome row, the URL editor, the find field, the
// menu, and the About line alike.
func TestBarRowsDrawTheResolvedStyle(t *testing.T) {
	modes := []struct {
		name  string
		enter func(s *streamSession)
	}{
		{"chrome", func(*streamSession) {}},
		{"edit", func(s *streamSession) { s.barMode, s.editor = barModeEdit, newURLEditor("https://a.io/") }},
		{"find", func(s *streamSession) { s.barMode, s.editor = barModeFind, newURLEditor("query") }},
		{"menu", func(s *streamSession) { s.barMode = barModeMenu }},
		{"about", func(s *streamSession) { s.barMode = barModeAbout }},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			st := NewState(60, 4)
			st.barSGR = barStyleLight
			s := &streamSession{st: &st, launchURL: "https://a.io/"}
			m.enter(s)
			var buf bytes.Buffer
			s.appendBar(&buf)
			if !strings.HasPrefix(buf.String(), "\x1b[4;1H"+barStyleLight) {
				t.Fatalf("%s row = %q, want the resolved light style", m.name, buf.String())
			}
			if strings.Contains(buf.String(), barStyleReverse) {
				t.Fatalf("%s row = %q, still carries hard-coded reverse video", m.name, buf.String())
			}
		})
	}
}
