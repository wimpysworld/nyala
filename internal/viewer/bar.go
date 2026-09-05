package viewer

import (
	"slices"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// engineRows is the pure engine-grid row decision: the viewer reserves the
// bottom terminal row for the bar on every backend, so the engine lays out,
// renders, and captures at one row less than the terminal grid. The render
// paths compose and place the page to the same reduced grid, so the bar row
// stays outside every frame. rows below two pass through unchanged: a
// one-row terminal has no room for a bar row beside a page row, so it keeps
// the full grid and draws no bar. This folds in the old tmux Sixel special
// case: tmux rescales any Sixel image taller than pane rows minus one, so
// Sixel under tmux wanted the bottom row free before the bar claimed it.
func engineRows(rows int) int {
	if rows >= 2 {
		return rows - 1
	}
	return rows
}

// Bar glyphs never use presentation selectors. Each render glyph occupies one
// cell in a fixed two-cell field, so backend changes do not move the group.
const (
	barGlyphBack          = "←" // ← U+2190 Back
	barGlyphForward       = "→" // → U+2192 Forward
	barGlyphReload        = "⟳" // ⟳ U+27F3 Reload
	barGlyphHome          = "⌂" // ⌂ U+2302 Home
	barGlyphMenu          = "☰" // ☰ U+2630 Menu
	barGlyphBlockQuadrant = "⬒" // ⬒ U+2B12 Block quadrant scheme
	barGlyphBlockSextant  = "⬢" // ⬢ U+2B22 Block sextant scheme
	barGlyphSixel         = "🅢" // 🅢 U+1F162 Sixel
	barGlyphKitty         = "🅺" // 🅺 U+1F17A Kitty
	barGlyphClose         = "✕" // ✕ close (menu, About, and find modes)
	barGlyphPrompt        = "❯" // ❯ URL edit prompt
	barGlyphFind          = "⌕" // ⌕ U+2315 find prompt
	barGlyphHTTPS         = "★" // ★ HTTPS transport
	barGlyphHTTP          = "☆" // ☆ HTTP transport
	barSeparator          = "│" // │ segment separator
	barEllipsis           = "…" // … URL truncation marker
)

// Bar row SGR openers, the base 16 ANSI colours only, so the bar follows the
// user's theme rather than pinning a colour of its own. Each resets first, so
// one opener fully describes the row whatever preceded it.
//
// barStyleReverse is the pre-detection and fallback style: reverse video on
// the terminal default pair, the bar Nyala drew before OSC 11 detection
// existed. barStyleDark and barStyleLight are the detected pair, a grey bar
// that sits with the terminal's own polarity: index 8 with index 7 ink on a
// dark terminal, index 7 with index 0 ink on a light one.
const (
	barStyleReverse = "\x1b[0;7m"
	barStyleDark    = "\x1b[0;37;100m"
	barStyleLight   = "\x1b[0;30;47m"
)

// barStyle returns the SGR opener for the bottom bar row. An unresolved
// barSGR, which is every State that skipped configureBackends, reads as
// reverse video.
func (s *State) barStyle() string {
	if s.barSGR == "" {
		return barStyleReverse
	}
	return s.barSGR
}

// resolveBarStyle picks the bar's SGR opener from the OSC 11 handshake reply.
// A dark terminal gets the dark grey bar and a light one the light grey bar.
// Reverse video holds whenever the polarity is unknown: the terminal stayed
// silent, answered unreadably, or runs under a multiplexer that sends no
// query.
func resolveBarStyle(dark, ok bool) string {
	if !ok {
		return barStyleReverse
	}
	if dark {
		return barStyleDark
	}
	return barStyleLight
}

// barSpinner is the braille startup spinner cycle shown before the URL while
// the launch navigation runs, before the first frame.
var barSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerGlyph returns the spinner frame for one tick step, wrapping so any
// non-negative step is valid.
func spinnerGlyph(step int) string {
	return barSpinner[step%len(barSpinner)]
}

// transportGlyph returns the URL-prefix glyph for the URL scheme. The filled
// star identifies HTTPS transport only. Other schemes reserve the same cell.
func transportGlyph(rawURL string) string {
	scheme, _, ok := strings.Cut(rawURL, ":")
	if !ok {
		return " "
	}
	switch {
	case strings.EqualFold(scheme, "https"):
		return barGlyphHTTPS
	case strings.EqualFold(scheme, "http"):
		return barGlyphHTTP
	default:
		return " "
	}
}

// backendGlyph returns the indicator glyph for the active backend. Block
// tracks the live glyph scheme, so an Alt+B scheme cycle changes the glyph.
func backendGlyph(backend Backend, scheme block.Scheme) string {
	switch backend {
	case BackendSixel:
		return barGlyphSixel
	case BackendKitty:
		return barGlyphKitty
	default:
		if scheme == block.SchemeSextant {
			return barGlyphBlockSextant
		}
		return barGlyphBlockQuadrant
	}
}

// colourBadge returns the four-cell colour label for the active backend.
// Graphics backends have fixed colour paths. Block reports its current colour
// mode and selected adaptive or static palette policy.
func colourBadge(s *State) string {
	switch s.Backend {
	case BackendKitty:
		return "RGB "
	case BackendSixel:
		return "256a"
	}
	switch s.Colour {
	case ColourXTerm256:
		if s.staticPaletteSelected {
			return "240s"
		}
		return "240a"
	case ColourXTermGrey:
		if s.staticPaletteSelected {
			return "24g "
		}
		return "240g"
	default:
		return "RGB "
	}
}

// barSegmentKind identifies one bottom-bar segment. Stage-3 mouse
// hit-testing maps a bar-row click column onto these kinds.
type barSegmentKind int

const (
	// barSegmentBack is the ← history-back button.
	barSegmentBack barSegmentKind = iota
	// barSegmentForward is the → history-forward button.
	barSegmentForward
	// barSegmentReload is the ⟳ reload button.
	barSegmentReload
	// barSegmentHome is the ⌂ launch-URL button.
	barSegmentHome
	// barSegmentIndicator is the backend indicator slot.
	barSegmentIndicator
	// barSegmentColour is the four-cell colour badge and its outer padding.
	barSegmentColour
	// barSegmentURL is the flexible URL region.
	barSegmentURL
	// barSegmentFPS is the fps meter text, present only when the meter is armed.
	barSegmentFPS
	// barSegmentMenu is the ☰ menu button, anchored to the bottom-right corner.
	barSegmentMenu
)

// barSegment is one laid-out segment: its kind, 1-based start column, and
// width in terminal cells. Every span derives from go-runewidth measurement,
// so a terminal-ambiguous glyph that measures wide shifts the layout without
// breaking the spans.
type barSegment struct {
	kind  barSegmentKind
	col   int
	width int
}

// barLayout is the computed bar layout: the bar width in cells and the
// segment spans in column order. Segments that do not fit a narrow terminal
// are absent.
type barLayout struct {
	cols     int
	segments []barSegment
	prefix   barSegment
}

// segment returns the span for kind and whether the layout holds it.
func (l barLayout) segment(kind barSegmentKind) (barSegment, bool) {
	for _, s := range l.segments {
		if s.kind == kind {
			return s, true
		}
	}
	return barSegment{}, false
}

// hit returns the segment kind at the 1-based bar column, the mouse
// hit-test against the spans the last chrome repaint laid out. ok is false
// on a filler cell (separator or padding) that owns no span.
func (l barLayout) hit(col int) (barSegmentKind, bool) {
	for _, s := range l.segments {
		if col >= s.col && col < s.col+s.width {
			return s.kind, true
		}
	}
	return 0, false
}

// boldSpan wraps the cells [col, col+width) of a plain bar row in SGR bold,
// the press and highlight feedback. It walks the row by measured cells, so a
// wide rune keeps the span aligned with the hit-test columns. A span outside
// the row returns the row unchanged.
func boldSpan(content string, col, width int) string {
	return boldSpans(content, []barSegment{{col: col, width: width}})
}

// boldSpans wraps each cell span [col, col+width) of a plain bar row in SGR
// bold in a single pass, so several spans never miscount cells across an
// earlier insertion's escapes. It walks the row by measured cells, so a wide
// rune keeps the spans aligned with the hit-test columns. Degenerate or
// out-of-row spans leave the row unchanged.
func boldSpans(content string, spans []barSegment) string {
	decorations := make([]barDecoration, 0, len(spans))
	for _, span := range spans {
		decorations = append(decorations, barDecoration{span: span, open: "\x1b[1m", close: "\x1b[22m"})
	}
	return decorateBar(content, decorations)
}

type barDecoration struct {
	span  barSegment
	open  string
	close string
}

// decorateBar inserts style bytes around cell spans after the plain row and its
// layout are complete. When spans overlap, it closes every active style at a
// boundary and reopens the styles that continue. A close sequence can therefore
// reset the full bar style without clearing another active decoration. Cell
// measurement never sees ANSI.
func decorateBar(content string, decorations []barDecoration) string {
	validDecorations := make([]barDecoration, 0, len(decorations))
	for _, decoration := range decorations {
		if decoration.span.col >= 1 && decoration.span.width >= 1 {
			validDecorations = append(validDecorations, decoration)
		}
	}
	if len(validDecorations) == 0 {
		return content
	}
	slices.SortStableFunc(validDecorations, func(a, b barDecoration) int {
		if byColumn := a.span.col - b.span.col; byColumn != 0 {
			return byColumn
		}
		return b.span.width - a.span.width
	})
	var b strings.Builder
	cell := 1
	idx := 0
	active := make([]barDecoration, 0, len(validDecorations))
	wrote := false
	closeActive := func() {
		for i := len(active) - 1; i >= 0; i-- {
			b.WriteString(active[i].close)
		}
	}
	openActive := func() {
		for _, decoration := range active {
			b.WriteString(decoration.open)
		}
	}
	for _, r := range content {
		expired := false
		for _, decoration := range active {
			if cell >= decoration.span.col+decoration.span.width {
				expired = true
				break
			}
		}
		if expired {
			closeActive()
			kept := active[:0]
			for _, decoration := range active {
				if cell < decoration.span.col+decoration.span.width {
					kept = append(kept, decoration)
				}
			}
			active = kept
		}
		start := len(active)
		for idx < len(validDecorations) && cell >= validDecorations[idx].span.col {
			active = append(active, validDecorations[idx])
			idx++
			wrote = true
		}
		if expired {
			openActive()
		} else {
			for _, decoration := range active[start:] {
				b.WriteString(decoration.open)
			}
		}
		b.WriteRune(r)
		cell += runewidth.RuneWidth(r)
	}
	if len(active) > 0 {
		closeActive()
	}
	if !wrote {
		return content
	}
	return b.String()
}

// urlPrefixStyle returns the prefix colour style. Known bar polarity gives the
// spinner an explicit high-contrast foreground. The reverse-video fallback
// keeps its default contrast. Reverse video needs a background selector to
// display the HTTPS transport glyph as yellow ink.
func urlPrefixStyle(barStyle string, loading bool, transport string) string {
	if loading {
		if barStyle == barStyleLight {
			return "\x1b[30m"
		}
		if barStyle == barStyleDark {
			return "\x1b[97m"
		}
		return ""
	}
	if transport == barGlyphHTTPS {
		if barStyle == barStyleReverse {
			return "\x1b[43m"
		}
		return "\x1b[33m"
	}
	return ""
}

// barContent carries the variable bar fields for one repaint. indicator is
// the backend glyph. spinner replaces the transport glyph during the launch
// navigation. transport can carry a glyph that the caller already derived.
// colour is the four-cell badge. fps is the fps meter text. An empty fps hides
// that segment. A non-empty signal replaces the URL text for as long as it
// holds.
type barContent struct {
	indicator string
	spinner   string
	transport string
	colour    string
	url       string
	fps       string
	signal    string
}

// barAtom is one fixed-width piece of the bar row: a segment's content or a
// filler (separator or padding) that owns no hit span.
type barAtom struct {
	kind    barSegmentKind
	text    string
	width   int
	segment bool
}

// barButton builds a glyph click-target atom: the glyph padded one cell each
// side, so a one-cell glyph gets the three-cell click target and a wide
// rendering keeps its padding.
func barButton(kind barSegmentKind, glyph string) barAtom {
	return barAtom{kind: kind, text: " " + glyph + " ", width: runewidth.StringWidth(glyph) + 2, segment: true}
}

// barRenderButton builds the render click target. The glyph has one trailing
// cell in a fixed two-cell field, with the standard button padding outside it.
func barRenderButton(glyph string) barAtom {
	field := runewidth.FillRight(runewidth.Truncate(glyph, 1, ""), 2)
	return barAtom{kind: barSegmentIndicator, text: " " + field + " ", width: 4, segment: true}
}

// barFiller builds a non-segment atom: a separator or padding cell.
func barFiller(text string) barAtom {
	return barAtom{text: text, width: runewidth.StringWidth(text)}
}

// renderBar lays out and renders one bar row for a cols-wide terminal. It
// returns the row content, exactly cols cells wide with no escapes, and the
// segment layout it drew. The chrome is
// ← │ → │ ⟳ │ ⌂ │ <transport> <url> [│ fps] │ <indicator> │ <colour> │ ☰: the menu
// anchors to the right corner. The render glyph and four-cell colour badge
// always sit immediately to its left. The optional fps text sits before the
// fixed status group. The URL region absorbs the remaining width and truncates
// with an ellipsis. A terminal too narrow for the chrome degrades to a URL-only
// row. A live signal replaces the URL text and uses the same width.
func renderBar(cols int, c barContent) (string, barLayout) {
	if cols <= 0 {
		return "", barLayout{cols: cols}
	}
	region := c.url
	hasPrefix := true
	if c.signal != "" {
		region = c.signal
		hasPrefix = false
	}
	if hasPrefix {
		prefix := c.transport
		if prefix == "" {
			prefix = transportGlyph(c.url)
		}
		if c.spinner != "" {
			prefix = c.spinner
		}
		region = runewidth.FillRight(runewidth.Truncate(prefix, 1, ""), 1) + " " + region
	}
	left := []barAtom{
		barButton(barSegmentBack, barGlyphBack),
		barFiller(barSeparator),
		barButton(barSegmentForward, barGlyphForward),
		barFiller(barSeparator),
		barButton(barSegmentReload, barGlyphReload),
		barFiller(barSeparator),
		barButton(barSegmentHome, barGlyphHome),
		barFiller(barSeparator),
	}
	right := []barAtom{barFiller(barSeparator)}
	if c.fps != "" {
		right = append(right,
			barFiller(" "),
			barAtom{kind: barSegmentFPS, text: c.fps, width: runewidth.StringWidth(c.fps), segment: true},
			barFiller(" "),
			barFiller(barSeparator),
		)
	}
	right = append(right,
		barRenderButton(c.indicator),
		barFiller(barSeparator),
		barAtom{kind: barSegmentColour, text: " " + runewidth.FillRight(runewidth.Truncate(c.colour, 4, ""), 4) + " ", width: 6, segment: true},
		barFiller(barSeparator),
		barButton(barSegmentMenu, barGlyphMenu),
	)

	fixed := 0
	for _, a := range left {
		fixed += a.width
	}
	for _, a := range right {
		fixed += a.width
	}
	urlWidth := cols - fixed
	if urlWidth < 1 {
		// Too narrow for the chrome: the whole row is the URL region.
		content := runewidth.FillRight(runewidth.Truncate(region, cols, barEllipsis), cols)
		layout := barLayout{
			cols:     cols,
			segments: []barSegment{{kind: barSegmentURL, col: 1, width: cols}},
		}
		if hasPrefix {
			layout.prefix = barSegment{col: 1, width: 1}
		}
		return content, layout
	}

	atoms := make([]barAtom, 0, len(left)+1+len(right))
	atoms = append(atoms, left...)
	atoms = append(atoms, barAtom{
		kind:    barSegmentURL,
		text:    runewidth.FillRight(runewidth.Truncate(region, urlWidth, barEllipsis), urlWidth),
		width:   urlWidth,
		segment: true,
	})
	atoms = append(atoms, right...)

	l := barLayout{cols: cols}
	var row []byte
	col := 1
	for _, a := range atoms {
		if a.segment {
			l.segments = append(l.segments, barSegment{kind: a.kind, col: col, width: a.width})
			if a.kind == barSegmentURL && hasPrefix {
				l.prefix = barSegment{col: col, width: 1}
			}
		}
		row = append(row, a.text...)
		col += a.width
	}
	return string(row), l
}
