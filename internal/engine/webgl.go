package engine

import (
	"context"
	_ "embed"
	"strings"
	"time"
	"unicode"

	"github.com/chromedp/chromedp"
	"github.com/mattn/go-runewidth"
	"github.com/wimpysworld/nyala/internal/render"
)

//go:embed webgl.js
var webGLSource string

// webGLProbeTimeout bounds the one-off context probe, so a stalled page
// evaluate cannot hold up session start. A timeout leaves the tier unknown.
const webGLProbeTimeout = 2 * time.Second

// webGLLabelMaxWidth caps the vendor label in terminal cells, so the About bar
// keeps room for the browser version beside it.
const webGLLabelMaxWidth = 24

// webGLResult is the value webgl.js returns: the highest context version it
// created (2, 1, or 0 for none) and the vendor and renderer strings Chromium
// reported for that context, empty when no context exists.
type webGLResult struct {
	Version  int    `json:"version"`
	Vendor   string `json:"vendor"`
	Renderer string `json:"renderer"`
}

// mapWebGLResult turns a probe answer into the state About shows. An evaluate
// error or a nil result reports WebGLUnknown, because the probe did not answer.
// Version 0 reports WebGLUnavailable with no label, because the page created no
// context. Versions 1 and 2 report their tier with the compact vendor label.
// Any other version reports WebGLUnknown.
func mapWebGLResult(res *webGLResult, err error) render.WebGLState {
	if err != nil || res == nil {
		return render.WebGLState{Tier: render.WebGLUnknown}
	}
	switch res.Version {
	case 0:
		return render.WebGLState{Tier: render.WebGLUnavailable}
	case 1:
		return render.WebGLState{Tier: render.WebGL1, Label: compactWebGLLabel(res.Vendor, res.Renderer)}
	case 2:
		return render.WebGLState{Tier: render.WebGL2, Label: compactWebGLLabel(res.Vendor, res.Renderer)}
	default:
		return render.WebGLState{Tier: render.WebGLUnknown}
	}
}

// compactWebGLLabel picks the short vendor text About shows in parentheses.
// It prefers the vendor string. When the vendor is empty it takes the first
// comma-separated token inside "ANGLE (...)" in the renderer, which is the
// vendor ANGLE wraps. The text loses control runes and every non-printable
// rune, collapses runs of whitespace to one space, and is cut at a word
// boundary to fit webGLLabelMaxWidth cells. When no whole word fits, the
// label is empty rather than a partial word.
func compactWebGLLabel(vendor, renderer string) string {
	label := printableText(vendor)
	if label == "" {
		label = printableText(angleVendor(renderer))
	}
	return fitWords(label, webGLLabelMaxWidth)
}

// angleVendor returns the first comma-separated token inside the "ANGLE (...)"
// wrapper of a renderer string, or "" when the renderer has no such wrapper.
func angleVendor(renderer string) string {
	_, inner, ok := strings.Cut(renderer, "ANGLE (")
	if !ok {
		return ""
	}
	inner, _, _ = strings.Cut(inner, ")")
	first, _, _ := strings.Cut(inner, ",")
	return first
}

// printableText removes terminal escape sequences and control runes, then
// collapses whitespace runs to one space. Every whitespace rune, including a
// tab, carriage return, or newline, becomes a space before the collapse, so
// the words on each side stay separate. An ESC starts a sequence that
// escapeLen measures and the whole sequence is removed. Any other rune that is
// not printable, which includes every remaining C0 and C1 control, drops out.
func printableText(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == asciiEscape:
			i += escapeLen(runes[i+1:])
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.IsPrint(r):
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// asciiEscape and asciiBell are the C0 controls that open and close terminal
// escape sequences.
const (
	asciiEscape = 0x1b
	asciiBell   = 0x07
)

// escapeLen returns how many runes after an ESC belong to its sequence. A CSI
// sequence runs from "[" to the first final byte in 0x40 to 0x7E. An OSC
// sequence runs from "]" to BEL or to the string terminator ESC "\", and a
// bare ESC inside it ends the sequence without being consumed, so the outer
// pass handles that ESC on its own. Any other ESC takes one following rune.
// A sequence without a terminator takes the rest of the text.
func escapeLen(rest []rune) int {
	if len(rest) == 0 {
		return 0
	}
	switch rest[0] {
	case '[':
		for i := 1; i < len(rest); i++ {
			if rest[i] >= 0x40 && rest[i] <= 0x7e {
				return i + 1
			}
		}
		return len(rest)
	case ']':
		for i := 1; i < len(rest); i++ {
			switch {
			case rest[i] == asciiBell:
				return i + 1
			case rest[i] == asciiEscape && i+1 < len(rest) && rest[i+1] == '\\':
				return i + 2
			case rest[i] == asciiEscape:
				return i
			}
		}
		return len(rest)
	}
	return 1
}

// fitWords returns the longest prefix of whole space-separated words whose
// display width fits in max cells, or "" when the first word alone is too wide.
func fitWords(s string, max int) string {
	if runewidth.StringWidth(s) <= max {
		return s
	}
	var kept string
	for _, word := range strings.Fields(s) {
		next := word
		if kept != "" {
			next = kept + " " + word
		}
		if runewidth.StringWidth(next) > max {
			break
		}
		kept = next
	}
	return kept
}

// probeWebGL runs webgl.js once against the session page and caches the mapped
// state. startSession calls it after fetchBrowserVersion, while the page is
// still about:blank, because context creation needs no document content. A
// cached tier other than WebGLUnknown skips the probe, so a crash rebuild
// reuses the same answer. An evaluate failure or timeout leaves the tier
// unknown, so the next session start tries again.
func (e *ChromiumEngine) probeWebGL(ctx context.Context) {
	e.mu.Lock()
	cached := e.webgl.Tier
	e.mu.Unlock()
	if cached != render.WebGLUnknown {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, webGLProbeTimeout)
	defer cancel()
	var res *webGLResult
	err := chromedp.Run(probeCtx, chromedp.Evaluate(webGLSource, &res))
	state := mapWebGLResult(res, err)
	e.mu.Lock()
	e.webgl = state
	e.mu.Unlock()
}

// WebGL returns the cached WebGL support snapshot the session probe recorded.
// Before the first session, or after a probe failure, the tier is
// WebGLUnknown and the label is empty.
func (e *ChromiumEngine) WebGL() render.WebGLState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.webgl
}
