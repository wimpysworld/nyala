package viewer

import (
	"bytes"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// dispatchSession builds a streamSession that dispatches keys synchronously
// (no keySender worker) onto the recording engine, so a test asserts the
// engine seam calls in order.
func dispatchSession(eng streamTestEngine, st *State) *streamSession {
	return &streamSession{eng: eng, st: st, tty: &bytes.Buffer{}}
}

// TestHandleKeyForwardsPageKeys pins the page-key forwarding: Tab, Shift+Tab
// (backtab), and Enter reach the engine as key codes for native focus
// traversal and activation, and none of them cycles the backend. Home and End
// forward as scroll codes to the engine's wheel transport.
func TestHandleKeyForwardsPageKeys(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
		want string
	}{
		{"tab", keyEvent{b: keyTab}, "Tab"},
		{"backtab", keyEvent{act: actionFocusPrev}, "ShiftTab"},
		{"enter", keyEvent{b: keyEnter}, "Enter"},
		{"home", keyEvent{scroll: ScrollHome}, "Home"},
		{"end", keyEvent{scroll: ScrollEnd}, "End"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			st.SetEnabled([]Backend{BackendKitty, BackendBlock})
			startBackend := st.Backend
			var sent []string
			var backends []string
			s := dispatchSession(streamTestEngine{
				sendKey:    func(code string) { sent = append(sent, code) },
				setBackend: func(name string) { backends = append(backends, name) },
			}, &st)

			if quit := s.handleKey(c.ev); quit {
				t.Fatalf("%s requested quit", c.name)
			}
			if len(sent) != 1 || sent[0] != c.want {
				t.Fatalf("SendKey calls = %v, want [%s]", sent, c.want)
			}
			if st.Backend != startBackend || len(backends) != 0 {
				t.Fatalf("%s cycled the backend: state %v, SetBackend calls %v", c.name, st.Backend, backends)
			}
		})
	}
}

// TestHandleKeyForwardsNav pins the navigation forwarding: Ctrl+R and F5
// reload, Alt+Left goes back, Alt+Right goes forward, and Alt+Home opens the
// launch page through the engine seam that holds the launch URL.
func TestHandleKeyForwardsNav(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
		want NavAction
	}{
		{"ctrl-r", keyEvent{b: keyCtrlR}, NavReload},
		{"f5", keyEvent{act: actionReload}, NavReload},
		{"alt-left", keyEvent{act: actionBack}, NavBack},
		{"alt-right", keyEvent{act: actionForward}, NavForward},
		{"alt-home", keyEvent{act: actionOpenHome}, NavHome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			var navs []NavAction
			s := dispatchSession(streamTestEngine{
				sendNav: func(a NavAction) { navs = append(navs, a) },
			}, &st)

			if quit := s.handleKey(c.ev); quit {
				t.Fatalf("%s requested quit", c.name)
			}
			if len(navs) != 1 || navs[0] != c.want {
				t.Fatalf("SendNav calls = %v, want [%d]", navs, c.want)
			}
		})
	}
}

// TestHandleKeyQuit pins the quit policy: Ctrl+Q quits and Ctrl+C stays as the
// interrupt fallback, while plain q is inert (printable keys stay free for
// future page form input) and reaches no engine seam.
func TestHandleKeyQuit(t *testing.T) {
	st := NewState(80, 24)
	var sent []string
	var navs []NavAction
	s := dispatchSession(streamTestEngine{
		sendKey: func(code string) { sent = append(sent, code) },
		sendNav: func(a NavAction) { navs = append(navs, a) },
	}, &st)

	if !s.handleKey(keyEvent{b: keyQuitCtrlQ}) {
		t.Fatal("Ctrl+Q did not quit")
	}
	if !s.handleKey(keyEvent{b: keyQuitCtrlC}) {
		t.Fatal("Ctrl+C did not quit")
	}
	for _, b := range []byte{'q', 'Q'} {
		if s.handleKey(keyEvent{b: b}) {
			t.Fatalf("plain %q quit; quit is Ctrl+Q", b)
		}
	}
	if len(sent) != 0 || len(navs) != 0 {
		t.Fatalf("quit keys reached the engine: keys %v, navs %v", sent, navs)
	}
}

// TestHandleKeyCycleBackendBothWays pins Alt+R stepping the enabled cycle
// forwards and Alt+Shift+R stepping it backwards, wrapping at both ends.
func TestHandleKeyCycleBackendBothWays(t *testing.T) {
	st := NewState(80, 24)
	st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
	eng := streamTestEngine{}
	done := make(chan struct{})
	defer close(done)
	s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}

	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendSixel {
		t.Fatalf("after Alt+R, backend = %v, want Sixel", st.Backend)
	}
	s.handleKey(keyEvent{act: actionCycleBackendReverse})
	if st.Backend != BackendKitty {
		t.Fatalf("after Alt+Shift+R, backend = %v, want Kitty", st.Backend)
	}
	s.handleKey(keyEvent{act: actionCycleBackendReverse})
	if st.Backend != BackendBlock {
		t.Fatalf("reverse cycle did not wrap: backend = %v, want Block", st.Backend)
	}
}

// TestCycleScheme pins the Alt+B order half -> quadrant -> sextant with wrap,
// the reverse order, and the sextant skip on a terminal that cannot self-draw
// sextant glyphs.
func TestCycleScheme(t *testing.T) {
	st := NewState(80, 24)
	st.sextantOK = true
	st.Scheme = block.SchemeHalfBlock
	want := []block.Scheme{block.SchemeQuadrant, block.SchemeSextant, block.SchemeHalfBlock}
	for i, w := range want {
		if got := st.CycleScheme(false); got != w {
			t.Fatalf("cycle %d = %d, want %d", i, got, w)
		}
	}
	if got := st.CycleScheme(true); got != block.SchemeSextant {
		t.Fatalf("reverse cycle = %d, want sextant", got)
	}

	st.sextantOK = false
	st.Scheme = block.SchemeQuadrant
	if got := st.CycleScheme(false); got != block.SchemeHalfBlock {
		t.Fatalf("unsupported sextant not skipped: got %d, want half", got)
	}
	if got := st.CycleScheme(true); got != block.SchemeQuadrant {
		t.Fatalf("reverse skip = %d, want quadrant", got)
	}
}

// TestCycleColour pins the Alt+C order truecolour -> 256 -> monochrome with
// wrap, the reverse order, and the truecolour skip under the Rio 0.4.x
// compatibility cap, the same availability state that caps --color=auto.
func TestCycleColour(t *testing.T) {
	st := NewState(80, 24)
	want := []ColourMode{ColourXTerm256, ColourMono, ColourTrue}
	for i, w := range want {
		if got := st.CycleColour(false); got != w {
			t.Fatalf("cycle %d = %d, want %d", i, got, w)
		}
	}
	if got := st.CycleColour(true); got != ColourMono {
		t.Fatalf("reverse cycle = %d, want monochrome", got)
	}

	st.compat.rio04 = true
	st.Colour = ColourMono
	if got := st.CycleColour(false); got != ColourXTerm256 {
		t.Fatalf("capped truecolour not skipped: got %d, want 256", got)
	}
	if got := st.CycleColour(true); got != ColourMono {
		t.Fatalf("reverse capped cycle = %d, want monochrome", got)
	}
}

// TestHandleKeyBlockCycleRecomposes pins the Alt+B and Alt+C repaint contract
// on the live Block backend: the differ resets, the clear goes out with the
// SGR reset first (the backend-switch clear rule), and the engine restarts at
// the unchanged grid so a static page re-emits a frame for the new
// composition.
func TestHandleKeyBlockCycleRecomposes(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
	}{
		{"alt-b", keyEvent{act: actionCycleBlocks}},
		{"alt-shift-b", keyEvent{act: actionCycleBlocksReverse}},
		{"alt-c", keyEvent{act: actionCycleColour}},
		{"alt-shift-c", keyEvent{act: actionCycleColourReverse}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(8, 4)
			st.SetEnabled([]Backend{BackendBlock})
			st.sextantOK = true
			diff := &block.GridDiff{}
			st.SetDiff(diff)
			// Seed the differ so an unchanged frame emits nothing; after the
			// cycle it must full-repaint.
			frame := render.Frame{Img: solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})}
			if err := st.RenderStreamFrame(frame, &bytes.Buffer{}); err != nil {
				t.Fatalf("seed Block frame: %v", err)
			}

			// The restart runs on the control goroutine, so collect it over a
			// channel rather than a shared slice.
			backendCh := make(chan string, 2)
			eng := streamTestEngine{setBackend: func(name string) { backendCh <- name }}
			done := make(chan struct{})
			defer close(done)
			out := &syncBuf{}
			s := &streamSession{eng: eng, st: &st, tty: out, ctl: newControl(eng, done), diff: diff}
			s.firstDrawn.Store(true)

			s.handleKey(c.ev)

			select {
			case name := <-backendCh:
				if name != "block" {
					t.Fatalf("cycle restarted the stream on %q, want block", name)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cycle did not restart the stream")
			}
			got := out.String()
			clearAt := strings.Index(got, clearScreen)
			if clearAt < 0 {
				t.Fatalf("cycle emitted no clear: %q", got)
			}
			if !strings.HasSuffix(got[:clearAt], sgrReset) {
				t.Fatalf("cycle clear did not reset the SGR first: %q", got)
			}
			var repaint bytes.Buffer
			if err := st.RenderStreamFrame(frame, &repaint); err != nil {
				t.Fatalf("post-cycle Block frame: %v", err)
			}
			if repaint.Len() == 0 {
				t.Fatal("differ was not reset: unchanged frame emitted nothing after the cycle")
			}
		})
	}
}

// TestHandleKeyBlockCycleOffBlockIsNoOp pins that Alt+B and Alt+C (and their
// reverse chords) taken while a graphics backend draws do nothing: no state
// change, no terminal write, no stream restart. Both cycles only affect Block
// rendering, so cycling off Block would change hidden state with no visible
// feedback and the next Block stint would arrive in an unexpected mode.
func TestHandleKeyBlockCycleOffBlockIsNoOp(t *testing.T) {
	backends := []struct {
		name    string
		backend Backend
	}{
		{"kitty", BackendKitty},
		{"sixel", BackendSixel},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			st := NewState(8, 4)
			st.SetEnabled([]Backend{b.backend, BackendBlock})
			st.sextantOK = true
			startScheme, startColour := st.Scheme, st.Colour

			var restarts []string
			eng := streamTestEngine{setBackend: func(name string) { restarts = append(restarts, name) }}
			out := &bytes.Buffer{}
			s := dispatchSession(eng, &st)
			s.tty = out
			s.firstDrawn.Store(true)

			s.handleKey(keyEvent{act: actionCycleBlocks})
			s.handleKey(keyEvent{act: actionCycleBlocksReverse})
			s.handleKey(keyEvent{act: actionCycleColour})
			s.handleKey(keyEvent{act: actionCycleColourReverse})

			if st.Scheme != startScheme || st.Colour != startColour {
				t.Fatalf("cycle off Block changed state: scheme %d->%d colour %d->%d",
					startScheme, st.Scheme, startColour, st.Colour)
			}
			if out.Len() != 0 {
				t.Fatalf("cycle off Block wrote to the terminal: %q", out.String())
			}
			if len(restarts) != 0 {
				t.Fatalf("cycle off Block restarted the stream: %v", restarts)
			}
		})
	}
}
