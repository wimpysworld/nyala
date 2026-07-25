package viewer

import (
	"bufio"
	"strings"
	"testing"
)

// TestDecodeCSI asserts the CSI body decoder maps the arrow finals A/B/C/D,
// the PageUp/PageDown bodies 5~/6~, and both encodings each of Home (H, 1~)
// and End (F, 4~) to their scroll keys, and rejects an unrecognised body.
func TestDecodeCSI(t *testing.T) {
	cases := []struct {
		body string
		want ScrollKey
		ok   bool
	}{
		{"A", ScrollUp, true},
		{"B", ScrollDown, true},
		{"C", ScrollRight, true},
		{"D", ScrollLeft, true},
		{"5~", ScrollPageUp, true},
		{"6~", ScrollPageDown, true},
		{"H", ScrollHome, true},
		{"1~", ScrollHome, true},
		{"7~", ScrollHome, true},
		{"F", ScrollEnd, true},
		{"4~", ScrollEnd, true},
		{"8~", ScrollEnd, true},
		{"99~", ScrollNone, false},
		{"", ScrollNone, false},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			got, ok := decodeCSI([]byte(c.body))
			if ok != c.ok || got != c.want {
				t.Fatalf("decodeCSI(%q) = (%d, %v), want (%d, %v)", c.body, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestDecodeCSIAction asserts the CSI body decoder maps the backtab (Z, how
// Shift+Tab arrives), F5 (15~), and the Alt-modified cursor keys (Alt+Left
// 1;3D, Alt+Right 1;3C, Alt+Home 1;3H and 1;3~) to their commands, and
// rejects an unrecognised body.
func TestDecodeCSIAction(t *testing.T) {
	cases := []struct {
		body string
		want keyAction
		ok   bool
	}{
		{"Z", actionFocusPrev, true},
		{"15~", actionReload, true},
		{"3~", actionDelete, true},
		{"1;3D", actionBack, true},
		{"1;3C", actionForward, true},
		{"1;3H", actionOpenHome, true},
		{"1;3~", actionOpenHome, true},
		{"99~", actionNone, false},
		{"", actionNone, false},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			got, ok := decodeCSIAction([]byte(c.body))
			if ok != c.ok || got != c.want {
				t.Fatalf("decodeCSIAction(%q) = (%d, %v), want (%d, %v)", c.body, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestByteKeyAction asserts the single-byte command map: Ctrl+Q quits (with
// Ctrl+C as the interrupt fallback), Ctrl+R reloads, Tab and Enter forward to
// the page, both Backspace encodings edit the focused field, Esc forwards to
// the page, and plain printable bytes, including q and Space, map to no
// command because they are page text, not a binding.
func TestByteKeyAction(t *testing.T) {
	cases := []struct {
		name string
		b    byte
		want keyAction
	}{
		{"ctrl-q", keyQuitCtrlQ, actionQuit},
		{"ctrl-c", keyQuitCtrlC, actionQuit},
		{"ctrl-r", keyCtrlR, actionReload},
		{"ctrl-l", keyCtrlL, actionEditURL},
		{"tab", keyTab, actionFocusNext},
		{"enter", keyEnter, actionActivate},
		{"backspace", keyBackspace, actionBackspace},
		{"del", keyDel, actionBackspace},
		{"esc", keyEsc, actionEscape},
		{"plain q", 'q', actionNone},
		{"plain Q", 'Q', actionNone},
		{"space", ' ', actionNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := byteKeyAction(c.b); got != c.want {
				t.Fatalf("byteKeyAction(%#x) = %d, want %d", c.b, got, c.want)
			}
		})
	}
}

// TestPrintableByte asserts the page-text byte range: Space through tilde are
// text, while the control bytes below Space, DEL, and the UTF-8 lead and
// continuation bytes are not, so they never reach the page as typed text.
func TestPrintableByte(t *testing.T) {
	for _, b := range []byte{' ', '!', '0', 'a', 'Z', '~'} {
		if !printableByte(b) {
			t.Fatalf("printableByte(%#x) = false, want true", b)
		}
	}
	for _, b := range []byte{0x00, keyBackspace, keyTab, keyEnter, keyEsc, keyDel, 0xc3, 0xa9} {
		if printableByte(b) {
			t.Fatalf("printableByte(%#x) = true, want false", b)
		}
	}
}

// TestNextEventCSI asserts the reader resolves whole CSI escape sequences into
// scroll events: each arrow, PageUp/PageDown, Home, and End sequence yields
// one scroll keyEvent, a plain byte yields a byte event, an unrecognised CSI
// sequence is dropped, and a bare ESC passes through as a byte event. It reads
// from a bufio.Reader the way readKeys does, so it pins the multi-byte parse.
func TestNextEventCSI(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantScroll ScrollKey
		wantByte   byte
		wantEmit   bool
	}{
		{"up arrow", "\x1b[A", ScrollUp, 0, true},
		{"down arrow", "\x1b[B", ScrollDown, 0, true},
		{"right arrow", "\x1b[C", ScrollRight, 0, true},
		{"left arrow", "\x1b[D", ScrollLeft, 0, true},
		{"page up", "\x1b[5~", ScrollPageUp, 0, true},
		{"page down", "\x1b[6~", ScrollPageDown, 0, true},
		{"home", "\x1b[H", ScrollHome, 0, true},
		{"home vt", "\x1b[1~", ScrollHome, 0, true},
		{"end", "\x1b[F", ScrollEnd, 0, true},
		{"end vt", "\x1b[4~", ScrollEnd, 0, true},
		{"plain byte", "q", ScrollNone, 'q', true},
		{"tab byte", "\t", ScrollNone, keyTab, true},
		{"enter byte", "\r", ScrollNone, keyEnter, true},
		{"ctrl-q byte", "\x11", ScrollNone, keyQuitCtrlQ, true},
		{"ctrl-r byte", "\x12", ScrollNone, keyCtrlR, true},
		{"unknown csi dropped", "\x1b[99~", ScrollNone, 0, false},
		{"bare esc passthrough", "\x1bq", ScrollNone, keyEsc, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if emit != c.wantEmit {
				t.Fatalf("nextEvent emit = %v, want %v", emit, c.wantEmit)
			}
			if !emit {
				return
			}
			if ev.scroll != c.wantScroll {
				t.Fatalf("nextEvent scroll = %d, want %d", ev.scroll, c.wantScroll)
			}
			if ev.scroll == ScrollNone && ev.b != c.wantByte {
				t.Fatalf("nextEvent byte = %q, want %q", ev.b, c.wantByte)
			}
		})
	}
}

// TestNextEventCSIBodyBounded asserts the CSI reader drops a body that never
// reaches a final byte within maxCSIBody, matching the paste and APC caps, so
// garbage input cannot grow the buffer without limit.
func TestNextEventCSIBodyBounded(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\x1b[" + strings.Repeat("1;", maxCSIBody) + "A"))
	first, err := br.ReadByte()
	if err != nil {
		t.Fatalf("read first byte: %v", err)
	}
	if _, emit := nextEvent(br, first); emit {
		t.Fatal("over-long CSI body emitted an event")
	}
}

// TestNextEventActions asserts the reader resolves the command escape
// sequences into action events: the Alt chords (ESC+letter, Shift carried by
// case), the backtab, F5, and the Alt-modified cursor keys. A trailing byte
// after each sequence pins that the parse consumes exactly the sequence.
func TestNextEventActions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want keyAction
	}{
		{"alt-r", "\x1br", actionCycleBackend},
		{"alt-shift-r", "\x1bR", actionCycleBackendReverse},
		{"alt-b", "\x1bb", actionCycleBlocks},
		{"alt-shift-b", "\x1bB", actionCycleBlocksReverse},
		{"alt-c", "\x1bc", actionCycleColour},
		{"alt-shift-c", "\x1bC", actionCycleColourReverse},
		{"alt-f", "\x1bf", actionOpenMenu},
		{"backtab", "\x1b[Z", actionFocusPrev},
		{"delete", "\x1b[3~", actionDelete},
		{"f5", "\x1b[15~", actionReload},
		{"alt-left", "\x1b[1;3D", actionBack},
		{"alt-right", "\x1b[1;3C", actionForward},
		{"alt-home", "\x1b[1;3H", actionOpenHome},
		{"alt-home vt", "\x1b[1;3~", actionOpenHome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in + "x"))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if !emit {
				t.Fatalf("nextEvent dropped %q", c.in)
			}
			if ev.act != c.want {
				t.Fatalf("nextEvent act = %d, want %d", ev.act, c.want)
			}
			rest, err := br.ReadByte()
			if err != nil || rest != 'x' {
				t.Fatalf("trailing byte after %q = %q, %v; want 'x'", c.in, rest, err)
			}
		})
	}
}

// TestReadKeysKeepsQuitKeysAfterDownArrow asserts a Down-arrow CSI sequence
// does not swallow later quit keys from the same input stream.
func TestReadKeysKeepsQuitKeysAfterDownArrow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want byte
	}{
		{"ctrl-q", "\x1b[B\x11", keyQuitCtrlQ},
		{"ctrl-c", "\x1b[B\x03", keyQuitCtrlC},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan struct{})
			defer close(done)
			keys := readKeys(strings.NewReader(c.in), done)

			first, ok := <-keys
			if !ok {
				t.Fatalf("first event missing, want Down arrow")
			}
			if first.scroll != ScrollDown {
				t.Fatalf("first event scroll = %d, want ScrollDown", first.scroll)
			}

			second, ok := <-keys
			if !ok {
				t.Fatalf("second event missing, want %q", c.want)
			}
			if second.scroll != ScrollNone || second.b != c.want {
				t.Fatalf("second event = %+v, want byte %q", second, c.want)
			}
		})
	}
}

// TestNextEventUTF8Runes asserts the reader assembles multi-byte UTF-8 into
// whole rune events (two, three, and four byte forms), leaves ASCII as byte
// events, and drops stray continuation bytes and malformed sequences without
// swallowing the byte that follows them.
func TestNextEventUTF8Runes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want rune
	}{
		{"two byte", "é", 'é'},
		{"three byte", "→", '→'},
		{"four byte", "\U0001f9a1", '\U0001f9a1'},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in + "x"))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if !emit {
				t.Fatalf("nextEvent dropped %q", c.in)
			}
			if ev.r != c.want {
				t.Fatalf("nextEvent rune = %q, want %q", ev.r, c.want)
			}
			rest, err := br.ReadByte()
			if err != nil || rest != 'x' {
				t.Fatalf("trailing byte after %q = %q, %v; want 'x'", c.in, rest, err)
			}
		})
	}

	t.Run("stray continuation dropped", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\x80x"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("stray continuation byte emitted an event")
		}
		if rest, err := br.ReadByte(); err != nil || rest != 'x' {
			t.Fatalf("byte after stray continuation = %q, %v; want 'x'", rest, err)
		}
	})
	t.Run("cut sequence unreads the cutter", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\xc3x"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("cut UTF-8 sequence emitted an event")
		}
		if rest, err := br.ReadByte(); err != nil || rest != 'x' {
			t.Fatalf("byte after cut sequence = %q, %v; want 'x'", rest, err)
		}
	})
}

// TestNextEventLoneEsc asserts the buffered-byte lone-Esc detection: an ESC
// with nothing buffered behind it (a bare Esc key press arrives alone in its
// read burst) emits an ESC byte event immediately, while an ESC with a
// buffered chord letter behind it stays an Alt chord.
func TestNextEventLoneEsc(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\x1b"))
	first, err := br.ReadByte()
	if err != nil {
		t.Fatalf("read first byte: %v", err)
	}
	ev, emit := nextEvent(br, first)
	if !emit || ev.b != keyEsc || ev.act != actionNone {
		t.Fatalf("lone Esc = (%+v, %v), want an ESC byte event", ev, emit)
	}

	br = bufio.NewReader(strings.NewReader("\x1br"))
	first, _ = br.ReadByte()
	ev, emit = nextEvent(br, first)
	if !emit || ev.act != actionCycleBackend {
		t.Fatalf("buffered chord = (%+v, %v), want the Alt+R action", ev, emit)
	}
}

// TestNextEventBracketedPaste asserts a bracketed paste (CSI 200~ to
// CSI 201~) arrives as one paste event carrying the payload whole, including
// newlines and embedded escape bytes, and that the byte after the terminator
// still arrives as a key. An empty paste is dropped.
func TestNextEventBracketedPaste(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "\x1b[200~https://example.org/\x1b[201~", "https://example.org/"},
		{"multi-line kept whole", "\x1b[200~one\ntwo\rthree\x1b[201~", "one\ntwo\rthree"},
		{"embedded escape", "\x1b[200~a\x1b[Zb\x1b[201~", "a\x1b[Zb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in + "x"))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if !emit {
				t.Fatalf("nextEvent dropped the paste %q", c.in)
			}
			if ev.paste != c.want {
				t.Fatalf("paste = %q, want %q", ev.paste, c.want)
			}
			rest, err := br.ReadByte()
			if err != nil || rest != 'x' {
				t.Fatalf("trailing byte after paste = %q, %v; want 'x'", rest, err)
			}
		})
	}

	t.Run("empty paste dropped", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\x1b[200~\x1b[201~x"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("empty paste emitted an event")
		}
		if rest, err := br.ReadByte(); err != nil || rest != 'x' {
			t.Fatalf("byte after empty paste = %q, %v; want 'x'", rest, err)
		}
	})
	t.Run("unterminated paste dropped", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\x1b[200~orphan"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("unterminated paste emitted an event")
		}
	})
}
