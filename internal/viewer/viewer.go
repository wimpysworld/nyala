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
	"fmt"
	"image"
	"image/color"
	"io"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	emmb64 "github.com/emmansun/base64"
	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/palette"
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
	// ColourXTermGrey maps block colours onto the xterm greyscale ramp.
	ColourXTermGrey
)

// State is the viewer's render and backend state. Cols and Rows are the terminal
// size in cells. Enabled lists the backends the Alt+R cycle steps through, in
// cycle order, highest-fidelity first with block last; the embedded cellPx is the
// real terminal cell pixel size used to scale the graphics backends.
type State struct {
	Backend   Backend               // active backend in the cycle
	Scheme    block.Scheme          // block glyph scheme; applies only to the block backend
	Colour    ColourMode            // block colour depth; applies only to the block backend
	Cols      int                   // terminal width in cells
	Rows      int                   // terminal height in cells
	Enabled   []Backend             // backends in the Alt+R cycle, highest-fidelity first
	cellPx                          // terminal cell pixel size for graphics scaling
	compat    terminalCompatibility // terminal-specific rendering compatibility policy
	colourCap ColourMode            // highest colour mode that Alt+C can select
	// automaticColourFallback records that auto detection selected True Colour.
	// Explicit colour flags and auto selections of lower modes leave it false.
	automaticColourFallback bool

	// sextantOK records whether the terminal self-draws sextant glyphs, resolved
	// once by configureBackends from the environment. The Alt+B scheme cycle
	// skips the sextant scheme when it is false, so cycling never lands on a
	// scheme the terminal cannot render.
	sextantOK bool

	// mux is the focus multiplexer hosting the session, recorded once by
	// configureBackends. The backend cycle and the Sixel pacing gates read
	// it; muxNone is the zero value on a native terminal.
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

	// kittyID is the last Kitty image ID, or 0 before the first frame or after
	// a reset. Herdr keeps one ID. The other routes place the next image before
	// deleting the previous one.
	kittyID int

	// kittyTmux selects inline Kitty passthrough with pane-local placeholders.
	kittyTmux      bool
	kittyTmuxPaint *tmuxKittyPlacement

	// kittyEnc is the reused Kitty live encoder. It holds the APC frame assembly
	// buffer across direct PNG frames. It is lazily built on the
	// first kitty frame and driven from the single draw goroutine, so it never
	// aliases. It stays nil on the block and sixel paths.
	kittyEnc *kitty.LiveEncoder

	// kittySHM creates the shared-memory objects the Kitty t=s fast path
	// transmits. It is lazily built on the first shm frame and driven from the
	// single draw goroutine. Reset and synchronous encode failure close it,
	// unlinking any objects the terminal never consumed. It stays nil on the
	// direct path.
	kittySHM kittySHMWriter

	// kittyWroteSHMID records the image id of the frame just rendered as an shm
	// transmit. The stream session reads and clears it per frame with
	// takeKittySHMID to correlate replies and drive the one-in-flight pacing gate.
	kittyWroteSHMID int

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

	// kittyKeyboard records that the startup handshake found the kitty keyboard
	// protocol, so Stream pushes its flags and the teardown pops them. It stays
	// false under a multiplexer, which runs no keyboard query, so those sessions
	// keep the legacy parser.
	kittyKeyboard bool

	// probe is the terminal handshake result configureBackends recorded, so
	// the About bar reads the terminal identity from one source. It stays the
	// zero value under GNU screen and herdr, which run no handshake.
	probe probeResult

	// pointerShapeOK records that the startup handshake confirmed OSC 22
	// pointer-shape support, resolved once by configureBackends. It stays
	// false under a multiplexer, whose reduced handshake sends no OSC 22
	// query, so those sessions never write a pointer-shape sequence.
	pointerShapeOK bool

	// paletteQueryOK records that the startup handshake confirmed OSC 4
	// palette-query support. configureBackends resolves it once, and runtime
	// palette selection does not change it.
	paletteQueryOK bool

	// staticPaletteSelected records the runtime Block palette policy. Its false
	// zero value selects adaptive palettes. The selection stays set across all
	// other runtime state changes.
	staticPaletteSelected bool

	// paletteEngine is the shared adaptive-palette engine for the 256 mode.
	// Inactive modes leave it alone, and its zero value carries no palette.
	// adaptivePalette puts it into the budget mode.
	paletteEngine palette.Engine

	// greyEngine is the scene-fitted luma ramp engine for the grey mode.
	// Inactive modes leave it alone. Its zero value carries no ramp, which draws
	// through the fixed xterm greyscale ramp.
	greyEngine palette.LumaEngine

	// paletteMode is the colour mode that last wrote OSC 4 registers. The
	// registers hold one mode's meaning at a time, so a colour cycle between
	// 256 and grey compares against it to release the registers before the new
	// mode's engine has fitted anything.
	paletteMode ColourMode

	// blockRenderer is the Block renderer ComposeBlockFrame configures once per
	// frame. It is held here rather than built per call because it is the single
	// point where the adaptive palette attaches: both compose entries take it by
	// value, and blockSGR reads its table back, so one frame's cells and its
	// registers always come from one palette.
	blockRenderer block.Renderer

	// paletteOSC4 is the OSC 4 palette write the last compose produced, as a run
	// of short sequences. It is empty when that compose reused the retained
	// palette or completed an exact re-fit, so that frame writes no palette bytes.
	// renderBlock drains it into the frame buffer ahead of the cells.
	paletteOSC4 []byte

	// paletteTiming arms palette work timing for one private stream render.
	// paletteObservation carries that render's counters back to streamSession
	// on the producer goroutine. RenderStreamFrame leaves timing off, so public
	// rendering and diagnostics read no clocks. paletteNow is a private clock
	// seam for tests; nil selects time.Now.
	paletteTiming      bool
	paletteObservation paletteFrameObservation
	paletteNow         func() time.Time

	// paletteSrc is the reusable palette sample the adaptive engine quantises:
	// one pixel per composed cell colour. It is reallocated only when the grid
	// changes size.
	paletteSrc *image.RGBA

	// paletteWrites records the OSC 4 registers this run wrote, so the teardown,
	// a backend switch, and a colour cycle out of the adaptive mode all write
	// the bare OSC 104 restore. It is a pointer because NewState returns a State
	// by value and an atomic.Bool cannot be copied. NewState allocates it, so
	// every production State carries the record before any goroutine starts;
	// paletteRecord covers a zero-value State.
	paletteWrites *paletteWriteRecord

	// fgSGR and bgSGR are the cached SGR writer pair the differ folds into its
	// reused per-frame buffer. sgrColour and sgrLUT record what the pair was
	// built from. The adaptive writers close over the palette table, so the pair
	// is rebuilt on a colour-mode change or a new table, never per frame.
	fgSGR, bgSGR block.SGRFunc
	sgrColour    ColourMode
	sgrLUT       []uint8

	// pointerShapes maps each queried pointer-shape name to whether the
	// terminal supports it. It is meaningful only when pointerShapeOK is
	// true; mapCursorToShape drops a shape absent from it.
	pointerShapes map[string]bool

	// barSGR is the SGR opener the bottom bar row writes, resolved once by
	// configureBackends from the OSC 11 background reply. Empty is the
	// pre-detection value, which barStyle reads as reverse video, so a State
	// that never ran a handshake still draws a legible bar.
	barSGR string
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

// SetPaletteQueryOK sets the OSC 4 palette-query verdict, so diagnostics and
// tests outside the package can select the scene-fitted 256 palette without
// running the terminal handshake. configureBackends writes the field directly.
func (s *State) SetPaletteQueryOK(ok bool) {
	s.paletteQueryOK = ok
}

// NewState builds the initial state: block as the safe pre-detection backend, the
// quadrant scheme, truecolour, and sized to a cols x rows terminal. The cycle
// holds every backend highest-fidelity first and the cell
// pixel size is the Route A default; configureBackends narrows the cycle, nominates
// the best available backend, and sets the real cell size after the handshake.
func NewState(cols, rows int) State {
	return State{
		Backend:       BackendBlock,
		Scheme:        block.SchemeQuadrant,
		Colour:        ColourTrue,
		colourCap:     ColourTrue,
		Cols:          cols,
		Rows:          rows,
		Enabled:       []Backend{BackendKitty, BackendSixel, BackendBlock},
		cellPx:        cellPx{W: cellraster.CellPxW, H: cellraster.CellPxH},
		paletteWrites: &paletteWriteRecord{},
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
	if s.Backend != enabled[0] {
		s.sixelBudget.invalidateFrame()
	}
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
	next := s.Enabled[(idx+dir+n)%n]
	if s.Backend != next {
		s.sixelBudget.invalidateFrame()
	}
	s.Backend = next
	return s.Backend
}

// schemeCycle is the Alt+B block glyph scheme cycle order: quadrant, sextant,
// wrapping. CycleScheme skips sextant when the terminal does not self-draw
// sextant glyphs.
var schemeCycle = []block.Scheme{block.SchemeQuadrant, block.SchemeSextant}

// CycleScheme steps the block glyph scheme through the quadrant -> sextant
// cycle (reverse steps backwards) and returns the new scheme. Sextant is
// skipped on a terminal off the allow-list, so the cycle only offers
// renderable schemes. The scheme applies only to the block backend; the
// stream dispatcher drops the cycle while a graphics backend draws, so no
// hidden state change surprises the next block stint.
func (s *State) CycleScheme(reverse bool) block.Scheme {
	s.Scheme = cycleValue(schemeCycle, s.Scheme, reverse, func(m block.Scheme) bool {
		if m == block.SchemeSextant {
			return s.sextantOK
		}
		return true
	})
	return s.Scheme
}

// colourCycle is the Alt+C block colour mode cycle order, highest capability
// first. CycleColour skips modes above the effective terminal cap.
var colourCycle = []ColourMode{ColourTrue, ColourXTerm256, ColourXTermGrey}

// CycleColour steps through the colour ladder and returns the new mode. It
// skips modes above the effective capability cap. The 256 rung draws a
// scene-fitted palette only when the adaptive policy is active, and the
// canonical xterm-256 table otherwise. The stream dispatcher drops the cycle
// while a graphics backend draws.
func (s *State) CycleColour(reverse bool) ColourMode {
	s.Colour = cycleValue(colourCycle, s.Colour, reverse, func(m ColourMode) bool {
		return m >= s.colourCap
	})
	return s.Colour
}

// adaptivePaletteSelected reports the selected Block palette policy. The
// terminal capability does not change this preference.
func (s *State) adaptivePaletteSelected() bool {
	return !s.staticPaletteSelected &&
		(s.Colour == ColourXTerm256 || s.Colour == ColourXTermGrey)
}

// adaptivePaletteActive reports whether the current Block mode can use the
// selected adaptive palette policy.
func (s *State) adaptivePaletteActive() bool {
	return s.adaptivePaletteSelected() && s.paletteQueryOK && s.Backend == BackendBlock
}

// togglePalettePolicy changes the selected Block palette policy when the
// active mode supports adaptive palettes. The returned control restores the
// terminal palette when the selection changes to static.
func (s *State) togglePalettePolicy() (changed bool, control string) {
	if !s.paletteQueryOK || s.Backend != BackendBlock ||
		(s.Colour != ColourXTerm256 && s.Colour != ColourXTermGrey) {
		return false, ""
	}
	s.staticPaletteSelected = !s.staticPaletteSelected
	s.paletteOSC4 = s.paletteOSC4[:0]
	if s.staticPaletteSelected {
		return true, s.releasePalette()
	}
	s.paletteEngine.Invalidate()
	s.greyEngine.Invalidate()
	return true, ""
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
	// A resize can also change cell pixels without changing the grid.
	s.sixelBudget.invalidateFrame()
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
	return s.renderStreamFrame(frame, w, false, false)
}

// renderStreamFrame draws a live frame and optionally places a Kitty page image
// below terminal text. paletteTiming records adaptive palette work for the
// stream meter. Public rendering and diagnostics leave both private options off.
func (s *State) renderStreamFrame(frame render.Frame, w io.Writer, kittyBelowText, paletteTiming bool) error {
	s.paletteObservation = paletteFrameObservation{}
	s.paletteTiming = paletteTiming
	defer func() { s.paletteTiming = false }()
	// The frame's pooled pixel plane, when present, returns to the engine on
	// every exit: painted, copied into shared memory, or dropped. Release is
	// nil-safe and exactly-once, so non-plane frames pay only a nil check.
	defer releaseFrame(frame)
	if !frame.HasImage() {
		return nil
	}
	if s.Backend == BackendKitty {
		if plane, ok := frame.Img.(*image.RGBA); ok && s.kittySHMLive() {
			if err := s.encodeKittySHM(plane, w, kittyBelowText); err != nil {
				s.closeKittySHM()
				return err
			}
			return nil
		}
		// A retained-payload frame with no shm plane to encode (a mode-change
		// race) and every direct-transport frame takes the direct encode. A
		// plane-only frame without the shm transport is dropped.
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
		return s.renderSixelBudgeted(img, frame.Release != nil, w)
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
// rows: the bottom-row bar reservation renders the page at rows-1, and
// fitting to the full grid would upscale the frame back over the bar row.
func (s *State) fitSixelToGrid(img image.Image) image.Image {
	gridW, gridH := render.GraphicsCaptureSize(s.Cols, engineRows(s.Rows), s.W, s.H)
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
	raw, err := emmb64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("viewer: decode retained PNG base64: %w", err)
	}
	img, err := pngdecode.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("viewer: decode retained PNG: %w", err)
	}
	return img, nil
}

// renderBlock composes the source onto the Cols x page-rows grid and writes it
// as SGR cells, honouring the selected colour mode.
// text is the crisp DOM-text overlay folded onto the composed grid; a nil text
// draws the bitmap alone.
//
// The block path composes at the page grid with no aspect-fit letterbox: the
// engine sizes the block frame to exactly the page grid geometry (cols*cellW
// by pageRows*cellH device pixels), so the bitmap maps one-to-one onto the
// grid. The overlay bins each run in whole-cell coordinates against the same grid,
// so a whole-grid compose keeps the bitmap and the overlay on one horizontal scale.
// An aspect-fit would assume a 1:2 cell aspect and letterbox the bitmap onto fewer
// columns whenever the real terminal cell aspect differs (a 13x27 cell letterboxes
// 195 columns onto 187), drifting the overlay right of the bitmap by a scale that
// grows with the column. The graphics backends draw the same engine-sized page-grid
// bitmap from the top-left, so they fill the whole page area without a placement
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
		// The registers must reach the terminal before the cells that name them.
		// The palette write, the differ reset, and the full repaint therefore go
		// into this one frame buffer in that order; a palette write in a later
		// buffer would draw this frame's cells in the previous frame's colours.
		// The reset makes the repaint full. The differ does not track which
		// retained cells use a register whose meaning changed.
		if ctl := s.pendingPaletteWrite(); ctl != nil {
			if _, werr := bw.Write(ctl); werr != nil {
				return werr
			}
			s.diff.Reset()
			if s.paletteTiming {
				s.paletteObservation.paletteFullRepaints = 1
			}
		}
		fg, bg := s.blockSGR()
		err = s.diff.Emit(bw, *grid, fg, bg)
	}
	if err != nil {
		return err
	}
	return bw.Flush()
}

// ComposeBlockFrame returns the exact Block grid consumed by the terminal
// writer. It composes at the page grid (engineRows of the terminal rows, the
// same reduced grid the engine captures at), so the bottom bar row stays
// outside the composed frame. The Block path reuses State scratch storage, so
// callers that retain the result across another compose must copy it.
func (s *State) ComposeBlockFrame(frame render.Frame) *block.Grid {
	if frame.Img == nil {
		return nil
	}
	rows := engineRows(s.Rows)
	// Attaching the palette here, on the one renderer both compose entries take,
	// is what keeps the RGBA entry and the native YCbCr entry on one palette.
	br := &s.blockRenderer
	*br = block.Renderer{Cols: s.Cols, Rows: rows, Scheme: s.Scheme, PaletteLUT: s.adaptiveLUT()}
	pxW, pxH := schemePixels(s.Scheme, s.Cols, rows)
	if pxW <= 0 || pxH <= 0 {
		return nil
	}
	s.ensureScratchPool()
	_, nativeYCbCr := frame.Img.(*image.YCbCr)
	srcW, srcH := frame.Img.Bounds().Dx(), frame.Img.Bounds().Dy()
	if nativeYCbCr && pxW <= srcW && pxH <= srcH && (pxW < srcW || pxH < srcH) {
		return s.updatePalette(s.scratch.compose.ComposeYCbCrInto(br, &s.scratch.out, frame, s.scratch.pool))
	}
	frame.Img = s.scratch.downscale.DownscaleInto(s.scratch.downscaleDst(pxW, pxH), frame.Img, s.scratch.pool)
	return s.updatePalette(s.scratch.compose.ComposeInto(br, &s.scratch.out, frame, s.scratch.pool))
}

// updatePalette drives the adaptive palette engine for one composed grid and
// returns that grid unchanged. It runs inside the compose, so both compose
// entries and every caller, live and diagnostic, move one palette.
// ColourXTerm256 drives the colour engine and ColourXTermGrey drives the luma
// ramp engine. When the adaptive policy is inactive, it clears the pending
// write and does nothing else.
//
// The engine's reuse verdict is the gate, never a frame count: a frame the drift
// test lets keep the retained palette leaves paletteOSC4 empty and therefore
// writes no palette bytes. An exact completed re-fit does the same. Every other
// re-fit can renumber registers, so its table and cached writers update before
// the frame emits cells. paletteMode then records the register mode.
func (s *State) updatePalette(grid *block.Grid) *block.Grid {
	s.paletteOSC4 = s.paletteOSC4[:0]
	if grid == nil || !s.adaptivePaletteActive() {
		return grid
	}
	switch s.Colour {
	case ColourXTerm256:
		var checkStart time.Time
		if s.paletteTiming {
			checkStart = s.paletteTimeNow()
		}
		src := s.paletteFrame(grid)
		eng := s.adaptivePalette()
		sample := eng.Sample(src)
		reuse := eng.Reuse(src.Bounds(), sample)
		if s.paletteTiming {
			s.paletteObservation.paletteChecks = 1
			s.paletteObservation.paletteCheckMS = paletteMSSince(checkStart, s.paletteTimeNow())
		}
		if reuse {
			return grid
		}
		var fitStart time.Time
		if s.paletteTiming {
			s.paletteObservation.paletteReuseFailures = 1
			fitStart = s.paletteTimeNow()
		}
		result := eng.Requantise(src.Bounds(), sample, palette.BudgetColours)
		if s.paletteTiming {
			s.paletteObservation.paletteFitMS = paletteMSSince(fitStart, s.paletteTimeNow())
			s.observePaletteRefit(result)
		}
		s.publishPaletteRefit(result, eng.LUT, eng.Palette, eng.Base())
	case ColourXTermGrey:
		var checkStart time.Time
		if s.paletteTiming {
			checkStart = s.paletteTimeNow()
		}
		src := s.paletteFrame(grid)
		eng := &s.greyEngine
		sample := eng.Sample(src)
		reuse := eng.Reuse(src.Bounds(), sample)
		if s.paletteTiming {
			s.paletteObservation.paletteChecks = 1
			s.paletteObservation.paletteCheckMS = paletteMSSince(checkStart, s.paletteTimeNow())
		}
		if reuse {
			return grid
		}
		var fitStart time.Time
		if s.paletteTiming {
			s.paletteObservation.paletteReuseFailures = 1
			fitStart = s.paletteTimeNow()
		}
		result := eng.Requantise(src.Bounds(), sample)
		if s.paletteTiming {
			s.paletteObservation.paletteFitMS = paletteMSSince(fitStart, s.paletteTimeNow())
			s.observePaletteRefit(result)
		}
		s.publishPaletteRefit(result, eng.LUT, eng.Palette, eng.Base())
	default:
		return grid
	}
	return grid
}

// publishPaletteRefit attaches a completed re-fit to the current frame. An
// exact re-fit keeps the terminal-held palette, its indexed writers and the
// retained cell diff. Every other re-fit publishes the complete palette.
func (s *State) publishPaletteRefit(result palette.RefitResult, lut []uint8, colours color.Palette, base int) {
	s.blockRenderer.PaletteLUT = lut
	if result.ExactNoOp {
		return
	}
	s.refreshBlockSGR(lut)
	before := len(s.paletteOSC4)
	var oscStart time.Time
	if s.paletteTiming {
		oscStart = s.paletteTimeNow()
	}
	s.paletteOSC4 = palette.AppendOSC4(s.paletteOSC4, colours, base)
	if s.paletteTiming {
		s.paletteObservation.paletteOSCMS = paletteMSSince(oscStart, s.paletteTimeNow())
		if oscBytes := len(s.paletteOSC4) - before; oscBytes > 0 {
			entries := len(colours)
			s.paletteObservation.paletteUpdates = 1
			s.paletteObservation.paletteOSCEntries = int64(entries)
			s.paletteObservation.paletteOSCSequences = int64((entries + palette.MaxOSC4Entries - 1) / palette.MaxOSC4Entries)
			s.paletteObservation.paletteOSCBytes = int64(oscBytes)
		}
	}
	s.paletteMode = s.Colour
}

// observePaletteRefit copies one completed engine result into the current
// frame observation. The caller invokes it only while palette timing is armed.
func (s *State) observePaletteRefit(result palette.RefitResult) {
	if result.LengthEqual {
		s.paletteObservation.paletteLengthEqualRefits = 1
	}
	if result.PaletteEqual {
		s.paletteObservation.paletteEqualRefits = 1
	}
	if result.LUTEqual {
		s.paletteObservation.paletteLUTEqualRefits = 1
	}
	if result.ExactNoOp {
		s.paletteObservation.paletteNoOpRefits = 1
	}
	s.paletteObservation.paletteChangedEntries = int64(result.ChangedEntries)
}

func (s *State) paletteTimeNow() time.Time {
	if s.paletteNow != nil {
		return s.paletteNow()
	}
	return time.Now()
}

func paletteMSSince(start, end time.Time) float64 {
	return float64(end.Sub(start)) / float64(time.Millisecond)
}

// paletteFrame writes the composed grid's colours into the reusable palette
// sample and returns it: one pixel per cell colour, the foreground at an even
// column and the background at the odd column beside it. The grid holds exactly
// the colours the terminal draws, so quantising it needs no second compose and
// serves the native YCbCr entry, which leaves no RGBA frame behind, as well as
// the RGBA entry. The sample is 2*Cols wide and Rows tall, so a resize changes
// its bounds and the engine drops the palette built for the old geometry.
func (s *State) paletteFrame(grid *block.Grid) *image.RGBA {
	bounds := image.Rect(0, 0, grid.Cols*2, grid.Rows)
	if s.paletteSrc == nil || s.paletteSrc.Bounds() != bounds {
		s.paletteSrc = image.NewRGBA(bounds)
	}
	pix := s.paletteSrc.Pix
	for row := range grid.Rows {
		offset := s.paletteSrc.PixOffset(0, row)
		for col := range grid.Cols {
			c := grid.At(col, row)
			pix[offset], pix[offset+1], pix[offset+2], pix[offset+3] = c.FG.R, c.FG.G, c.FG.B, 0xff
			pix[offset+4], pix[offset+5], pix[offset+6], pix[offset+7] = c.BG.R, c.BG.G, c.BG.B, 0xff
			offset += 8
		}
	}
	return s.paletteSrc
}

// paletteWriteRecord records the OSC 4 registers one run wrote. live is the
// mid-run gate: releasePalette clears it as it hands the bare OSC 104 to the
// ordered control path, so a later switch that disturbed nothing writes no
// needless restore. ever is sticky and the teardown reads it, because the
// control write is asynchronous and a fatal signal or a failed write can lose
// the queued restore; a teardown reading the cleared gate would then leave the
// adaptive palette on the terminal after Nyala exits. The teardown's restore is
// idempotent, so the sticky record costs at most one redundant OSC 104 at exit.
// Both fields are atomic because the fatal-signal watcher goroutine reads them
// while the producer goroutine writes them.
type paletteWriteRecord struct {
	live atomic.Bool
	ever atomic.Bool
}

// pendingPaletteWrite returns the OSC 4 bytes the last compose produced,
// or nil when that compose reused the retained palette. It records that the
// terminal now holds written registers, which is what releasePalette and the
// teardown read. The next compose rewrites the buffer, so the caller must write
// the bytes before it composes again.
func (s *State) pendingPaletteWrite() []byte {
	if len(s.paletteOSC4) == 0 {
		return nil
	}
	record := s.paletteRecord()
	record.live.Store(true)
	record.ever.Store(true)
	return s.paletteOSC4
}

// paletteRecord returns the shared OSC 4 write record, allocating it for a
// zero-value State. Only the draw goroutine calls it, so the lazy allocation
// never races the fatal-signal watcher, which reads through paletteDisturbed.
func (s *State) paletteRecord() *paletteWriteRecord {
	if s.paletteWrites == nil {
		s.paletteWrites = &paletteWriteRecord{}
	}
	return s.paletteWrites
}

// paletteDisturbed reports whether any frame in this run wrote OSC 4 registers.
// The teardown reads it, so it is safe on any goroutine and on a State that
// never drew a frame. It stays true after releasePalette, so an exit that races
// the queued restore still writes its own.
func (s *State) paletteDisturbed() bool {
	return s.paletteWrites != nil && s.paletteWrites.ever.Load()
}

// releasePalette returns the bare OSC 104 restore when the 256 or grey mode
// wrote registers the terminal no longer needs, and the empty string otherwise.
// It clears the mid-run gate and drops both retained palettes, so a later
// adaptive frame in either mode re-quantises and rewrites the registers the
// reset discarded. The gate matters: a run that never disturbed the palette
// must leave the user's theme untouched.
func (s *State) releasePalette() string {
	if !s.paletteRecord().live.Swap(false) {
		return ""
	}
	s.paletteEngine.Invalidate()
	s.greyEngine.Invalidate()
	return palette.OSC104
}

// paletteLoading reports whether the current Block mode writes OSC 4
// registers. recomposeBlock reads it to decide whether a cycle keeps the
// registers.
func (s *State) paletteLoading() bool {
	return s.adaptivePaletteActive()
}

// adaptivePalette returns the adaptive palette engine in the 240-entry budget
// mode, which is what a terminal needs: registers 0..15 keep the user's theme
// and the engine writes 16..255 only. SetBudget is idempotent, so the repeat
// call costs one comparison.
func (s *State) adaptivePalette() *palette.Engine {
	s.paletteEngine.SetBudget(true)
	return &s.paletteEngine
}

// adaptiveLUT returns the RGB555 lookup table the Block path maps cell colours
// through: the colour engine's table in ColourXTerm256 and the luma ramp
// engine's table in ColourXTermGrey. It returns nil when the colour mode is
// neither, the adaptive policy is inactive, or the mode's engine holds no
// palette yet. A nil table falls back to the static xterm-256 table inside
// block.PaletteIndex. The grey SGR writers guard nil themselves, because grey
// must never fall back to colour.
func (s *State) adaptiveLUT() []uint8 {
	if !s.adaptivePaletteActive() {
		return nil
	}
	switch s.Colour {
	case ColourXTerm256:
		return s.adaptivePalette().LUT
	case ColourXTermGrey:
		return s.greyEngine.LUT
	default:
		return nil
	}
}

// blockSGR returns the foreground and background SGR writers the cell differ
// uses for the current colour mode, rebuilding the pair only when the mode or
// the attached palette table changes. The adaptive writers close over the
// palette table, so a per-frame rebuild would allocate on the frame path; every
// other mode returns a capture-free literal. It reads the table off
// blockRenderer, which ComposeBlockFrame attaches before the writers run, so a
// frame's cells and its registers describe one palette.
func (s *State) blockSGR() (fg, bg block.SGRFunc) {
	lut := s.blockRenderer.PaletteLUT
	if s.fgSGR == nil || s.sgrColour != s.Colour || !sameTable(s.sgrLUT, lut) {
		s.refreshBlockSGR(lut)
	}
	return s.fgSGR, s.bgSGR
}

// refreshBlockSGR rebuilds the cached SGR writer pair from the current colour
// mode and the given palette table. It is the explicit rebuild point: a
// requantisation that hands the Block path a new table calls it, and blockSGR
// calls it whenever it finds the cached pair stale.
func (s *State) refreshBlockSGR(lut []uint8) {
	s.sgrColour = s.Colour
	s.sgrLUT = lut
	adaptive := s.adaptivePaletteActive()
	s.fgSGR = fgSGRFunc(s.Colour, lut, adaptive)
	s.bgSGR = bgSGRFunc(s.Colour, lut, adaptive)
}

// sameTable reports whether two palette tables are the same slice: equal length
// and, when non-empty, one backing array. The engine rebuilds its table in
// place, so a captured slice stays correct across a requantisation and only a
// new slice header needs a rebuilt writer pair.
func sameTable(a, b []uint8) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || &a[0] == &b[0]
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
// in the given scheme: 2x4 per cell for quadrant, 2x3 per cell for
// sextant. The compose step needs the source at this extent so
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

// fgSGRFunc binds a colour mode, an adaptive palette table, and the OSC 4
// probe verdict into a block.SGRFunc for the cell differ, so the differ stays
// colour-mode neutral while reusing the viewer's fg SGR rules. Each mode
// appends into the differ's reused buffer, so no per-cell string allocates.
// ColourXTerm256 with paletteOK maps through lut, which holds absolute
// terminal registers; a nil lut falls back to the static xterm-256 table.
// ColourXTerm256 without paletteOK maps through the static table directly.
// ColourXTermGrey with paletteOK and a non-nil lut maps through lut, the luma
// ramp registers; without paletteOK, or before the ramp engine has fitted a
// table, it maps through the fixed xterm greyscale ramp, because a nil lut
// would fall back to colour inside block.PaletteIndex. Every other mode ignores
// lut and paletteOK.
func fgSGRFunc(mode ColourMode, lut []uint8, paletteOK bool) block.SGRFunc {
	switch mode {
	case ColourXTerm256:
		if paletteOK {
			return func(dst []byte, c color.RGBA) []byte {
				return appendSGR256(dst, fg256Prefix, block.PaletteIndex(lut, c))
			}
		}
		return func(dst []byte, c color.RGBA) []byte {
			return appendSGR256(dst, fg256Prefix, block.XTerm256(c))
		}
	case ColourXTermGrey:
		if paletteOK && lut != nil {
			return func(dst []byte, c color.RGBA) []byte {
				return appendSGR256(dst, fg256Prefix, block.PaletteIndex(lut, c))
			}
		}
		return func(dst []byte, c color.RGBA) []byte {
			return appendSGR256(dst, fg256Prefix, block.XTermGrey(c))
		}
	default:
		return block.AppendTrueColorFG
	}
}

// bgSGRFunc binds a colour mode, an adaptive palette table, and the OSC 4
// probe verdict into a block.SGRFunc for the cell differ, so the differ stays
// colour-mode neutral while reusing the viewer's bg SGR rules. Each mode
// appends into the differ's reused buffer, so no per-cell string allocates.
// ColourXTerm256 with paletteOK maps through lut, which holds absolute
// terminal registers; a nil lut falls back to the static xterm-256 table.
// ColourXTerm256 without paletteOK maps through the static table directly.
// ColourXTermGrey with paletteOK and a non-nil lut maps through lut, the luma
// ramp registers; without paletteOK, or before the ramp engine has fitted a
// table, it maps through the fixed xterm greyscale ramp, because a nil lut
// would fall back to colour inside block.PaletteIndex. Every other mode ignores
// lut and paletteOK.
func bgSGRFunc(mode ColourMode, lut []uint8, paletteOK bool) block.SGRFunc {
	switch mode {
	case ColourXTerm256:
		if paletteOK {
			return func(dst []byte, c color.RGBA) []byte {
				return appendSGR256(dst, bg256Prefix, block.PaletteIndex(lut, c))
			}
		}
		return func(dst []byte, c color.RGBA) []byte {
			return appendSGR256(dst, bg256Prefix, block.XTerm256(c))
		}
	case ColourXTermGrey:
		if paletteOK && lut != nil {
			return func(dst []byte, c color.RGBA) []byte {
				return appendSGR256(dst, bg256Prefix, block.PaletteIndex(lut, c))
			}
		}
		return func(dst []byte, c color.RGBA) []byte {
			return appendSGR256(dst, bg256Prefix, block.XTermGrey(c))
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
		// The first draw clears the startup bar screen or prior terminal
		// state, so the differ must full-repaint against the blank screen,
		// not a retained grid.
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

// pushTitle and popTitle are XTWINOPS title-stack operations. Push saves the
// terminal's window title on entry and pop restores the saved title on exit,
// so the page title Nyala sets while running never outlives the session.
const (
	pushTitle = "\x1b[22;0t"
	popTitle  = "\x1b[23;0t"
)

// setTitle returns the OSC 0 sequence that sets the window and icon title.
// The terminator is ST, never BEL, so the payload can never be confused with
// a BEL a hostile page smuggled into the title text.
func setTitle(title string) string {
	return "\x1b]0;" + title + "\x1b\\"
}

// progressSequence returns the OSC 9;4 sequence that reports page-load
// progress to the terminal. Both fields are always present, because some
// terminals treat a missing percent field oddly. The terminator is ST, never
// BEL, matching setTitle.
func progressSequence(state, percent int) string {
	return "\x1b]9;4;" + strconv.Itoa(state) + ";" + strconv.Itoa(percent) + "\x1b\\"
}

// progressClear is the OSC 9;4 clear sequence the teardown writes
// unconditionally, so no exit path leaves a progress indicator behind.
const progressClear = "\x1b]9;4;0;0\x1b\\"

// enterTerminalModes returns the mode-enable sequence Stream writes once after
// the startup handshake: the full-screen escapes, mouse reporting, bracketed
// paste, and the kitty keyboard protocol when this terminal answered the
// capability query. Every mode it turns on is undone by exitTerminal, which runs
// on normal exit, on every signal in fatalSignals, and on an error return, so
// the pairing is one function either side.
func enterTerminalModes(kittyKeyboard bool) string {
	modes := enterFullScreen + enableMouseReporting + enableBracketedPaste + pushTitle
	if kittyKeyboard {
		modes += enableKittyKeyboard
	}
	return modes
}

// exitTerminal returns the teardown sequence for the interactive viewer. It
// resets SGR attributes, disables mouse reporting and bracketed paste
// unconditionally (pairing the enableMouseReporting write and a quit taken
// mid URL edit, so a leaked mode cannot spray reports or paste markers into
// the shell), pops the kitty keyboard protocol when kittyKeyboard records that
// this session pushed it (the pop is conditional where the other resets are
// not, because the protocol is a stack: an unmatched pop would discard the
// entry an outer application pushed), resets Sixel display mode, restores
// auto-wrap, and shows the cursor. Resetting DECSDM here also covers an interrupted Sixel write that
// did not reach its per-frame reset. After a frame has drawn, it first moves to
// column one of the final rendered row and emits one CRLF. The necessary
// single-line scroll leaves the complete browser area above a clean row for an
// FPS summary or the shell prompt, independent of the cursor position left by
// Block, Sixel, or Kitty. Before the first frame it preserves the cursor
// position, so a startup failure does not displace terminal content. The title
// pop is emitted only when titlePushed records that enterTerminalModes wrote
// the matching push, because the title stack is shared: an unmatched pop would
// discard the entry an outer application, such as tmux, saved. The palette
// restore is emitted only when paletteWritten records that the adaptive colour
// mode set OSC 4 registers, so a run that never disturbed the palette leaves the
// user's theme untouched.
//
// The palette restore lives here, beside the other unconditional resets, rather
// than in exitTerminalWithTitle, which owns one thing: the title-pop fallback.
// Every teardown, including the fatal-signal path, goes through this function,
// so one addition covers them all.
func exitTerminal(rows int, drawn, kittyKeyboard, titlePushed, paletteWritten bool) string {
	const reset = "\x1b[0m"
	restore := disableMouseReporting + disableBracketedPaste
	if kittyKeyboard {
		restore += disableKittyKeyboard
	}
	restore += endSixelDisplayMode + "\x1b[?7h\x1b[?25h"
	if titlePushed {
		restore += popTitle
	}
	// Clear any OSC 9;4 progress indicator unconditionally, like the mouse
	// reporting disable: the indicator is not stack-based, so clearing one that
	// was never set is harmless, and every exit path including a fatal signal
	// must clear it.
	restore += progressClear
	// Restore the terminal palette when the adaptive colour mode wrote OSC 4
	// registers. WezTerm holds the written colours until a reset arrives, so an
	// exit without this write leaves the shell drawing in the last page's
	// registers; the write is not a WezTerm workaround, it pairs every OSC 4.
	if paletteWritten {
		restore += palette.OSC104
	}
	if !drawn || rows <= 0 {
		return reset + restore
	}
	return reset + "\x1b[" + strconv.Itoa(rows) + ";1H\r\n" + restore
}

// exitTerminalWithTitle wraps exitTerminal for the teardown write. When the
// compatibility policy records that this terminal ignores the title pop, it
// follows the pop with an explicit neutral title. The payload is a fixed
// non-empty string because an empty OSC 0 payload resets to the terminal
// default title, not the title saved by the push. The fallback is tied to the
// pop: when no push was written, there is no pop and no neutral title either.
func exitTerminalWithTitle(rows int, drawn, kittyKeyboard, titlePushed, paletteWritten, titlePopIgnored bool) string {
	out := exitTerminal(rows, drawn, kittyKeyboard, titlePushed, paletteWritten)
	if titlePushed && titlePopIgnored {
		out += setTitle("terminal")
	}
	return out
}

// maxTitleBytes caps a sanitised title. The bound limits a smuggled payload,
// and a longer title is unreadable in a tab or window list anyway.
const maxTitleBytes = 128

// sanitiseTitle prepares a page title for the OSC 0 write. It strips every C0
// control, DEL, and C1 control through stripControls, caps the result at
// maxTitleBytes cut on a rune boundary, and on an empty result falls back to
// the URL host, or to the raw URL when parsing fails or yields no host.
func sanitiseTitle(title, rawURL string) string {
	clean := truncateTitle(stripControls(title))
	if clean != "" {
		return clean
	}
	fallback := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		fallback = u.Host
	}
	return truncateTitle(stripControls(fallback))
}

// truncateTitle cuts s at maxTitleBytes on a rune boundary, so the cap never
// splits a multi-byte rune into an invalid tail byte.
func truncateTitle(s string) string {
	if len(s) <= maxTitleBytes {
		return s
	}
	cut := maxTitleBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
