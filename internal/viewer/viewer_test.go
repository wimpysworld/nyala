package viewer

import (
	"bytes"
	"image"
	"image/color"
	"os"
	"strings"
	"testing"

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
	for i := 0; i < 3; i++ {
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

// TestTogglesApplyOnBlockWithSubset asserts the scheme and colour toggles still
// act on block when the cycle omits sixel.
func TestTogglesApplyOnBlockWithSubset(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.SetEnabled([]Backend{BackendKitty, BackendBlock})
	if got := s.ToggleScheme(); got != block.SchemeHalfBlock {
		t.Fatalf("scheme toggle = %d, want SchemeHalfBlock", got)
	}
	if got := s.ToggleColour(); got != ColourXTerm256 {
		t.Fatalf("colour toggle = %d, want ColourXTerm256", got)
	}
	s.CycleBackend() // block
	s.CycleBackend() // kitty
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

// TestColourToggleCycle asserts the colour toggle cycles three modes:
// ColourTrue -> ColourXTerm256 -> ColourMono -> ColourTrue.
func TestColourToggleCycle(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	if s.Colour != ColourTrue {
		t.Fatalf("initial colour = %d, want ColourTrue", s.Colour)
	}
	want := []ColourMode{ColourXTerm256, ColourMono, ColourTrue}
	for i, w := range want {
		if got := s.ToggleColour(); got != w {
			t.Fatalf("toggle %d = %d, want %d", i, got, w)
		}
	}
}

// TestTogglesSurviveBackendCycle asserts cycling away from block and back keeps
// the chosen scheme and colour: the toggles are independent of the backend.
func TestTogglesSurviveBackendCycle(t *testing.T) {
	s := NewState(120, 40, 64, 64)
	s.ToggleScheme()
	s.ToggleColour()
	s.CycleBackend() // kitty
	s.CycleBackend() // sixel
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

// TestRenderFrameGraphicsCentres asserts the graphics backends move the cursor to
// the centred cell offset before the bitmap, matching the block path: a
// letterboxed fit emits the CUP escape \x1b[<row>;<col>H at the 1-based offset, and
// a frame that fills the grid emits no cursor move. The kitty delete-all escape
// (\x1b_Ga=d) must still precede the move on the kitty path.
func TestRenderFrameGraphicsCentres(t *testing.T) {
	cases := []struct {
		name             string
		cols, rows       int
		imgW, imgH       int
		wantMove         string // empty means assert no CUP escape
		wantOffsetColRow string
	}{
		{"column letterbox", 8, 4, 16, 64, "\x1b[1;4H", "OffsetCol 3"},
		{"row letterbox", 4, 4, 16, 16, "\x1b[2;1H", "OffsetRow 1"},
		{"fills grid", 8, 4, 16, 16, "", "both offsets 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
			for _, b := range []Backend{BackendSixel, BackendKitty} {
				s := NewState(c.cols, c.rows, c.imgW, c.imgH)
				s.Backend = b
				var buf bytes.Buffer
				if err := s.RenderFrame(src, &buf); err != nil {
					t.Fatalf("render backend %d: %v", b, err)
				}
				out := buf.String()
				if c.wantMove == "" {
					if strings.Contains(out, "\x1b[") && cupPresent(out) {
						t.Fatalf("backend %d (%s): emitted a CUP move, want none", b, c.wantOffsetColRow)
					}
					continue
				}
				if !strings.Contains(out, c.wantMove) {
					t.Fatalf("backend %d (%s): output missing CUP escape %q", b, c.wantOffsetColRow, c.wantMove)
				}
				if b == BackendKitty {
					di := strings.Index(out, kittyDeleteAll)
					mi := strings.Index(out, c.wantMove)
					if di < 0 || mi < 0 || di > mi {
						t.Fatalf("kitty: delete-all must precede the CUP move (delete=%d move=%d)", di, mi)
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
	if err := s.RenderFrame(solidImage(color.RGBA{A: 0xff}), &buf); err != nil {
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
	for i := 0; i < len(out); i++ {
		if out[i] == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("line feed at byte %d not preceded by carriage return", i)
		}
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatalf("output ends with a newline; a full-height grid would scroll the top row off")
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
	configureBackends(&st, tty, 80, 24, "auto")

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
	configureBackends(&st, tty, 80, 24, "auto")

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
		t.Fatalf("capture default = %dx%d, want 8x16 (docs/STAGE-0.md:84)", capture.CellPxW, capture.CellPxH)
	}
	st := NewState(80, 24, 64, 64)
	if st.CellPxX != 8 || st.CellPxY != 16 {
		t.Fatalf("default cell px = %dx%d, want 8x16", st.CellPxX, st.CellPxY)
	}
}

// TestCellPxInBounds exercises the real winsize sanity window. winsizeCellPixels
// reads a live ioctl, so the bound is split into the pure cellPxInBounds, which
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
			if got := cellPxInBounds(c.pxX, c.pxY); got != c.want {
				t.Fatalf("cellPxInBounds(%d, %d) = %v, want %v", c.pxX, c.pxY, got, c.want)
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
			x, y, ok := deriveCellPixels(c.xpixel, c.ypixel, c.col, c.row)
			if ok != c.wantOK {
				t.Fatalf("deriveCellPixels(%d,%d,%d,%d) ok = %v, want %v",
					c.xpixel, c.ypixel, c.col, c.row, ok, c.wantOK)
			}
			if ok && (x != c.wantX || y != c.wantY) {
				t.Fatalf("cell px = %dx%d, want %dx%d", x, y, c.wantX, c.wantY)
			}
		})
	}
}

// TestWinsizeCellPixelsNonTTY pins the ioctl-failure fall-through the 8x16
// default relies on. A non-terminal fd has no winsize, so the ioctl fails and
// winsizeCellPixels returns ok=false before the pure core runs.
func TestWinsizeCellPixelsNonTTY(t *testing.T) {
	tty := newTempTTY(t)
	if _, _, ok := winsizeCellPixels(tty.Fd(), 80, 24); ok {
		t.Fatalf("winsizeCellPixels on a non-tty returned ok=true, want false")
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
