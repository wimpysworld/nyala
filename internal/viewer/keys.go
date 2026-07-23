package viewer

import (
	"bufio"
	"io"
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
	keyQuitCtrlC = 0x03
	keyTab       = 0x09
	keyEnter     = 0x0d
	keyQuitCtrlQ = 0x11
	keyCtrlR     = 0x12
	keyEsc       = 0x1b
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
	default:
		return actionNone, false
	}
}

// decodeCSI maps a CSI sequence body (the bytes after ESC [) to a scroll key.
// It recognises the arrow finals A/B/C/D, the PageUp/PageDown sequences 5~/6~,
// and both encodings each of Home (H, 1~) and End (F, 4~). It returns
// ScrollNone, false for any other body, so an unrecognised escape sequence is
// ignored. The reader strips the leading ESC [ before calling it.
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
	case "H", "1~":
		return ScrollHome, true
	case "F", "4~":
		return ScrollEnd, true
	default:
		return ScrollNone, false
	}
}

// decodeCSIAction maps a CSI sequence body to a viewer command: the backtab
// (Z, how Shift+Tab arrives), F5 (15~), and the Alt-modified cursor keys
// (modifier parameter 3): Alt+Left 1;3D, Alt+Right 1;3C, and Alt+Home in
// both its 1;3H and 1;3~ encodings, mirroring the two plain Home forms. ok
// is false for any other body.
func decodeCSIAction(body []byte) (keyAction, bool) {
	switch string(body) {
	case "Z":
		return actionFocusPrev, true
	case "15~":
		return actionReload, true
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
// ScrollNone), a recognised scroll key (scroll set), a decoded escape-sequence
// command (act set), a decoded SGR mouse report (mouse set), or a Kitty
// graphics response (apc set). The reader resolves escape sequences so the
// consumer handles whole events, not raw bytes; single-byte commands (Tab,
// Enter, Ctrl+Q) stay byte events the dispatcher maps through byteKeyAction.
type keyEvent struct {
	b      byte        // the single key byte when scroll is ScrollNone
	scroll ScrollKey   // the recognised scroll key, or ScrollNone for a byte
	act    keyAction   // the decoded escape-sequence command, or actionNone
	apc    string      // Kitty graphics response body after ESC _ G, e.g. "i=1;OK"; "" otherwise
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

// nextEvent turns one read byte into a keyEvent. A non-escape byte is a byte
// event. An ESC that opens a CSI sequence (next byte '[') is read to its final
// byte (0x40..0x7e) and decoded; a recognised sequence is a scroll, action, or
// mouse event, an SGR motion report is consumed and dropped (motion dispatch
// is deferred), and an unrecognised or malformed sequence is dropped (emit
// false) without leaking its bytes as keys. An ESC that opens an APC (next
// byte '_') is read to its String Terminator; a Kitty graphics response (body
// leading with 'G') emits an apc event for the shm pacing gate, any other APC
// is dropped. An ESC followed by an Alt-chord letter (altKeyAction) is that
// chord's action event. A bare ESC, or ESC followed by any other byte, is
// passed through as a byte event so it is not swallowed; a lone ESC does
// nothing downstream, so no disambiguation timer is needed. It reads only the
// bytes the sequence needs, so it does not block on the next key.
func nextEvent(br *bufio.Reader, b byte) (keyEvent, bool) {
	if b != keyEsc {
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
