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
)

// keyQuitCtrlC is the Ctrl-C byte; keyTab is the Tab byte; keyEsc is the escape
// byte that opens a CSI sequence.
const (
	keyQuitCtrlC = 0x03
	keyTab       = 0x09
	keyEsc       = 0x1b
)

// decodeCSI maps a CSI sequence body (the bytes after ESC [) to a scroll key. It
// recognises the arrow finals A/B/C/D and the PageUp/PageDown sequences 5~/6~.
// It returns ScrollNone, false for any other body, so an unrecognised escape
// sequence is ignored. The reader strips the leading ESC [ before calling it.
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
	default:
		return ScrollNone, false
	}
}

// keyEvent is one parsed key from the reader: a single byte (b, with scroll ==
// ScrollNone), a recognised scroll key (scroll set), a decoded SGR mouse
// report (mouse set), or a Kitty graphics response (apc set). The reader
// resolves escape sequences so the consumer handles whole events, not raw
// bytes.
type keyEvent struct {
	b      byte        // the single key byte when scroll is ScrollNone
	scroll ScrollKey   // the recognised scroll key, or ScrollNone for a byte
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
// byte (0x40..0x7e) and decoded; a recognised sequence is a scroll or mouse
// event, an SGR motion report is consumed and dropped (motion dispatch is
// deferred), and an unrecognised or malformed sequence is dropped (emit
// false) without leaking its bytes as keys. An ESC that opens an APC (next
// byte '_') is read to its String Terminator; a Kitty graphics response (body
// leading with 'G') emits an apc event for the shm pacing gate, any other APC
// is dropped. A bare ESC, or ESC not followed by '[' or '_', is passed through
// as a byte event so it is not swallowed. It reads only the bytes the sequence
// needs, so it does not block on the next key.
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
