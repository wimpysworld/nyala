// Package viewer is the interactive raw-terminal image viewer for Nyala.
//
// The viewer loads a PNG, fits it to the terminal, and re-fits on resize. It
// cycles the render backend (block, sixel, kitty) behind the render.Renderer
// seam, toggles the block glyph scheme (quadrant, half-block), and toggles the
// block colour depth (truecolour, xterm-256). Switching backend swaps only the
// Renderer value; nothing above the seam changes.
//
// State holds the whole machine and is pure and testable without a live
// terminal: every hotkey is a method that mutates State, and Fit and Renderer
// derive their result from State alone. Run wires State to a raw TTY.
package viewer

import (
	"fmt"
	"image"
	"image/color"
	_ "image/png" // register PNG decoder for image.Decode
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	cterm "github.com/charmbracelet/x/term"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// Backend identifies one render backend in the cycle order. The Tab hotkey
// advances it block -> sixel -> kitty -> block.
type Backend int

const (
	// BackendBlock is the owned-grid block-cell renderer; it honours the glyph
	// scheme and colour toggles. It is the first backend in the cycle.
	BackendBlock Backend = iota
	// BackendSixel is the Sixel graphics backend. It ignores the scheme and
	// colour toggles, which apply only to the block backend.
	BackendSixel
	// BackendKitty is the Kitty graphics backend. It ignores the scheme and
	// colour toggles, which apply only to the block backend.
	BackendKitty
	// backendCount bounds the cycle; it is not a backend.
	backendCount
)

// ColourMode selects the block backend's colour depth. The colour hotkey flips
// it between truecolour and xterm-256. The graphics backends ignore it.
type ColourMode int

const (
	// ColourTrue emits 24-bit truecolour SGR for the block backend.
	ColourTrue ColourMode = iota
	// ColourXTerm256 maps each block cell colour onto the xterm-256 palette.
	ColourXTerm256
)

// State is the viewer's full state machine. It is pure: every field is data and
// every hotkey method mutates only this struct, so the machine is exercised in a
// unit test without a live terminal. Cols and Rows are the terminal size in
// cells; ImgW and ImgH are the loaded image's pixel dimensions.
type State struct {
	Backend Backend      // active backend in the cycle
	Scheme  block.Scheme // block glyph scheme; applies only to the block backend
	Colour  ColourMode   // block colour depth; applies only to the block backend
	Cols    int          // terminal width in cells
	Rows    int          // terminal height in cells
	ImgW    int          // loaded image width in pixels
	ImgH    int          // loaded image height in pixels
}

// NewState builds the initial state: the block backend, the quadrant scheme,
// truecolour, sized to a cols x rows terminal showing an imgW x imgH image.
func NewState(cols, rows, imgW, imgH int) State {
	return State{
		Backend: BackendBlock,
		Scheme:  block.SchemeQuadrant,
		Colour:  ColourTrue,
		Cols:    cols,
		Rows:    rows,
		ImgW:    imgW,
		ImgH:    imgH,
	}
}

// CycleBackend advances the backend block -> sixel -> kitty -> block and returns
// the new backend. The scheme and colour state are untouched, so toggling them
// on block and cycling away then back preserves the chosen scheme and colour.
func (s *State) CycleBackend() Backend {
	s.Backend = (s.Backend + 1) % backendCount
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

// ToggleColour flips the block colour depth between truecolour and xterm-256 and
// returns the new mode. It changes only the colour mode; the active backend is
// unchanged.
func (s *State) ToggleColour() ColourMode {
	if s.Colour == ColourTrue {
		s.Colour = ColourXTerm256
	} else {
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

// Action is the outcome of feeding one key to the state machine: whether the
// viewer should redraw and whether it should quit.
type Action struct {
	Redraw bool // the state changed; the caller should re-render
	Quit   bool // the user asked to quit
}

// keyQuitCtrlC is the Ctrl-C byte; keyTab is the Tab byte.
const (
	keyQuitCtrlC = 0x03
	keyTab       = 0x09
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
		return s.renderBlock(src, w)
	}
	return s.renderGraphics(src, fit, w)
}

// renderBlock composes the source onto the fit-and-letterbox grid and writes it
// as SGR cells, honouring the colour toggle: truecolour SGR or xterm-256 SGR.
// FitGrid recomputes the fit-and-letterbox placement from the State dimensions,
// so the image stays centred without a passed-in Fit.
func (s *State) renderBlock(src *image.RGBA, w io.Writer) error {
	br := block.Renderer{Cols: s.Cols, Rows: s.Rows, Scheme: s.Scheme}
	grid := br.FitGrid(s.ImgW, s.ImgH, func(cols, rows int) block.Grid {
		pxW, pxH := schemePixels(s.Scheme, cols, rows)
		sub := metrics.Downscale(src, pxW, pxH)
		inner := block.Renderer{Cols: cols, Rows: rows, Scheme: s.Scheme}
		return inner.Compose(render.Frame{Img: sub})
	})
	return writeGrid(grid, s.Colour, w)
}

// renderGraphics downscales the source to the fitted pixel extent and encodes it
// with the active graphics backend. Sixel and kitty draw a bitmap, so the fit
// maps to pixels at the Route A cell size; the image keeps its aspect.
func (s *State) renderGraphics(src *image.RGBA, fit block.Fit, w io.Writer) error {
	pxW := fit.FitCols * capture.CellPxW
	pxH := fit.FitRows * capture.CellPxH
	sub := metrics.Downscale(src, pxW, pxH)
	return s.Renderer().Render(render.Frame{Img: sub}, w)
}

// schemePixels returns the source-pixel extent a cols x rows block grid samples
// in the given scheme: 2x4 per cell for quadrant, 1x2 per cell for half-block.
// The compose step needs the source at this extent so each cell maps to its
// own block of pixels.
func schemePixels(scheme block.Scheme, cols, rows int) (pxW, pxH int) {
	if scheme == block.SchemeHalfBlock {
		return cols, rows * 2
	}
	return cols * 2, rows * 4
}

// writeGrid writes a composed grid to w as SGR cells, one block glyph per cell,
// in the chosen colour mode. Truecolour emits 24-bit SGR; xterm-256 maps each
// cell's fg and bg onto the 256-colour palette. Each row ends with an SGR reset
// and a CRLF. The carriage return returns the cursor to column 0 when auto-wrap
// is off in raw mode. It mirrors block.Renderer.Render but adds the colour
// toggle the concrete renderer does not carry.
func writeGrid(g block.Grid, mode ColourMode, w io.Writer) error {
	var sb strings.Builder
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			c := g.At(col, row)
			sb.WriteString(fgSGR(c.FG, mode))
			sb.WriteString(bgSGR(c.BG, mode))
			sb.WriteRune(c.Glyph)
		}
		sb.WriteString("\x1b[0m\r\n")
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// fgSGR returns the foreground SGR for a colour in the chosen mode.
func fgSGR(c color.RGBA, mode ColourMode) string {
	if mode == ColourXTerm256 {
		return fmt.Sprintf("\x1b[38;5;%dm", block.XTerm256(c))
	}
	return block.TrueColorFG(c)
}

// bgSGR returns the background SGR for a colour in the chosen mode.
func bgSGR(c color.RGBA, mode ColourMode) string {
	if mode == ColourXTerm256 {
		return fmt.Sprintf("\x1b[48;5;%dm", block.XTerm256(c))
	}
	return block.TrueColorBG(c)
}

// LoadPNG decodes the PNG at path into an *image.RGBA in RGBA channel order. A
// non-RGBA decoded image is converted to RGBA so the render path reads one
// pixel layout.
func LoadPNG(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("viewer: open png: %w", err)
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("viewer: decode png: %w", err)
	}
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba, nil
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			rgba.Set(x-b.Min.X, y-b.Min.Y, img.At(x, y))
		}
	}
	return rgba, nil
}

// clearScreen homes the cursor and clears the terminal so the next frame draws
// from the top-left without stale cells.
const clearScreen = "\x1b[H\x1b[2J"

// enterFullScreen disables auto-wrap (DECAWM, "\x1b[?7l") and hides the cursor
// ("\x1b[?25l"). With auto-wrap off, a full-width row does not enter the
// pending-wrap state, so the explicit carriage return and line feed advance one
// physical line per grid row.
const enterFullScreen = "\x1b[?7l\x1b[?25l"

// exitFullScreen re-enables auto-wrap (DECAWM, "\x1b[?7h") and shows the cursor
// ("\x1b[?25h"). Run writes it during teardown to restore the terminal to its
// default state before the termios restore.
const exitFullScreen = "\x1b[?7h\x1b[?25h"

// Run loads the PNG at path and runs the interactive viewer on the given TTY
// until the user quits. It puts the terminal into raw mode, draws on resize and
// on each redraw hotkey, and restores the terminal on exit. It returns an error
// only on a setup or I/O failure; a clean quit returns nil. Run is the live
// wiring around State; the state machine itself is tested without it.
func Run(path string, tty *os.File) error {
	src, err := LoadPNG(path)
	if err != nil {
		return err
	}

	fd := tty.Fd()
	if !cterm.IsTerminal(fd) {
		return fmt.Errorf("viewer: %s is not a terminal", tty.Name())
	}
	cols, rows, err := cterm.GetSize(fd)
	if err != nil {
		return fmt.Errorf("viewer: terminal size: %w", err)
	}

	oldState, err := cterm.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("viewer: raw mode: %w", err)
	}
	defer func() { _ = cterm.Restore(fd, oldState) }()

	if _, err := io.WriteString(tty, enterFullScreen); err != nil {
		return err
	}
	// Registered after the Restore defer, so LIFO runs it first: re-enable
	// auto-wrap and show the cursor while still in raw mode, then Restore.
	defer func() { _, _ = io.WriteString(tty, exitFullScreen) }()

	b := src.Bounds()
	st := NewState(cols, rows, b.Dx(), b.Dy())

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	draw := func() error {
		if _, werr := io.WriteString(tty, clearScreen); werr != nil {
			return werr
		}
		return st.RenderFrame(src, tty)
	}
	if err := draw(); err != nil {
		return err
	}

	keys := readKeys(tty)
	for {
		select {
		case <-winch:
			if nc, nr, gerr := cterm.GetSize(fd); gerr == nil {
				st.Resize(nc, nr)
				if derr := draw(); derr != nil {
					return derr
				}
			}
		case key, ok := <-keys:
			if !ok {
				return nil
			}
			act := st.HandleKey(key)
			if act.Quit {
				return nil
			}
			if act.Redraw {
				if derr := draw(); derr != nil {
					return derr
				}
			}
		}
	}
}

// readKeys reads single bytes from r and sends them on the returned channel,
// closing it on EOF or read error. It runs in its own goroutine so Run can
// select between keys and resize signals.
func readKeys(r io.Reader) <-chan byte {
	out := make(chan byte)
	go func() {
		defer close(out)
		buf := make([]byte, 1)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				out <- buf[0]
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}
