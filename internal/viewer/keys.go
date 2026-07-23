package viewer

import (
	"bufio"
	"io"
	"unicode/utf8"
)

// ScrollKey is a recognised scroll key. The viewer does not track scroll state
// (the engine owns the offset); the push consumer forwards the pressed key to
// engine.SendKey. ScrollNone is the zero value: no scroll key.
type ScrollKey int

const (
	// ScrollNone is the zero value: the key is not a scroll key.
	ScrollNone ScrollKey = iota
	// ScrollUp is the Up arrow (ESC [ A).
	ScrollUp
	// ScrollDown is the Down arrow (ESC [ B).
	ScrollDown
	// ScrollRight is the Right arrow (ESC [ C).
	ScrollRight
	// ScrollLeft is the Left arrow (ESC [ D).
	ScrollLeft
	// ScrollPageUp is the PageUp key (ESC [ 5 ~).
	ScrollPageUp
	// ScrollPageDown is the PageDown key (ESC [ 6 ~).
	ScrollPageDown
	// ScrollHome is the Home key (ESC [ H or ESC [ 1 ~); it scrolls to the top.
	ScrollHome
	// ScrollEnd is the End key (ESC [ F or ESC [ 4 ~); it scrolls to the bottom.
	ScrollEnd
)

// Raw-mode control bytes the reader and the dispatcher recognise. keyEsc opens
// an escape sequence; the rest arrive as single bytes. Ctrl-Q reaches the
// reader only because raw mode clears IXON, the XON/XOFF flow-control flag
// that would otherwise consume it; setupViewer's raw-mode seam is pinned on
// that flag by test.
const (
	keyCtrlA     = 0x01
	keyQuitCtrlC = 0x03
	keyCtrlE     = 0x05
	keyBackspace = 0x08
	keyTab       = 0x09
	keyCtrlK     = 0x0b
	keyCtrlL     = 0x0c
	keyEnter     = 0x0d
	keyQuitCtrlQ = 0x11
	keyCtrlR     = 0x12
	keyCtrlU     = 0x15
	keyCtrlW     = 0x17
	keyEsc       = 0x1b
	keyDel       = 0x7f
)

// keyAction is a recognised viewer command decoded from the input stream: a
// forwarded page key, a navigation request, a quit, or a render-mode cycle.
// actionNone is the zero value: the event carries no command.
type keyAction int

const (
	// actionNone means the event maps to no viewer command.
	actionNone keyAction = iota
	// actionQuit ends the session (Ctrl+Q, with Ctrl+C as the interrupt
	// fallback so a wedged page can never trap the user).
	actionQuit
	// actionFocusNext forwards Tab to the page for native focus traversal.
	actionFocusNext
	// actionFocusPrev forwards Shift+Tab (CSI Z backtab) to the page.
	actionFocusPrev
	// actionActivate forwards Enter to the page to activate the focused element.
	actionActivate
	// actionReload reloads the page (Ctrl+R or F5).
	actionReload
	// actionBack goes one history entry back (Alt+Left).
	actionBack
	// actionForward goes one history entry forward (Alt+Right).
	actionForward
	// actionOpenHome navigates to the launch URL (Alt+Home).
	actionOpenHome
	// actionCycleBackend cycles the render backend forwards (Alt+R).
	actionCycleBackend
	// actionCycleBackendReverse cycles the render backend backwards (Alt+Shift+R).
	actionCycleBackendReverse
	// actionCycleBlocks cycles the Block glyph scheme (Alt+B).
	actionCycleBlocks
	// actionCycleBlocksReverse cycles the Block glyph scheme backwards (Alt+Shift+B).
	actionCycleBlocksReverse
	// actionCycleColour cycles the Block colour mode (Alt+C).
	actionCycleColour
	// actionCycleColourReverse cycles the Block colour mode backwards (Alt+Shift+C).
	actionCycleColourReverse
	// actionEditURL enters the bar's URL edit mode (Ctrl+L).
	actionEditURL
	// actionOpenMenu opens the bar's menu mode (Alt+F, Chromium's menu key;
	// F10 is often grabbed by terminal emulators).
	actionOpenMenu
	// actionDelete is the Delete key (CSI 3~). The chrome dispatch ignores
	// it; the URL editor deletes the rune under the cursor.
	actionDelete
)

// byteKeyAction maps a single key byte to its viewer command. Plain printable
// bytes map to none: they stay free for future page form input, which is why
// plain q no longer quits (Ctrl+Q does).
func byteKeyAction(b byte) keyAction {
	switch b {
	case keyQuitCtrlQ, keyQuitCtrlC:
		return actionQuit
	case keyCtrlR:
		return actionReload
	case keyCtrlL:
		return actionEditURL
	case keyTab:
		return actionFocusNext
	case keyEnter:
		return actionActivate
	default:
		return actionNone
	}
}

// altKeyAction maps an ESC-prefixed letter (an Alt chord: most terminals send
// Alt+letter as ESC then the letter, with Shift carried by its case) to its
// cycle command. ok is false for any other byte, so an unrelated ESC-prefixed
// byte keeps the bare-ESC passthrough behaviour.
func altKeyAction(b byte) (keyAction, bool) {
	switch b {
	case 'r':
		return actionCycleBackend, true
	case 'R':
		return actionCycleBackendReverse, true
	case 'b':
		return actionCycleBlocks, true
	case 'B':
		return actionCycleBlocksReverse, true
	case 'c':
		return actionCycleColour, true
	case 'C':
		return actionCycleColourReverse, true
	case 'f':
		return actionOpenMenu, true
	default:
		return actionNone, false
	}
}

// decodeCSI maps a CSI sequence body (the bytes after ESC [) to a scroll key.
// It recognises the arrow finals A/B/C/D, the PageUp/PageDown sequences 5~/6~,
// and every encoding of Home (H, 1~, and the rxvt 7~) and End (F, 4~, and the
// rxvt 8~). It returns ScrollNone, false for any other body, so an
// unrecognised escape sequence is ignored. The reader strips the leading
// ESC [ before calling it.
func decodeCSI(body []byte) (ScrollKey, bool) {
	switch string(body) {
	case "A":
		return ScrollUp, true
	case "B":
		return ScrollDown, true
	case "C":
		return ScrollRight, true
	case "D":
		return ScrollLeft, true
	case "5~":
		return ScrollPageUp, true
	case "6~":
		return ScrollPageDown, true
	case "H", "1~", "7~":
		return ScrollHome, true
	case "F", "4~", "8~":
		return ScrollEnd, true
	default:
		return ScrollNone, false
	}
}

// decodeCSIAction maps a CSI sequence body to a viewer command: the backtab
// (Z, how Shift+Tab arrives), F5 (15~), Delete (3~), and the Alt-modified
// cursor keys (modifier parameter 3): Alt+Left 1;3D, Alt+Right 1;3C, and
// Alt+Home in both its 1;3H and 1;3~ encodings, mirroring the two plain Home
// forms. ok is false for any other body.
func decodeCSIAction(body []byte) (keyAction, bool) {
	switch string(body) {
	case "Z":
		return actionFocusPrev, true
	case "15~":
		return actionReload, true
	case "3~":
		return actionDelete, true
	case "1;3D":
		return actionBack, true
	case "1;3C":
		return actionForward, true
	case "1;3H", "1;3~":
		return actionOpenHome, true
	default:
		return actionNone, false
	}
}

// keyEvent is one parsed key from the reader: a single byte (b, with scroll ==
// ScrollNone), an assembled multi-byte UTF-8 rune (r set), a recognised scroll
// key (scroll set), a decoded escape-sequence command (act set), a decoded SGR
// mouse report (mouse set), a bracketed-paste payload (paste set), or a Kitty
// graphics response (apc set). The reader resolves escape sequences so the
// consumer handles whole events, not raw bytes; single-byte commands (Tab,
// Enter, Ctrl+Q) stay byte events the dispatcher maps through byteKeyAction.
type keyEvent struct {
	b      byte        // the single key byte when scroll is ScrollNone
	r      rune        // an assembled multi-byte UTF-8 rune; 0 for any other event
	scroll ScrollKey   // the recognised scroll key, or ScrollNone for a byte
	act    keyAction   // the decoded escape-sequence command, or actionNone
	apc    string      // Kitty graphics response body after ESC _ G, e.g. "i=1;OK"; "" otherwise
	paste  string      // bracketed-paste payload delivered as one unit; "" otherwise
	mouse  *MouseEvent // decoded SGR mouse report; nil otherwise
}

// readKeys reads keys from r and sends them on the returned channel, closing it
// on EOF or read error. It runs in its own goroutine so Run can select between
// keys and resize signals. A CSI escape sequence (ESC [ ... final) is parsed
// into a scroll key via decodeCSI; a recognised sequence emits one scroll
// keyEvent and an unrecognised one is dropped. Every other byte emits a byte
// keyEvent. The returned channel is buffered size 1, and each send selects on
// done, so a send cannot block forever once the consumer stops: the goroutine
// exits when done closes or on read EOF/error. A blocking raw-fd Read may keep
// the goroutine parked until the next byte arrives, after which the done select
// returns; this is not instant teardown.
func readKeys(r io.Reader, done <-chan struct{}) <-chan keyEvent {
	out := make(chan keyEvent, 1)
	go func() {
		defer close(out)
		br := bufio.NewReader(r)
		for {
			b, err := br.ReadByte()
			if err != nil {
				return
			}
			ev, emit := nextEvent(br, b)
			if emit {
				select {
				case out <- ev:
				case <-done:
					return
				}
			}
		}
	}()
	return out
}

// nextEvent turns one read byte into a keyEvent. A non-escape ASCII byte is a
// byte event; a UTF-8 leading byte is assembled into a whole rune event. An
// ESC that opens a CSI sequence (next byte '[') is read to its final byte
// (0x40..0x7e) and decoded; a recognised sequence is a scroll, action, paste,
// or mouse event, an SGR motion report is consumed and dropped (motion
// dispatch is deferred), and an unrecognised or malformed sequence is dropped
// (emit false) without leaking its bytes as keys. An ESC that opens an APC
// (next byte '_') is read to its String Terminator; a Kitty graphics response
// (body leading with 'G') emits an apc event for the shm pacing gate, any
// other APC is dropped. An ESC followed by an Alt-chord letter (altKeyAction)
// is that chord's action event. A lone Esc press is detected by the
// buffered-byte check: a terminal delivers an escape sequence in one write,
// so its bytes share one read burst and sit buffered behind the ESC, while a
// bare Esc key arrives with nothing buffered and emits an ESC byte event
// immediately. An ESC followed by any other buffered byte also passes through
// as an ESC byte event so it is not swallowed. It reads only the bytes the
// sequence needs, so it does not block on the next key.
func nextEvent(br *bufio.Reader, b byte) (keyEvent, bool) {
	if b != keyEsc {
		if b >= 0x80 {
			return assembleUTF8(br, b)
		}
		return keyEvent{b: b}, true
	}
	if br.Buffered() == 0 {
		return keyEvent{b: b}, true
	}
	next, err := br.ReadByte()
	if err != nil {
		return keyEvent{b: b}, true
	}
	if next == '_' {
		body, ok := readAPCBody(br)
		if !ok || len(body) == 0 || body[0] != 'G' {
			return keyEvent{}, false
		}
		return keyEvent{apc: string(body[1:])}, true
	}
	if next != '[' {
		if act, ok := altKeyAction(next); ok {
			return keyEvent{act: act}, true
		}
		_ = br.UnreadByte()
		return keyEvent{b: b}, true
	}
	body := []byte{}
	for {
		c, err := br.ReadByte()
		if err != nil {
			return keyEvent{}, false
		}
		if c >= 0x40 && c <= 0x7e {
			body = append(body, c)
			break
		}
		body = append(body, c)
	}
	if string(body) == pasteBegin {
		paste, ok := readPasteBody(br)
		if !ok || paste == "" {
			return keyEvent{}, false
		}
		return keyEvent{paste: paste}, true
	}
	if m, motion, ok := decodeSGRMouse(body); ok {
		if motion {
			return keyEvent{}, false
		}
		return keyEvent{mouse: &m}, true
	}
	if k, ok := decodeCSI(body); ok {
		return keyEvent{scroll: k}, true
	}
	if act, ok := decodeCSIAction(body); ok {
		return keyEvent{act: act}, true
	}
	return keyEvent{}, false
}

// assembleUTF8 assembles one multi-byte UTF-8 rune from its leading byte and
// the continuation bytes behind it, so a typed or pasted non-ASCII character
// arrives downstream as a whole rune event, never as raw bytes. A stray
// continuation byte, an invalid leading byte, or a malformed sequence is
// dropped; a non-continuation byte that cuts a sequence short is unread so it
// is not swallowed with the malformed rune.
func assembleUTF8(br *bufio.Reader, b byte) (keyEvent, bool) {
	var n int
	switch {
	case b&0xe0 == 0xc0:
		n = 2
	case b&0xf0 == 0xe0:
		n = 3
	case b&0xf8 == 0xf0:
		n = 4
	default:
		return keyEvent{}, false
	}
	buf := make([]byte, 1, n)
	buf[0] = b
	for len(buf) < n {
		c, err := br.ReadByte()
		if err != nil {
			return keyEvent{}, false
		}
		if c&0xc0 != 0x80 {
			_ = br.UnreadByte()
			return keyEvent{}, false
		}
		buf = append(buf, c)
	}
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size <= 1 {
		return keyEvent{}, false
	}
	return keyEvent{r: r}, true
}

// Bracketed-paste framing. The terminal wraps a paste in CSI 200~ and
// CSI 201~ while mode 2004 is on; the parser delivers the payload between
// them as one paste event. maxPasteBody bounds the payload so a lost
// terminator cannot grow the buffer without limit.
const (
	pasteBegin   = "200~"
	pasteEnd     = "\x1b[201~"
	maxPasteBody = 1 << 20
)

// readPasteBody reads a bracketed-paste payload up to its CSI 201~
// terminator and returns the bytes between the markers. ok is false on a
// read error or an over-long payload, and the paste is dropped.
func readPasteBody(br *bufio.Reader) (string, bool) {
	var body []byte
	matched := 0
	for len(body) <= maxPasteBody {
		c, err := br.ReadByte()
		if err != nil {
			return "", false
		}
		if c == pasteEnd[matched] {
			matched++
			if matched == len(pasteEnd) {
				return string(body), true
			}
			continue
		}
		// The partial terminator match was payload after all: flush it,
		// then restart the match against this byte.
		body = append(body, pasteEnd[:matched]...)
		matched = 0
		if c == pasteEnd[0] {
			matched = 1
			continue
		}
		body = append(body, c)
	}
	return "", false
}

// maxAPCBody bounds an APC body read, so garbage input that opens an APC but
// never terminates it cannot grow the buffer without limit. A Kitty graphics
// response is a few dozen bytes.
const maxAPCBody = 4096

// readAPCBody reads an APC body up to its String Terminator (ESC backslash)
// and returns the bytes between the introducer and the terminator. ok is false
// on a read error or an over-long body, and the event is dropped.
func readAPCBody(br *bufio.Reader) ([]byte, bool) {
	var body []byte
	for len(body) <= maxAPCBody {
		c, err := br.ReadByte()
		if err != nil {
			return nil, false
		}
		if c != keyEsc {
			body = append(body, c)
			continue
		}
		n, err := br.ReadByte()
		if err != nil {
			return nil, false
		}
		if n == '\\' {
			return body, true
		}
		body = append(body, c, n)
	}
	return nil, false
}
