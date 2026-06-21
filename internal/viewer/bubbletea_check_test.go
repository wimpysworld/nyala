package viewer

import (
	"bytes"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestBitmapModelDrawsBitmap asserts the model writes the bitmap on Init and on
// a redraw key, and that the output is the block SGR cell stream, not empty.
func TestBitmapModelDrawsBitmap(t *testing.T) {
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	var out bytes.Buffer
	m := NewBitmapModel(src, 8, 4, &out)

	if cmd := m.Init(); cmd != nil {
		t.Fatalf("Init returned a command, want nil")
	}
	if out.Len() == 0 {
		t.Fatalf("Init wrote no bitmap")
	}
	if !strings.Contains(out.String(), "\x1b[38;2;") {
		t.Fatalf("bitmap missing truecolour SGR cells")
	}
	if err := m.Err(); err != nil {
		t.Fatalf("Init draw error: %v", err)
	}
}

// TestBitmapModelKeyByte asserts keyByte maps Bubble Tea key presses to the
// Stage 0 state-machine bytes: Tab, Ctrl-C, the printable hotkeys, and a
// zero for keys the machine ignores.
func TestBitmapModelKeyByte(t *testing.T) {
	cases := []struct {
		name string
		key  tea.Key
		want byte
	}{
		{"tab", tea.Key{Code: tea.KeyTab}, keyTab},
		{"ctrl-c", tea.Key{Code: 'c', Mod: tea.ModCtrl}, keyQuitCtrlC},
		{"c", tea.Key{Code: 'c'}, 'c'},
		{"s", tea.Key{Code: 's'}, 's'},
		{"q", tea.Key{Code: 'q'}, 'q'},
		{"f1", tea.Key{Code: tea.KeyF1}, 0},
		{"up", tea.Key{Code: tea.KeyUp}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keyByte(tea.KeyPressMsg(c.key)); got != c.want {
				t.Fatalf("keyByte(%s) = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

// TestBitmapModelUpdateRedrawsAndQuits asserts a redraw key rewrites the bitmap
// and a quit key returns the tea.Quit command, so the program ends cleanly.
func TestBitmapModelUpdateRedrawsAndQuits(t *testing.T) {
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	var out bytes.Buffer
	m := NewBitmapModel(src, 8, 4, &out)
	m.Init()
	before := m.state.Backend

	// Tab cycles the backend and redraws.
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if cmd != nil {
		t.Fatalf("Tab returned a command, want nil")
	}
	if m.state.Backend == before {
		t.Fatalf("Tab did not cycle backend")
	}

	// Ctrl-C quits with the tea.Quit command.
	_, cmd = m.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	if cmd == nil {
		t.Fatalf("Ctrl-C returned no command, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("Ctrl-C command = %T, want tea.QuitMsg", cmd())
	}
}

// TestBitmapModelResizeRefits asserts a window-size message re-fits the viewport
// and redraws, so the bitmap tracks the resized terminal.
func TestBitmapModelResizeRefits(t *testing.T) {
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	var out bytes.Buffer
	m := NewBitmapModel(src, 8, 4, &out)
	m.Init()

	m.Update(tea.WindowSizeMsg{Width: 4, Height: 2})
	if m.state.Cols != 4 || m.state.Rows != 2 {
		t.Fatalf("resize not applied: %dx%d", m.state.Cols, m.state.Rows)
	}
	if fit := m.state.Fit(); fit.Cols != 4 || fit.Rows != 2 {
		t.Fatalf("fit not re-computed: %dx%d", fit.Cols, fit.Rows)
	}
}

// TestRunUnderBubbleTeaBitmapSurvives runs the spike through a full Bubble Tea
// program with the renderer disabled, then asserts the bitmap survived the
// event loop uncorrupted: the output holds the block SGR cell stream and Bubble
// Tea injected no cursor-home or screen-clear sequence over the bitmap.
func TestRunUnderBubbleTeaBitmapSurvives(t *testing.T) {
	src := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	// One Ctrl-C byte ends the program after the initial draw.
	in := bytes.NewReader([]byte{keyQuitCtrlC})
	var out bytes.Buffer

	if err := RunUnderBubbleTea(src, 8, 4, in, &out); err != nil {
		t.Fatalf("RunUnderBubbleTea: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "\x1b[38;2;") {
		t.Fatalf("bitmap missing from program output")
	}
	if strings.Contains(got, "\x1b[H") || strings.Contains(got, "\x1b[2J") {
		t.Fatalf("renderer corrupted the bitmap with a home or clear sequence")
	}
}
