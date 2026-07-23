package viewer

import (
	"bufio"
	"strings"
	"testing"
)

// TestDecodeCSI asserts the CSI body decoder maps the arrow finals A/B/C/D and
// the PageUp/PageDown bodies 5~/6~ to their scroll keys, and rejects an
// unrecognised body (here the Home key, ESC [ H).
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
		{"H", ScrollNone, false},
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

// TestNextEventCSI asserts the reader resolves whole CSI escape sequences into
// scroll events: each arrow and PageUp/PageDown sequence yields one scroll
// keyEvent, a plain byte yields a byte event, an unrecognised CSI sequence (Home,
// ESC [ H) is dropped, and a bare ESC passes through as a byte event. It reads
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
		{"plain byte", "q", ScrollNone, 'q', true},
		{"tab byte", "\t", ScrollNone, keyTab, true},
		{"unknown csi dropped", "\x1b[H", ScrollNone, 0, false},
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

// TestReadKeysKeepsQuitKeysAfterDownArrow asserts a Down-arrow CSI sequence
// does not swallow later quit keys from the same input stream.
func TestReadKeysKeepsQuitKeysAfterDownArrow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want byte
	}{
		{"q", "\x1b[Bq", 'q'},
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
