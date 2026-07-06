package viewer

import (
	"bufio"
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// TestCycleBackendOrder asserts the default cycle is best-first kitty -> sixel ->
// block: the initial active backend is kitty and Tab steps down, wrapping.
func TestCycleBackendOrder(t *testing.T) {
	s := NewState(120, 40, 64, 64)
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
// so the initial active backend is kitty and Tab steps block -> kitty.
func TestCycleBackendSkipsDisabled(t *testing.T) {
	s := NewState(120, 40, 64, 64)
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
	s := NewState(120, 40, 64, 64)
	s.SetEnabled([]Backend{BackendBlock})
	for i := range 3 {
		if got := s.CycleBackend(); got != BackendBlock {
			t.Fatalf("cycle %d: got %d, want BackendBlock", i, got)
		}
	}
}

// TestCycleBackendAllThree asserts CycleBackend steps the full best-first set
// kitty -> sixel -> block, wrapping, the same as the default cycle. The initial
// active backend is the kitty ceiling.
func TestCycleBackendAllThree(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
	if s.Backend != BackendKitty {
		t.Fatalf("after SetEnabled, backend = %d, want BackendKitty", s.Backend)
	}
	want := []Backend{BackendSixel, BackendBlock, BackendKitty}
	for i, w := range want {
		if got := s.CycleBackend(); got != w {
			t.Fatalf("cycle %d: got %d, want %d", i, got, w)
		}
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
			s := NewState(120, 40, 64, 64)
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

// TestResolveScheme asserts the interactive --blocks resolution: half and
// quadrant map straight through, auto and sextant render sextant on a supported
// terminal and degrade to quadrant on an unsupported one. It also pins the
// engine-name mapping for each resolved scheme.
func TestResolveScheme(t *testing.T) {
	supportedEnv := []string{"TERM=xterm-kitty"}
	unsupportedEnv := []string{"TERM=xterm-256color"}
	cases := []struct {
		name    string
		blocks  string
		env     []string
		scheme  block.Scheme
		engName string
	}{
		{"quadrant", "quadrant", supportedEnv, block.SchemeQuadrant, "quadrant"},
		{"half", "half", supportedEnv, block.SchemeHalfBlock, "half"},
		{"auto supported", "auto", supportedEnv, block.SchemeSextant, "sextant"},
		{"auto unsupported", "auto", unsupportedEnv, block.SchemeQuadrant, "quadrant"},
		{"sextant supported", "sextant", supportedEnv, block.SchemeSextant, "sextant"},
		{"sextant unsupported", "sextant", unsupportedEnv, block.SchemeQuadrant, "quadrant"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveScheme(c.blocks, c.env); got != c.scheme {
				t.Errorf("resolveScheme(%q, env) = %d, want %d", c.blocks, got, c.scheme)
			}
			if got := schemeEngineName(c.scheme); got != c.engName {
				t.Errorf("schemeEngineName(%d) = %q, want %q", c.scheme, got, c.engName)
			}
		})
	}
}

// TestResolveCaptureScheme asserts the deterministic capture --blocks resolution,
// which runs no terminal detection: sextant forces sextant, half forces
// half-block, and everything else, including auto, forces quadrant so the default
// capture and corpus sweeps are unchanged.
func TestResolveCaptureScheme(t *testing.T) {
	cases := []struct {
		blocks string
		scheme block.Scheme
	}{
		{"sextant", block.SchemeSextant},
		{"half", block.SchemeHalfBlock},
		{"quadrant", block.SchemeQuadrant},
		{"auto", block.SchemeQuadrant},
		{"", block.SchemeQuadrant},
	}
	for _, c := range cases {
		if got := resolveCaptureScheme(c.blocks); got != c.scheme {
			t.Errorf("resolveCaptureScheme(%q) = %d, want %d", c.blocks, got, c.scheme)
		}
	}
}

// TestResizeRefits asserts a resize changes the fit, so the viewer re-fits the
// image to the new terminal size.
func TestResizeRefits(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	before := s.Fit()
	s.Resize(40, 20)
	after := s.Fit()
	if before == after {
		t.Fatalf("fit unchanged after resize: %+v", after)
	}
	if after.Cols != 40 || after.Rows != 20 {
		t.Fatalf("fit grid = %dx%d, want 40x20", after.Cols, after.Rows)
	}
}

// TestRendererSwapsBehindSeam asserts each backend yields a render.Renderer with
// the matching Name, and that switching backend changes only the returned value:
// the same Frame renders through every backend with no error.
func TestRendererSwapsBehindSeam(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	frame := render.Frame{Img: solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})}

	cases := []struct {
		backend Backend
		name    string
	}{
		{BackendBlock, "block"},
		{BackendSixel, "sixel"},
		{BackendKitty, "kitty"},
	}
	for _, c := range cases {
		s.Backend = c.backend
		r := s.Renderer()
		if r.Name() != c.name {
			t.Fatalf("backend %d name = %q, want %q", c.backend, r.Name(), c.name)
		}
		var buf bytes.Buffer
		if err := r.Render(frame, &buf); err != nil {
			t.Fatalf("render backend %q: %v", c.name, err)
		}
		if buf.Len() == 0 {
			t.Fatalf("render backend %q: empty output", c.name)
		}
	}
}

// TestRendererConcreteTypes asserts each backend resolves to its concrete type,
// confirming the swap is a renderer-value swap and nothing above the seam.
func TestRendererConcreteTypes(t *testing.T) {
	s := NewState(8, 4, 16, 16)

	s.Backend = BackendBlock
	if _, ok := s.Renderer().(*block.Renderer); !ok {
		t.Fatalf("block backend = %T, want *block.Renderer", s.Renderer())
	}
	s.Backend = BackendSixel
	if _, ok := s.Renderer().(*sixel.Renderer); !ok {
		t.Fatalf("sixel backend = %T, want *sixel.Renderer", s.Renderer())
	}
	s.Backend = BackendKitty
	if _, ok := s.Renderer().(*kitty.Renderer); !ok {
		t.Fatalf("kitty backend = %T, want *kitty.Renderer", s.Renderer())
	}
}

// TestHandleKey asserts the hotkey table: Tab cycles, 'q' and Ctrl-C quit, and
// any other key (including the retired 's' and 'c' toggles) is an inert no-op.
func TestHandleKey(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
	if s.Backend != BackendKitty {
		t.Fatalf("initial backend = %d, want BackendKitty", s.Backend)
	}

	if act := s.HandleKey(keyTab); !act.Redraw || act.Quit {
		t.Fatalf("Tab action = %+v, want redraw", act)
	}
	if s.Backend != BackendSixel {
		t.Fatalf("Tab did not cycle backend: %d", s.Backend)
	}

	// The scheme and colour toggles are retired: 's' and 'c' are now inert, so
	// they redraw nothing and leave the scheme and colour untouched.
	for _, b := range []byte{'s', 'S', 'c', 'C'} {
		if act := s.HandleKey(b); act.Redraw || act.Quit {
			t.Fatalf("%q action = %+v, want inert no-op", b, act)
		}
	}
	if s.Scheme != block.SchemeQuadrant {
		t.Fatalf("scheme changed after retired toggle: %d", s.Scheme)
	}
	if s.Colour != ColourTrue {
		t.Fatalf("colour changed after retired toggle: %d", s.Colour)
	}

	if act := s.HandleKey('z'); act.Redraw || act.Quit {
		t.Fatalf("unknown key action = %+v, want no-op", act)
	}

	if act := s.HandleKey('q'); !act.Quit {
		t.Fatalf("'q' action = %+v, want quit", act)
	}
	if act := s.HandleKey(keyQuitCtrlC); !act.Quit {
		t.Fatalf("Ctrl-C action = %+v, want quit", act)
	}
}

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

// TestStreamLoopQuitsAfterDownArrowWhenSendKeyBlocks pins the regression where
// a blocked scroll dispatch keeps the loop from seeing a later quit key.
func TestStreamLoopQuitsAfterDownArrowWhenSendKeyBlocks(t *testing.T) {
	cases := []struct {
		name string
		quit keyEvent
	}{
		{"q", keyEvent{b: 'q'}},
		{"ctrl-c", keyEvent{b: keyQuitCtrlC}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24, 1, 1)
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
		{"q", 'q'},
		{"Q", 'Q'},
		{"ctrl-c", keyQuitCtrlC},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24, 1, 1)
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24, 1, 1)
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
			keys <- keyEvent{b: 'q'}
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
	st := NewState(80, 24, 1, 1)
	s := &streamSession{eng: streamTestEngine{frames: make(chan render.Frame)}, st: &st}
	keys := make(chan keyEvent)
	want := errors.New("navigate failed")
	navigated := make(chan error, 1)
	navigated <- want

	if err := s.waitForNavigate(nil, keys, navigated); !errors.Is(err, want) {
		t.Fatalf("waitForNavigate error = %v, want %v", err, want)
	}
}

// TestRenderFrameBlockColourMode asserts the block path honours the colour
// toggle: truecolour emits 24-bit SGR (38;2), xterm-256 emits indexed SGR (38;5).
func TestRenderFrameBlockColourMode(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})

	var trueBuf bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: src}, &trueBuf); err != nil {
		t.Fatalf("render truecolour: %v", err)
	}
	if !strings.Contains(trueBuf.String(), "\x1b[38;2;") {
		t.Fatalf("truecolour output missing 24-bit SGR")
	}

	s.Colour = ColourXTerm256
	var idxBuf bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: src}, &idxBuf); err != nil {
		t.Fatalf("render xterm-256: %v", err)
	}
	out := idxBuf.String()
	if !strings.Contains(out, "\x1b[38;5;") {
		t.Fatalf("xterm-256 output missing indexed SGR")
	}
	if strings.Contains(out, "\x1b[38;2;") {
		t.Fatalf("xterm-256 output leaked 24-bit SGR")
	}
}

// TestRenderFrameGraphics asserts the graphics backends emit a non-empty framed
// payload through RenderStreamFrame for a fitted image.
func TestRenderFrameGraphics(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})

	for _, b := range []Backend{BackendSixel, BackendKitty} {
		s.Backend = b
		var buf bytes.Buffer
		if err := s.RenderStreamFrame(render.Frame{Img: src}, &buf); err != nil {
			t.Fatalf("render backend %d: %v", b, err)
		}
		if buf.Len() == 0 {
			t.Fatalf("render backend %d: empty output", b)
		}
	}
}

// TestRenderFrameGraphicsFillsGrid asserts the graphics backends fill the whole
// grid from the top-left and never emit a letterbox CUP move, whatever the image
// aspect. The engine sizes the graphics bitmap to the full grid, so the identity
// graphicsFit maps it one-to-one and the 1:2 FitImage stays out of the path; a
// 1:2 fit would letterbox a non-1:2 grid and shift the bitmap off the corner. The
// first kitty frame transmits under image id 1 (i=1) with no preceding delete, the
// double-buffer start state.
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
			src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
			for _, b := range []Backend{BackendSixel, BackendKitty} {
				s := NewState(c.cols, c.rows, c.imgW, c.imgH)
				s.Backend = b
				var buf bytes.Buffer
				if err := s.RenderStreamFrame(render.Frame{Img: src}, &buf); err != nil {
					t.Fatalf("render backend %d: %v", b, err)
				}
				out := buf.String()
				if cupPresent(out) {
					t.Fatalf("backend %d (%s): emitted a CUP move, want none (identity fit fills the grid)", b, c.name)
				}
				if b == BackendKitty {
					if !strings.Contains(out, "i=1") || !strings.Contains(out, "a=T") {
						t.Fatalf("kitty: first frame missing the i=1 transmit-and-place")
					}
					if strings.Contains(out, "a=d") {
						t.Fatalf("kitty: first frame emitted a delete, want none")
					}
				}
			}
		})
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
	s := NewState(0, 0, 16, 16)
	var buf bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: solidImage(color.RGBA{A: 0xff})}, &buf); err != nil {
		t.Fatalf("render zero grid: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("zero grid wrote %d bytes, want 0", buf.Len())
	}
}

// TestWriteGridRowsJoinedByCRLF asserts writeGrid joins rows with CRLF, not a
// bare LF, and emits no trailing newline after the last row. With auto-wrap off
// in raw mode, the carriage return is needed to return the cursor to column 0
// each row; a bare LF leaves it interlaced. A trailing newline on a full-height
// grid scrolls the top row off the terminal.
func TestWriteGridRowsJoinedByCRLF(t *testing.T) {
	g := block.Grid{Cols: 2, Rows: 2, Cells: make([]block.Cell, 4)}
	for i := range g.Cells {
		g.Cells[i] = block.Cell{Glyph: ' ', FG: color.RGBA{A: 0xff}, BG: color.RGBA{A: 0xff}}
	}
	var buf bytes.Buffer
	if err := writeGrid(g, ColourTrue, &buf); err != nil {
		t.Fatalf("writeGrid: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, "\n"); got != g.Rows-1 {
		t.Fatalf("line feed count = %d, want %d (one between each row)", got, g.Rows-1)
	}
	for i := range len(out) {
		if out[i] == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("line feed at byte %d not preceded by carriage return", i)
		}
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatalf("output ends with a newline; a full-height grid would scroll the top row off")
	}
}

// TestWriteGridEmitsText asserts writeGrid prefers a text cell's overlay
// grapheme over its bitmap glyph in every colour mode, and a Wide trailing cell
// emits nothing.
func TestWriteGridEmitsText(t *testing.T) {
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff}
	g := block.Grid{Cols: 2, Rows: 1, Cells: []block.Cell{
		{Glyph: 0x2588, FG: fg, BG: bg, Text: "Q"},
		{Glyph: 0x2588, FG: fg, BG: bg, Wide: true},
	}}
	for _, mode := range []ColourMode{ColourTrue, ColourXTerm256, ColourMono} {
		var buf bytes.Buffer
		if err := writeGrid(g, mode, &buf); err != nil {
			t.Fatalf("writeGrid mode %d: %v", mode, err)
		}
		out := buf.String()
		if got := strings.Count(out, "Q"); got != 1 {
			t.Fatalf("mode %d: grapheme count = %d, want 1", mode, got)
		}
		if strings.ContainsRune(out, 0x2588) {
			t.Fatalf("mode %d: output contains the bitmap rune; overlay must win", mode)
		}
	}
}

// TestWriteGridWideTrailingCellEmitsNothing places a width-2 grapheme so its
// lead cell holds the grapheme and the next cell is the Wide trailing cell. The
// rendered bytes must contain the grapheme exactly once and the trailing cell
// must contribute no printable character: the lead cell's wide grapheme already
// paints both columns, so a space would add a third column and misalign the row.
func TestWriteGridWideTrailingCellEmitsNothing(t *testing.T) {
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff}
	g := block.Grid{Cols: 2, Rows: 1, Cells: []block.Cell{
		{Glyph: 0x2588, FG: fg, BG: bg, Text: "世"},
		{Glyph: 0x2588, FG: fg, BG: bg, Wide: true},
	}}
	for _, mode := range []ColourMode{ColourTrue, ColourXTerm256, ColourMono} {
		var buf bytes.Buffer
		if err := writeGrid(g, mode, &buf); err != nil {
			t.Fatalf("writeGrid mode %d: %v", mode, err)
		}
		out := buf.String()
		if got := strings.Count(out, "世"); got != 1 {
			t.Fatalf("mode %d: grapheme count = %d, want 1", mode, got)
		}
		if strings.ContainsRune(out, ' ') {
			t.Fatalf("mode %d: output %q contains a space; the Wide trailing cell must emit nothing", mode, out)
		}
	}
}

// muxKeys is every focus-multiplexer env var multiplexed reads. clearMuxEnv
// unsets all of them for the current test, registering cleanup, so a host-set
// TMUX or ZELLIJ does not leak into a false-branch case.
var muxKeys = []string{"TMUX", "ZELLIJ", "HERDR_ENV", "HERDR_PANE_ID", "STY"}

func clearMuxEnv(t *testing.T) {
	t.Helper()
	for _, k := range muxKeys {
		t.Setenv(k, "") // register the cleanup restore
		os.Unsetenv(k)  // then remove it so LookupEnv reports absent
	}
}

// TestMultiplexedEnvKeys asserts multiplexed reports true for each focus
// multiplexer and false when none is set. ZELLIJ is a presence signal: a bare
// ZELLIJ=0 still gates. herdr needs both HERDR_ENV=1 and HERDR_PANE_ID; one key
// alone does not gate.
func TestMultiplexedEnvKeys(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"none set", nil, false},
		{"tmux", map[string]string{"TMUX": "/tmp/tmux-1000/default,123,0"}, true},
		{"zellij present", map[string]string{"ZELLIJ": "1"}, true},
		{"zellij zero is presence", map[string]string{"ZELLIJ": "0"}, true},
		{"screen", map[string]string{"STY": "1234.pts-0.host"}, true},
		{"herdr both keys", map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p1"}, true},
		{"herdr env only", map[string]string{"HERDR_ENV": "1"}, false},
		{"herdr pane only", map[string]string{"HERDR_PANE_ID": "p1"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMuxEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got := multiplexed(); got != c.want {
				t.Fatalf("multiplexed() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestConfigureBackendsSkipsHandshakeUnderMux asserts that under a multiplexer
// env var configureBackends enables block only and does not run probeTerminal.
// probeTerminal writes queryBytes() to the tty as its first act, so a tty file
// left empty after the call proves the handshake did not run. A non-mux baseline
// proves the same file path does carry a write when the handshake runs.
func TestConfigureBackendsSkipsHandshakeUnderMux(t *testing.T) {
	clearMuxEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")

	tty := newTempTTY(t)
	st := NewState(80, 24, 64, 64)
	if err := configureBackends(&st, tty, 80, 24, "auto"); err != nil {
		t.Fatalf("configureBackends: %v", err)
	}

	if len(st.Enabled) != 1 || st.Enabled[0] != BackendBlock {
		t.Fatalf("enabled = %v, want [BackendBlock]", st.Enabled)
	}
	if n := ttyByteCount(t, tty); n != 0 {
		t.Fatalf("handshake wrote %d bytes under mux; probeTerminal must not run", n)
	}
}

// TestConfigureBackendsRunsHandshakeWithoutMux is the skip-handshake baseline:
// with no mux env var set, configureBackends calls probeTerminal, which writes
// the query batch to the tty. A non-empty tty file proves the handshake ran, so
// the empty-file assertion under mux is meaningful, not vacuous.
func TestConfigureBackendsRunsHandshakeWithoutMux(t *testing.T) {
	clearMuxEnv(t)

	tty := newTempTTY(t)
	st := NewState(80, 24, 64, 64)
	if err := configureBackends(&st, tty, 80, 24, "auto"); err != nil {
		t.Fatalf("configureBackends: %v", err)
	}

	if n := ttyByteCount(t, tty); n == 0 {
		t.Fatalf("handshake wrote no bytes without mux; probeTerminal should run")
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

// ttyByteCount returns the number of bytes written to the temp tty file.
func ttyByteCount(t *testing.T, f *os.File) int64 {
	t.Helper()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat tty: %v", err)
	}
	return info.Size()
}

// TestWriteGridMonoNoSGR asserts writeGrid in ColourMono emits each cell's glyph
// with no fg SGR (\x1b[38;) and no bg SGR (\x1b[48;), an SGR reset (\x1b[0m) per
// row, and a reset-plus-CRLF (\x1b[0m\r\n) between rows (rows-1, none trailing).
// A leaked fg/bg sequence fails the test.
func TestWriteGridMonoNoSGR(t *testing.T) {
	g := block.Grid{Cols: 3, Rows: 2, Cells: make([]block.Cell, 6)}
	for i := range g.Cells {
		g.Cells[i] = block.Cell{Glyph: '#', FG: color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff}, BG: color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff}}
	}
	var buf bytes.Buffer
	if err := writeGrid(g, ColourMono, &buf); err != nil {
		t.Fatalf("writeGrid: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[38;") {
		t.Fatalf("mono output leaked foreground SGR (\\x1b[38;)")
	}
	if strings.Contains(out, "\x1b[48;") {
		t.Fatalf("mono output leaked background SGR (\\x1b[48;)")
	}
	if got := strings.Count(out, "\x1b[0m"); got != g.Rows {
		t.Fatalf("reset count = %d, want %d (one per row)", got, g.Rows)
	}
	if got := strings.Count(out, "\x1b[0m\r\n"); got != g.Rows-1 {
		t.Fatalf("reset-CRLF count = %d, want %d (one between each row)", got, g.Rows-1)
	}
}

// TestCellPxDefaultWhenGeometryAbsent asserts that when every geometry report is
// absent the resolved cell pixel size is the 8x16 default (8 wide, 16 tall). The
// source of truth is the pinned Route A raster recorded in docs/STAGE-0.md and
// capture.CellPxW (8) / capture.CellPxH (16). NewState seeds these, and a temp
// file tty has no winsize and no handshake reply, so the chain rests on them.
func TestCellPxDefaultWhenGeometryAbsent(t *testing.T) {
	if capture.CellPxW != 8 || capture.CellPxH != 16 {
		t.Fatalf("capture default = %dx%d, want 8x16 (pinned Route A raster in docs/STAGE-0.md)", capture.CellPxW, capture.CellPxH)
	}
	st := NewState(80, 24, 64, 64)
	if st.W != 8 || st.H != 16 {
		t.Fatalf("default cell px = %dx%d, want 8x16", st.W, st.H)
	}
}

// TestCellPxInBounds exercises the real winsize sanity window. winsizeCellPixels
// reads a live ioctl, so the bound is split into the pure cellPx.inBounds, which
// this table drives directly with the boundary values. That puts the real
// comparison operators under test: flipping a < to <= at an edge fails a case
// here. A derived cell px is accepted only when 4..32 wide and 8..64 tall.
func TestCellPxInBounds(t *testing.T) {
	if winsizeMinCellW != 4 || winsizeMaxCellW != 32 {
		t.Fatalf("width bound = %d..%d, want 4..32", winsizeMinCellW, winsizeMaxCellW)
	}
	if winsizeMinCellH != 8 || winsizeMaxCellH != 64 {
		t.Fatalf("height bound = %d..%d, want 8..64", winsizeMinCellH, winsizeMaxCellH)
	}
	cases := []struct {
		name string
		pxX  int
		pxY  int
		want bool
	}{
		{"in range", 8, 16, true},
		{"min edge", winsizeMinCellW, winsizeMinCellH, true},
		{"max edge", winsizeMaxCellW, winsizeMaxCellH, true},
		{"width below min", winsizeMinCellW - 1, 16, false},
		{"width above max", winsizeMaxCellW + 1, 16, false},
		{"height below min", 8, winsizeMinCellH - 1, false},
		{"height above max", 8, winsizeMaxCellH + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (cellPx{W: c.pxX, H: c.pxY}).inBounds(); got != c.want {
				t.Fatalf("cellPx{%d, %d}.inBounds() = %v, want %v", c.pxX, c.pxY, got, c.want)
			}
		})
	}
}

// TestDeriveCellPixels exercises the pure core of winsizeCellPixels with
// synthetic winsize fields the live ioctl cannot supply. It pins the named
// 32000x0 bogus report (see docs/STAGE-1.md): Ypixel 0 trips the
// zero-guard, so it resolves to ok=false before any division. It also pins a
// healthy report and a derived cell px outside the sanity window.
func TestDeriveCellPixels(t *testing.T) {
	cases := []struct {
		name                     string
		xpixel, ypixel, col, row int
		wantX, wantY             int
		wantOK                   bool
	}{
		{"healthy 80x24 at 8x16", 640, 384, 80, 24, 8, 16, true},
		{"32000x0 bogus winsize", 32000, 0, 80, 24, 0, 0, false},
		{"zero xpixel", 0, 384, 80, 24, 0, 0, false},
		{"zero col", 640, 384, 0, 24, 0, 0, false},
		{"zero row", 640, 384, 80, 0, 0, 0, false},
		{"derived cell px too wide", 32000, 384, 80, 24, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			px, ok := deriveCellPixels(c.xpixel, c.ypixel, c.col, c.row)
			if ok != c.wantOK {
				t.Fatalf("deriveCellPixels(%d,%d,%d,%d) ok = %v, want %v",
					c.xpixel, c.ypixel, c.col, c.row, ok, c.wantOK)
			}
			if ok && (px.W != c.wantX || px.H != c.wantY) {
				t.Fatalf("cell px = %dx%d, want %dx%d", px.W, px.H, c.wantX, c.wantY)
			}
		})
	}
}

// TestParseCellPxOverride exercises the NYALA_CELL_PX override parser. It pins
// the diagnostic knob: a valid "WxH" resolves to that cell size and takes
// precedence, an unset value resolves to ok=false, and any malformed value fails
// loudly with an error rather than falling back.
func TestParseCellPxOverride(t *testing.T) {
	cases := []struct {
		name         string
		value        string
		set          bool
		wantW, wantH int
		wantOK       bool
		wantErr      bool
	}{
		{name: "unset", value: "", set: false, wantOK: false},
		{name: "valid 16x33", value: "16x33", set: true, wantW: 16, wantH: 33, wantOK: true},
		{name: "empty set value", value: "", set: true, wantErr: true},
		{name: "no separator", value: "1633", set: true, wantErr: true},
		{name: "non-integer width", value: "axb", set: true, wantErr: true},
		{name: "trailing junk", value: "16x33px", set: true, wantErr: true},
		{name: "zero width", value: "0x33", set: true, wantErr: true},
		{name: "zero height", value: "16x0", set: true, wantErr: true},
		{name: "negative width", value: "-16x33", set: true, wantErr: true},
		{name: "negative height", value: "16x-33", set: true, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			px, ok, err := parseCellPxOverride(c.value, c.set)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseCellPxOverride(%q, %v) err = nil, want error", c.value, c.set)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCellPxOverride(%q, %v): %v", c.value, c.set, err)
			}
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (px.W != c.wantW || px.H != c.wantH) {
				t.Fatalf("cell px = %dx%d, want %dx%d", px.W, px.H, c.wantW, c.wantH)
			}
		})
	}
}

// TestWinsizeCellPixelsNonTTY pins the ioctl-failure fall-through the 8x16
// default relies on. A non-terminal fd has no winsize, so the ioctl fails and
// winsizeCellPixels returns ok=false before the pure core runs.
func TestWinsizeCellPixelsNonTTY(t *testing.T) {
	tty := newTempTTY(t)
	if _, ok := winsizeCellPixels(tty.Fd(), 80, 24); ok {
		t.Fatalf("winsizeCellPixels on a non-tty returned ok=true, want false")
	}
}

type streamTestEngine struct {
	frames     chan render.Frame
	sendKey    func(string)
	setBackend func(string)
}

func (e streamTestEngine) SetBackend(name string) {
	if e.setBackend != nil {
		e.setBackend(name)
	}
}

func (e streamTestEngine) SetScheme(string) {}

func (e streamTestEngine) EnableCapture(io.Writer) CaptureSession { return nil }

func (e streamTestEngine) SetCaptureSettleHold(bool) {}

func (e streamTestEngine) SetCellPx(int, int) {}

func (e streamTestEngine) SetLayoutWidth(int) {}

func (e streamTestEngine) Resize(int, int) {}

func (e streamTestEngine) Navigate(string) error { return nil }

func (e streamTestEngine) Frames() <-chan render.Frame { return e.frames }

func (e streamTestEngine) SendKey(code string) {
	if e.sendKey != nil {
		e.sendKey(code)
	}
}

func (e streamTestEngine) SendScroll(int) {}

func (e streamTestEngine) Close() error { return nil }

// solidImage builds a 16 x 16 RGBA image filled with one colour.
func solidImage(c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = c.R
		img.Pix[i+1] = c.G
		img.Pix[i+2] = c.B
		img.Pix[i+3] = c.A
	}
	return img
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
		Img:  benchStreamImage(120*capture.CellPxW, 40*capture.CellPxH),
		Text: benchStreamRuns(40),
	}
}

// BenchmarkRenderStreamFrameUnchanged is the live-path allocation baseline that
// gates P2-8. It drives State.RenderStreamFrame on the block backend through the
// cell differ at the fixed 120x40 grid, redrawing the same text-bearing frame.
// The differ is primed with one draw outside the timed loop, so each measured
// iteration is the diffed steady-state path: an unchanged frame that emits no
// terminal bytes but still composes the grid. b.ReportAllocs reports allocs/op
// and B/op. P2-8 must lower the unchanged-frame allocs/op against this baseline.
func BenchmarkRenderStreamFrameUnchanged(b *testing.B) {
	s := NewState(120, 40, 120*capture.CellPxW, 40*capture.CellPxH)
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
// a first-frame full repaint against a blank screen. It is not the P2-8 gate; it
// frames the cost of the worst case beside the steady-state baseline.
func BenchmarkRenderStreamFrameChanged(b *testing.B) {
	s := NewState(120, 40, 120*capture.CellPxW, 40*capture.CellPxH)
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
	s := NewState(8, 4, 16, 16)
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
