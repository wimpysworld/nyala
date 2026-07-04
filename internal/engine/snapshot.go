package engine

import (
	"context"
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/chromedp/cdproto/domsnapshot"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render"
)

// snapshotStyles is the computed-style whitelist requested from
// DOMSnapshot.captureSnapshot, in the order the browser returns them per layout
// node. The order is load-bearing: it must match the style index constants
// below, which the decode reads by position.
var snapshotStyles = []string{"color", "visibility", "opacity", "clip", "clip-path", "position"}

// Style index constants into a layout node's Styles entry. The order is
// load-bearing and must match snapshotStyles; a reorder silently misreads the
// computed styles.
const (
	styleColour     = 0
	styleVisibility = 1
	styleOpacity    = 2
	styleClip       = 3
	styleClipPath   = 4
	stylePosition   = 5
)

// captureSnapshot runs DOMSnapshot.captureSnapshot against ctx, requesting the
// computed color, visibility, opacity, clip, clip-path, and position styles, the
// paint order, and the
// blended text-colour opacities. It returns the snapshot documents and the
// shared string table that the document indices refer to. The pure
// decodeGlyphRuns maps that pair to glyph runs without a browser.
func captureSnapshot(ctx context.Context) (documents []*domsnapshot.DocumentSnapshot, strings []string, err error) {
	action := domsnapshot.CaptureSnapshot(snapshotStyles).
		WithIncludePaintOrder(true).
		WithIncludeTextColorOpacities(true)
	// chromedp.Run injects the cdproto executor into the context; calling the
	// action's Do directly returns "invalid context" because the bare session
	// context carries no executor.
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		documents, strings, err = action.Do(runCtx)
		return err
	}))
	return documents, strings, err
}

// captureVerifyBitmap captures the current forced-layout viewport over CDP
// (page.captureScreenshot) and decodes it to an *image.RGBA in RGBA channel order.
// It is the same-generation verification bitmap the ink-anchoring pass reads: the
// snapshot goroutine calls it in the same sequence as the DOMSnapshot read, so the
// bitmap and the decoded runs share one page generation and a card that paints after
// the DOMSnapshot read is captured here, not lagged a frame behind on the screencast.
// The device metrics override is already applied, so the screenshot returns the
// current forced-layout viewport. It is verification-only and never displayed. A
// capture or decode error returns nil so the caller anchors nothing, the same as
// before the pairing landed. It runs on the snapshot goroutine and holds no lock
// across the chromedp.Run.
func captureVerifyBitmap(ctx context.Context) (*image.RGBA, error) {
	var raw []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		data, capErr := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(runCtx)
		if capErr != nil {
			return capErr
		}
		raw = data
		return nil
	}))
	if err != nil {
		return nil, err
	}
	return decodeImageBytes(raw)
}

// decodeGlyphRuns maps a DOM snapshot to crisp text runs without any browser
// dependency, so it is unit-testable on canned structs. It walks every inline
// text box in every document, converts each box's device-pixel bounds to CSS
// pixels by dividing by dpr, subtracts the page scroll offset (the bounds are
// document-absolute and the bitmap is the scrolled view), and bins the box
// top-left to a cell by rounding.
// Each box becomes one render.GlyphRun whose graphemes are clipped to the grid's
// right edge (cellW*cols cells from the box's start column), counting cells with
// the Stage 2 width rule so a width-2 cluster is never half-emitted; overflow
// graphemes are dropped because the bitmap underneath already shows them. The
// box's computed color style feeds GlyphRun.FG in RGBA channel order.
func decodeGlyphRuns(documents []*domsnapshot.DocumentSnapshot, strings []string, dpr, scrollX, scrollY float64, cellW, cellH float64, cols int) []render.GlyphRun {
	if cellW <= 0 || cellH <= 0 || dpr <= 0 {
		return nil
	}
	var runs []render.GlyphRun
	for _, doc := range documents {
		if doc == nil || doc.TextBoxes == nil || doc.Layout == nil {
			continue
		}
		runs = appendDocumentRuns(runs, doc, strings, dpr, scrollX, scrollY, cellW, cellH, cols)
	}
	return runs
}

// appendDocumentRuns decodes one document's text boxes into runs. Each text box
// references a layout node by LayoutIndex; that node carries the full layout
// text and the computed color. The box covers a UTF-16 substring of that text
// given by Start and Length.
func appendDocumentRuns(runs []render.GlyphRun, doc *domsnapshot.DocumentSnapshot, strings []string, dpr, scrollX, scrollY float64, cellW, cellH float64, cols int) []render.GlyphRun {
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
		if boxHidden(layout, strings, layoutIdx, bounds, dpr, cellH) {
			continue
		}
		col, row, clipped := mapBoxToCells(bounds, dpr, scrollX, scrollY, cellW, cellH, cols, text)
		if clipped == "" {
			continue
		}
		runs = append(runs, render.GlyphRun{
			Col:   col,
			Row:   row,
			Text:  clipped,
			FG:    layoutColour(layout, strings, layoutIdx),
			X:     bounds[0]/dpr - scrollX,
			Y:     bounds[1]/dpr - scrollY,
			W:     bounds[2] / dpr,
			H:     bounds[3] / dpr,
			CellW: cellW,
		})
	}
	return runs
}

// mapBoxToCells maps one text box to its cell position and clipped text without
// a browser, so it is unit-testable on plain floats. The bounds are the CDP
// device-pixel [x, y, w, h]; dpr converts them to CSS pixels and the scroll
// offset shifts document-absolute coordinates to the scrolled on-screen view.
// It rounds the box top-left to a cell, trims all trailing whitespace the
// soft-wrap layout adds (a space or a newline), then clips the text to the cells
// left before the box's own right edge and the grid's right edge (cellW*cols).
// The box width is a pure clip bound, never a source of advance: the run's origin
// snap round(px/cellW) is the single source of horizontal position, per-grapheme
// whole-cell stepping (render.ClipToCells, block.applyOverlay) is the single
// source of advance, and the box right edge only caps how far the run may run.
// Snapping both edges the same way (round(right/cellW) minus round(left/cellW))
// keeps a box a rounding epsilon short of N whole cells at N cells, so a
// font-metric residue never drops the last cell, while a wrongly scaled cellW can
// only clip the run short, never push it wide.
// Page insets are arbitrary sub-cell offsets, so round centres the residue at
// zero and stays order-preserving, keeping the bins monotonic and non-overlapping.
func mapBoxToCells(bounds []float64, dpr, scrollX, scrollY float64, cellW, cellH float64, cols int, text string) (col, row int, clipped string) {
	px := bounds[0]/dpr - scrollX
	py := bounds[1]/dpr - scrollY
	bw := bounds[2] / dpr
	col = int(math.Round(px / cellW))
	row = int(math.Round(py / cellH))
	text = strings.TrimRight(text, " \n")
	if text == "" {
		return col, row, ""
	}
	rightCol := int(math.Round((px + bw) / cellW))
	edgeCol := cols
	if rightCol < edgeCol {
		edgeCol = rightCol
	}
	return col, row, render.ClipToCells(text, edgeCol-col)
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
// captureSnapshot requests the snapshotStyles whitelist, so each layout node's
// Styles entry holds one string-table index per style; the colour is at
// styleColour. A missing or unparseable colour falls back to opaque black.
func layoutColour(layout *domsnapshot.LayoutTreeSnapshot, strings []string, layoutIdx int) color.RGBA {
	if layoutIdx < 0 || layoutIdx >= len(layout.Styles) {
		return color.RGBA{A: 0xff}
	}
	style := layout.Styles[layoutIdx]
	if len(style) <= styleColour {
		return color.RGBA{A: 0xff}
	}
	return parseCSSColour(stringAt(strings, int(style[styleColour])))
}

// hiddenOpacityEps is the effective-opacity floor below which a text box is
// treated as invisible and dropped. A visibility:hidden or opacity:0 element
// still owns a DOMSnapshot layout box, so its text would compose phantom runs
// over the page without this drop.
const hiddenOpacityEps = 0.05

// boxHidden reports whether a text box should be dropped as invisible. It drops
// on four conditions: the layout node's computed visibility is hidden or
// collapse; its effective opacity (own computed opacity times the blended text
// opacity) is at or near zero; the box is clip-offscreen (an absolute or fixed
// element whose computed clip-path collapses to inset(50%) or whose computed clip
// rect collapses to a zero or inverted area, the screen-reader-only pattern); or
// the box is subpixel-short in height (below a quarter cell, so it carries no
// legible glyph). An absent visibility style
// means visible, an absent or unparseable opacity means 1, and a nil or short
// TextColorOpacities means 1, so a fixture without these style slots is never
// dropped.
func boxHidden(layout *domsnapshot.LayoutTreeSnapshot, strings []string, layoutIdx int, bounds []float64, dpr float64, cellH float64) bool {
	switch layoutVisibility(layout, strings, layoutIdx) {
	case "hidden", "collapse":
		return true
	}
	if layoutOpacity(layout, strings, layoutIdx)*layoutTextOpacity(layout, layoutIdx) < hiddenOpacityEps {
		return true
	}
	if clipOffscreen(layoutStyle(layout, strings, layoutIdx, stylePosition), layoutStyle(layout, strings, layoutIdx, styleClip), layoutStyle(layout, strings, layoutIdx, styleClipPath)) {
		return true
	}
	return bounds[3]/dpr < cellH/4
}

// clipOffscreen reports whether an element is clipped fully offscreen by the
// screen-reader-only pattern: it must be absolutely or fixed positioned, and
// either its clip-path collapses to inset(50%) or its clip rect collapses to a
// zero or inverted area. A statically positioned element is never offscreen this
// way (its clip has no effect without absolute positioning), so absent or static
// position keeps the box.
func clipOffscreen(position, clip, clipPath string) bool {
	switch strings.TrimSpace(position) {
	case "absolute", "fixed":
	default:
		return false
	}
	if strings.TrimSpace(clipPath) == "inset(50%)" {
		return true
	}
	return clipRectCollapsed(clip)
}

// clipRectCollapsed reports whether a computed clip value is a rect(...) whose
// clipped area collapses to zero or inverts: right at or below left, or bottom at
// or below top. The four offsets are top, right, bottom, left (CSS order),
// separated by commas or spaces. An "auto" offset means the box edge, so it never
// collapses that axis. A non-rect value (clip "auto" or absent) never collapses.
func clipRectCollapsed(clip string) bool {
	clip = strings.TrimSpace(clip)
	open := strings.IndexByte(clip, '(')
	if !strings.HasPrefix(clip, "rect") || open < 0 || !strings.HasSuffix(clip, ")") {
		return false
	}
	fields := strings.FieldsFunc(clip[open+1:len(clip)-1], func(r rune) bool {
		return r == ',' || r == ' '
	})
	if len(fields) != 4 {
		return false
	}
	top, topAuto := clipOffset(fields[0])
	right, rightAuto := clipOffset(fields[1])
	bottom, bottomAuto := clipOffset(fields[2])
	left, leftAuto := clipOffset(fields[3])
	horizontalCollapsed := !rightAuto && !leftAuto && right <= left
	verticalCollapsed := !bottomAuto && !topAuto && bottom <= top
	return horizontalCollapsed || verticalCollapsed
}

// clipOffset parses one clip-rect offset. It returns the numeric CSS-pixel value
// and whether the offset is "auto" (the box edge, which never collapses its axis).
// A "px" suffix is stripped; an unparseable value reads as auto so it never
// collapses.
func clipOffset(s string) (value float64, auto bool) {
	s = strings.TrimSpace(s)
	if s == "auto" {
		return 0, true
	}
	s = strings.TrimSuffix(s, "px")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, true
	}
	return v, false
}

// layoutStyle reads the computed style at styleIdx from a layout node's Styles
// entry, or "" when the slot is absent. It reads any whitelisted style by its
// index constant; the "" default means the style is unset, so a fixture without
// the slot never trips a filter that reads it.
func layoutStyle(layout *domsnapshot.LayoutTreeSnapshot, strings []string, layoutIdx, styleIdx int) string {
	if layoutIdx < 0 || layoutIdx >= len(layout.Styles) {
		return ""
	}
	style := layout.Styles[layoutIdx]
	if len(style) <= styleIdx {
		return ""
	}
	return stringAt(strings, int(style[styleIdx]))
}

// layoutVisibility reads the computed visibility of a layout node, or "" when the
// style slot is absent. The Styles entry holds one string-table index per
// requested style; visibility sits at styleVisibility. An absent entry means the
// element is visible, so the "" default never drops a run.
func layoutVisibility(layout *domsnapshot.LayoutTreeSnapshot, strings []string, layoutIdx int) string {
	if layoutIdx < 0 || layoutIdx >= len(layout.Styles) {
		return ""
	}
	style := layout.Styles[layoutIdx]
	if len(style) <= styleVisibility {
		return ""
	}
	return stringAt(strings, int(style[styleVisibility]))
}

// layoutOpacity reads the computed opacity of a layout node, or 1 when the style
// slot is absent or unparseable. The opacity sits at styleOpacity in the Styles
// entry. The default of 1 keeps a fixture without the opacity slot fully opaque,
// so it is never dropped.
func layoutOpacity(layout *domsnapshot.LayoutTreeSnapshot, strings []string, layoutIdx int) float64 {
	if layoutIdx < 0 || layoutIdx >= len(layout.Styles) {
		return 1
	}
	style := layout.Styles[layoutIdx]
	if len(style) <= styleOpacity {
		return 1
	}
	v, err := strconv.ParseFloat(stringAt(strings, int(style[styleOpacity])), 64)
	if err != nil {
		return 1
	}
	return v
}

// layoutTextOpacity reads the blended text opacity of a layout node from the
// snapshot's TextColorOpacities list, or 1 when the list is nil or too short.
// The blended value folds in ancestor opacity, so an ancestor with opacity:0
// zeroes it even when the element's own opacity is 1. The default of 1 keeps a
// fixture without the list fully opaque, so it is never dropped.
func layoutTextOpacity(layout *domsnapshot.LayoutTreeSnapshot, layoutIdx int) float64 {
	if layoutIdx < 0 || layoutIdx >= len(layout.TextColorOpacities) {
		return 1
	}
	return layout.TextColorOpacities[layoutIdx]
}

// stringAt returns the string-table entry at idx, or "" when idx is out of range.
func stringAt(strings []string, idx int) string {
	if idx < 0 || idx >= len(strings) {
		return ""
	}
	return strings[idx]
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
	out.R = parseColourByte(fields[0])
	out.G = parseColourByte(fields[1])
	out.B = parseColourByte(fields[2])
	if len(fields) >= 4 {
		if a, err := strconv.ParseFloat(fields[3], 64); err == nil {
			out.A = uint8(math.Round(math.Max(0, math.Min(1, a)) * 255))
		}
	}
	return out
}

// parseColourByte parses a numeric CSS colour channel and clamps it to 0..255. A
// fractional value (some serialisers emit floats) rounds to nearest.
func parseColourByte(s string) uint8 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return uint8(math.Round(math.Max(0, math.Min(255, v))))
}
