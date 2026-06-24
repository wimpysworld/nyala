package engine

import (
	"context"
	"image/color"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/chromedp/cdproto/domsnapshot"
	"github.com/chromedp/chromedp"
	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/wimpysworld/nyala/internal/render"
)

// captureSnapshot runs DOMSnapshot.captureSnapshot against ctx, requesting the
// computed color style and the paint order. It returns the snapshot documents
// and the shared string table that the document indices refer to. The pure
// decodeGlyphRuns maps that pair to glyph runs without a browser.
func captureSnapshot(ctx context.Context) (documents []*domsnapshot.DocumentSnapshot, strings []string, err error) {
	action := domsnapshot.CaptureSnapshot([]string{"color"}).
		WithIncludePaintOrder(true)
	// chromedp.Run injects the cdproto executor into the context; calling the
	// action's Do directly returns "invalid context" because the bare session
	// context carries no executor.
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		documents, strings, err = action.Do(runCtx)
		return err
	}))
	return documents, strings, err
}

// decodeGlyphRuns maps a DOM snapshot to crisp text runs without any browser
// dependency, so it is unit-testable on canned structs. It walks every inline
// text box in every document, converts each box's device-pixel bounds to CSS
// pixels by dividing by dpr, re-adds the page scroll offset (the snapshot bounds
// ignore it), and bins the box top-left to a cell with the Stage 2 floor rule.
// Each box becomes one render.GlyphRun whose graphemes are clipped to
// floor(boxWidthPx/cellW) display cells, counting cells with the Stage 2 width
// rule so a width-2 cluster is never half-emitted; overflow graphemes are
// dropped because the bitmap underneath already shows them. The box's computed
// color style feeds GlyphRun.FG in RGBA channel order.
func decodeGlyphRuns(documents []*domsnapshot.DocumentSnapshot, strings []string, dpr, scrollX, scrollY float64, cellW, cellH int) []render.GlyphRun {
	if cellW <= 0 || cellH <= 0 || dpr <= 0 {
		return nil
	}
	var runs []render.GlyphRun
	for _, doc := range documents {
		if doc == nil || doc.TextBoxes == nil || doc.Layout == nil {
			continue
		}
		runs = appendDocumentRuns(runs, doc, strings, dpr, scrollX, scrollY, cellW, cellH)
	}
	return runs
}

// appendDocumentRuns decodes one document's text boxes into runs. Each text box
// references a layout node by LayoutIndex; that node carries the full layout
// text and the computed color. The box covers a UTF-16 substring of that text
// given by Start and Length.
func appendDocumentRuns(runs []render.GlyphRun, doc *domsnapshot.DocumentSnapshot, strings []string, dpr, scrollX, scrollY float64, cellW, cellH int) []render.GlyphRun {
	boxes := doc.TextBoxes
	layout := doc.Layout
	for i := range boxes.LayoutIndex {
		if i >= len(boxes.Bounds) || i >= len(boxes.Start) || i >= len(boxes.Length) {
			break
		}
		layoutIdx := int(boxes.LayoutIndex[i])
		text := layoutBoxText(layout, strings, layoutIdx, boxes.Start[i], boxes.Length[i])
		if text == "" {
			continue
		}
		bounds := boxes.Bounds[i]
		if len(bounds) < 4 {
			continue
		}
		px := bounds[0]/dpr + scrollX
		py := bounds[1]/dpr + scrollY
		col := int(math.Floor(px / float64(cellW)))
		row := int(math.Floor(py / float64(cellH)))
		maxCells := int(math.Floor((bounds[2] / dpr) / float64(cellW)))
		clipped := clipToCells(text, maxCells)
		if clipped == "" {
			continue
		}
		runs = append(runs, render.GlyphRun{
			Col:  col,
			Row:  row,
			Text: clipped,
			FG:   layoutColour(layout, strings, layoutIdx),
		})
	}
	return runs
}

// layoutBoxText returns the box's substring of its layout node's text. Start and
// length count UTF-16 code units, matching the CDP textBox convention, so the
// slice is taken in UTF-16 space and re-encoded to a Go string.
func layoutBoxText(layout *domsnapshot.LayoutTreeSnapshot, strings []string, layoutIdx int, start, length int64) string {
	if layoutIdx < 0 || layoutIdx >= len(layout.Text) {
		return ""
	}
	full := stringAt(strings, int(layout.Text[layoutIdx]))
	if full == "" || length <= 0 {
		return ""
	}
	units := utf16.Encode([]rune(full))
	from := int(start)
	to := from + int(length)
	if from < 0 || from >= len(units) {
		return ""
	}
	if to > len(units) {
		to = len(units)
	}
	return string(utf16.Decode(units[from:to]))
}

// layoutColour reads the computed color of a layout node and maps it to RGBA.
// captureSnapshot was called with the single style color, so each layout node's
// Styles entry holds one string-table index: the serialised CSS colour. A
// missing or unparseable colour falls back to opaque black.
func layoutColour(layout *domsnapshot.LayoutTreeSnapshot, strings []string, layoutIdx int) color.RGBA {
	if layoutIdx < 0 || layoutIdx >= len(layout.Styles) {
		return color.RGBA{A: 0xff}
	}
	style := layout.Styles[layoutIdx]
	if len(style) == 0 {
		return color.RGBA{A: 0xff}
	}
	return parseCSSColour(stringAt(strings, int(style[0])))
}

// stringAt returns the string-table entry at idx, or "" when idx is out of range.
func stringAt(strings []string, idx int) string {
	if idx < 0 || idx >= len(strings) {
		return ""
	}
	return strings[idx]
}

// clipToCells trims text to at most maxCells display cells, measuring each
// grapheme cluster with the Stage 2 width rule. A width-2 cluster that would
// exceed the budget is dropped whole, never half-emitted. A non-positive
// maxCells yields the empty string.
func clipToCells(text string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		cluster := gr.Str()
		w := cellWidth(cluster)
		if used+w > maxCells {
			break
		}
		b.WriteString(cluster)
		used += w
	}
	return b.String()
}

// cellWidth measures the display width of one grapheme cluster, clamped to 1 or
// 2, mirroring the Stage 2 block overlay rule so the clip budget and the
// downstream cell binning agree. A width-0 cluster counts as 1.
func cellWidth(grapheme string) int {
	if runewidth.StringWidth(grapheme) >= 2 {
		return 2
	}
	return 1
}

// parseCSSColour parses a serialised CSS colour from a computed style into RGBA.
// Computed color serialises as rgb(r, g, b) or rgba(r, g, b, a); the alpha is a
// 0..1 float scaled to 0..255. An unrecognised value returns opaque black.
func parseCSSColour(s string) color.RGBA {
	s = strings.TrimSpace(s)
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return color.RGBA{A: 0xff}
	}
	fields := strings.FieldsFunc(s[open+1:len(s)-1], func(r rune) bool {
		return r == ',' || r == '/' || r == ' '
	})
	if len(fields) < 3 {
		return color.RGBA{A: 0xff}
	}
	out := color.RGBA{A: 0xff}
	out.R = clampByte(fields[0])
	out.G = clampByte(fields[1])
	out.B = clampByte(fields[2])
	if len(fields) >= 4 {
		if a, err := strconv.ParseFloat(fields[3], 64); err == nil {
			out.A = uint8(math.Round(math.Max(0, math.Min(1, a)) * 255))
		}
	}
	return out
}

// clampByte parses a numeric CSS colour channel and clamps it to 0..255. A
// fractional value (some serialisers emit floats) rounds to nearest.
func clampByte(s string) uint8 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return uint8(math.Round(math.Max(0, math.Min(255, v))))
}
