// This file is the Stage 0 Bubble Tea coexistence spike.
//
// It is a check, not the Stage 4 chrome. It hosts the Stage 0 viewer viewport
// inside a Bubble Tea v2 program started with WithoutRenderer, and confirms the
// raw bitmap survives next to Bubble Tea's event loop. With the cell renderer
// disabled, Bubble Tea issues no cursor moves or screen clears, so the bitmap
// the viewport writes is never overwritten.
//
// The bitmap write is kept in one place, BitmapModel.draw, so a later stage can
// move it to a reserved blank region without touching the event handling. The
// model pulls in none of the Stage 4 chrome: it wraps the existing State and
// reuses RenderFrame.

package viewer

import (
	"image"
	"io"

	tea "charm.land/bubbletea/v2"
)

// BitmapModel hosts the Stage 0 viewer viewport under Bubble Tea. It is a
// minimal tea.Model: Update applies the same hotkeys as the raw-terminal
// viewer, and draw writes the bitmap to an isolated writer. View returns an
// empty view because the renderer is disabled; the bitmap is the only output.
type BitmapModel struct {
	state State       // the Stage 0 viewer state machine
	src   *image.RGBA // the full-resolution source image
	out   io.Writer   // the isolated bitmap sink, never touched by Bubble Tea
	err   error       // first write error, surfaced on quit
}

// NewBitmapModel builds a BitmapModel for the src image sized to a cols x rows
// terminal, writing the bitmap to out. The model starts on the block backend,
// the quadrant scheme, and truecolour, matching the raw-terminal viewer.
func NewBitmapModel(src *image.RGBA, cols, rows int, out io.Writer) BitmapModel {
	b := src.Bounds()
	return BitmapModel{
		state: NewState(cols, rows, b.Dx(), b.Dy()),
		src:   src,
		out:   out,
	}
}

// Init draws the first frame. It returns no command, so the program waits for
// input after the initial bitmap is written.
func (m *BitmapModel) Init() tea.Cmd {
	m.draw()
	return nil
}

// Update applies one message. A key press feeds the Stage 0 state machine: a
// redraw rewrites the bitmap, a quit ends the program. A window-size message
// re-fits to the new terminal and redraws. Other messages are ignored. The
// returned model carries any write error so Run can surface it.
func (m *BitmapModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		act := m.state.HandleKey(keyByte(msg))
		if act.Quit {
			return m, tea.Quit
		}
		if act.Redraw {
			m.draw()
		}
	case tea.WindowSizeMsg:
		m.state.Resize(msg.Width, msg.Height)
		m.draw()
	}
	return m, nil
}

// View returns an empty view. The renderer is disabled by WithoutRenderer, so
// Bubble Tea emits nothing; the bitmap written by draw is the sole output and
// is never overwritten by cursor moves or clears.
func (m *BitmapModel) View() tea.View {
	return tea.NewView("")
}

// Err reports the first bitmap write error, or nil. Run reads it after Wait.
func (m *BitmapModel) Err() error { return m.err }

// draw writes one frame of the bitmap to the isolated writer. This is the only
// bitmap write; a later stage moves it to a reserved blank region by changing
// this method alone. The first error is recorded and stops further writes.
func (m *BitmapModel) draw() {
	if m.err != nil {
		return
	}
	m.err = m.state.RenderFrame(m.src, m.out)
}

// keyByte maps a Bubble Tea key press to the single byte the Stage 0 state
// machine expects: the Tab and Ctrl-C control bytes, or the rune's low byte for
// the printable hotkeys (q, s, c). Keys outside this set map to zero, which the
// state machine ignores.
func keyByte(msg tea.KeyPressMsg) byte {
	k := msg.Key()
	switch k.Code {
	case tea.KeyTab:
		return keyTab
	case 'c':
		if k.Mod&tea.ModCtrl != 0 {
			return keyQuitCtrlC
		}
		return 'c'
	default:
		if k.Code > 0 && k.Code < 0x80 {
			return byte(k.Code)
		}
		return 0
	}
}

// RunUnderBubbleTea hosts the bitmap viewport in a Bubble Tea v2 program with
// the cell renderer disabled, reading keys from in and writing both the program
// output and the bitmap to out. It is the spike's live wiring: it proves the
// viewport coexists with the event loop without corrupting the bitmap. It
// returns on quit, surfacing a setup, run, or bitmap write error.
func RunUnderBubbleTea(src *image.RGBA, cols, rows int, in io.Reader, out io.Writer) error {
	m := NewBitmapModel(src, cols, rows, out)
	p := tea.NewProgram(
		&m,
		tea.WithoutRenderer(),
		tea.WithInput(in),
		tea.WithOutput(out),
	)
	if _, err := p.Run(); err != nil {
		return err
	}
	return m.Err()
}
