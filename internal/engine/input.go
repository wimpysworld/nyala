package engine

import (
	"context"
	_ "embed"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// wheelLineStepPx is one line step in CSS pixels for the wheel transport, the
// step a typical wheel notch scrolls by.
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
	_ = e.runInput(ctx, action)
}

// runInput is the one bounded input dispatch, returning the outcome for a caller
// that reads a value back from the page (the search seam) rather than only
// sending an event. dispatchInput drops the error, which is all a fire-and-forget
// keystroke needs. A nil context is a dead session, so the action is dropped.
func (e *ChromiumEngine) runInput(ctx context.Context, action chromedp.Action) error {
	if ctx == nil {
		return nil
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
	return err
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
	// scrolls marks a key whose default action scrolls the page when no field
	// holds focus, so its dispatch also adopts the page's settled offset.
	scrolls bool
}

// strokeKeys maps the named key codes the viewer forwards for native handling.
// Any other code stays on the KeyChar path for a focused element to receive.
// Backspace, Delete, and Escape carry no char event: Chromium runs each from
// the key down, the first two editing the focused field and the last
// dismissing page state. A char event would type a character instead.
//
// The caret keys, the arrows plus Home, End, and the Page keys, stroke for the
// same reason Space does: Chromium arbitrates between moving the caret in a
// focused field and scrolling the page, so the viewer needs no focus state of
// its own. Their default action moves the page under the engine's held scroll
// offset, so each dispatch adopts what the page settled at.
//
// KeyA is the letter key select-all rides on. It carries no char text, so the
// stroke is the key press alone and types nothing; the Ctrl bit the caller
// stamps on it is what makes Chromium run its own select-all, in the focused
// field or over the document. A typed letter never comes this way: SendText
// takes the printableStroke route, which is what generates the character.
var strokeKeys = map[string]strokeKey{
	"KeyA":       {key: "a", vkey: 65},
	"Tab":        {key: "Tab", vkey: 9},
	"Enter":      {key: "Enter", vkey: 13, text: "\r"},
	"Backspace":  {key: "Backspace", vkey: 8},
	"Delete":     {key: "Delete", vkey: 46},
	"Escape":     {key: "Escape", vkey: 27},
	"ArrowLeft":  {key: "ArrowLeft", vkey: 37, scrolls: true},
	"ArrowUp":    {key: "ArrowUp", vkey: 38, scrolls: true},
	"ArrowRight": {key: "ArrowRight", vkey: 39, scrolls: true},
	"ArrowDown":  {key: "ArrowDown", vkey: 40, scrolls: true},
	"Home":       {key: "Home", vkey: 36, scrolls: true},
	"End":        {key: "End", vkey: 35, scrolls: true},
	"PageUp":     {key: "PageUp", vkey: 33, scrolls: true},
	"PageDown":   {key: "PageDown", vkey: 34, scrolls: true},
}

// strokeScrolls reports whether a key's browser-native default action can move
// the page's scroll offset, which the engine otherwise owns. It is false for a
// code with no stroke mapping.
func strokeScrolls(code string) bool {
	return strokeKeys[code].scrolls
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

//go:embed paste.js
var pasteTargetSource string

// pasteTargetNone is the paste-target script's answer for a page state
// Chromium will drop the insert from. Its other two answers, "field" for an
// editable target and "unknown" for a page the script cannot read, both mean
// the same thing here: say nothing.
const pasteTargetNone = "none"

// SendPaste inserts one pasted unit at the caret through Input.insertText, so
// the whole payload lands atomically however long it is. Paste never takes the
// typed-character stroke SendText uses, not even for a one-character payload: a
// paste is not a key press, so it must report no keydown and run no key default
// action, and a pasted space must not scroll the page. Empty text is a no-op,
// and the dispatch is bounded like every other input, so a wedged browser drops
// the paste rather than parking the caller.
//
// It reports discarded only when the page proved the insert had no editable
// target, so the viewer can say the paste reached nothing rather than leaving
// the reader to guess. The target read rides the same bounded dispatch as the
// insert, one round trip, and the insert runs either way, so the paste itself
// behaves exactly as it did before. A dropped dispatch and a cross-origin frame
// both report false: neither proves where the text went.
func (e *ChromiumEngine) SendPaste(text string) (discarded bool) {
	if text == "" {
		return false
	}
	ctx := e.inputCtx()
	if ctx == nil {
		return false
	}
	var target string
	if err := e.runInput(ctx, chromedp.Tasks{
		chromedp.Evaluate(pasteTargetSource, &target),
		input.InsertText(text),
	}); err != nil {
		return false
	}
	return target == pasteTargetNone
}

// scrollCellHeightPlaceholder is the token buildScrollScript substitutes the
// snap cell height into.
const scrollCellHeightPlaceholder = "__NYALA_CELL_HEIGHT__"

//go:embed scroll.js
var scrollSource string

// scrollResult is the page's settled scroll offset after a key stroke ran its
// own scroll, which the engine adopts as its held offset so the wheel
// transport and crash recovery keep working from where the page actually sits.
type scrollResult struct {
	ScrollX float64 `json:"sx"`
	ScrollY float64 `json:"sy"`
}

// buildScrollScript returns the page script that snaps and reports the settled
// scroll offset, with cellHeight substituted as the CSS-pixel cell pitch a
// quantising backend snaps to. A zero cell height reports the offset unsnapped.
func buildScrollScript(cellHeight float64) string {
	return strings.Replace(scrollSource, scrollCellHeightPlaceholder,
		strconv.FormatFloat(cellHeight, 'g', -1, 64), 1)
}

// dispatchScrollingStroke runs one caret-key stroke and the scroll adopt read
// in a single bounded dispatch, so the page's settled offset is read in the
// same round trip that pressed the key and a wedged browser still drops the
// keystroke rather than parking the caller. Chromium decides what the key did:
// it moves the caret in a focused field or scrolls the page, and either way the
// read reports where the page ended up. A failed dispatch adopts nothing, so a
// dropped keystroke leaves the held offset as it was.
func (e *ChromiumEngine) dispatchScrollingStroke(ctx context.Context, tasks chromedp.Tasks) {
	e.mu.Lock()
	cellHeight := e.snapCellHeightLocked()
	e.mu.Unlock()
	var res scrollResult
	tasks = append(tasks, chromedp.Evaluate(buildScrollScript(cellHeight), &res))
	if err := e.runInput(ctx, tasks); err != nil {
		return
	}
	e.mu.Lock()
	e.adoptScrollLocked(res.ScrollX, res.ScrollY)
	e.mu.Unlock()
}
