// Package viewer is the interactive raw-terminal frame viewer for Nyala.
//
// The viewer draws each frame an Engine pushes to the terminal. It cycles the
// render backend (block, sixel, kitty). The block glyph scheme (quadrant,
// half-block) is set
// once from the --blocks flag and the colour depth from the --color flag, so
// neither is a live toggle.
//
// State holds the viewer's render and backend state. Stream wires it to a raw
// TTY, handles hotkeys, and draws an Engine's pushed frames.
package viewer

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"strconv"
	"strings"

	cterm "github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/pngdecode"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
	"github.com/wimpysworld/nyala/internal/rowband"
)

// Backend identifies one render backend in the cycle order. The Tab hotkey
// advances through the enabled backends and wraps; configureBackends orders them
// highest-fidelity first (kitty, then sixel) with block last as the floor.
type Backend int

const (
	// BackendBlock is the owned-grid block-cell renderer; it honours the glyph
	// scheme and colour mode. It is the floor backend, placed last in the cycle.
	BackendBlock Backend = iota
	// BackendSixel is the Sixel graphics backend. It ignores the scheme and
	// colour mode, which apply only to the block backend.
	BackendSixel
	// BackendKitty is the Kitty graphics backend. It ignores the scheme and
	// colour mode, which apply only to the block backend.
	BackendKitty
)

// ColourMode selects the block backend's colour depth, resolved once from the
// --color flag. The graphics backends ignore it.
type ColourMode int

const (
	// ColourTrue emits 24-bit truecolour SGR for the block backend.
	ColourTrue ColourMode = iota
	// ColourXTerm256 maps each block cell colour onto the xterm-256 palette.
	ColourXTerm256
	// ColourMono emits each cell's glyph with no fg/bg SGR, rendering a 1-bit
	// silhouette on the terminal default fg/bg. detect_colour.go cites this value.
	ColourMono
)

// State is the viewer's render and backend state. Cols and Rows are the terminal
// size in cells. Enabled lists the backends the Tab cycle steps through, in cycle
// order, highest-fidelity first with block last; the embedded cellPx is the real
// terminal cell pixel size used to scale the graphics backends.
type State struct {
	Backend Backend      // active backend in the cycle
	Scheme  block.Scheme // block glyph scheme; applies only to the block backend
	Colour  ColourMode   // block colour depth; applies only to the block backend
	Cols    int          // terminal width in cells
	Rows    int          // terminal height in cells
	Enabled []Backend    // backends in the Tab cycle, highest-fidelity first
	cellPx               // terminal cell pixel size for graphics scaling

	// diff is the Block cell differ. The interactive stream session and Block
	// diagnostics set it before composition so renderBlock emits only the cells
	// that changed since the previous frame, with CUP and SGR coalescing.
	diff *block.GridDiff

	// scratch holds the reusable per-frame buffers for the Block path: the
	// downscale destination, the compose grid, and the buffered tty writer. It is
	// used by the single-goroutine interactive draw path and diagnostics, so the
	// buffers never alias across goroutines.
	scratch frameScratch

	// kittyID is the Kitty image id the last drawn kitty frame placed, or 0 before
	// the first kitty frame. The kitty path double-buffers by alternating id 1 and
	// 2: each frame places the new image under the next id, then deletes the
	// previous id, so the swap never blanks the screen. resetKitty clears it back
	// to 0 when Tab cycles the backend away from kitty. It stays 0 on the block and
	// sixel paths.
	kittyID int

	// kittyEnc is the reused Kitty live encoder. It holds the APC frame assembly
	// buffer across direct PNG frames. It is lazily built on the
	// first kitty frame and driven from the single draw goroutine, so it never
	// aliases. It stays nil on the block and sixel paths.
	kittyEnc *kitty.LiveEncoder

	// sixelEnc is the reused Sixel renderer. It retains encoder scratch across
	// moving frames and switches its output through an internal writer proxy.
	// The viewer draw path is serial, so one renderer owns the whole stream.
	sixelEnc *sixel.Renderer
}

// frameScratch is the Block path's reusable per-frame buffers. Each draw
// fully overwrites the downscale pixels and recomposes the grid, so reuse carries
// no stale content. dstW and dstH record the downscale size so the destination
// reallocates only when the grid or scheme changes the pixel extent. bw buffers
// the tty writer so each frame Resets and Flushes one held writer rather than
// allocating a new bufio.Writer. The zero value is unset; renderBlock lazily fills
// it on the first diff draw.
type frameScratch struct {
	dst        *image.RGBA   // reused downscale destination
	dstW, dstH int           // current downscale pixel extent
	out        block.Grid    // reused compose output grid
	bw         *bufio.Writer // reused buffered tty writer
	bwSize     int           // current tty buffer capacity in bytes

	// pool fans both the block downscale and the compose binarise across one
	// persistent worker set. It is built once, lazily on the first Block compose, and
	// reused every frame, so the hot path spawns no goroutine. The live diff path is
	// single-goroutine, so the one pool is driven from one caller and never overlaps
	// calls.
	pool *rowband.Pool

	// downscale and compose are the pool-driving band drivers, each holding one band
	// closure built once so a reused frame allocates nothing. downscale splits the
	// source area-average, compose splits the binarise; both fan over the shared pool
	// and both are byte-identical to their serial twins.
	downscale *metrics.ParallelDownscale
	compose   *block.ParallelCompose
}

// downscaleDst returns a width x height *image.RGBA, reusing the held destination
// when the extent is unchanged and reallocating only on a size change. The block
// downscale fully overwrites every pixel, so a reused destination carries no
// stale content.
func (f *frameScratch) downscaleDst(width, height int) *image.RGBA {
	if f.dst == nil || f.dstW != width || f.dstH != height {
		f.dst = image.NewRGBA(image.Rect(0, 0, width, height))
		f.dstW, f.dstH = width, height
	}
	return f.dst
}

// writer returns the held bufio.Writer reset onto w, sized to hold a whole frame
// in one flush. size is the per-frame byte budget: a full-refresh or scroll frame
// runs to ~150KB, so the default 4KB buffer would auto-flush many times mid-frame
// and each mid-frame flush presents a torn partial frame. Sizing the buffer to the
// frame keeps the differ's many small writes as one flush and one syscall. The
// buffer reallocates only when the grid grows the budget; a reset reuses it. The
// live diff path is single-goroutine, so the held writer never aliases.
func (f *frameScratch) writer(w io.Writer, size int) *bufio.Writer {
	if f.bw == nil || f.bwSize < size {
		f.bw = bufio.NewWriterSize(w, size)
		f.bwSize = size
	} else {
		f.bw.Reset(w)
	}
	return f.bw
}

// frameBufferSize is the per-frame tty buffer budget for the live diff path:
// enough bytes to hold a full-refresh frame in one flush so the buffered writer
// does not auto-flush mid-frame. It budgets 48 bytes per cell (a worst-case cell
// is a CUP move plus an SGR fg+bg plus a multi-byte glyph) and a fixed slack for
// the frame prefix and the synchronised-output markers.
func frameBufferSize(cols, rows int) int {
	const perCell = 48
	const slack = 256
	return cols*rows*perCell + slack
}

// SetDiff sets the Block cell differ. The interactive stream session and Block
// diagnostics pass a pointer to a block.GridDiff before the first composition;
// renderBlock then emits only the cells that changed since the previous frame.
// It is a no-op for the graphics backends.
func (s *State) SetDiff(d *block.GridDiff) {
	s.diff = d
}

// NewState builds the initial state: block as the safe pre-detection backend, the
// quadrant scheme, truecolour, and sized to a cols x rows terminal. The cycle
// holds every backend highest-fidelity first and the cell
// pixel size is the Route A default; configureBackends narrows the cycle, nominates
// the best available backend, and sets the real cell size after the handshake.
func NewState(cols, rows int) State {
	return State{
		Backend: BackendBlock,
		Scheme:  block.SchemeQuadrant,
		Colour:  ColourTrue,
		Cols:    cols,
		Rows:    rows,
		Enabled: []Backend{BackendKitty, BackendSixel, BackendBlock},
		cellPx:  cellPx{W: cellraster.CellPxW, H: cellraster.CellPxH},
	}
}

// SetEnabled records the backends the Tab cycle steps through, in cycle order,
// and resets the active backend to the first enabled backend. Callers order the
// slice highest-fidelity first and place block last as the floor, so enabled[0]
// is the best available backend and the cycle never rests on a disabled one.
func (s *State) SetEnabled(enabled []Backend) {
	if len(enabled) == 0 {
		enabled = []Backend{BackendBlock}
	}
	s.Enabled = enabled
	s.Backend = enabled[0]
}

// CycleBackend advances to the next enabled backend in cycle order and wraps,
// then returns the new backend. With block only it stays on block. The scheme and
// colour state are untouched, so cycling away from block and back preserves the
// chosen scheme and colour.
func (s *State) CycleBackend() Backend {
	if len(s.Enabled) == 0 {
		return s.Backend
	}
	idx := 0
	for i, b := range s.Enabled {
		if b == s.Backend {
			idx = i
			break
		}
	}
	s.Backend = s.Enabled[(idx+1)%len(s.Enabled)]
	return s.Backend
}

// Resize records a new terminal size in cells. The next frame reflects the new
// size.
func (s *State) Resize(cols, rows int) {
	s.Cols = cols
	s.Rows = rows
}

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

// RenderStreamFrame draws one live screencast frame, folding its crisp DOM-text
// overlay on the block path. The engine sizes the capture to the full grid and
// bins each text run in whole-cell coordinates, so the overlay aligns when the
// block path composes at the same full grid. The graphics backends carry no
// overlay and the engine already sized their bitmap to the terminal target, so
// they encode frame.Img as final with no second downscale: the engine is the one
// graphics sizing owner. A nil or degenerate (zero-extent) image is a no-op; this
// one guard covers both the block and the graphics paths, so neither downscales to
// a zero extent.
func (s *State) RenderStreamFrame(frame render.Frame, w io.Writer) error {
	if !frame.HasImage() {
		return nil
	}
	if s.Backend == BackendKitty {
		if frame.EncodedPNG == "" {
			return nil
		}
		return s.encodeKittyPNG(frame.EncodedPNG, w)
	}
	img := frame.Img
	if img == nil {
		var err error
		img, err = decodeFramePNG(frame.EncodedPNG)
		if err != nil {
			return err
		}
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil
	}
	if s.Backend == BackendBlock {
		return s.renderBlock(img, frame.Text, w)
	}
	if s.Cols <= 0 || s.Rows <= 0 {
		return nil
	}
	if s.sixelEnc == nil {
		s.sixelEnc = &sixel.Renderer{}
	}
	return s.sixelEnc.Render(render.Frame{Img: img}, w)
}

// decodeFramePNG materialises a retained CDP PNG only when a non-Kitty consumer
// receives a frame queued before a backend switch. Normal settled Kitty frames
// never call it.
func decodeFramePNG(data string) (image.Image, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("viewer: decode retained PNG base64: %w", err)
	}
	img, err := pngdecode.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("viewer: decode retained PNG: %w", err)
	}
	return img, nil
}

// renderBlock composes the source onto the full Cols x Rows grid and writes it
// as SGR cells, honouring the colour toggle: truecolour SGR or xterm-256 SGR.
// text is the crisp DOM-text overlay folded onto the composed grid; a nil text
// draws the bitmap alone.
//
// The block path composes at the full grid with no aspect-fit letterbox: the
// engine sizes the block frame to exactly the terminal grid geometry (cols*cellW
// by rows*cellH device pixels), so the bitmap maps one-to-one onto the Cols x Rows
// grid. The overlay bins each run in whole-cell coordinates against the same grid,
// so a full-grid compose keeps the bitmap and the overlay on one horizontal scale.
// An aspect-fit would assume a 1:2 cell aspect and letterbox the bitmap onto fewer
// columns whenever the real terminal cell aspect differs (a 13x27 cell letterboxes
// 195 columns onto 187), drifting the overlay right of the bitmap by a scale that
// grows with the column. The graphics backends draw the same engine-sized full-grid
// bitmap from the top-left, so they fill the whole grid without a placement
// stage.
func (s *State) renderBlock(src image.Image, text []render.GlyphRun, w io.Writer) error {
	grid := s.ComposeBlockFrame(render.Frame{Img: src, Text: text})
	if grid == nil {
		return nil
	}
	bw := s.scratch.writer(w, frameBufferSize(s.Cols, s.Rows))
	var err error
	if s.Colour == ColourTrue {
		err = s.diff.EmitTrueColour(bw, *grid)
	} else {
		err = s.diff.Emit(bw, *grid, fgSGRFunc(s.Colour), bgSGRFunc(s.Colour))
	}
	if err != nil {
		return err
	}
	return bw.Flush()
}

// ComposeBlockFrame returns the exact Block grid consumed by the terminal
// writer. The Block path reuses State scratch storage, so callers that retain the
// result across another compose must copy it.
func (s *State) ComposeBlockFrame(frame render.Frame) *block.Grid {
	if frame.Img == nil {
		return nil
	}
	br := block.Renderer{Cols: s.Cols, Rows: s.Rows, Scheme: s.Scheme}
	pxW, pxH := schemePixels(s.Scheme, s.Cols, s.Rows)
	if pxW <= 0 || pxH <= 0 {
		return nil
	}
	if s.scratch.pool == nil {
		s.scratch.pool = rowband.New()
		s.scratch.downscale = metrics.NewParallelDownscale()
		s.scratch.compose = block.NewParallelCompose()
	}
	_, nativeYCbCr := frame.Img.(*image.YCbCr)
	srcW, srcH := frame.Img.Bounds().Dx(), frame.Img.Bounds().Dy()
	if nativeYCbCr && pxW <= srcW && pxH <= srcH && (pxW < srcW || pxH < srcH) {
		return s.scratch.compose.ComposeYCbCrInto(&br, &s.scratch.out, frame, s.scratch.pool)
	}
	frame.Img = s.scratch.downscale.DownscaleInto(s.scratch.downscaleDst(pxW, pxH), frame.Img, s.scratch.pool)
	return s.scratch.compose.ComposeInto(&br, &s.scratch.out, frame, s.scratch.pool)
}

// encodeKittyPNG transmits a retained CDP PNG unchanged, then deletes the prior
// double-buffer image.
func (s *State) encodeKittyPNG(data string, w io.Writer) error {
	next := nextKittyID(s.kittyID)
	if s.kittyEnc == nil {
		s.kittyEnc = &kitty.LiveEncoder{}
	}
	if err := s.kittyEnc.EncodePNGID(data, next, w); err != nil {
		return err
	}
	if s.kittyID != 0 {
		if _, err := io.WriteString(w, kitty.DeleteImage(s.kittyID)); err != nil {
			return err
		}
	}
	s.kittyID = next
	return nil
}

// nextKittyID returns the next double-buffer image id, alternating between 1 and
// 2. A zero prev (no image placed yet) yields 1, so the first frame uses id 1.
func nextKittyID(prev int) int {
	if prev == 1 {
		return 2
	}
	return 1
}

// resetKitty deletes both double-buffer image ids and frees their data, then
// clears the tracked id so a later return to the kitty backend rebuilds from a
// clean slate with id 1. It is a no-op when no kitty image has been placed. The
// stream session calls it when Tab cycles the backend away from kitty.
func (s *State) resetKitty(w io.Writer) error {
	if s.kittyID == 0 {
		return nil
	}
	if _, err := io.WriteString(w, kitty.DeleteImage(1)+kitty.DeleteImage(2)); err != nil {
		return err
	}
	s.kittyID = 0
	return nil
}

// schemePixels returns the source-pixel extent a cols x rows block grid samples
// in the given scheme: 2x4 per cell for quadrant, 1x2 per cell for half-block,
// 2x3 per cell for sextant. The compose step needs the source at this extent so
// each cell maps to its own block of pixels. It maps the block.Scheme to a
// render.BlockScheme via renderScheme, then calls render.BlockCaptureSize, the
// shared helper the engine also calls.
func schemePixels(scheme block.Scheme, cols, rows int) (pxW, pxH int) {
	return render.BlockCaptureSize(renderScheme(scheme), cols, rows)
}

// appendSGR256 appends an xterm-256 SGR escape, prefix + index + "m", to dst with
// strconv.AppendInt and returns the extended slice. It is the allocation-free
// build the differ folds into its reused per-frame buffer.
func appendSGR256(dst []byte, prefix string, idx uint8) []byte {
	dst = append(dst, prefix...)
	dst = strconv.AppendInt(dst, int64(idx), 10)
	return append(dst, 'm')
}

// fgSGRFunc binds a colour mode into a block.SGRFunc for the cell differ, so the
// differ stays colour-mode neutral while reusing the viewer's fg SGR rules. Each
// mode appends into the differ's reused buffer, so no per-cell string allocates.
func fgSGRFunc(mode ColourMode) block.SGRFunc {
	switch mode {
	case ColourMono:
		return func(dst []byte, _ color.RGBA) []byte { return dst }
	case ColourXTerm256:
		return func(dst []byte, c color.RGBA) []byte {
			return appendSGR256(dst, fg256Prefix, block.XTerm256(c))
		}
	default:
		return block.AppendTrueColorFG
	}
}

// bgSGRFunc binds a colour mode into a block.SGRFunc for the cell differ, so the
// differ stays colour-mode neutral while reusing the viewer's bg SGR rules. Each
// mode appends into the differ's reused buffer, so no per-cell string allocates.
func bgSGRFunc(mode ColourMode) block.SGRFunc {
	switch mode {
	case ColourMono:
		return func(dst []byte, _ color.RGBA) []byte { return dst }
	case ColourXTerm256:
		return func(dst []byte, c color.RGBA) []byte {
			return appendSGR256(dst, bg256Prefix, block.XTerm256(c))
		}
	default:
		return block.AppendTrueColorBG
	}
}

// fg256Prefix and bg256Prefix are the xterm-256 foreground and background SGR
// prefixes (ESC [ 38;5; and ESC [ 48;5;). appendSGR256 appends the palette index
// and the trailing "m".
const (
	fg256Prefix = "\x1b[38;5;"
	bg256Prefix = "\x1b[48;5;"
)

// framePrefix returns the cursor escape the steady draw path emits
// before a frame's render bytes, and seeds the differ for the first draw. The
// first draw returns the full clearScreen and resets diff so it full-repaints
// against the blank screen; every later draw returns cursorHome only, so the
// render overwrites each cell in place with no per-frame full clear. diff may be
// nil (the graphics backends carry none), in which case no seed runs.
func framePrefix(firstDrawn bool, diff *block.GridDiff) string {
	if firstDrawn {
		return cursorHome
	}
	if diff != nil {
		// The first draw clears the loading cue or prior terminal state, so the
		// differ must full-repaint against the blank screen, not a
		// retained grid.
		diff.Reset()
	}
	return clearScreen
}

// clearScreen homes the cursor and clears the terminal so the next frame draws
// from the top-left without stale cells. The draw path uses it for the first
// draw and on resize; the steady path homes the cursor with cursorHome and
// overwrites every cell in place, so the full erase is not needed per frame.
const clearScreen = "\x1b[H\x1b[2J"

// cursorHome homes the cursor without erasing, so the steady draw path overwrites
// every cell in place. Auto-wrap is off and each cell is rewritten, so nothing
// stale remains after the overwrite.
const cursorHome = "\x1b[H"

// beginSyncUpdate and endSyncUpdate bracket a frame in a DEC 2026 synchronised
// update (ESC [ ? 2026 h and ESC [ ? 2026 l), so the terminal presents the whole
// frame atomically and never scans out a partial frame while the render bytes
// land. A terminal without DEC 2026 ignores the private mode, so the live draw
// path emits them unconditionally.
const (
	beginSyncUpdate = "\x1b[?2026h"
	endSyncUpdate   = "\x1b[?2026l"
)

// enterFullScreen disables auto-wrap (DECAWM, "\x1b[?7l") and hides the cursor
// ("\x1b[?25l"). With auto-wrap off, a full-width row does not enter the
// pending-wrap state, so the explicit carriage return and line feed advance one
// physical line per grid row.
const enterFullScreen = "\x1b[?7l\x1b[?25l"

// exitFullScreen re-enables auto-wrap (DECAWM, "\x1b[?7h") and shows the cursor
// ("\x1b[?25h"). Run writes it during teardown to restore the terminal to its
// default state before the termios restore.
const exitFullScreen = "\x1b[?7h\x1b[?25h"

// configureBackends decides which backends the cycle offers and resolves the
// real cell pixel size, then writes both into st. Behind a multiplexer (TMUX,
// ZELLIJ, herdr, or STY set) it offers block only because Nyala does not use
// graphics passthrough. Otherwise it runs one terminal handshake and offers a
// graphics backend only when the terminal confirms it. The colour mode resolves first, on
// every path, because it is multiplexer-agnostic, so it must not strand at the
// NewState default behind an early return. A NYALA_CELL_PX override (a diagnostic
// and harness knob) takes precedence over the ioctl and handshake cell; a
// malformed override returns an error so the run fails loudly.
func configureBackends(st *State, tty *os.File, cols, rows int, colourFlag string) error {
	detected := detectColour(os.Environ(), cterm.IsTerminal(tty.Fd()))
	st.Colour = resolveColour(colourFlag, os.Environ(), detected)

	overrideValue, overrideSet := os.LookupEnv(cellPxEnv)
	overridePx, overrideOK, err := parseCellPxOverride(overrideValue, overrideSet)
	if err != nil {
		return err
	}

	winPx, winOK := winsizeCellPixels(tty.Fd(), cols, rows)
	switch {
	case overrideOK:
		st.cellPx = overridePx
	case winOK:
		st.cellPx = winPx
	}

	if multiplexed() {
		st.SetEnabled([]Backend{BackendBlock})
		return nil
	}

	res := probeTerminal(tty, cols, rows)
	if !overrideOK && !winOK {
		st.cellPx = res.cellPx
	}

	enabled := []Backend{}
	if res.Kitty {
		enabled = append(enabled, BackendKitty)
	}
	if res.Sixel {
		enabled = append(enabled, BackendSixel)
	}
	enabled = append(enabled, BackendBlock)
	st.SetEnabled(enabled)
	return nil
}

// multiplexed reports whether the viewer runs under a focus multiplexer: tmux
// (TMUX), zellij (ZELLIJ present, any value), herdr (HERDR_ENV=1 with
// HERDR_PANE_ID present), or GNU screen (STY). A bare ZELLIJ=0 is a presence
// signal, not a truth value; herdr needs both keys so a stray var does not gate
// graphics off. Nyala offers block only under a multiplexer because it does not
// use graphics passthrough there.
func multiplexed() bool {
	if os.Getenv("TMUX") != "" || os.Getenv("STY") != "" {
		return true
	}
	if _, ok := os.LookupEnv("ZELLIJ"); ok {
		return true
	}
	if os.Getenv("HERDR_ENV") == "1" {
		if _, ok := os.LookupEnv("HERDR_PANE_ID"); ok {
			return true
		}
	}
	return false
}

// cellPx is a terminal cell's pixel size: the width and height of one character
// cell. The two values always travel together (resolve, validate, scale), so the
// pair carries one type rather than a loose (pxX, pxY) int pair through the
// resolution chain.
type cellPx struct {
	W int // cell width in pixels
	H int // cell height in pixels
}

// winsizeCellPixels resolves the cell pixel size from the TIOCGWINSZ ioctl. It
// returns ok=false when the kernel reports no pixel size, so the caller falls
// back to the handshake replies. cols and rows are unused here but kept for a
// symmetric resolution chain with parseCellPixels.
func winsizeCellPixels(fd uintptr, _, _ int) (cellPx, bool) {
	ws, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	if err != nil || ws == nil {
		return cellPx{}, false
	}
	return deriveCellPixels(int(ws.Xpixel), int(ws.Ypixel), int(ws.Col), int(ws.Row))
}

// deriveCellPixels turns a winsize report into a per-cell pixel size. It is the
// pure core of winsizeCellPixels, split out so the zero-guard and the sanity
// bound are testable without a live ioctl. It returns ok=false when any field
// is zero, so a bogus report (for example a 32000x0 winsize, Ypixel 0) falls
// through, and ok=false when the derived cell px is outside the sane window.
func deriveCellPixels(xpixel, ypixel, col, row int) (cellPx, bool) {
	if xpixel == 0 || ypixel == 0 || col == 0 || row == 0 {
		return cellPx{}, false
	}
	c := cellPx{W: xpixel / col, H: ypixel / row}
	if !c.inBounds() {
		return cellPx{}, false
	}
	return c, true
}

// inBounds reports whether a derived cell pixel size sits inside the winsize
// sanity window: 4..32 wide and 8..64 tall. A size outside it is a bogus winsize
// report, so the caller falls through to the handshake or the 8x16 default.
func (c cellPx) inBounds() bool {
	return c.W >= winsizeMinCellW && c.W <= winsizeMaxCellW &&
		c.H >= winsizeMinCellH && c.H <= winsizeMaxCellH
}

// winsize cell-px sanity bounds. A derived cell width or height outside this
// window is treated as a bogus winsize report and rejected to the safe default.
const (
	winsizeMinCellW = 4
	winsizeMaxCellW = 32
	winsizeMinCellH = 8
	winsizeMaxCellH = 64
)

// cellPxEnv is the environment variable name for the cell-pixel-size override.
const cellPxEnv = "NYALA_CELL_PX"

// parseCellPxOverride parses a NYALA_CELL_PX override value of the form "WxH"
// (two positive integers, for example "16x33"). It is a diagnostic and harness
// knob: it lets a headless run behind a synthetic pty, where no winsize pixel
// report exists, pin the cell pixel size and reproduce a real terminal's device
// quantisation. When set (set=true) it takes precedence over the ioctl-derived
// and handshake-probed cell; production without the variable never calls this.
// It returns ok=false when unset. A set value that is empty, not "WxH", or holds
// a non-integer, zero, or negative width or height returns an error naming
// NYALA_CELL_PX, so a bad override fails loudly rather than falling back.
func parseCellPxOverride(value string, set bool) (cellPx, bool, error) {
	if !set {
		return cellPx{}, false, nil
	}
	w, h, found := strings.Cut(value, "x")
	if !found {
		return cellPx{}, false, fmt.Errorf("%s=%q: want WxH, two positive integers", cellPxEnv, value)
	}
	pxW, errW := strconv.Atoi(w)
	pxH, errH := strconv.Atoi(h)
	if errW != nil || errH != nil || pxW <= 0 || pxH <= 0 {
		return cellPx{}, false, fmt.Errorf("%s=%q: want WxH, two positive integers", cellPxEnv, value)
	}
	return cellPx{W: pxW, H: pxH}, true, nil
}

// keyEvent is one parsed key from the reader: either a single byte (b, with
// scroll == ScrollNone) or a recognised scroll key (scroll set, b unused). The
// reader resolves CSI escape sequences into scroll keys so the consumer handles
// whole keys, not raw bytes.
type keyEvent struct {
	b      byte      // the single key byte when scroll is ScrollNone
	scroll ScrollKey // the recognised scroll key, or ScrollNone for a byte
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
// byte (0x40..0x7e) and decoded; a recognised sequence is a scroll event, an
// unrecognised one is dropped (emit false). A bare ESC, or ESC not followed by
// '[', is passed through as a byte event so it is not swallowed. It reads only
// the bytes the sequence needs, so it does not block on the next key.
func nextEvent(br *bufio.Reader, b byte) (keyEvent, bool) {
	if b != keyEsc {
		return keyEvent{b: b}, true
	}
	next, err := br.ReadByte()
	if err != nil {
		return keyEvent{b: b}, true
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
	if k, ok := decodeCSI(body); ok {
		return keyEvent{scroll: k}, true
	}
	return keyEvent{}, false
}
