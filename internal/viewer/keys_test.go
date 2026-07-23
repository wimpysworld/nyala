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
		{"F", ScrollEnd, true},
		{"4~", ScrollEnd, true},
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
// the page, and plain printable bytes, including q, map to no command, so
// printable keys stay free for future page form input.
func TestByteKeyAction(t *testing.T) {
	cases := []struct {
		name string
		b    byte
		want keyAction
	}{
		{"ctrl-q", keyQuitCtrlQ, actionQuit},
		{"ctrl-c", keyQuitCtrlC, actionQuit},
		{"ctrl-r", keyCtrlR, actionReload},
		{"tab", keyTab, actionFocusNext},
		{"enter", keyEnter, actionActivate},
		{"plain q", 'q', actionNone},
		{"plain Q", 'Q', actionNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := byteKeyAction(c.b); got != c.want {
				t.Fatalf("byteKeyAction(%#x) = %d, want %d", c.b, got, c.want)
			}
		})
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
		{"backtab", "\x1b[Z", actionFocusPrev},
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
