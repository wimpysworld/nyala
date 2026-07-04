// Package viewer is the interactive raw-terminal frame viewer for Nyala.
//
// The viewer draws each frame an Engine pushes, fits it to the terminal, and
// re-fits on resize. It cycles the render backend (block, sixel, kitty) behind
// the render.Renderer seam, toggles the block glyph scheme (quadrant,
// half-block), and toggles the block colour depth (truecolour, xterm-256).
// Switching backend swaps only the Renderer value; nothing above the seam
// changes.
//
// State holds the whole machine and is pure and testable without a live
// terminal: every hotkey is a method that mutates State, and Fit and Renderer
// derive their result from State alone. Stream wires State to a raw TTY and an
// Engine's pushed frames.
package viewer

import (
	"bufio"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"strconv"
	"strings"

	cterm "github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// Backend identifies one render backend in the cycle order. The Tab hotkey
// advances through the enabled backends and wraps; configureBackends orders them
// highest-fidelity first (kitty, then sixel) with block last as the floor.
type Backend int

const (
	// BackendBlock is the owned-grid block-cell renderer; it honours the glyph
	// scheme and colour toggles. It is the floor backend, placed last in the cycle.
	BackendBlock Backend = iota
	// BackendSixel is the Sixel graphics backend. It ignores the scheme and
	// colour toggles, which apply only to the block backend.
	BackendSixel
	// BackendKitty is the Kitty graphics backend. It ignores the scheme and
	// colour toggles, which apply only to the block backend.
	BackendKitty
)

// ColourMode selects the block backend's colour depth. The colour hotkey flips
// it between truecolour and xterm-256. The graphics backends ignore it.
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

// State is the viewer's full state machine. It is pure: every field is data and
// every hotkey method mutates only this struct, so the machine is exercised in a
// unit test without a live terminal. Cols and Rows are the terminal size in
// cells; ImgW and ImgH are the loaded image's pixel dimensions. Enabled lists the
// backends the Tab cycle steps through, in cycle order, highest-fidelity first
// with block last; the embedded cellPx is the real terminal cell pixel size used
// to scale the graphics backends.
type State struct {
	Backend Backend      // active backend in the cycle
	Scheme  block.Scheme // block glyph scheme; applies only to the block backend
	Colour  ColourMode   // block colour depth; applies only to the block backend
	Cols    int          // terminal width in cells
	Rows    int          // terminal height in cells
	ImgW    int          // loaded image width in pixels
	ImgH    int          // loaded image height in pixels
	Enabled []Backend    // backends in the Tab cycle, block first
	cellPx               // terminal cell pixel size for graphics scaling

	// captureGrid is the capture-only grid sink. It defaults nil. The capture
	// driver sets it before a block draw so renderBlock writes the composed
	// letterbox grid here as well as to the terminal, yielding both the drawn
	// bytes and the serialised grid from one compose with no drift. Off the
	// capture path it stays nil and renderBlock writes nothing to it.
	captureGrid *block.Grid

	// diff is the live-path cell differ. It defaults nil. The interactive stream
	// session sets it so renderBlock emits only the cells that changed since the
	// previous frame, with CUP and SGR coalescing. When nil (the capture path and
	// the static-image path) renderBlock writes the full grid via writeGrid, so
	// those paths keep their byte output. The viewer live path is not golden-
	// pinned, so the diffed output may differ from a full rewrite while the
	// rendered result matches.
	diff *block.GridDiff

	// scratch holds the reusable per-frame buffers for the live diff path: the
	// downscale destination, the inner compose grid, the fit-and-letterbox output
	// grid, and the buffered tty writer. It is used only when diff is set, which
	// is the single-goroutine interactive draw path, so the buffers never alias
	// across goroutines. Off the diff path (capture and static image) it stays
	// untouched and those paths keep their fresh-allocation byte output.
	scratch frameScratch
}

// frameScratch is the live diff path's reusable per-frame buffers. Each draw
// fully overwrites the downscale pixels and recomposes both grids, so reuse
// carries no stale content. dstW and dstH record the downscale size so the
// destination reallocates only when the grid or scheme changes the pixel extent.
// bw buffers the tty writer so each frame Resets and Flushes one held writer
// rather than allocating a new bufio.Writer. The zero value is unset; renderBlock
// lazily fills it on the first diff draw.
type frameScratch struct {
	dst        *image.RGBA   // reused downscale destination
	dstW, dstH int           // current downscale pixel extent
	inner      block.Grid    // reused inner compose grid
	out        block.Grid    // reused fit-and-letterbox output grid
	bw         *bufio.Writer // reused buffered tty writer
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

// writer returns the held bufio.Writer reset onto w, allocating it once. The live
// diff path wraps the tty so the differ's many small writes coalesce into one
// syscall, and the held writer is reset per frame rather than reallocated.
func (f *frameScratch) writer(w io.Writer) *bufio.Writer {
	if f.bw == nil {
		f.bw = bufio.NewWriter(w)
	} else {
		f.bw.Reset(w)
	}
	return f.bw
}

// SetDiff sets the live-path cell differ. The interactive stream session passes a
// pointer to a block.GridDiff once before the draw loop; renderBlock then emits
// only the cells that changed since the previous frame. Pass nil to fall back to
// the full-grid writeGrid path. It is a no-op for the graphics backends.
func (s *State) SetDiff(d *block.GridDiff) {
	s.diff = d
}

// SetCaptureGrid sets the capture-only grid sink. The capture driver passes a
// pointer to a block.Grid before each block draw; renderBlock fills it with the
// composed letterbox grid it draws, so the serialised grid never drifts from the
// drawn bytes. Pass nil to clear it. It is a no-op for the graphics backends,
// which compose no cell grid.
func (s *State) SetCaptureGrid(g *block.Grid) {
	s.captureGrid = g
}

// NewState builds the initial state: block as the safe pre-detection backend, the
// quadrant scheme, truecolour, sized to a cols x rows terminal showing an imgW x
// imgH image. The cycle holds every backend highest-fidelity first and the cell
// pixel size is the Route A default; configureBackends narrows the cycle, nominates
// the best available backend, and sets the real cell size after the handshake.
func NewState(cols, rows, imgW, imgH int) State {
	return State{
		Backend: BackendBlock,
		Scheme:  block.SchemeQuadrant,
		Colour:  ColourTrue,
		Cols:    cols,
		Rows:    rows,
		ImgW:    imgW,
		ImgH:    imgH,
		Enabled: []Backend{BackendKitty, BackendSixel, BackendBlock},
		cellPx:  cellPx{W: capture.CellPxW, H: capture.CellPxH},
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
// colour state are untouched, so toggling them on block and cycling away then
// back preserves the chosen scheme and colour.
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

// ToggleScheme flips the block glyph scheme between quadrant and half-block and
// returns the new scheme. It changes only the scheme; the active backend is
// unchanged, so the toggle takes effect when the block backend is active.
func (s *State) ToggleScheme() block.Scheme {
	if s.Scheme == block.SchemeQuadrant {
		s.Scheme = block.SchemeHalfBlock
	} else {
		s.Scheme = block.SchemeQuadrant
	}
	return s.Scheme
}

// ToggleColour cycles the block colour depth truecolour -> xterm-256 ->
// monochrome -> truecolour and returns the new mode. It changes only the colour
// mode; the active backend is unchanged.
func (s *State) ToggleColour() ColourMode {
	switch s.Colour {
	case ColourTrue:
		s.Colour = ColourXTerm256
	case ColourXTerm256:
		s.Colour = ColourMono
	default:
		s.Colour = ColourTrue
	}
	return s.Colour
}

// Resize records a new terminal size in cells. The next Fit and Renderer reflect
// the new size, so the viewer re-fits the image to the resized terminal.
func (s *State) Resize(cols, rows int) {
	s.Cols = cols
	s.Rows = rows
}

// Fit returns the fit-and-letterbox placement of the loaded image inside the
// current terminal grid, preserving the image aspect at the 1:2 cell aspect. It
// derives from State alone, so a resize changes the fit without other state.
func (s *State) Fit() block.Fit {
	return block.FitImage(s.ImgW, s.ImgH, s.Cols, s.Rows)
}

// Renderer returns the render.Renderer for the active backend. Switching backend
// changes only this returned value; the Frame above the seam is identical across
// backends. The block renderer carries the current scheme; the graphics backends
// carry no scheme or colour state.
func (s *State) Renderer() render.Renderer {
	switch s.Backend {
	case BackendSixel:
		return &sixel.Renderer{}
	case BackendKitty:
		return &kitty.Renderer{}
	default:
		return &block.Renderer{Cols: s.Cols, Rows: s.Rows, Scheme: s.Scheme}
	}
}

// ScrollKey is a recognised scroll key. The viewer does not track scroll state
// (the engine owns the offset); it surfaces the pressed scroll key on Action so
// the push consumer (stream.go) forwards it to engine.SendKey. ScrollNone is the
// zero value: no scroll key.
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

// Action is the outcome of feeding one key to the state machine: whether the
// viewer should redraw, whether it should quit, and any recognised scroll key to
// forward. Scroll is ScrollNone unless the key was a scroll key.
type Action struct {
	Redraw bool      // the state changed; the caller should re-render
	Quit   bool      // the user asked to quit
	Scroll ScrollKey // a recognised scroll key to forward, or ScrollNone
}

// keyQuitCtrlC is the Ctrl-C byte; keyTab is the Tab byte; keyEsc is the escape
// byte that opens a CSI sequence.
const (
	keyQuitCtrlC = 0x03
	keyTab       = 0x09
	keyEsc       = 0x1b
)

// HandleKey applies one key byte to the state machine and reports the action.
// Tab cycles the backend; 's' toggles the glyph scheme; 'c' toggles the colour
// depth; 'q' or Ctrl-C quits. Any other key is ignored with no redraw. It never
// touches the terminal, so it is unit-tested directly.
func (s *State) HandleKey(b byte) Action {
	switch b {
	case keyTab:
		s.CycleBackend()
		return Action{Redraw: true}
	case 's', 'S':
		s.ToggleScheme()
		return Action{Redraw: true}
	case 'c', 'C':
		s.ToggleColour()
		return Action{Redraw: true}
	case 'q', 'Q', keyQuitCtrlC:
		return Action{Quit: true}
	default:
		return Action{}
	}
}

// HandleScrollKey surfaces a recognised scroll key as an Action for the push
// consumer to forward to the engine. The viewer holds no scroll state, so it
// does not redraw or quit; it carries the key on Action.Scroll. ScrollNone is a
// no-op. It never touches the terminal, so it is unit-tested directly.
func (s *State) HandleScrollKey(k ScrollKey) Action {
	if k == ScrollNone {
		return Action{}
	}
	return Action{Scroll: k}
}

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

// RenderFrame fits the source image to the current terminal grid and writes one
// frame to w with the active backend. The block backend fits onto the owned cell
// grid and honours the scheme and colour toggles; the graphics backends encode
// the fitted image directly. The source is the full-resolution loaded image; it
// is downscaled to the fit extent here.
func (s *State) RenderFrame(src *image.RGBA, w io.Writer) error {
	fit := s.Fit()
	if fit.FitCols <= 0 || fit.FitRows <= 0 {
		return nil
	}
	if s.Backend == BackendBlock {
		return s.renderBlock(src, nil, w)
	}
	return s.renderGraphics(src, fit, w)
}

// RenderStreamFrame draws one live screencast frame, folding its crisp DOM-text
// overlay on the block path. The engine sizes the capture to the full grid and
// bins each text run in whole-cell coordinates, so the overlay aligns when the
// block path composes at the same full grid. The graphics backends carry no
// overlay and the engine already sized their bitmap to the terminal target, so
// they draw frame.Img as final via renderGraphicsFinal with no second downscale:
// the engine is the one graphics sizing owner. A nil or degenerate (zero-extent)
// image is a no-op; this one guard covers both the block and the graphics paths,
// so neither downscales to a zero extent.
func (s *State) RenderStreamFrame(frame render.Frame, w io.Writer) error {
	if frame.Img == nil {
		return nil
	}
	b := frame.Img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil
	}
	if s.Backend == BackendBlock {
		return s.renderBlock(frame.Img, frame.Text, w)
	}
	fit := s.Fit()
	if fit.FitCols <= 0 || fit.FitRows <= 0 {
		return nil
	}
	return s.renderGraphicsFinal(frame.Img, fit, w)
}

// renderBlock composes the source onto the fit-and-letterbox grid and writes it
// as SGR cells, honouring the colour toggle: truecolour SGR or xterm-256 SGR.
// FitGrid recomputes the fit-and-letterbox placement from the State dimensions,
// so the image stays centred without a passed-in Fit. text is the crisp DOM-text
// overlay folded onto the composed grid; a nil text draws the bitmap alone.
func (s *State) renderBlock(src *image.RGBA, text []render.GlyphRun, w io.Writer) error {
	// One block.Renderer value drives both FitGrid and the inner Compose, so the
	// grid size and scheme come from a single source.
	br := block.Renderer{Cols: s.Cols, Rows: s.Rows, Scheme: s.Scheme}

	// The live diff path is single-goroutine, so it reuses the per-frame buffers:
	// the downscale destination, both grids, and the buffered tty writer. The
	// capture and static-image paths (diff nil) keep the fresh-allocation path so
	// their byte output stays identical.
	if s.diff != nil {
		grid := br.FitGridInto(&s.scratch.out, s.ImgW, s.ImgH, func(cols, rows int) *block.Grid {
			pxW, pxH := schemePixels(s.Scheme, cols, rows)
			sub := metrics.DownscaleInto(s.scratch.downscaleDst(pxW, pxH), src)
			inner := block.Renderer{Cols: cols, Rows: rows, Scheme: s.Scheme}
			return inner.ComposeInto(&s.scratch.inner, render.Frame{Img: sub, Text: text})
		})
		if s.captureGrid != nil {
			// Copy the cells, never alias: the live scratch grid is reused next frame,
			// so the capture sink must own an independent backing array to serialise.
			s.captureGrid.Cols, s.captureGrid.Rows = grid.Cols, grid.Rows
			s.captureGrid.Cells = append(s.captureGrid.Cells[:0], grid.Cells...)
		}
		bw := s.scratch.writer(w)
		if err := s.diff.Emit(bw, *grid, fgSGRFunc(s.Colour), bgSGRFunc(s.Colour)); err != nil {
			return err
		}
		return bw.Flush()
	}

	grid := br.FitGrid(s.ImgW, s.ImgH, func(cols, rows int) block.Grid {
		pxW, pxH := schemePixels(s.Scheme, cols, rows)
		sub := metrics.Downscale(src, pxW, pxH)
		inner := block.Renderer{Cols: cols, Rows: rows, Scheme: s.Scheme}
		return inner.Compose(render.Frame{Img: sub, Text: text})
	})
	if s.captureGrid != nil {
		*s.captureGrid = grid
	}
	return writeGrid(grid, s.Colour, w)
}

// renderGraphics downscales the source to the fitted pixel extent and encodes it
// with the active graphics backend. Sixel and kitty draw a bitmap, so the fit
// maps to pixels at the real terminal cell size, filling the letterbox; the image
// keeps its aspect. The static-image path (RenderFrame) uses it because its source
// is a full-resolution loaded image that must be fitted and downscaled here.
func (s *State) renderGraphics(src *image.RGBA, fit block.Fit, w io.Writer) error {
	pxW, pxH := render.GraphicsCaptureSize(fit.FitCols, fit.FitRows, s.W, s.H)
	sub := metrics.Downscale(src, pxW, pxH)
	return s.encodeGraphics(sub, fit, w)
}

// renderGraphicsFinal encodes an already-sized graphics bitmap as final, with no
// second downscale. The live stream path uses it: the engine sized the bitmap to
// the terminal pixel target, so it is the one sizing owner and the viewer must not
// resize the frame again. It positions and encodes through the same encodeGraphics
// seam as the static path.
func (s *State) renderGraphicsFinal(src *image.RGBA, fit block.Fit, w io.Writer) error {
	return s.encodeGraphics(src, fit, w)
}

// encodeGraphics positions the cursor at the letterbox offset and encodes img
// with the active graphics backend. It is the shared tail of renderGraphics (which
// downscales first) and renderGraphicsFinal (which passes the engine-sized bitmap
// through), so the kitty delete-all and the centring offset stay identical on both
// paths.
func (s *State) encodeGraphics(img *image.RGBA, fit block.Fit, w io.Writer) error {
	if s.Backend == BackendKitty {
		if _, err := io.WriteString(w, kittyDeleteAll); err != nil {
			return err
		}
	}
	if move := cursorPosition(fit.OffsetRow, fit.OffsetCol); move != "" {
		if _, err := io.WriteString(w, move); err != nil {
			return err
		}
	}
	return s.Renderer().Render(render.Frame{Img: img}, w)
}

// cursorPosition returns the CUP escape (\x1b[<row>;<col>H) that moves the
// cursor to a centred cell offset, taking row and col as 0-based cell offsets
// and emitting 1-based coordinates. It returns the empty string when both
// offsets are 0, so a frame that fills the grid keeps clean output. The graphics
// backends use it to centre the bitmap at the same offset the block path bakes
// into letterbox cells via block.Renderer.FitGrid.
func cursorPosition(row, col int) string {
	if row == 0 && col == 0 {
		return ""
	}
	return fmt.Sprintf(cupFormat, row+1, col+1)
}

// schemePixels returns the source-pixel extent a cols x rows block grid samples
// in the given scheme: 2x4 per cell for quadrant, 1x2 per cell for half-block.
// The compose step needs the source at this extent so each cell maps to its
// own block of pixels. It maps the block.Scheme to render.BlockCaptureSize's
// quadrant bool, the shared helper the engine also calls.
func schemePixels(scheme block.Scheme, cols, rows int) (pxW, pxH int) {
	return render.BlockCaptureSize(scheme == block.SchemeQuadrant, cols, rows)
}

// writeGrid writes a composed grid to w as SGR cells, one block glyph per cell,
// in the chosen colour mode. Truecolour emits 24-bit SGR; xterm-256 maps each
// cell's fg and bg onto the 256-colour palette; monochrome emits the glyph alone
// with no fg/bg SGR, rendering a 1-bit silhouette on the terminal default fg/bg.
// Each row ends with an SGR reset; rows are joined by CRLF with no trailing
// newline after the last row, so a full-height grid does not scroll the top row
// off a raw-mode terminal. The carriage return returns the cursor to column 0
// when auto-wrap is off in raw mode. It adds the colour toggle the concrete
// block.Renderer.Render does not carry.
func writeGrid(g block.Grid, mode ColourMode, w io.Writer) error {
	var sb strings.Builder
	for row := range g.Rows {
		if row > 0 {
			sb.WriteString("\r\n")
		}
		for col := range g.Cols {
			c := g.At(col, row)
			if err := block.WriteCell(&sb, c, fgSGR(c.FG, mode), bgSGR(c.BG, mode)); err != nil {
				return err
			}
		}
		sb.WriteString(sgrReset)
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// fgSGR returns the foreground SGR for a colour in the chosen mode. Monochrome
// emits no foreground SGR.
func fgSGR(c color.RGBA, mode ColourMode) string {
	switch mode {
	case ColourMono:
		return ""
	case ColourXTerm256:
		return sgr256(fg256Prefix, block.XTerm256(c))
	default:
		return block.TrueColorFG(c)
	}
}

// bgSGR returns the background SGR for a colour in the chosen mode. Monochrome
// emits no background SGR.
func bgSGR(c color.RGBA, mode ColourMode) string {
	switch mode {
	case ColourMono:
		return ""
	case ColourXTerm256:
		return sgr256(bg256Prefix, block.XTerm256(c))
	default:
		return block.TrueColorBG(c)
	}
}

// sgr256 builds an xterm-256 SGR escape, prefix + index + "m", with
// strconv.AppendInt into a stack buffer instead of fmt.Sprintf. The index is a
// uint8, so the escape never exceeds the buffer. One string allocation remains
// for the return value; the change drops the fmt reflection path.
func sgr256(prefix string, idx uint8) string {
	var buf [16]byte
	b := append(buf[:0], prefix...)
	b = strconv.AppendInt(b, int64(idx), 10)
	b = append(b, 'm')
	return string(b)
}

// fgSGRFunc binds a colour mode into a block.SGRFunc for the cell differ, so the
// differ stays colour-mode neutral while reusing the viewer's fg SGR rules.
func fgSGRFunc(mode ColourMode) block.SGRFunc {
	return func(c color.RGBA) string { return fgSGR(c, mode) }
}

// bgSGRFunc binds a colour mode into a block.SGRFunc for the cell differ, so the
// differ stays colour-mode neutral while reusing the viewer's bg SGR rules.
func bgSGRFunc(mode ColourMode) block.SGRFunc {
	return func(c color.RGBA) string { return bgSGR(c, mode) }
}

// cupFormat is the CUP escape that moves the cursor to a 1-based row;column
// (ESC [ row ; col H). cursorPosition fills it with 1-based coordinates.
const cupFormat = "\x1b[%d;%dH"

// sgrReset is the SGR reset escape (ESC [ 0 m) that ends each grid row's colour
// run.
const sgrReset = "\x1b[0m"

// fg256Prefix and bg256Prefix are the xterm-256 foreground and background SGR
// prefixes (ESC [ 38;5; and ESC [ 48;5;). sgr256 appends the palette index and
// the trailing "m".
const (
	fg256Prefix = "\x1b[38;5;"
	bg256Prefix = "\x1b[48;5;"
)

// framePrefix returns the cursor escape the steady and capture draw paths emit
// before a frame's render bytes, and seeds the differ for the first draw. The
// first draw returns the full clearScreen and resets diff so it full-repaints
// against the blank screen; every later draw returns cursorHome only, so the
// render overwrites each cell in place with no per-frame full clear. diff may be
// nil (the graphics backends carry none), in which case no seed runs. Both the
// live stream draw and the capture draw call it, so the only allowed difference
// between a live and a captured draw is which writer the bytes land on.
func framePrefix(firstDrawn bool, diff *block.GridDiff) string {
	if firstDrawn {
		return cursorHome
	}
	if diff != nil {
		// The first draw clears the loading cue (live) or the prior terminal state
		// (capture), so the differ must full-repaint against the blank screen, not a
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

// kittyDeleteAll deletes every Kitty image and frees its data (a=d,d=A). The
// viewer writes it before a kitty redraw because clearScreen erases text but
// not Kitty images, so without it each redraw would layer a new image over the
// old ones.
const kittyDeleteAll = "\x1b_Ga=d,d=A\x1b\\"

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
// ZELLIJ, herdr, or STY set) it offers block only, because Stage 0 has no
// passthrough. Otherwise it runs one terminal handshake and offers a graphics
// backend only when the terminal confirms it. The colour mode resolves first, on
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
// graphics off. Stage 0 does not pass graphics through a multiplexer, so it
// offers block only there.
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
