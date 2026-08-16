package viewer

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

// TestCycleBackendOrder asserts the default cycle is best-first kitty -> sixel ->
// block: the initial active backend is kitty and the cycle steps down, wrapping.
func TestCycleBackendOrder(t *testing.T) {
	s := NewState(120, 40)
	s.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
	if s.Backend != BackendKitty {
		t.Fatalf("initial backend = %d, want BackendKitty", s.Backend)
	}
	want := []Backend{BackendSixel, BackendBlock, BackendKitty}
	for i, w := range want {
		if got := s.CycleBackend(); got != w {
			t.Fatalf("cycle %d: got %d, want %d", i, got, w)
		}
	}
}

// TestCycleBackendSkipsDisabled asserts CycleBackend steps only through the
// enabled set. With sixel disabled, configureBackends orders it kitty -> block,
// so the initial active backend is kitty and the cycle steps block -> kitty.
func TestCycleBackendSkipsDisabled(t *testing.T) {
	s := NewState(120, 40)
	s.SetEnabled([]Backend{BackendKitty, BackendBlock})
	if s.Backend != BackendKitty {
		t.Fatalf("after SetEnabled, backend = %d, want BackendKitty", s.Backend)
	}
	want := []Backend{BackendBlock, BackendKitty, BackendBlock}
	for i, w := range want {
		if got := s.CycleBackend(); got != w {
			t.Fatalf("cycle %d: got %d, want %d", i, got, w)
		}
	}
}

// TestCycleBackendBlockOnly asserts CycleBackend stays on block when block is the
// only enabled backend.
func TestCycleBackendBlockOnly(t *testing.T) {
	s := NewState(120, 40)
	s.SetEnabled([]Backend{BackendBlock})
	for i := range 3 {
		if got := s.CycleBackend(); got != BackendBlock {
			t.Fatalf("cycle %d: got %d, want BackendBlock", i, got)
		}
	}
}

// TestExitTerminal pins the shared backend exit contract: reset SGR, move below
// a rendered full-height frame with exactly one CRLF, then disable mouse
// reporting and bracketed paste unconditionally, reset Sixel display mode,
// restore auto-wrap, and reveal the cursor. An exit before the first frame
// restores terminal modes in place, without scrolling unrelated content.
func TestExitTerminal(t *testing.T) {
	const want = "\x1b[0m\x1b[24;1H\r\n\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?80l\x1b[?7h\x1b[?25h\x1b[23;0t"
	for _, backend := range []Backend{BackendBlock, BackendKitty, BackendSixel} {
		t.Run(backend.String(), func(t *testing.T) {
			if got := exitTerminal(24, true, false); got != want {
				t.Fatalf("exitTerminal(24, true, false) = %q, want %q", got, want)
			}
		})
	}

	const restoreInPlace = "\x1b[0m\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?80l\x1b[?7h\x1b[?25h\x1b[23;0t"
	if got := exitTerminal(24, false, false); got != restoreInPlace {
		t.Fatalf("startup exit = %q, want %q", got, restoreInPlace)
	}
	if got := exitTerminal(0, true, false); got != restoreInPlace {
		t.Fatalf("invalid-row exit = %q, want %q", got, restoreInPlace)
	}

	// A session that pushed the kitty keyboard protocol pops exactly one entry,
	// between the paste reset and the Sixel reset, and a session that did not
	// push emits no pop, so an outer application's entry survives.
	const pushed = "\x1b[0m\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[<u\x1b[?80l\x1b[?7h\x1b[?25h\x1b[23;0t"
	if got := exitTerminal(0, false, true); got != pushed {
		t.Fatalf("kitty-keyboard exit = %q, want %q", got, pushed)
	}
	if strings.Count(pushed, disableKittyKeyboard) != 1 {
		t.Fatalf("teardown pops the protocol %d times, want once", strings.Count(pushed, disableKittyKeyboard))
	}
	if strings.Contains(restoreInPlace, disableKittyKeyboard) {
		t.Fatalf("a session that never pushed still popped: %q", restoreInPlace)
	}
}

// TestTerminalModesPairEnableWithTeardown asserts every mode Stream turns on is
// turned off by the teardown that runs on normal exit, on a terminating signal, and on an
// error return, and that the kitty keyboard protocol is pushed and popped
// together: on where the handshake found it, off again on the way out, and
// neither touched on a terminal that never advertised it.
func TestTerminalModesPairEnableWithTeardown(t *testing.T) {
	pairs := []struct{ enable, disable string }{
		{enableMouseReporting, disableMouseReporting},
		{enableBracketedPaste, disableBracketedPaste},
		{pushTitle, popTitle},
	}
	for _, kitty := range []bool{false, true} {
		modes := enterTerminalModes(kitty)
		teardown := exitTerminal(24, true, kitty)
		for _, p := range pairs {
			if !strings.Contains(modes, p.enable) {
				t.Fatalf("enterTerminalModes(%t) = %q, missing %q", kitty, modes, p.enable)
			}
			if !strings.Contains(teardown, p.disable) {
				t.Fatalf("exitTerminal with kitty=%t = %q, missing %q", kitty, teardown, p.disable)
			}
		}
		if got := strings.Contains(modes, enableKittyKeyboard); got != kitty {
			t.Fatalf("enterTerminalModes(%t) pushes the protocol = %v, want %v", kitty, got, kitty)
		}
		if got := strings.Contains(teardown, disableKittyKeyboard); got != kitty {
			t.Fatalf("exitTerminal with kitty=%t pops the protocol = %v, want %v", kitty, got, kitty)
		}
	}
}

// TestExitTerminalWithTitle asserts the teardown for a terminal that ignores
// the title pop follows the pop with a fixed neutral title, and that a normal
// terminal gets the plain teardown with no OSC write.
func TestExitTerminalWithTitle(t *testing.T) {
	plain := exitTerminal(24, true, false)
	if got := exitTerminalWithTitle(24, true, false, false); got != plain {
		t.Fatalf("exitTerminalWithTitle without the flag = %q, want %q", got, plain)
	}
	want := plain + setTitle("terminal")
	if got := exitTerminalWithTitle(24, true, false, true); got != want {
		t.Fatalf("exitTerminalWithTitle with the flag = %q, want %q", got, want)
	}
}

// TestSanitiseTitle covers the hostile-title paths: C0 and C1 stripping, the
// byte cap cut on a rune boundary, the host fallback for an empty title, and
// the raw-URL fallback when the URL does not parse to a host.
func TestSanitiseTitle(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		rawURL string
		want   string
	}{
		{"hostile OSC and BEL", "\x1b]0;pwned\x07", "https://example.com/", "]0;pwned"},
		{"C0 stripped", "a\x00b\x1fc\rd", "https://example.com/", "abcd"},
		{"DEL and C1 stripped", "a\x7fb\u0085c\u009bd", "https://example.com/", "abcd"},
		{"cap on rune boundary", strings.Repeat("a", 126) + "éé", "https://example.com/", strings.Repeat("a", 126) + "é"},
		{"empty falls back to host", "", "https://example.com/path", "example.com"},
		{"controls-only falls back to host", "\x1b\x07", "https://example.com/", "example.com"},
		{"unparsable falls back to raw URL", "", "://not-a-url", "://not-a-url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitiseTitle(tc.title, tc.rawURL)
			if got != tc.want {
				t.Fatalf("sanitiseTitle(%q, %q) = %q, want %q", tc.title, tc.rawURL, got, tc.want)
			}
			if len(got) > maxTitleBytes {
				t.Fatalf("sanitised title is %d bytes, cap is %d", len(got), maxTitleBytes)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("sanitised title %q is not valid UTF-8", got)
			}
			if emitted := setTitle(got); strings.ContainsRune(emitted, '\x07') {
				t.Fatalf("setTitle(%q) contains BEL", got)
			}
		})
	}
}

// TestSetTitleUsesSTTerminator pins the OSC 0 terminator as ST, never BEL.
func TestSetTitleUsesSTTerminator(t *testing.T) {
	if got := setTitle("Nyala"); got != "\x1b]0;Nyala\x1b\\" {
		t.Fatalf("setTitle(\"Nyala\") = %q, want %q", got, "\x1b]0;Nyala\x1b\\")
	}
}

// TestSetEnabledNominatesCeiling asserts SetEnabled nominates the best available
// backend: enabled[0] is the active backend and block sits last as the floor. It
// pins the orderings configureBackends builds for each detected set.
func TestSetEnabledNominatesCeiling(t *testing.T) {
	cases := []struct {
		name       string
		enabled    []Backend
		wantActive Backend
		wantLast   Backend
	}{
		{"kitty and sixel", []Backend{BackendKitty, BackendSixel, BackendBlock}, BackendKitty, BackendBlock},
		{"kitty only", []Backend{BackendKitty, BackendBlock}, BackendKitty, BackendBlock},
		{"sixel only", []Backend{BackendSixel, BackendBlock}, BackendSixel, BackendBlock},
		{"block only", []Backend{BackendBlock}, BackendBlock, BackendBlock},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewState(120, 40)
			s.SetEnabled(c.enabled)
			if s.Backend != c.wantActive {
				t.Fatalf("active backend = %d, want %d", s.Backend, c.wantActive)
			}
			if last := s.Enabled[len(s.Enabled)-1]; last != c.wantLast {
				t.Fatalf("last enabled = %d, want %d (block floor)", last, c.wantLast)
			}
		})
	}
}

// TestStreamLoopQuitsAfterDownArrowWhenSendKeyBlocks asserts that a blocked scroll
// dispatch does not keep the loop from seeing a later quit key.
func TestStreamLoopQuitsAfterDownArrowWhenSendKeyBlocks(t *testing.T) {
	cases := []struct {
		name string
		quit keyEvent
	}{
		{"ctrl-q", keyEvent{b: keyQuitCtrlQ}},
		{"ctrl-c", keyEvent{b: keyQuitCtrlC}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			sendStarted := make(chan string, 1)
			releaseSend := make(chan struct{})
			e := streamTestEngine{
				frames: make(chan render.Frame),
				sendKey: func(code string) {
					sendStarted <- code
					<-releaseSend
				},
			}
			keySender := newKeySender(e)
			defer keySender.close()
			s := &streamSession{eng: e, st: &st, keySender: keySender}
			keys := make(chan keyEvent, 2)
			keys <- keyEvent{scroll: ScrollDown}
			keys <- c.quit

			loopDone := make(chan error, 1)
			go func() {
				loopDone <- s.loop(nil, keys)
			}()

			select {
			case code := <-sendStarted:
				if code != "ArrowDown" {
					close(releaseSend)
					t.Fatalf("SendKey code = %q, want ArrowDown", code)
				}
			case <-time.After(100 * time.Millisecond):
				close(releaseSend)
				t.Fatalf("SendKey was not called for Down arrow")
			}

			select {
			case err := <-loopDone:
				close(releaseSend)
				if err != nil {
					t.Fatalf("loop returned error: %v", err)
				}
			case <-time.After(100 * time.Millisecond):
				close(releaseSend)
				<-loopDone
				t.Fatalf("loop did not quit after Down arrow then %s", c.name)
			}
		})
	}
}

// TestWaitForNavigateQuitsWhileNavigatePending asserts quit keys are handled
// during startup, before Navigate returns and before a frame channel is ready.
func TestWaitForNavigateQuitsWhileNavigatePending(t *testing.T) {
	cases := []struct {
		name string
		key  byte
	}{
		{"ctrl-q", keyQuitCtrlQ},
		{"ctrl-c", keyQuitCtrlC},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			s := &streamSession{eng: streamTestEngine{frames: make(chan render.Frame)}, st: &st}
			keys := make(chan keyEvent, 1)
			keys <- keyEvent{b: c.key}
			navigated := make(chan error)

			if err := s.waitForNavigate(nil, keys, navigated); err != nil {
				t.Fatalf("waitForNavigate returned error on quit: %v", err)
			}
		})
	}
}

// TestWaitForNavigateIgnoresBrowserKeysWhileNavigatePending pins the startup
// key policy: before Navigate completes, quit keys are live but browser keys and
// backend cycling must not touch the engine or viewer state.
func TestWaitForNavigateIgnoresBrowserKeysWhileNavigatePending(t *testing.T) {
	cases := []struct {
		name string
		key  keyEvent
	}{
		{"scroll", keyEvent{scroll: ScrollDown}},
		{"tab", keyEvent{b: keyTab}},
		{"plain q", keyEvent{b: 'q'}},
		{"alt-r cycle", keyEvent{act: actionCycleBackend}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			startBackend := st.Backend
			var sentKeys []string
			var backends []string
			eng := streamTestEngine{
				frames: make(chan render.Frame),
				sendKey: func(code string) {
					sentKeys = append(sentKeys, code)
				},
				setBackend: func(name string) {
					backends = append(backends, name)
				},
			}
			s := &streamSession{eng: eng, st: &st}
			keys := make(chan keyEvent, 2)
			keys <- c.key
			keys <- keyEvent{b: keyQuitCtrlQ}
			navigated := make(chan error)

			if err := s.waitForNavigate(nil, keys, navigated); err != nil {
				t.Fatalf("waitForNavigate returned error after startup key: %v", err)
			}
			if len(sentKeys) != 0 {
				t.Fatalf("SendKey calls = %v, want none before Navigate completes", sentKeys)
			}
			if len(backends) != 0 {
				t.Fatalf("SetBackend calls = %v, want none before Navigate completes", backends)
			}
			if st.Backend != startBackend {
				t.Fatalf("Backend = %v, want %v before Navigate completes", st.Backend, startBackend)
			}
		})
	}
}

// TestWaitForNavigateReturnsNavigateError keeps startup errors visible once
// Navigate does finish, so browser launch failures still reach the caller.
func TestWaitForNavigateReturnsNavigateError(t *testing.T) {
	st := NewState(80, 24)
	s := &streamSession{eng: streamTestEngine{frames: make(chan render.Frame)}, st: &st}
	keys := make(chan keyEvent)
	want := errors.New("navigate failed")
	navigated := make(chan error, 1)
	navigated <- want

	if err := s.waitForNavigate(nil, keys, navigated); !errors.Is(err, want) {
		t.Fatalf("waitForNavigate error = %v, want %v", err, want)
	}
}

// TestRenderFrameBlockColourModes asserts the production differ emits every row
// after Reset, emits no cell payload for an unchanged second draw, and honours
// the selected truecolour, xterm-256, or monochrome mode.
func TestRenderFrameBlockColourModes(t *testing.T) {
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	for _, tc := range []struct {
		name       string
		mode       ColourMode
		wantSGR    string
		forbidSGRs bool
	}{
		{name: "truecolour", mode: ColourTrue, wantSGR: "\x1b[38;2;"},
		{name: "xterm-256", mode: ColourXTerm256, wantSGR: "\x1b[38;5;"},
		{name: "monochrome", mode: ColourMono, forbidSGRs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewState(8, 4)
			s.Colour = tc.mode
			var d block.GridDiff
			s.SetDiff(&d)
			d.Reset()

			var first bytes.Buffer
			if err := s.RenderStreamFrame(render.Frame{Img: src}, &first); err != nil {
				t.Fatalf("first draw: %v", err)
			}
			out := first.String()
			// The compose targets the page grid: the bar reserves the bottom row.
			for row := range engineRows(s.Rows) {
				if move := fmt.Sprintf("\x1b[%d;1H", row+1); !strings.Contains(out, move) {
					t.Fatalf("first draw omitted row %d CUP %q", row, move)
				}
			}
			if tc.wantSGR != "" && !strings.Contains(out, tc.wantSGR) {
				t.Fatalf("first draw missing %q SGR", tc.wantSGR)
			}
			if tc.forbidSGRs && (strings.Contains(out, "\x1b[38;") || strings.Contains(out, "\x1b[48;")) {
				t.Fatalf("monochrome draw leaked colour SGR: %q", out)
			}

			var second bytes.Buffer
			if err := s.RenderStreamFrame(render.Frame{Img: src}, &second); err != nil {
				t.Fatalf("unchanged draw: %v", err)
			}
			if second.Len() != 0 {
				t.Fatalf("unchanged draw emitted %q", second.String())
			}
		})
	}
}

// TestRenderFrameGraphics asserts the graphics backends emit a non-empty framed
// payload through RenderStreamFrame for an engine-sized image.
func TestRenderFrameGraphics(t *testing.T) {
	s := NewState(8, 4)
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})

	for _, b := range []Backend{BackendSixel, BackendKitty} {
		s.Backend = b
		frame := render.Frame{Img: src}
		if b == BackendKitty {
			frame = render.Frame{EncodedPNG: encodedPNG(t, src)}
		}
		var buf bytes.Buffer
		if err := s.RenderStreamFrame(frame, &buf); err != nil {
			t.Fatalf("render backend %d: %v", b, err)
		}
		if buf.Len() == 0 {
			t.Fatalf("render backend %d: empty output", b)
		}
	}
}

// TestRenderFrameGraphicsFillsGrid asserts the graphics backends fill the whole
// grid from the top-left and never emit a letterbox CUP move, whatever the image
// aspect. An aspect-fit would letterbox a non-1:2 grid and shift the bitmap off
// the corner. Each backend fills the grid its own way: Kitty passes the payload
// through unchanged and carries the c=/r= keys so the terminal scales the
// placement to the cell area, while Sixel rasters at fixed pixel size, so the
// viewer fits an off-grid bitmap to the grid extent before encoding
// (fitSixelToGrid). The first kitty frame transmits under image id 1 (i=1) with
// no preceding delete, the double-buffer start state.
func TestRenderFrameGraphicsFillsGrid(t *testing.T) {
	cases := []struct {
		name       string
		cols, rows int
		imgW, imgH int
	}{
		{"tall image", 8, 4, 16, 64},
		{"square image on square grid", 4, 4, 16, 16},
		{"fills grid", 8, 4, 16, 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := solidImageSize(c.imgW, c.imgH, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
			for _, b := range []Backend{BackendSixel, BackendKitty} {
				// The terminal has one extra row for the bar; the page grid
				// the placement and the Sixel fit target is c.rows.
				s := NewState(c.cols, c.rows+1)
				s.Backend = b
				var buf bytes.Buffer
				frame := render.Frame{Img: src}
				if b == BackendKitty {
					frame = render.Frame{EncodedPNG: encodedPNG(t, src)}
				}
				if err := s.RenderStreamFrame(frame, &buf); err != nil {
					t.Fatalf("render backend %d: %v", b, err)
				}
				out := buf.String()
				if cupPresent(out) {
					t.Fatalf("backend %d (%s): emitted a CUP move, want none (identity fit fills the grid)", b, c.name)
				}

				var (
					decoded *image.RGBA
					err     error
				)
				want := src.Bounds()
				if b == BackendKitty {
					if !strings.Contains(out, "i=1") || !strings.Contains(out, "a=T") {
						t.Fatalf("kitty: first frame missing the i=1 transmit-and-place")
					}
					if strings.Contains(out, "a=d") {
						t.Fatalf("kitty: first frame emitted a delete, want none")
					}
					// The payload passes through at the source size; the grid fill is
					// the terminal's, via the c=/r= placement keys.
					gridKeys := fmt.Sprintf("c=%d,r=%d", c.cols, c.rows)
					if !strings.Contains(out, gridKeys) {
						t.Fatalf("kitty: transmit missing the %s grid-fit keys", gridKeys)
					}
					decoded, err = renderdecode.DecodeKitty(buf.Bytes())
				} else {
					// Sixel rasters at pixel size, so the viewer fits an off-grid
					// bitmap to the grid extent before encoding.
					gridW, gridH := render.GraphicsCaptureSize(c.cols, c.rows, s.W, s.H)
					want = image.Rect(0, 0, gridW, gridH)
					decoded, err = renderdecode.DecodeSixel(buf.Bytes())
				}
				if err != nil {
					t.Fatalf("decode backend %d output: %v", b, err)
				}
				if got := decoded.Bounds(); !got.Eq(want) {
					t.Fatalf("backend %d decoded bounds = %v, want full-grid target %v", b, got, want)
				}
			}
		})
	}
}

func TestRenderFrameKittyDropsImageOnlyFrame(t *testing.T) {
	s := NewState(8, 4)
	s.Backend = BackendKitty
	var out bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: solidImage(color.RGBA{A: 0xff})}, &out); err != nil {
		t.Fatalf("render image-only Kitty frame: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("image-only Kitty frame wrote %d bytes, want 0", out.Len())
	}
}

// cupPresent reports whether out holds a cursor-position escape (\x1b[<n>;<n>H).
// It distinguishes the CUP move from the other CSI escapes the graphics payload
// may carry, so the fills-grid case asserts the move is absent, not that no CSI
// escape exists.
func cupPresent(out string) bool {
	for i := 0; i+1 < len(out); i++ {
		if out[i] == '\x1b' && out[i+1] == '[' {
			j := i + 2
			for j < len(out) && (out[j] == ';' || (out[j] >= '0' && out[j] <= '9')) {
				j++
			}
			if j < len(out) && out[j] == 'H' {
				return true
			}
		}
	}
	return false
}

// TestRenderFrameNoFit asserts a degenerate grid (zero cells) writes nothing and
// does not error, so the viewer tolerates a collapsed terminal.
func TestRenderFrameNoFit(t *testing.T) {
	s := NewState(0, 0)
	s.SetDiff(&block.GridDiff{})
	var buf bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: solidImage(color.RGBA{A: 0xff})}, &buf); err != nil {
		t.Fatalf("render zero grid: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("zero grid wrote %d bytes, want 0", buf.Len())
	}
}

// clearMuxEnv unsets every focus-multiplexer env var in muxKeys for the
// current test, registering cleanup, so a host-set TMUX or ZELLIJ does not
// leak into a false-branch case.
func clearMuxEnv(t *testing.T) {
	t.Helper()
	for _, k := range muxKeys {
		t.Setenv(k, "") // register the cleanup restore
		os.Unsetenv(k)  // then remove it so LookupEnv reports absent
	}
}

// newTempTTY returns a regular temp file standing in for the tty. It is not a
// terminal, so IsTerminal is false and the winsize ioctl fails, which is the
// intended path: configureBackends falls through to the 8x16 default and any
// probeTerminal write lands in the file where the test can read it back.
func newTempTTY(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "nyala-tty-*")
	if err != nil {
		t.Fatalf("temp tty: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestFitSixelToGrid pins the Sixel grid fit: a frame at the grid extent
// returns unchanged (the common path pays nothing), a height-only shortfall
// under one cell draws as-is (the engine's floored fractional-density row
// band, not a zoom), and a stale-density frame (both axes short, the post-zoom
// shape) scales onto the full grid extent.
func TestFitSixelToGrid(t *testing.T) {
	st := NewState(10, 5) // cell 8x16 default: page grid 10x4, extent 80x64
	exact := image.NewRGBA(image.Rect(0, 0, 80, 64))
	if got := st.fitSixelToGrid(exact); got != exact {
		t.Fatal("exact-extent frame was rescaled, want identity")
	}
	rowBand := image.NewRGBA(image.Rect(0, 0, 80, 50)) // short by under one 16px cell
	if got := st.fitSixelToGrid(rowBand); got != rowBand {
		t.Fatal("sub-cell height shortfall was rescaled, want identity")
	}
	stale := image.NewRGBA(image.Rect(0, 0, 64, 51)) // stale-density post-zoom frame
	got := st.fitSixelToGrid(stale)
	if b := got.Bounds(); b.Dx() != 80 || b.Dy() != 64 {
		t.Fatalf("stale frame fitted to %dx%d, want the 80x64 grid extent", b.Dx(), b.Dy())
	}
}

// TestFitSixelToGridHonoursBottomRowReservation pins the reconciliation
// between fitSixelToGrid and the engine-grid bar reservation: an engine-sized
// (rows-1) frame passes through unchanged rather than being upscaled back
// over the reserved bar row, and a stale full-height frame is rescaled down
// to the reserved extent.
func TestFitSixelToGridHonoursBottomRowReservation(t *testing.T) {
	st := NewState(8, 4)
	st.Backend = BackendSixel
	gridW, gridH := 8*st.W, 3*st.H // engine grid: rows-1

	engineSized := image.NewRGBA(image.Rect(0, 0, gridW, gridH))
	if got := st.fitSixelToGrid(engineSized); got != engineSized {
		t.Fatalf("engine-sized frame was rescaled: bounds %v, want unchanged %v",
			got.Bounds(), engineSized.Bounds())
	}

	fullHeight := image.NewRGBA(image.Rect(0, 0, gridW, 4*st.H))
	got := st.fitSixelToGrid(fullHeight)
	if got.Bounds().Dx() != gridW || got.Bounds().Dy() != gridH {
		t.Fatalf("full-height frame fit = %v, want %dx%d (reserved bottom row)",
			got.Bounds(), gridW, gridH)
	}
}

type streamTestEngine struct {
	frames         chan render.Frame
	sendKey        func(string)
	sendText       func(string)
	sendPaste      func(string)
	setBackend     func(string)
	sendNav        func(NavAction)
	sendMouse      func(MouseEvent)
	openURL        func(string)
	currentURL     string
	currentTitle   string
	browserVersion string
	find           func(string)
	findNext       func()
	findPrev       func()
	findClear      func()
	findState      *FindState
	selection      func() (string, bool)
	// pasteDiscarded is what SendPaste reports: true stands for a page that
	// held no editable target, so the viewer signals the discarded paste.
	pasteDiscarded bool
}

func (e streamTestEngine) SetBackend(backend Backend) {
	if e.setBackend != nil {
		e.setBackend(backend.String())
	}
}

func (e streamTestEngine) SetCellPx(int, int) {}

func (e streamTestEngine) Resize(int, int) {}

func (e streamTestEngine) Navigate(string) error { return nil }

func (e streamTestEngine) Frames() <-chan render.Frame { return e.frames }

func (e streamTestEngine) SendKey(code string) {
	if e.sendKey != nil {
		e.sendKey(code)
	}
}

func (e streamTestEngine) SendText(text string) {
	if e.sendText != nil {
		e.sendText(text)
	}
}

func (e streamTestEngine) SendPaste(text string) bool {
	if e.sendPaste != nil {
		e.sendPaste(text)
	}
	return e.pasteDiscarded
}

func (e streamTestEngine) SendNav(a NavAction) {
	if e.sendNav != nil {
		e.sendNav(a)
	}
}

func (e streamTestEngine) CurrentURL() string { return e.currentURL }

func (e streamTestEngine) CurrentTitle() string { return e.currentTitle }

func (e streamTestEngine) OpenURL(url string) {
	if e.openURL != nil {
		e.openURL(url)
	}
}

func (e streamTestEngine) BrowserVersion() string { return e.browserVersion }

func (e streamTestEngine) Find(query string) {
	if e.find != nil {
		e.find(query)
	}
}

func (e streamTestEngine) FindNext() {
	if e.findNext != nil {
		e.findNext()
	}
}

func (e streamTestEngine) FindPrev() {
	if e.findPrev != nil {
		e.findPrev()
	}
}

func (e streamTestEngine) FindClear() {
	if e.findClear != nil {
		e.findClear()
	}
}

func (e streamTestEngine) FindState() FindState {
	if e.findState != nil {
		return *e.findState
	}
	return FindState{}
}

func (e streamTestEngine) Selection() (string, bool) {
	if e.selection != nil {
		return e.selection()
	}
	return "", false
}

func (e streamTestEngine) SendMouse(ev MouseEvent) {
	if e.sendMouse != nil {
		e.sendMouse(ev)
	}
}

func (e streamTestEngine) Close() error { return nil }

// solidImage builds a 16 x 16 RGBA image filled with one colour.
func solidImage(c color.RGBA) *image.RGBA {
	return solidImageSize(16, 16, c)
}

// benchStreamImage builds a w x h RGBA viewport bitmap with a deterministic
// non-uniform pattern, so the block downscale and compose do real work rather
// than collapsing a flat fill. The pattern is a fixed function of the pixel
// coordinates, so the bitmap is identical run to run.
func benchStreamImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8((x * 5) & 0xff)
			img.Pix[i+1] = uint8((y * 3) & 0xff)
			img.Pix[i+2] = uint8((x + y) & 0xff)
			img.Pix[i+3] = 0xff
		}
	}
	return img
}

// benchStreamRuns builds a deterministic crisp-text overlay: a row of short text
// runs down the left of the grid, so the block compose folds a representative
// overlay rather than drawing the bitmap alone.
func benchStreamRuns(rows int) []render.GlyphRun {
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	runs := make([]render.GlyphRun, 0, rows)
	for row := range rows {
		runs = append(runs, render.GlyphRun{
			Col: 0, Row: row, Text: "nyala line", FG: fg,
			X: 0, Y: float64(row * 16), W: 80, H: 16,
		})
	}
	return runs
}

// benchStreamFrame builds the representative text-bearing block frame the
// RenderStreamFrame benchmark drives, sized to the 120x40 diagnostics grid at the
// 8x16 cell default (960x640 px). It is deterministic, so the benchmark is stable
// run to run.
func benchStreamFrame() render.Frame {
	return render.Frame{
		Img:  benchStreamImage(120*cellraster.CellPxW, 40*cellraster.CellPxH),
		Text: benchStreamRuns(40),
	}
}

// BenchmarkRenderStreamFrameUnchanged is the live-path allocation baseline. It
// drives State.RenderStreamFrame on the block backend through the
// cell differ at the fixed 120x40 grid, redrawing the same text-bearing frame.
// The differ is primed with one draw outside the timed loop, so each measured
// iteration is the diffed steady-state path: an unchanged frame that emits no
// terminal bytes but still composes the grid. b.ReportAllocs reports allocs/op
// and B/op for tracking steady-state allocation costs.
func BenchmarkRenderStreamFrameUnchanged(b *testing.B) {
	s := NewState(120, 40)
	s.SetEnabled([]Backend{BackendBlock})
	var d block.GridDiff
	s.SetDiff(&d)
	frame := benchStreamFrame()

	// Prime the differ so the timed loop measures the unchanged-frame steady state,
	// not the first full repaint.
	if err := s.RenderStreamFrame(frame, io.Discard); err != nil {
		b.Fatalf("prime RenderStreamFrame: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := s.RenderStreamFrame(frame, io.Discard); err != nil {
			b.Fatalf("RenderStreamFrame: %v", err)
		}
	}
}

// BenchmarkRenderStreamFrameChanged is the full-repaint companion to the
// unchanged-frame baseline. It resets the differ each iteration, so every draw is
// a first-frame full repaint against a blank screen. It frames the worst-case
// cost beside the steady-state baseline.
func BenchmarkRenderStreamFrameChanged(b *testing.B) {
	s := NewState(120, 40)
	s.SetEnabled([]Backend{BackendBlock})
	var d block.GridDiff
	s.SetDiff(&d)
	frame := benchStreamFrame()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		d.Reset()
		if err := s.RenderStreamFrame(frame, io.Discard); err != nil {
			b.Fatalf("RenderStreamFrame: %v", err)
		}
	}
}

// diffGridOf builds a cols x rows block grid filled with one cell value, for the
// viewer-side cell-diff tests.
func diffGridOf(cols, rows int, fill block.Cell) block.Grid {
	g := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for i := range g.Cells {
		g.Cells[i] = fill
	}
	return g
}

// TestRenderStreamFrameRoutesThroughDiff asserts the viewer routes block frames
// through the cell differ once SetDiff is set: a second identical frame emits no
// bytes, the diff's defining behaviour, where a full rewrite would re-emit every
// cell.
func TestRenderStreamFrameRoutesThroughDiff(t *testing.T) {
	s := NewState(8, 4)
	s.SetEnabled([]Backend{BackendBlock})
	var d block.GridDiff
	s.SetDiff(&d)

	frame := render.Frame{Img: solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})}

	var first bytes.Buffer
	if err := s.RenderStreamFrame(frame, &first); err != nil {
		t.Fatalf("first RenderStreamFrame: %v", err)
	}
	if first.Len() == 0 {
		t.Fatal("first frame emitted nothing through the differ")
	}
	var second bytes.Buffer
	if err := s.RenderStreamFrame(frame, &second); err != nil {
		t.Fatalf("second RenderStreamFrame: %v", err)
	}
	if second.Len() != 0 {
		t.Fatalf("unchanged frame re-emitted through the differ: %q", second.String())
	}
}

// TestViewerDiffChangedCellCountDrivesEmit pins AC 4 on the viewer's differ path:
// one changed cell emits escapes for that cell only, not the whole grid. It drives
// the viewer's own GridDiff with the viewer's colour funcs in monochrome, so the
// only escape is the single cell's CUP.
func TestViewerDiffChangedCellCountDrivesEmit(t *testing.T) {
	var d block.GridDiff
	fg, bg := fgSGRFunc(ColourMono), bgSGRFunc(ColourMono)
	g := diffGridOf(6, 3, block.Cell{Glyph: 'a'})
	if err := d.Emit(&bytes.Buffer{}, g, fg, bg); err != nil {
		t.Fatal(err)
	}
	g.At(4, 2).Glyph = 'z'
	var out bytes.Buffer
	if err := d.Emit(&out, g, fg, bg); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "\x1b["); n != 1 {
		t.Fatalf("want one CUP for the single changed cell, got %d: %q", n, got)
	}
	if strings.Contains(got, "a") {
		t.Fatalf("emitted an unchanged cell: %q", got)
	}
	if !strings.HasSuffix(got, "z") {
		t.Fatalf("changed glyph not last byte: %q", got)
	}
}

// TestViewerDiffUniformRowOneSGRPair pins AC 5 on the viewer's differ path: a
// uniform-colour changed row sends one truecolour SGR pair, not one per cell.
func TestViewerDiffUniformRowOneSGRPair(t *testing.T) {
	var d block.GridDiff
	fg, bg := fgSGRFunc(ColourTrue), bgSGRFunc(ColourTrue)
	g := diffGridOf(5, 1, block.Cell{
		Glyph: 'a',
		FG:    color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff},
		BG:    color.RGBA{R: 0x40, G: 0x50, B: 0x60, A: 0xff},
	})
	if err := d.Emit(&bytes.Buffer{}, g, fg, bg); err != nil {
		t.Fatal(err)
	}
	for col := range 5 {
		g.At(col, 0).Glyph = 'z'
	}
	var out bytes.Buffer
	if err := d.Emit(&out, g, fg, bg); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "\x1b[38;2;"); n != 1 {
		t.Fatalf("want one fg SGR for the uniform row, got %d: %q", n, got)
	}
	if n := strings.Count(got, "\x1b[48;2;"); n != 1 {
		t.Fatalf("want one bg SGR for the uniform row, got %d: %q", n, got)
	}
}

// TestViewerDiffContiguousRunOneCUP pins AC 6 on the viewer's differ path: a
// contiguous changed run emits a single CUP for the run. Monochrome removes SGR
// noise, so every escape in the stream is a CUP.
func TestViewerDiffContiguousRunOneCUP(t *testing.T) {
	var d block.GridDiff
	fg, bg := fgSGRFunc(ColourMono), bgSGRFunc(ColourMono)
	g := diffGridOf(6, 1, block.Cell{Glyph: 'a'})
	if err := d.Emit(&bytes.Buffer{}, g, fg, bg); err != nil {
		t.Fatal(err)
	}
	for _, col := range []int{1, 2, 3} {
		g.At(col, 0).Glyph = 'z'
	}
	var out bytes.Buffer
	if err := d.Emit(&out, g, fg, bg); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "\x1b["); n != 1 {
		t.Fatalf("want one CUP for the contiguous run, got %d: %q", n, got)
	}
	if n := strings.Count(got, "z"); n != 3 {
		t.Fatalf("want 3 changed glyphs in the run, got %d: %q", n, got)
	}
}
