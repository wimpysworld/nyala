package viewer

import (
	"bufio"
	"io"
	"strings"
	"unicode/utf8"
)

// ScrollKey is a recognised caret key. The viewer does not track scroll or
// focus state (the engine owns the offset and Chromium owns focus); the push
// consumer forwards the pressed key to engine.SendKey, and the page decides
// whether it moves the caret or scrolls. ScrollNone is the zero value: no
// caret key.
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
	// ScrollHome is the Home key (ESC [ H or ESC [ 1 ~): the line start in a
	// focused field, the document top otherwise.
	ScrollHome
	// ScrollEnd is the End key (ESC [ F or ESC [ 4 ~): the line end in a
	// focused field, the document bottom otherwise.
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
	keyCtrlF     = 0x06
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
	// actionOpenFind enters the bar's find mode (Ctrl+F, the desktop-browser
	// binding; the byte is otherwise unbound and reaches the page as nothing).
	actionOpenFind
	// actionOpenMenu opens the bar's menu mode (Alt+F, Chromium's menu key;
	// F10 is often grabbed by terminal emulators).
	actionOpenMenu
	// actionSelectAll forwards Ctrl+A to the page, the desktop-browser
	// select-all chord. Chromium decides what it selects: the focused field's
	// text, or the whole document when nothing holds focus. The byte keeps its
	// readline meaning inside the bar's text fields, which claim every key
	// before chrome mode sees one.
	actionSelectAll
	// actionCopySelection copies the page's selection to the terminal clipboard
	// (Alt+W, the Emacs kill-ring-save chord). Ctrl+C quits and Ctrl+Shift+C is
	// the terminal's own copy, which a legacy terminal cannot even report apart
	// from Ctrl+C, so the copy chord lives with the other Alt commands.
	actionCopySelection
	// actionDelete forwards Delete (CSI 3~) to the page so the focused field
	// deletes forward. The bar's text fields claim it first and delete the
	// rune under their own cursor.
	actionDelete
	// actionBackspace forwards Backspace to the page so the focused field
	// edits. Terminals send it as either 0x08 or DEL.
	actionBackspace
	// actionEscape forwards Escape to the page. It is reached only in chrome:
	// a bar mode claims Escape first and closes itself.
	actionEscape
)

// byteKeyAction maps a single key byte to its viewer command. A printable byte
// maps to none: it is page text, forwarded by the chrome dispatch rather than
// bound to a command, which is why plain q does not quit (Ctrl+Q does).
func byteKeyAction(b byte) keyAction {
	switch b {
	case keyQuitCtrlQ, keyQuitCtrlC:
		return actionQuit
	case keyCtrlR:
		return actionReload
	case keyCtrlL:
		return actionEditURL
	case keyCtrlF:
		return actionOpenFind
	case keyCtrlA:
		return actionSelectAll
	case keyTab:
		return actionFocusNext
	case keyEnter:
		return actionActivate
	case keyBackspace, keyDel:
		return actionBackspace
	case keyEsc:
		return actionEscape
	default:
		return actionNone
	}
}

// printableByte reports whether b is a printable ASCII byte (Space through
// tilde). Those bytes are text: the chrome dispatch forwards them to the
// focused page element and the URL editor inserts them.
func printableByte(b byte) bool {
	return b >= 0x20 && b < keyDel
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
	case 'w':
		return actionCopySelection, true
	default:
		return actionNone, false
	}
}

// Kitty keyboard-protocol control sequences. The query asks the terminal for its
// current progressive-enhancement flags; only an implementing terminal answers,
// which is how the handshake detects support. The enable pushes Nyala's flags
// onto the terminal's mode stack and the disable pops that one entry, so a
// terminal already running the protocol for an outer application (a multiplexer,
// a shell) gets its own state back untouched.
//
// The pushed flag set is 1, disambiguate escape codes, and nothing else.
// Disambiguation alone delivers what Nyala binds: Escape arrives as a report
// rather than a byte whose meaning depends on what follows it, and the chords
// with ambiguous or absent legacy encodings (Shift+Enter, the Alt letters)
// arrive as reports carrying their modifiers. The other flags are deliberately
// not requested. Event types (2) would add key-release reports that double every
// keystroke, report-all-keys (8) would move plain typing onto the report path
// for no gain, and alternate keys (4) and associated text (16) add parsing
// surface and per-terminal variation Nyala has no use for.
const (
	queryKittyKeyboard   = "\x1b[?u"
	enableKittyKeyboard  = "\x1b[>1u"
	disableKittyKeyboard = "\x1b[<u"
)

// Kitty keyboard-protocol key codes for the functional keys Nyala binds. A text
// key reports the codepoint of its unshifted character, so the letter chords
// decode through the legacy tables by lower-casing that codepoint.
const (
	kittyKeyTab       = 9
	kittyKeyEnter     = 13
	kittyKeyEscape    = 27
	kittyKeyBackspace = 127
)

// Kitty keyboard-protocol modifier bits, one less than the reported value: the
// report carries 1 plus the bitmask, so a plain key reports 1 and Shift+Enter
// reports 2.
const (
	kittyModShift = 1
	kittyModAlt   = 2
	kittyModCtrl  = 4
)

// decodeKittyKey maps a kitty keyboard-protocol key report (a CSI body ending in
// 'u') to a viewer command and the CDP modifier bits the chord carried. The body
// is "key[:alternates][;modifiers[:event]]"; sub-parameters are cut because the
// pushed flag set never asks for alternates or event types, so only the leading
// value of each parameter is meaningful.
//
// It reports the same actions the legacy parser produces for the same physical
// keys, so a supporting terminal behaves like a legacy one: Escape reaches the
// page, Enter activates, Tab traverses focus, Backspace edits, the Ctrl chords
// keep their bindings, and the Alt letters keep their cycles with Shift read from
// the modifier rather than the letter's case. Shift+Enter returns actionActivate
// with the Shift bit set, which is the signal find-in-page uses for its
// previous-match step; every other unmapped report returns ok false and is
// dropped, so an unbound chord never leaks bytes as keys.
func decodeKittyKey(body []byte) (act keyAction, mod int, ok bool) {
	n := len(body)
	if n < 2 || body[n-1] != 'u' {
		return actionNone, 0, false
	}
	params := strings.Split(string(body[:n-1]), ";")
	code, codeOK := kittyParam(params[0])
	if !codeOK {
		return actionNone, 0, false
	}
	report := 1
	if len(params) > 1 {
		v, modOK := kittyParam(params[1])
		if !modOK {
			return actionNone, 0, false
		}
		report = v
	}
	bits := report - 1
	if bits < 0 {
		return actionNone, 0, false
	}
	mod = cdpModifiersFromKitty(bits)
	switch code {
	case kittyKeyEscape:
		return actionEscape, mod, true
	case kittyKeyEnter:
		return actionActivate, mod, true
	case kittyKeyTab:
		if bits&kittyModShift != 0 {
			return actionFocusPrev, mod, true
		}
		return actionFocusNext, mod, true
	case kittyKeyBackspace:
		return actionBackspace, mod, true
	}
	// A letter chord: Ctrl maps through the C0 byte table and Alt through the
	// Alt-chord table, with Shift carried by the letter's case there.
	letter, isLetter := kittyLetter(code)
	switch {
	case !isLetter:
		return actionNone, 0, false
	case bits&kittyModCtrl != 0:
		if a := byteKeyAction(letter & 0x1f); a != actionNone {
			return a, mod, true
		}
	case bits&kittyModAlt != 0:
		chord := letter
		if bits&kittyModShift != 0 {
			chord = letter - ('a' - 'A')
		}
		if a, chordOK := altKeyAction(chord); chordOK {
			return a, mod, true
		}
	}
	return actionNone, 0, false
}

// kittyKeyEvent turns a decoded key report into the event a legacy terminal
// would have produced for the same physical key. The keys that have a legacy
// control byte become that byte event, carrying the report's modifiers, and
// every other chord stays an action event. Normalising here, at the one decode
// point, keeps every consumer on a single code path: the chrome dispatch, the
// three bar-mode handlers that bind Esc, Enter, Tab, and Backspace by byte, and
// the pre-navigation quit check all behave the same whether the protocol is
// pushed or not. Shift+Tab is deliberately not folded into Tab: it is a distinct
// binding, and it already arrives as an action on the legacy path too.
func kittyKeyEvent(act keyAction, mod int) keyEvent {
	if b, ok := legacyKeyByte(act); ok {
		return keyEvent{b: b, mod: mod}
	}
	return keyEvent{act: act, mod: mod}
}

// legacyKeyByte returns the control byte a legacy terminal sends for the key an
// action names, and whether one exists. Quit maps to Ctrl+Q, the primary quit
// byte, because the interrupt fallback carries the same action. Select-all maps
// back to Ctrl+A so a supporting terminal's report reaches the bar's text fields
// as the byte they bind for the line start, exactly as a legacy one does.
func legacyKeyByte(act keyAction) (byte, bool) {
	switch act {
	case actionEscape:
		return keyEsc, true
	case actionSelectAll:
		return keyCtrlA, true
	case actionActivate:
		return keyEnter, true
	case actionFocusNext:
		return keyTab, true
	case actionBackspace:
		return keyDel, true
	case actionQuit:
		return keyQuitCtrlQ, true
	default:
		return 0, false
	}
}

// kittyParam parses one CSI u parameter, keeping the leading value and dropping
// any ':' sub-parameters. An empty parameter is the protocol's default of 1, and
// a non-numeric one is malformed.
func kittyParam(p string) (int, bool) {
	if v, _, found := strings.Cut(p, ":"); found {
		p = v
	}
	if p == "" {
		return 1, true
	}
	n := 0
	for i := range len(p) {
		if p[i] < '0' || p[i] > '9' {
			return 0, false
		}
		n = n*10 + int(p[i]-'0')
	}
	return n, true
}

// kittyLetter returns the ASCII letter a text-key report names, lower-cased, so
// the legacy chord tables can key on it. ok is false for any other codepoint.
func kittyLetter(code int) (letter byte, ok bool) {
	switch {
	case code >= 'a' && code <= 'z':
		return byte(code), true
	case code >= 'A' && code <= 'Z':
		return byte(code) + ('a' - 'A'), true
	default:
		return 0, false
	}
}

// cdpModifiersFromKitty translates kitty modifier bits into the CDP modifier
// bitmask the rest of the viewer speaks, so a decoded chord carries its
// modifiers in one currency.
func cdpModifiersFromKitty(bits int) int {
	mod := 0
	if bits&kittyModShift != 0 {
		mod |= cdpModShift
	}
	if bits&kittyModAlt != 0 {
		mod |= cdpModAlt
	}
	if bits&kittyModCtrl != 0 {
		mod |= cdpModCtrl
	}
	return mod
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
	mod    int         // CDP modifier bits a kitty key report carried; 0 on the legacy path
	apc    string      // Kitty graphics response body after ESC _ G, e.g. "i=1;OK"; "" otherwise
	osc    string      // consumed OSC reply body, carried for a future handler; "" means unset
	dcs    string      // consumed DCS reply body, carried for a future handler; "" means unset
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
// or mouse event (an SGR drag motion report among them), and an unrecognised or
// malformed sequence is dropped (emit false) without leaking its bytes as
// keys. An ESC that opens an APC
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
	if next == ']' {
		// An OSC reply from the terminal (colour, title). Consume it so its
		// bytes do not leak as keys, and carry the body for a future handler.
		// Truncation at a read boundary needs no handling here: ReadByte
		// blocks until the rest of the reply arrives.
		body, ok := readOSCBody(br)
		if !ok || len(body) == 0 {
			return keyEvent{}, false
		}
		return keyEvent{osc: string(body)}, true
	}
	if next == 'P' {
		// A DCS reply from the terminal. A DCS ends on ST only, so the APC
		// reader is exact. Consume it and carry the body for a future
		// handler. Truncation at a read boundary needs no handling here:
		// ReadByte blocks until the rest of the reply arrives.
		body, ok := readAPCBody(br)
		if !ok || len(body) == 0 {
			return keyEvent{}, false
		}
		return keyEvent{dcs: string(body)}, true
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
		if len(body) > maxCSIBody {
			return keyEvent{}, false
		}
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
	if m, ok := decodeSGRMouse(body); ok {
		return keyEvent{mouse: &m}, true
	}
	if k, ok := decodeCSI(body); ok {
		return keyEvent{scroll: k}, true
	}
	if act, ok := decodeCSIAction(body); ok {
		return keyEvent{act: act}, true
	}
	// A kitty keyboard-protocol key report, present only while the protocol is
	// pushed. It normalises to the legacy event for the same key, so no consumer
	// needs to know which path it came from. An unbound report is dropped like
	// any unrecognised CSI, and so is a late flags reply from the startup query.
	if act, mod, ok := decodeKittyKey(body); ok {
		return kittyKeyEvent(act, mod), true
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

// maxCSIBody bounds a CSI body read, matching the paste and APC caps, so
// garbage input that opens a CSI but never sends a final byte cannot grow the
// buffer without limit. The longest recognised body is an SGR mouse report of
// a few dozen bytes.
const maxCSIBody = 256

// maxAPCBody bounds the APC, OSC, and DCS body reads, so garbage input that
// opens one of these sequences but never terminates it cannot grow the buffer
// without limit. 4096 is ample: the largest expected reply, a Kitty graphics
// response, fits, and colour and title replies are under 100 bytes.
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
	// Over-long body: drain to the terminator without buffering, so the
	// sequence's remaining bytes never leak as key events, then drop it.
	drainToTerminator(br, false)
	return nil, false
}

// drainToTerminator consumes bytes without buffering until the sequence's
// terminator: the String Terminator (ESC backslash), and BEL too when
// allowBEL is set (an OSC accepts both). The caller drops the event whole,
// so an over-long sequence costs bounded memory.
func drainToTerminator(br *bufio.Reader, allowBEL bool) {
	esc := false
	for {
		c, err := br.ReadByte()
		if err != nil {
			return
		}
		if esc && c == '\\' {
			return
		}
		if allowBEL && c == '\x07' {
			return
		}
		esc = c == keyEsc
	}
}

// readOSCBody reads an OSC body up to its BEL or String Terminator
// (ESC backslash) and returns the bytes between the introducer and the
// terminator. ok is false on a read error or an over-long body, and the
// event is dropped.
func readOSCBody(br *bufio.Reader) ([]byte, bool) {
	var body []byte
	for len(body) <= maxAPCBody {
		c, err := br.ReadByte()
		if err != nil {
			return nil, false
		}
		if c == '\x07' {
			return body, true
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
	// Over-long body: drain to the terminator without buffering, so the
	// sequence's remaining bytes never leak as key events, then drop it.
	drainToTerminator(br, true)
	return nil, false
}
