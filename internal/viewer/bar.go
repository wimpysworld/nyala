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

// Bar glyphs, all Basic Multilingual Plane with no emoji-presentation
// lookalikes, and never followed by U+FE0F. The Block indicator glyphs echo
// their scheme's subcell shape with rising fill density.
const (
	barGlyphBack          = "←" // ← U+2190 Back
	barGlyphForward       = "→" // → U+2192 Forward
	barGlyphReload        = "⟳" // ⟳ U+27F3 Reload
	barGlyphHome          = "⌂" // ⌂ U+2302 Home
	barGlyphMenu          = "☰" // ☰ U+2630 Menu
	barGlyphBlockHalf     = "⬒" // ⬒ U+2B12 Block half scheme
	barGlyphBlockQuadrant = "⬔" // ⬔ U+2B14 Block quadrant scheme
	barGlyphBlockSextant  = "▦" // ▦ U+25A6 Block sextant scheme
	barGlyphSixel         = "⬣" // ⬣ U+2B23 Sixel
	barGlyphKitty         = "✪" // ✪ U+272A Kitty
	barGlyphClose         = "✕" // ✕ close (menu, About, and find modes)
	barGlyphPrompt        = "❯" // ❯ URL edit prompt
	barGlyphFind          = "⌕" // ⌕ U+2315 find prompt
	barSeparator          = "│" // │ segment separator
	barEllipsis           = "…" // … URL truncation marker
)

// barSpinner is the braille startup spinner cycle shown in the indicator
// slot while the launch navigation runs, before the first frame.
var barSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerGlyph returns the spinner frame for one tick step, wrapping so any
// non-negative step is valid.
func spinnerGlyph(step int) string {
	return barSpinner[step%len(barSpinner)]
}

// backendGlyph returns the indicator glyph for the active backend. Block
// tracks the live glyph scheme, so an Alt+B scheme cycle moves the die.
func backendGlyph(backend Backend, scheme block.Scheme) string {
	switch backend {
	case BackendSixel:
		return barGlyphSixel
	case BackendKitty:
		return barGlyphKitty
	default:
		switch scheme {
		case block.SchemeHalfBlock:
			return barGlyphBlockHalf
		case block.SchemeSextant:
			return barGlyphBlockSextant
		default:
			return barGlyphBlockQuadrant
		}
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
	// barSegmentIndicator is the backend indicator (or startup spinner) slot.
	barSegmentIndicator
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
// earlier insertion's escapes. Spans must not overlap; they are applied in
// column order. It walks the row by measured cells, so a wide rune keeps the
// spans aligned with the hit-test columns. Degenerate or out-of-row spans
// leave the row unchanged.
func boldSpans(content string, spans []barSegment) string {
	valid := make([]barSegment, 0, len(spans))
	for _, sp := range spans {
		if sp.col >= 1 && sp.width >= 1 {
			valid = append(valid, sp)
		}
	}
	if len(valid) == 0 {
		return content
	}
	slices.SortFunc(valid, func(a, b barSegment) int { return a.col - b.col })
	var b strings.Builder
	cell := 1
	idx := 0
	open, wrote := false, false
	for _, r := range content {
		if open && cell >= valid[idx].col+valid[idx].width {
			b.WriteString("\x1b[22m")
			open = false
			idx++
		}
		if !open && idx < len(valid) && cell >= valid[idx].col {
			b.WriteString("\x1b[1m")
			open = true
			wrote = true
		}
		b.WriteRune(r)
		cell += runewidth.RuneWidth(r)
	}
	if open {
		b.WriteString("\x1b[22m")
	}
	if !wrote {
		return content
	}
	return b.String()
}

// barContent carries the variable bar fields for one repaint. indicator is
// the backend glyph, or a spinner frame during the launch navigation. fps is
// the fps meter text; empty means the meter is off and the segment absent.
type barContent struct {
	indicator string
	url       string
	fps       string
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

// barFiller builds a non-segment atom: a separator or padding cell.
func barFiller(text string) barAtom {
	return barAtom{text: text, width: runewidth.StringWidth(text)}
}

// renderBar lays out and renders one bar row for a cols-wide terminal. It
// returns the row content, exactly cols cells wide with no escapes, and the
// segment layout it drew. The chrome is
// ← │ → │ ⟳ │ ⌂ │ <indicator> <url> │ [fps │] ☰: the menu anchors to the
// right corner behind its own separator, the fps text sits immediately left
// of it only when present, and the URL region absorbs the remaining width,
// truncating with an ellipsis. A terminal too narrow for the chrome degrades
// to a URL-only row.
func renderBar(cols int, c barContent) (string, barLayout) {
	if cols <= 0 {
		return "", barLayout{cols: cols}
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
		barButton(barSegmentIndicator, c.indicator),
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
	right = append(right, barButton(barSegmentMenu, barGlyphMenu))

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
		content := runewidth.FillRight(runewidth.Truncate(c.url, cols, barEllipsis), cols)
		return content, barLayout{cols: cols, segments: []barSegment{{kind: barSegmentURL, col: 1, width: cols}}}
	}

	atoms := make([]barAtom, 0, len(left)+1+len(right))
	atoms = append(atoms, left...)
	atoms = append(atoms, barAtom{
		kind:    barSegmentURL,
		text:    runewidth.FillRight(runewidth.Truncate(c.url, urlWidth, barEllipsis), urlWidth),
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
		}
		row = append(row, a.text...)
		col += a.width
	}
	return string(row), l
}
