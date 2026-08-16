package viewer

import emmb64 "github.com/emmansun/base64"

// OSC 52 framing for a clipboard write. The target parameter is `c`,
// CLIPBOARD, and nothing else: `p` would write the X11 PRIMARY selection, which
// is the terminal's own Shift-drag selection channel and not the user's copy.
// The payload is base64, so the copied text cannot carry an escape of its own
// into the terminal parser however hostile the page is. BEL terminates the
// string rather than ST, because BEL is what tmux's built-in Ms capability and
// the ecosystem's copy tools emit, so it is the form terminals are tested on.
//
// The sequence is write-only. Nyala never sends the `?` query form, so it can
// never read the clipboard back, and the page can never reach the user's
// clipboard contents through the viewer.
//
// It goes to the terminal unwrapped, under a multiplexer as well as outside one.
// tmux's default `set-clipboard external` forbids an application from setting
// the clipboard, so a stock tmux drops the copy silently. `set-clipboard on`
// makes tmux handle the sequence itself, which both sets the outer terminal's
// clipboard through its own Ms capability and creates a tmux paste buffer, so
// a copy is pastable inside the session too. Wrapping the sequence in tmux's DCS passthrough instead
// would need `allow-passthrough on`, bypass tmux's own buffer, and assume the
// outer terminal takes raw OSC 52, so the plain form is the better of two paths
// that both need one line of user configuration. zellij and GNU screen handle or
// drop the same plain sequence on their own terms, and neither is given a second
// encoding here.
const (
	osc52Prefix     = "\x1b]52;c;"
	osc52Terminator = "\a"
)

// clipboardMaxBytes bounds the text one clipboard write carries. The de-facto
// OSC 52 sequence limit is 100,000 bytes, of which the framing takes 8 and the
// base64 payload 4 bytes per 3 bytes of text, so 74,994 bytes of text is the
// largest that fits. An over-long sequence is not a clipboard problem but a
// render problem: a terminal that gives up part way through prints the rest of
// the payload over the page, so the bound protects the frame the viewer just
// drew. A selection over it is refused rather than truncated, because a
// silently shortened clipboard is a paste the user discovers is wrong later,
// while a refused copy leaves the clipboard as it was.
const clipboardMaxBytes = 74994

// osc52Clipboard returns the OSC 52 sequence that writes text to the terminal's
// CLIPBOARD selection, or nil when there is nothing to write: empty text, or
// text over clipboardMaxBytes. Returning nil for the over-long case is the
// refusal, so no caller can emit a sequence past the bound.
func osc52Clipboard(text string) []byte {
	if text == "" || len(text) > clipboardMaxBytes {
		return nil
	}
	seq := make([]byte, 0, len(osc52Prefix)+emmb64.StdEncoding.EncodedLen(len(text))+len(osc52Terminator))
	seq = append(seq, osc52Prefix...)
	seq = emmb64.StdEncoding.AppendEncode(seq, []byte(text))
	return append(seq, osc52Terminator...)
}

// copySelection copies the page's current selection to the terminal clipboard,
// the Alt+W binding and the menu's Copy command. Selecting text writes nothing
// on its own: this is the only path to the clipboard, and only an explicit user
// action reaches it.
//
// The page read runs on the serialised worker, so a wedged browser never freezes
// quit or draw, and the worker hands the outcome back to the consumer loop rather
// than writing the terminal itself: OSC 52 is a control sequence, so it must go
// out through the one ordered writer path like a bar repaint. The fallback with
// no worker reads inline, which is the browser-free test path.
//
// Either path reports what happened on the bar: the bytes a completed copy
// wrote, or the refusal an over-bound selection earned, because a silent copy
// and a silent refusal look exactly alike.
func (s *streamSession) copySelection() {
	if s.keySender != nil {
		s.keySender.sendCopy(s.outcomes)
		return
	}
	if o, ok := copyOutcome(s.eng.Selection()); ok {
		s.applyOutcome(o)
	}
}

// writeClipboard emits one clipboard write for text through the ordered control
// path, so the sequence never interleaves with a frame write or a bar repaint
// however busy the page is. Nothing to write emits nothing, so a copy with no
// selection and a refused over-long selection both leave the terminal untouched.
func (s *streamSession) writeClipboard(text string) {
	if seq := osc52Clipboard(text); seq != nil {
		_ = s.emitControl(seq)
	}
}
