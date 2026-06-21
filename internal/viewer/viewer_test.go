package viewer

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// TestCycleBackendOrder asserts Tab steps block -> sixel -> kitty -> block.
func TestCycleBackendOrder(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	if s.Backend != BackendBlock {
		t.Fatalf("initial backend = %d, want BackendBlock", s.Backend)
	}
	want := []Backend{BackendSixel, BackendKitty, BackendBlock}
	for i, w := range want {
		if got := s.CycleBackend(); got != w {
			t.Fatalf("cycle %d: got %d, want %d", i, got, w)
		}
	}
}

// TestCycleBackendSkipsDisabled asserts CycleBackend steps only through the
// enabled set: with sixel disabled it goes block -> kitty -> block.
func TestCycleBackendSkipsDisabled(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.SetEnabled([]Backend{BackendBlock, BackendKitty})
	if s.Backend != BackendBlock {
		t.Fatalf("after SetEnabled, backend = %d, want BackendBlock", s.Backend)
	}
	want := []Backend{BackendKitty, BackendBlock, BackendKitty}
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
	for i := 0; i < 3; i++ {
		if got := s.CycleBackend(); got != BackendBlock {
			t.Fatalf("cycle %d: got %d, want BackendBlock", i, got)
		}
	}
}

// TestCycleBackendAllThree asserts CycleBackend steps the full set block -> sixel
// -> kitty -> block, the same as the default cycle.
func TestCycleBackendAllThree(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.SetEnabled([]Backend{BackendBlock, BackendSixel, BackendKitty})
	want := []Backend{BackendSixel, BackendKitty, BackendBlock}
	for i, w := range want {
		if got := s.CycleBackend(); got != w {
			t.Fatalf("cycle %d: got %d, want %d", i, got, w)
		}
	}
}

// TestTogglesApplyOnBlockWithSubset asserts the scheme and colour toggles still
// act on block when the cycle omits sixel.
func TestTogglesApplyOnBlockWithSubset(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.SetEnabled([]Backend{BackendBlock, BackendKitty})
	if got := s.ToggleScheme(); got != block.SchemeHalfBlock {
		t.Fatalf("scheme toggle = %d, want SchemeHalfBlock", got)
	}
	if got := s.ToggleColour(); got != ColourXTerm256 {
		t.Fatalf("colour toggle = %d, want ColourXTerm256", got)
	}
	s.CycleBackend() // kitty
	s.CycleBackend() // block
	if s.Scheme != block.SchemeHalfBlock || s.Colour != ColourXTerm256 {
		t.Fatalf("toggles lost after cycle: scheme=%d colour=%d", s.Scheme, s.Colour)
	}
}

// TestSchemeToggleFlips asserts the scheme toggle flips quadrant <-> half-block.
func TestSchemeToggleFlips(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	if s.Scheme != block.SchemeQuadrant {
		t.Fatalf("initial scheme = %d, want SchemeQuadrant", s.Scheme)
	}
	if got := s.ToggleScheme(); got != block.SchemeHalfBlock {
		t.Fatalf("first toggle = %d, want SchemeHalfBlock", got)
	}
	if got := s.ToggleScheme(); got != block.SchemeQuadrant {
		t.Fatalf("second toggle = %d, want SchemeQuadrant", got)
	}
}

// TestColourToggleFlips asserts the colour toggle flips truecolour <-> xterm-256.
func TestColourToggleFlips(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	if s.Colour != ColourTrue {
		t.Fatalf("initial colour = %d, want ColourTrue", s.Colour)
	}
	if got := s.ToggleColour(); got != ColourXTerm256 {
		t.Fatalf("first toggle = %d, want ColourXTerm256", got)
	}
	if got := s.ToggleColour(); got != ColourTrue {
		t.Fatalf("second toggle = %d, want ColourTrue", got)
	}
}

// TestTogglesSurviveBackendCycle asserts cycling away from block and back keeps
// the chosen scheme and colour: the toggles are independent of the backend.
func TestTogglesSurviveBackendCycle(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.ToggleScheme()
	s.ToggleColour()
	s.CycleBackend() // sixel
	s.CycleBackend() // kitty
	s.CycleBackend() // block
	if s.Scheme != block.SchemeHalfBlock {
		t.Fatalf("scheme after cycle = %d, want SchemeHalfBlock", s.Scheme)
	}
	if s.Colour != ColourXTerm256 {
		t.Fatalf("colour after cycle = %d, want ColourXTerm256", s.Colour)
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

// TestHandleKey asserts the hotkey table: Tab cycles, 's' and 'c' toggle and
// redraw, 'q' and Ctrl-C quit, and an unknown key is a no-op.
func TestHandleKey(t *testing.T) {
	s := NewState(120, 40, 64, 64)

	if act := s.HandleKey(keyTab); !act.Redraw || act.Quit {
		t.Fatalf("Tab action = %+v, want redraw", act)
	}
	if s.Backend != BackendSixel {
		t.Fatalf("Tab did not cycle backend: %d", s.Backend)
	}

	if act := s.HandleKey('s'); !act.Redraw || act.Quit {
		t.Fatalf("'s' action = %+v, want redraw", act)
	}
	if s.Scheme != block.SchemeHalfBlock {
		t.Fatalf("'s' did not toggle scheme: %d", s.Scheme)
	}

	if act := s.HandleKey('c'); !act.Redraw || act.Quit {
		t.Fatalf("'c' action = %+v, want redraw", act)
	}
	if s.Colour != ColourXTerm256 {
		t.Fatalf("'c' did not toggle colour: %d", s.Colour)
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

// TestRenderFrameBlockColourMode asserts the block path honours the colour
// toggle: truecolour emits 24-bit SGR (38;2), xterm-256 emits indexed SGR (38;5).
func TestRenderFrameBlockColourMode(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})

	var trueBuf bytes.Buffer
	if err := s.RenderFrame(src, &trueBuf); err != nil {
		t.Fatalf("render truecolour: %v", err)
	}
	if !strings.Contains(trueBuf.String(), "\x1b[38;2;") {
		t.Fatalf("truecolour output missing 24-bit SGR")
	}

	s.ToggleColour()
	var idxBuf bytes.Buffer
	if err := s.RenderFrame(src, &idxBuf); err != nil {
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
// payload through RenderFrame for a fitted image.
func TestRenderFrameGraphics(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})

	for _, b := range []Backend{BackendSixel, BackendKitty} {
		s.Backend = b
		var buf bytes.Buffer
		if err := s.RenderFrame(src, &buf); err != nil {
			t.Fatalf("render backend %d: %v", b, err)
		}
		if buf.Len() == 0 {
			t.Fatalf("render backend %d: empty output", b)
		}
	}
}

// TestRenderFrameNoFit asserts a degenerate grid (zero cells) writes nothing and
// does not error, so the viewer tolerates a collapsed terminal.
func TestRenderFrameNoFit(t *testing.T) {
	s := NewState(0, 0, 16, 16)
	var buf bytes.Buffer
	if err := s.RenderFrame(solidImage(color.RGBA{A: 0xff}), &buf); err != nil {
		t.Fatalf("render zero grid: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("zero grid wrote %d bytes, want 0", buf.Len())
	}
}

// TestWriteGridRowsEndCRLF asserts writeGrid terminates every row with CRLF, not
// a bare LF. With auto-wrap off in raw mode, the carriage return is needed to
// return the cursor to column 0 each row; a bare LF leaves it interlaced.
func TestWriteGridRowsEndCRLF(t *testing.T) {
	g := block.Grid{Cols: 2, Rows: 2, Cells: make([]block.Cell, 4)}
	for i := range g.Cells {
		g.Cells[i] = block.Cell{Glyph: ' ', FG: color.RGBA{A: 0xff}, BG: color.RGBA{A: 0xff}}
	}
	var buf bytes.Buffer
	if err := writeGrid(g, ColourTrue, &buf); err != nil {
		t.Fatalf("writeGrid: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, "\n"); got != g.Rows {
		t.Fatalf("line feed count = %d, want %d (one per row)", got, g.Rows)
	}
	for i := 0; i < len(out); i++ {
		if out[i] == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("line feed at byte %d not preceded by carriage return", i)
		}
	}
}

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
