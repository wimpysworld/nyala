package engine

import (
	"context"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Scroll step sizes in CSS pixels for the wheel transport. A line step matches a
// typical wheel notch; a page step is the viewport height, filled in at dispatch
// from the resolved cell geometry.
const wheelLineStepPx = 40

// wheelTickPx is the CSS-pixel delta one SGR wheel event dispatches: three
// wheelLineStepPx lines, the Charm viewport convention. Wheel events are
// accumulable: high-precision devices emit several per physical tick and each
// one dispatches, so the transport sums a burst instead of debouncing it.
const wheelTickPx = 3 * wheelLineStepPx

// inputDispatchTimeout bounds one input dispatch. A wedged browser must drop the
// keystroke rather than park the viewer's key goroutine forever, so each dispatch
// runs under this deadline; on expiry the key is dropped and the failure counted.
const inputDispatchTimeout = 2 * time.Second

// dispatchInput runs one input action under a bounded context so a wedged browser
// drops the keystroke instead of parking the caller goroutine. It records the
// consecutive failure count for debug inspection and never holds e.mu across
// chromedp.Run.
func (e *ChromiumEngine) dispatchInput(ctx context.Context, action chromedp.Action) {
	if ctx == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, inputDispatchTimeout)
	defer cancel()
	err := chromedp.Run(runCtx, action)
	e.mu.Lock()
	if err != nil {
		e.inputFailures++
	} else {
		e.inputFailures = 0
	}
	e.mu.Unlock()
}

// wheelCentre returns the CSS-pixel viewport centre the wheel transport
// dispatches at. DispatchMouseEvent speaks CSS pixels, but cols*cellW and
// rows*cellH are device pixels, so both axes divide by the layout scale Z, the
// same divide pageStepPx applies to its delta; at Z 1 the divide is a no-op.
func wheelCentre(cols, rows, cellW, cellH int, z float64) (cx, cy float64) {
	return float64(cols*cellW) / 2 / z, float64(rows*cellH) / 2 / z
}

// strokeKey describes one named key the engine forwards as a full CDP key
// stroke: rawKeyDown, an optional char, then keyUp. The stroke gives the page
// the browser-native default action: Tab traverses focus with the focus ring,
// Enter activates the focused element. rawKeyDown suppresses Chromium's own
// char synthesis, so a key that produces text spells that text out here;
// printableStroke takes the opposite route for typed characters, letting the
// keyDown generate the keypress from its text.
type strokeKey struct {
	key  string // the DispatchKeyEvent key value, for example "Tab"
	vkey int64  // the Windows and native virtual key code
	text string // the char-event text, empty when the key produces none
}

// strokeKeys maps the named key codes the viewer forwards for native handling.
// Any other code stays on the KeyChar path for a focused element to receive.
// Backspace and Escape carry no char event: Chromium runs both from the key
// down, editing the focused field and dismissing page state respectively.
var strokeKeys = map[string]strokeKey{
	"Tab":       {key: "Tab", vkey: 9},
	"Enter":     {key: "Enter", vkey: 13, text: "\r"},
	"Backspace": {key: "Backspace", vkey: 8},
	"Escape":    {key: "Escape", vkey: 27},
}

// keyStrokeParams builds the CDP key events one full key stroke dispatches,
// in order: rawKeyDown, char when the key produces text (Enter's "\r"), then
// keyUp, each carrying the key value, the physical code, the virtual key
// codes, and the CDP modifier bits. It returns nil for a code with no stroke
// mapping, so the caller falls through to the KeyChar path.
func keyStrokeParams(code string, mod int) []*input.DispatchKeyEventParams {
	sk, ok := strokeKeys[code]
	if !ok {
		return nil
	}
	stamp := func(p *input.DispatchKeyEventParams) *input.DispatchKeyEventParams {
		return p.WithKey(sk.key).
			WithCode(code).
			WithWindowsVirtualKeyCode(sk.vkey).
			WithNativeVirtualKeyCode(sk.vkey).
			WithModifiers(input.Modifier(mod))
	}
	params := []*input.DispatchKeyEventParams{stamp(input.DispatchKeyEvent(input.KeyRawDown))}
	if sk.text != "" {
		params = append(params, stamp(input.DispatchKeyEvent(input.KeyChar)).WithText(sk.text).WithUnmodifiedText(sk.text))
	}
	return append(params, stamp(input.DispatchKeyEvent(input.KeyUp)))
}

// keyStrokeTasks wraps a stroke's key events as one chromedp action, so the
// whole down-char-up sequence runs under a single bounded dispatch.
func keyStrokeTasks(params []*input.DispatchKeyEventParams) chromedp.Tasks {
	tasks := make(chromedp.Tasks, len(params))
	for i, p := range params {
		tasks[i] = p
	}
	return tasks
}

// printableStroke builds the CDP key events one typed printable ASCII
// character dispatches: keyDown carrying the character as its text, then keyUp.
// The keyDown type (not the rawKeyDown the named strokes use) makes Chromium
// generate the keypress from the text itself, so one stroke both types into the
// focused element and gives the key its browser-native default action. That is
// what keeps Space arbitration in the browser with no focus tracking in the
// viewer: a space types into a focused field and scrolls the page otherwise. It
// also means a page that listens on keydown, such as a shortcut handler or an
// in-page editor, sees the key the user pressed. It returns nil for anything
// but a single ASCII character, so the caller inserts multi-byte or longer text
// atomically instead of synthesising a key event per byte.
func printableStroke(text string) []*input.DispatchKeyEventParams {
	if len(text) != 1 || text[0] < 0x20 || text[0] >= 0x7f {
		return nil
	}
	code, vkey := printableKeyIdentity(text[0])
	stamp := func(p *input.DispatchKeyEventParams) *input.DispatchKeyEventParams {
		return p.WithKey(text).
			WithCode(code).
			WithWindowsVirtualKeyCode(vkey).
			WithNativeVirtualKeyCode(vkey)
	}
	down := stamp(input.DispatchKeyEvent(input.KeyDown)).WithText(text).WithUnmodifiedText(text)
	return []*input.DispatchKeyEventParams{down, stamp(input.DispatchKeyEvent(input.KeyUp))}
}

// printableKeyIdentity returns the physical key code and virtual key code for a
// printable ASCII byte, for the characters a keyboard layout cannot disagree
// about: Space, the digits, and the letters, whose virtual key code is the
// upper-case ASCII value. Punctuation sits on layout-specific keys, so it
// carries no code and a zero virtual key code and travels on its text alone,
// which is all typing needs; inventing a US-layout code for it would report a
// key the user may not have pressed.
func printableKeyIdentity(c byte) (code string, vkey int64) {
	switch {
	case c == ' ':
		return "Space", ' '
	case c >= '0' && c <= '9':
		return "Digit" + string(c), int64(c)
	case c >= 'a' && c <= 'z':
		upper := c - ('a' - 'A')
		return "Key" + string(upper), int64(upper)
	case c >= 'A' && c <= 'Z':
		return "Key" + string(c), int64(c)
	default:
		return "", 0
	}
}

// inputCtx snapshots the session context an input dispatch runs under. It is
// nil before the first navigate and after teardown, so the caller drops the
// event rather than dispatching into a dead session.
func (e *ChromiumEngine) inputCtx() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ctx
}

// SendText forwards typed text to the focused element. A single ASCII
// character dispatches a full keyDown/keyUp stroke, so the page sees the key
// press and Chromium keeps its default action for it; longer or multi-byte text
// inserts atomically through Input.insertText, so a composed character never
// depends on per-byte key synthesis. Empty text is a no-op, and the dispatch is
// bounded like every other input, so a wedged browser drops the text rather
// than parking the caller.
func (e *ChromiumEngine) SendText(text string) {
	if text == "" {
		return
	}
	ctx := e.inputCtx()
	if ctx == nil {
		return
	}
	if params := printableStroke(text); params != nil {
		e.dispatchInput(ctx, keyStrokeTasks(params))
		return
	}
	e.dispatchInput(ctx, input.InsertText(text))
}

// SendPaste inserts one pasted unit at the caret through Input.insertText, so
// the whole payload lands atomically however long it is. Paste never takes the
// typed-character stroke SendText uses, not even for a one-character payload: a
// paste is not a key press, so it must report no keydown and run no key default
// action, and a pasted space must not scroll the page. Empty text is a no-op,
// and the dispatch is bounded like every other input, so a wedged browser drops
// the paste rather than parking the caller.
func (e *ChromiumEngine) SendPaste(text string) {
	if text == "" {
		return
	}
	ctx := e.inputCtx()
	if ctx == nil {
		return
	}
	e.dispatchInput(ctx, input.InsertText(text))
}

// edgeScrollPx is the vertical wheel delta one End press dispatches: far past
// any practical document height, so the page clamps at its bottom. The held
// scroll offset inflates past the real clamped offset, which crash recovery
// tolerates (a replay clamps at the bottom again) and a later Home corrects
// exactly, because Home dispatches the negated held target.
const edgeScrollPx = 1 << 24

// scrollToEdge scrolls the page to its top or bottom through the one wheel
// transport, so the engine keeps owning the scroll offset. Home negates the
// held target, landing the target at exactly 0 alongside the page's own top
// clamp; End dispatches edgeScrollPx and lets the page clamp at the bottom.
func (e *ChromiumEngine) scrollToEdge(toEnd bool) {
	if toEnd {
		e.dispatchWheel(0, edgeScrollPx)
		return
	}
	e.mu.Lock()
	dy := -e.scrollYTarget
	e.mu.Unlock()
	if dy != 0 {
		e.dispatchWheel(0, dy)
	}
}

// scrollKeyDelta maps a scroll key to a wheel delta in CSS pixels. It returns
// ok=false for any key that is not a scroll key, so the caller routes it to the
// focused-element key path instead. pageStepPx is the viewport height in CSS
// pixels, used for the Page keys; the line keys ignore it. The delta sign follows
// the wheel convention: positive deltaY scrolls the page content up (toward the
// end), so a down key produces a positive deltaY.
func scrollKeyDelta(code string, pageStepPx int) (dx, dy int, ok bool) {
	switch code {
	case "ArrowDown":
		return 0, wheelLineStepPx, true
	case "ArrowUp":
		return 0, -wheelLineStepPx, true
	case "ArrowRight":
		return wheelLineStepPx, 0, true
	case "ArrowLeft":
		return -wheelLineStepPx, 0, true
	case "PageDown":
		return 0, pageStepPx, true
	case "PageUp":
		return 0, -pageStepPx, true
	default:
		return 0, 0, false
	}
}
