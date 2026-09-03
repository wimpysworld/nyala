package viewer

import (
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/wimpysworld/nyala/internal/render"
)

// About row state glyphs. The glyph alone carries the meaning; the colour
// pass in colourGlyphs is a hint layered on top.
const (
	aboutGlyphOn      = "✓" // ✓ U+2713 capability on
	aboutGlyphOff     = "✗" // ✗ U+2717 capability off
	aboutGlyphUnknown = "?" // ? no probe answer
	aboutGlyphWebGL1  = "➀" // ➀ U+2780 WebGL 1 context
	aboutGlyphWebGL2  = "➁" // ➁ U+2781 WebGL 2 context
)

// aboutClose is the fixed right end of the About row: the separator and the
// close control. The close span is its last three cells.
const aboutClose = " " + barSeparator + " " + barGlyphClose + " "

// barField is one About row field: its display text, its drop priority, and
// whether it glues to the previous field with no separator between them. When
// the row does not fit, the field with the highest priority number drops
// first, so priority 1 never drops.
type barField struct {
	text     string
	priority int
	glued    bool
}

// aboutSnapshot builds the capability snapshot the About row renders: the
// build version, the engine's browser product, and the engine's WebGL probe
// when the engine reports one.
func (s *streamSession) aboutSnapshot() capabilitySnapshot {
	browser := ""
	if s.eng != nil {
		browser = s.eng.BrowserVersion()
	}
	var webgl render.WebGLState
	if r, ok := s.eng.(webGLReporter); ok {
		webgl = r.WebGL()
	}
	return s.st.capabilitySnapshot(s.version, browser, webgl)
}

// aboutFields lays out the About row fields in visual order with their drop
// priorities: the version, the browser, the terminal, Kitty with its
// transport, Sixel, Sextant, the colour depth, the WebGL tier, and the WebGL
// vendor. The browser and terminal fields are omitted when empty. The vendor
// is a glued field, so its double space before the parenthesis survives the
// join and it drops on its own before the tier.
func aboutFields(c capabilitySnapshot) []barField {
	fields := []barField{{text: "Nyala " + c.Version, priority: 2}}
	if c.Browser != "" {
		fields = append(fields, barField{text: c.Browser, priority: 3})
	}
	if c.Terminal != "" {
		fields = append(fields, barField{text: c.Terminal, priority: 4})
	}
	fields = append(fields,
		barField{text: kittyField(c.Kitty, c.KittyTransport), priority: 5},
		barField{text: stateGlyph(c.Sixel) + " Sixel", priority: 6},
		barField{text: stateGlyph(c.Sextant) + " Sextant", priority: 7},
		barField{text: "Colour: " + colourLabel(c.Colour), priority: 8},
		barField{text: "WebGL " + webGLGlyph(c.WebGL.Tier), priority: 9},
	)
	if c.WebGL.Label != "" {
		fields = append(fields, barField{text: "  (" + c.WebGL.Label + ")", priority: 10, glued: true})
	}
	return fields
}

// kittyField returns the Kitty field text: the state glyph, the name, and the
// effective transport in parentheses when Kitty is on.
func kittyField(state capabilityState, transport KittyTransport) string {
	text := stateGlyph(state) + " Kitty"
	if state == capOn {
		text += " (" + string(transport) + ")"
	}
	return text
}

// stateGlyph maps a capability state onto its row glyph. capUnknown never
// reaches the row from capabilitySnapshot, so the "?" branch guards the zero
// value only. An out-of-range value reads as off, the decided state.
func stateGlyph(state capabilityState) string {
	switch state {
	case capUnknown:
		return aboutGlyphUnknown
	case capOn:
		return aboutGlyphOn
	default:
		return aboutGlyphOff
	}
}

// webGLGlyph maps a WebGL tier onto its row glyph: ➁ for WebGL 2, ➀ for
// WebGL 1, ✗ when no context could be created, and ? when the probe gave no
// answer.
func webGLGlyph(tier render.WebGLTier) string {
	switch tier {
	case render.WebGL2:
		return aboutGlyphWebGL2
	case render.WebGL1:
		return aboutGlyphWebGL1
	case render.WebGLUnavailable:
		return aboutGlyphOff
	default:
		return aboutGlyphUnknown
	}
}

// renderBarAbout lays out the About row for a cols-wide terminal: a leading
// space, the kept fields joined by the separator, padding, and the fixed
// ` │ ✕ ` end. Fields drop whole, highest priority number first, until the
// row fits, and one ellipsis field stands in for whatever dropped. A field
// glued to a dropped field drops with it. When no field fits the row is
// ` … │ ✕ `, and a terminal narrower than that falls back to
// closeAnchoredRow, which keeps only whole spans. It returns the plain row
// content, exactly cols cells wide, the click spans (the close alone), and the
// cells of every ✓ and ✗ for the colour pass.
func renderBarAbout(cols int, fields []barField) (content string, spans []barSpan, glyphs []barSegment) {
	if cols <= 0 {
		return "", nil, nil
	}
	closeW := runewidth.StringWidth(aboutClose)
	kept := make([]bool, len(fields))
	for i := range kept {
		kept[i] = true
	}
	dropped := false
	var left string
	for {
		left, glyphs = joinAboutFields(fields, kept, dropped)
		if runewidth.StringWidth(left)+closeW <= cols {
			break
		}
		drop, dropPriority := -1, 0
		for i, f := range fields {
			if kept[i] && (drop < 0 || f.priority > dropPriority) {
				drop, dropPriority = i, f.priority
			}
		}
		if drop < 0 {
			content, spans = closeAnchoredRow(cols, left, nil)
			return content, spans, nil
		}
		kept[drop] = false
		for j := drop + 1; j < len(fields) && fields[j].glued; j++ {
			kept[j] = false
		}
		dropped = true
	}
	pad := cols - runewidth.StringWidth(left) - closeW
	content = left + strings.Repeat(" ", pad) + aboutClose
	closeSpanW := runewidth.StringWidth(" " + barGlyphClose + " ")
	spans = []barSpan{{item: barSpanClose, col: cols - closeSpanW + 1, width: closeSpanW}}
	return content, spans, glyphs
}

// joinAboutFields builds the left part of the About row from the kept
// fields, with the ellipsis field appended when dropped is set. It returns
// the text and the cell of every ✓ and ✗ in it.
func joinAboutFields(fields []barField, kept []bool, dropped bool) (string, []barSegment) {
	var b strings.Builder
	var glyphs []barSegment
	b.WriteString(" ")
	col := 2
	first := true
	add := func(text string, glued bool) {
		if !first && !glued {
			b.WriteString(" " + barSeparator + " ")
			col += runewidth.StringWidth(" " + barSeparator + " ")
		}
		first = false
		for _, r := range text {
			w := runewidth.RuneWidth(r)
			if s := string(r); s == aboutGlyphOn || s == aboutGlyphOff {
				glyphs = append(glyphs, barSegment{col: col, width: w})
			}
			col += w
		}
		b.WriteString(text)
	}
	for i, f := range fields {
		if kept[i] {
			add(f.text, f.glued)
		}
	}
	if dropped {
		add(barEllipsis, false)
	}
	return b.String(), glyphs
}

// colourGlyphs paints each ✓ cell green and each ✗ cell red in one pass over a
// plain About row, then writes restore after each so the bar style resumes.
// restore must reset and reopen the bar style, because barStyleLight sets an
// explicit foreground that SGR 39 would not bring back. Colour is a hint only:
// the caller skips this pass under ColourXTermGrey and the glyph still reads.
func colourGlyphs(content string, glyphs []barSegment, restore string) string {
	if len(glyphs) == 0 {
		return content
	}
	var b strings.Builder
	cell := 1
	idx := 0
	for _, r := range content {
		if idx < len(glyphs) && cell == glyphs[idx].col {
			if string(r) == aboutGlyphOn {
				b.WriteString("\x1b[32m")
			} else {
				b.WriteString("\x1b[31m")
			}
			b.WriteRune(r)
			b.WriteString(restore)
			idx++
		} else {
			b.WriteRune(r)
		}
		cell += runewidth.RuneWidth(r)
	}
	return b.String()
}
