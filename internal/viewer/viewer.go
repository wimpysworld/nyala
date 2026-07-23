// Package viewer is the interactive raw-terminal frame viewer for Nyala.
//
// The viewer draws each frame an Engine pushes to the terminal. It cycles the
// render backend (block, sixel, kitty) on Alt+R, the block glyph scheme on
// Alt+B, and the block colour depth on Alt+C; the --blocks and --color flags
// set the starting scheme and depth.
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
	"strconv"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/pngdecode"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
	"github.com/wimpysworld/nyala/internal/rowband"
)

// Backend identifies one render backend in the cycle order.
type Backend = render.Backend

const (
	// BackendBlock is the owned-grid block-cell renderer; it honours the glyph
	// scheme and colour mode. It is the floor backend, placed last in the cycle.
	BackendBlock = render.BackendBlock
	// BackendSixel is the Sixel graphics backend. It ignores the scheme and
	// colour mode, which apply only to the block backend.
	BackendSixel = render.BackendSixel
	// BackendKitty is the Kitty graphics backend. It ignores the scheme and
	// colour mode, which apply only to the block backend.
	BackendKitty = render.BackendKitty
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
// size in cells. Enabled lists the backends the Alt+R cycle steps through, in
// cycle order, highest-fidelity first with block last; the embedded cellPx is the
// real terminal cell pixel size used to scale the graphics backends.
type State struct {
	Backend Backend               // active backend in the cycle
	Scheme  block.Scheme          // block glyph scheme; applies only to the block backend
	Colour  ColourMode            // block colour depth; applies only to the block backend
	Cols    int                   // terminal width in cells
	Rows    int                   // terminal height in cells
	Enabled []Backend             // backends in the Alt+R cycle, highest-fidelity first
	cellPx                        // terminal cell pixel size for graphics scaling
	compat  terminalCompatibility // terminal-specific rendering compatibility policy

	// sextantOK records whether the terminal self-draws sextant glyphs, resolved
	// once by configureBackends from the environment. The Alt+B scheme cycle
	// skips the sextant scheme when it is false, so cycling never lands on a
	// scheme the terminal cannot render.
	sextantOK bool

	// mux is the focus multiplexer hosting the session, recorded once by
	// configureBackends. engineRows reads it to reserve the bottom terminal
	// row for Sixel under tmux; muxNone (the zero value) keeps every
	// native-terminal path at the full grid.
	mux multiplexerKind

	// cellPxForced records that the NYALA_CELL_PX override supplied cellPx, so a
	// resize never re-derives the pair from the winsize and the diagnostic
	// geometry stays pinned for the whole run.
	cellPxForced bool

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
	// to 0 when the backend cycle leaves kitty. It stays 0 on the block and
	// sixel paths.
	kittyID int

	// kittyEnc is the reused Kitty live encoder. It holds the APC frame assembly
	// buffer across direct PNG frames. It is lazily built on the
	// first kitty frame and driven from the single draw goroutine, so it never
	// aliases. It stays nil on the block and sixel paths.
	kittyEnc *kitty.LiveEncoder

	// kittySHM creates the shared-memory objects the Kitty t=s fast path
	// transmits. It is lazily built on the first shm frame and driven from the
	// single draw goroutine. resetKitty and demoteKittySHM close it, unlinking
	// any objects the terminal never consumed. It stays nil on the direct path.
	kittySHM kittySHMWriter

	// kittyShmDemoted marks the runtime fallback from the shm transport to
	// direct PNG. An shm create or write failure sets it for the rest of the
	// Kitty stint, so a flaky /dev/shm cannot fail every frame; a cycle back to
	// Kitty re-arms it via rearmKittySHM because the startup probe proved the
	// transport.
	kittyShmDemoted bool

	// kittyShmDemoteErr holds the error that demoted the shm transport until
	// the stream session collects it with takeKittySHMDemotion for its
	// one-shot log and engine decode toggle.
	kittyShmDemoteErr error

	// kittyWroteSHM records that the frame just rendered went out as an shm
	// transmit. The stream session reads and clears it per frame with
	// takeKittySHMWrote to drive the one-in-flight pacing gate.
	kittyWroteSHM bool

	// sixelEnc is the reused Sixel renderer. It retains encoder scratch across
	// moving frames and switches its output through an internal writer proxy.
	// The viewer draw path is serial, so one renderer owns the whole stream.
	sixelEnc *sixel.Renderer

	// sixelBudget is the tmux Sixel byte-budget state. configureBackends arms
	// its limit under tmux only; a zero limit keeps every other environment on
	// the direct streaming encode.
	sixelBudget sixelBudget

	// kittyTransport is the effective Kitty transmission medium, resolved once
	// by configureBackends from the --kitty-transport override, the deny list,
	// the startup shm probe, and the allow list. It is always direct or shm.
	kittyTransport KittyTransport
}

// KittyTransport returns the effective Kitty transmission medium resolved by
// configureBackends: direct or shm, never auto.
func (s *State) KittyTransport() KittyTransport {
	return s.kittyTransport
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

// SetEnabled records the backends the Alt+R cycle steps through, in cycle order,
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
	return s.cycleBackendStep(1)
}

// CycleBackendReverse steps to the previous enabled backend in cycle order and
// wraps, then returns the new backend, the Alt+Shift+R inverse of CycleBackend.
func (s *State) CycleBackendReverse() Backend {
	return s.cycleBackendStep(-1)
}

// cycleBackendStep moves dir steps (+1 forwards, -1 backwards) through the
// enabled cycle and wraps in either direction.
func (s *State) cycleBackendStep(dir int) Backend {
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
	n := len(s.Enabled)
	s.Backend = s.Enabled[(idx+dir+n)%n]
	return s.Backend
}

// schemeCycle is the Alt+B block glyph scheme cycle order: half, quadrant,
// sextant, wrapping. CycleScheme skips sextant when the terminal does not
// self-draw sextant glyphs.
var schemeCycle = []block.Scheme{block.SchemeHalfBlock, block.SchemeQuadrant, block.SchemeSextant}

// CycleScheme steps the block glyph scheme through the half -> quadrant ->
// sextant cycle (reverse steps backwards) and returns the new scheme. A scheme
// the terminal cannot render (sextant off the allow-list) is skipped, so the
// cycle only offers renderable schemes. The scheme applies only to the block
// backend; the stream dispatcher drops the cycle while a graphics backend
// draws, so no hidden state change surprises the next block stint.
func (s *State) CycleScheme(reverse bool) block.Scheme {
	s.Scheme = cycleValue(schemeCycle, s.Scheme, reverse, func(m block.Scheme) bool {
		return m != block.SchemeSextant || s.sextantOK
	})
	return s.Scheme
}

// colourCycle is the Alt+C block colour mode cycle order: truecolour,
// xterm-256, monochrome, wrapping. CycleColour skips truecolour where the
// terminal compatibility policy caps it.
var colourCycle = []ColourMode{ColourTrue, ColourXTerm256, ColourMono}

// CycleColour steps the block colour mode through the truecolour -> 256 ->
// monochrome cycle (reverse steps backwards) and returns the new mode. A mode
// the terminal cannot sustain (truecolour under Rio 0.4.x, whose style table
// truecolour Block frames exhaust) is skipped, following the same
// compatibility state that caps --color=auto. The mode applies only to the
// block backend; the stream dispatcher drops the cycle while a graphics
// backend draws.
func (s *State) CycleColour(reverse bool) ColourMode {
	s.Colour = cycleValue(colourCycle, s.Colour, reverse, func(m ColourMode) bool {
		return m != ColourTrue || s.compat.trueColourAllowed()
	})
	return s.Colour
}

// cycleValue steps current through cycle (backwards when reverse), skipping
// values allowed rejects, and wraps. It returns current unchanged when every
// other value is disallowed, so a fully capped cycle is a no-op rather than a
// spin.
func cycleValue[T comparable](cycle []T, current T, reverse bool, allowed func(T) bool) T {
	idx := 0
	for i, v := range cycle {
		if v == current {
			idx = i
			break
		}
	}
	dir := 1
	if reverse {
		dir = -1
	}
	n := len(cycle)
	for step := 1; step < n; step++ {
		next := cycle[(idx+dir*step+n*step)%n]
		if allowed(next) {
			return next
		}
	}
	return current
}

// Resize records a new terminal size in cells. The next frame reflects the new
// size.
func (s *State) Resize(cols, rows int) {
	s.Cols = cols
	s.Rows = rows
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
	return s.renderStreamFrame(frame, w, false)
}

// renderStreamFrame draws a live frame and optionally places a Kitty page image
// below terminal text. The option follows the live-badge policy separately from
// FPS measurement, so public rendering, diagnostics, non-FPS output, and timed
// Rio Kitty output retain normal placement.
func (s *State) renderStreamFrame(frame render.Frame, w io.Writer, kittyBelowText bool) error {
	// The frame's pooled pixel plane, when present, returns to the engine on
	// every exit: painted, copied into shared memory, or dropped. Release is
	// nil-safe and exactly-once, so non-plane frames pay only a nil check.
	defer releaseFrame(frame)
	if !frame.HasImage() {
		return nil
	}
	if s.Backend == BackendKitty {
		if plane, ok := frame.Img.(*image.RGBA); ok && s.kittySHMLive() {
			err := s.encodeKittySHM(plane, w, kittyBelowText)
			if err == nil {
				return nil
			}
			// Runtime demotion: the direct PNG path takes over for the rest
			// of the Kitty stint. This frame still paints when it carries a
			// retained payload; a plane-only frame is dropped and its plane
			// released by the deferred hook.
			s.demoteKittySHM(err)
		}
		// A retained-payload frame in shm mode (a mode-change race) and every
		// demoted or direct-transport frame takes the direct encode. A plane
		// frame with no payload after demotion falls out here and is dropped.
		if frame.EncodedPNG == "" {
			return nil
		}
		return s.encodeKittyPNG(frame.EncodedPNG, w, kittyBelowText)
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
	img = s.fitSixelToGrid(img)
	if s.sixelBudget.limit > 0 {
		return s.renderSixelBudgeted(img, w)
	}
	return s.sixelEnc.Render(render.Frame{Img: img}, w)
}

// fitSixelToGrid scales a Sixel frame onto the cell-grid pixel extent when it
// no longer spans it. After a mid-session zoom the engine keeps delivering the
// complete viewport at the stale launch paint density, and Sixel, unlike
// Kitty, has no placement key the terminal scales by, so the viewer fits the
// bitmap itself through the shared block scaler (area-average down,
// nearest-fill up). The steady state pays nothing: a frame at the grid extent
// returns unchanged, and a height-only shortfall under one cell is the
// engine's floored fractional-density row band, not a zoom, so it draws as-is
// like today rather than rescaling every frame on a device-pixel-cell
// terminal. The target height uses the engine grid rows, not the terminal
// rows: under the tmux bottom-row reservation the engine renders at
// rows-1, and fitting to the full grid would upscale the frame back over the
// reserved row.
func (s *State) fitSixelToGrid(img image.Image) image.Image {
	gridW, gridH := render.GraphicsCaptureSize(s.Cols, engineRows(s.Backend, s.mux, s.Rows), s.W, s.H)
	if gridW <= 0 || gridH <= 0 {
		return img
	}
	b := img.Bounds()
	dh := gridH - b.Dy()
	if dh < 0 {
		dh = -dh
	}
	if b.Dx() == gridW && dh < s.H {
		return img
	}
	s.ensureScratchPool()
	return s.scratch.downscale.DownscaleInto(s.scratch.downscaleDst(gridW, gridH), img, s.scratch.pool)
}

// ensureScratchPool lazily builds the shared rowband pool and its two band
// drivers on first use, so every scaling caller reuses one persistent worker
// set and the hot path spawns no goroutine.
func (s *State) ensureScratchPool() {
	if s.scratch.pool == nil {
		s.scratch.pool = rowband.New()
		s.scratch.downscale = metrics.NewParallelDownscale()
		s.scratch.compose = block.NewParallelCompose()
	}
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
	s.ensureScratchPool()
	_, nativeYCbCr := frame.Img.(*image.YCbCr)
	srcW, srcH := frame.Img.Bounds().Dx(), frame.Img.Bounds().Dy()
	if nativeYCbCr && pxW <= srcW && pxH <= srcH && (pxW < srcW || pxH < srcH) {
		return s.scratch.compose.ComposeYCbCrInto(&br, &s.scratch.out, frame, s.scratch.pool)
	}
	frame.Img = s.scratch.downscale.DownscaleInto(s.scratch.downscaleDst(pxW, pxH), frame.Img, s.scratch.pool)
	return s.scratch.compose.ComposeInto(&br, &s.scratch.out, frame, s.scratch.pool)
}

// releaseFrame returns a frame's pooled pixel plane to the engine, if any. The
// engine sets Release only on kitty-shm decoded frames and makes it
// exactly-once, so every other path pays a nil check and a double call is a
// safe no-op.
func releaseFrame(f render.Frame) {
	if f.Release != nil {
		f.Release()
	}
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
// render overwrites each cell in place with no per-frame full clear. diff may
// be nil (the graphics backends carry none), in which case no seed runs.
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

// sgrReset clears all SGR attributes, so a following erase fills with the
// terminal default background rather than the last drawn cell's colours.
const sgrReset = "\x1b[0m"

// clearScreen homes the cursor and clears the terminal so the next frame draws
// from the top-left without stale cells. The draw path uses it for the first
// draw and on resize; the steady path homes the cursor with cursorHome and
// overwrites every cell in place, so the full erase is not needed per frame.
const clearScreen = "\x1b[H\x1b[2J"

// cursorHome homes the cursor without erasing, so the steady draw path overwrites
// every cell in place. Auto-wrap is off and each cell is rewritten, so nothing
// stale remains after the overwrite.
const cursorHome = "\x1b[H"

// beginSixelDisplayMode and endSixelDisplayMode bracket a live Sixel image with
// DECSDM. Set mode 80 selects fixed display placement: the image starts at the
// screen origin without moving the text cursor or scrolling the viewport. Reset
// returns the terminal to its normal scrolling Sixel mode after the payload.
const (
	beginSixelDisplayMode = "\x1b[?80h"
	endSixelDisplayMode   = "\x1b[?80l"
)

// beginSyncUpdate and endSyncUpdate bracket a frame in a DEC 2026 synchronised
// update (ESC [ ? 2026 h and ESC [ ? 2026 l), so the terminal presents the whole
// frame atomically and never scans out a partial frame while the render bytes
// land. A terminal without DEC 2026 ignores the private mode. The live path emits
// the markers for every frame except the detected Foot Sixel compatibility case.
const (
	beginSyncUpdate = "\x1b[?2026h"
	endSyncUpdate   = "\x1b[?2026l"
)

// enterFullScreen disables auto-wrap (DECAWM, "\x1b[?7l") and hides the cursor
// ("\x1b[?25l"). With auto-wrap off, a full-width row does not enter the
// pending-wrap state, so the explicit carriage return and line feed advance one
// physical line per grid row.
const enterFullScreen = "\x1b[?7l\x1b[?25l"

// exitTerminal returns the teardown sequence for the interactive viewer. It
// resets SGR attributes, disables mouse reporting unconditionally (pairing
// the enableMouseReporting write, so a leaked mode cannot spray SGR reports
// into the shell), resets Sixel display mode, restores auto-wrap, and shows
// the cursor. Resetting DECSDM here also covers an interrupted Sixel write that
// did not reach its per-frame reset. After a frame has drawn, it first moves to
// column one of the final rendered row and emits one CRLF. The necessary
// single-line scroll leaves the complete browser area above a clean row for an
// FPS summary or the shell prompt, independent of the cursor position left by
// Block, Sixel, or Kitty. Before the first frame it preserves the cursor
// position, so a startup failure does not displace terminal content.
func exitTerminal(rows int, drawn bool) string {
	const reset = "\x1b[0m"
	const restore = disableMouseReporting + endSixelDisplayMode + "\x1b[?7h\x1b[?25h"
	if !drawn || rows <= 0 {
		return reset + restore
	}
	return reset + "\x1b[" + strconv.Itoa(rows) + ";1H\r\n" + restore
}
