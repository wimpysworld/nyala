package viewer

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/palette"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestSwitchBackendOpOrder pins the restart closure: a backend switch never
// moves the engine grid (the bottom-row bar reservation applies to every
// backend), so the closure issues SetBackend alone.
func TestSwitchBackendOpOrder(t *testing.T) {
	eng := &switchOpEngine{}
	switchBackendOp(BackendSixel)(eng)
	want := []string{"SetBackend(sixel)"}
	if !reflect.DeepEqual(eng.calls(), want) {
		t.Fatalf("calls = %v; want %v", eng.calls(), want)
	}
}

// switchOpEngine records the engine calls a control closure makes, in order,
// under a mutex so the Tab wiring test can read them across the control
// goroutine.
type switchOpEngine struct {
	mu  sync.Mutex
	got []string
}

func (e *switchOpEngine) SetBackend(b render.Backend) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, fmt.Sprintf("SetBackend(%s)", b))
}

func (e *switchOpEngine) Resize(cols, rows int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, fmt.Sprintf("Resize(%d,%d)", cols, rows))
}

func (e *switchOpEngine) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.got...)
}

func (e *switchOpEngine) SetCellPx(int, int)          {}
func (e *switchOpEngine) Navigate(string) error       { return nil }
func (e *switchOpEngine) Frames() <-chan render.Frame { return nil }
func (e *switchOpEngine) SendKey(string)              {}
func (e *switchOpEngine) SendText(string)             {}
func (e *switchOpEngine) SendPaste(string) bool       { return false }
func (e *switchOpEngine) SendNav(NavAction)           {}
func (e *switchOpEngine) CurrentURL() string          { return "" }
func (e *switchOpEngine) CurrentTitle() string        { return "" }
func (e *switchOpEngine) PointerCursor() string       { return "" }

func (e *switchOpEngine) LoadState() render.LoadState { return render.LoadState{} }
func (e *switchOpEngine) OpenURL(string)              {}
func (e *switchOpEngine) BrowserVersion() string      { return "" }
func (e *switchOpEngine) Find(string)                 {}
func (e *switchOpEngine) FindNext()                   {}
func (e *switchOpEngine) FindPrev()                   {}
func (e *switchOpEngine) FindClear()                  {}
func (e *switchOpEngine) FindState() FindState        { return FindState{} }
func (e *switchOpEngine) Selection() (string, bool)   { return "", false }
func (e *switchOpEngine) SendMouse(MouseEvent)        {}
func (e *switchOpEngine) Close() error                { return nil }

// TestTabKeepsEngineGridUnderMultiplexer drives handleKey through Tab under
// tmux with Sixel active and asserts a backend switch never resizes the
// engine grid: the bottom-row bar reservation applies to every backend, so
// only SetBackend restarts land, while the viewer State keeps the full
// terminal grid throughout.
func TestTabKeepsEngineGridUnderMultiplexer(t *testing.T) {
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)
	ctl := newControl(eng, done)

	st := NewState(80, 24)
	st.mux = muxTmux
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	s := &streamSession{eng: eng, st: &st, ctl: ctl, tty: &bytes.Buffer{}}

	// Sixel to Block, then back: no Resize rides either switch. Wait out the
	// first restart before the second Tab, because the one-slot control
	// channel drops a queued duplicate.
	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendBlock {
		t.Fatalf("backend after first Tab = %v, want Block", st.Backend)
	}
	wantFirst := []string{"SetBackend(block)"}
	if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), wantFirst) }) {
		t.Fatalf("calls after the first Tab = %v; want %v", eng.calls(), wantFirst)
	}
	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendSixel {
		t.Fatalf("backend after second Tab = %v, want Sixel", st.Backend)
	}
	want := []string{"SetBackend(block)", "SetBackend(sixel)"}
	if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), want) }) {
		t.Fatalf("calls after the two Tabs = %v; want %v", eng.calls(), want)
	}

	// The viewer's own grid (bar row included) never shrinks.
	if st.Cols != 80 || st.Rows != 24 {
		t.Fatalf("viewer grid = %dx%d, want 80x24", st.Cols, st.Rows)
	}
}

// TestTabSwitchAwayFromSixelClearsAndResetsDiff pins the switch-seam clear
// that removes a stale Sixel raster: a Sixel-to-Block Tab must erase the
// screen (SGR reset first, so background colour erase fills with the default)
// before the first Block frame, and must invalidate the Block differ so that
// frame full-repaints instead of skipping cells matching the previous Block
// stint that the raster still covers.
func TestTabSwitchAwayFromSixelClearsAndResetsDiff(t *testing.T) {
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)
	ctl := newControl(eng, done)

	st := NewState(8, 4)
	st.mux = muxTmux
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	// Seed the differ with a previous Block stint, so an unchanged frame emits
	// nothing, then return to Sixel as the active backend before the Tab.
	blockFrame := render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	st.Backend = BackendBlock
	if err := st.RenderStreamFrame(blockFrame, io.Discard); err != nil {
		t.Fatalf("seed Block diff: %v", err)
	}
	var unchanged bytes.Buffer
	if err := st.RenderStreamFrame(blockFrame, &unchanged); err != nil {
		t.Fatalf("render unchanged Block frame: %v", err)
	}
	if unchanged.Len() != 0 {
		t.Fatalf("unchanged Block frame wrote %d bytes, want 0", unchanged.Len())
	}
	st.Backend = BackendSixel

	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: &st, tty: out, ctl: ctl, diff: diff}
	s.firstDrawn.Store(true)
	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendBlock {
		t.Fatalf("backend after Tab = %v, want Block", st.Backend)
	}

	got := out.String()
	clearAt := strings.Index(got, clearScreen)
	if clearAt < 0 {
		t.Fatalf("Sixel-to-Block switch emitted no clear: %q", got)
	}
	if !strings.HasSuffix(got[:clearAt], sgrReset) {
		t.Fatalf("Sixel-to-Block clear erases with a stale SGR still active: %q", got)
	}
	var repainted bytes.Buffer
	if err := st.RenderStreamFrame(blockFrame, &repainted); err != nil {
		t.Fatalf("render Block frame after switch: %v", err)
	}
	if repainted.Len() == 0 {
		t.Fatal("Block diff was not reset on the switch away from Sixel")
	}
}

// TestTabSwitchClearsAndRepaintsBar pins the shared switch rule: every
// backend change erases the screen (SGR reset first) and repaints the bottom
// bar after the clear, and the restart carries no grid resize because the
// bottom-row bar reservation applies to every backend. A pre-frame switch
// clears too: the bar repaint makes a pre-frame clear safe.
func TestTabSwitchClearsAndRepaintsBar(t *testing.T) {
	for _, drawn := range []bool{true, false} {
		name := "after first frame"
		if !drawn {
			name = "before first frame"
		}
		t.Run(name, func(t *testing.T) {
			eng := &switchOpEngine{}
			done := make(chan struct{})
			defer close(done)
			ctl := newControl(eng, done)

			st := NewState(80, 24)
			st.mux = muxTmux
			st.SetEnabled([]Backend{BackendSixel, BackendBlock})
			st.Backend = BackendBlock

			out := &bytes.Buffer{}
			s := &streamSession{eng: eng, st: &st, tty: out, ctl: ctl}
			s.firstDrawn.Store(drawn)
			s.handleKey(keyEvent{act: actionCycleBackend})
			if st.Backend != BackendSixel {
				t.Fatalf("backend after Tab = %v, want Sixel", st.Backend)
			}
			got := out.String()
			if !strings.Contains(got, sgrReset+clearScreen) {
				t.Fatalf("Block-to-Sixel switch emitted no clear: %q", got)
			}
			if !strings.Contains(got, "\x1b[24;1H") {
				t.Fatalf("switch clear did not repaint the bar on the bottom row: %q", got)
			}
			if !strings.Contains(got, "│ 256a │ ☰") {
				t.Fatalf("backend switch bar %q missing the Sixel colour badge", got)
			}
			if drawn && !strings.Contains(got, "│ 🅢  │ 256a │ ☰") {
				t.Fatalf("backend switch bar %q missing the Sixel render glyph", got)
			}
			want := []string{"SetBackend(sixel)"}
			if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), want) }) {
				t.Fatalf("calls after Block-to-Sixel Tab = %v; want %v", eng.calls(), want)
			}
		})
	}
}

// TestTabSwitchEmitsNoClearWhenBackendUnchanged pins the shared rule's guard:
// a Tab that lands on the same backend (a one-backend cycle) emits no clear.
func TestTabSwitchEmitsNoClearWhenBackendUnchanged(t *testing.T) {
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)
	ctl := newControl(eng, done)
	st := NewState(80, 24)
	st.SetEnabled([]Backend{BackendBlock})
	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: &st, tty: out, ctl: ctl}
	s.firstDrawn.Store(true)
	s.handleKey(keyEvent{act: actionCycleBackend})
	if out.Len() != 0 {
		t.Fatalf("same-backend Tab wrote %q, want nothing", out.String())
	}
}

// TestAltPTogglesAndRecomposesBlock checks both palette-policy directions in
// the RGB and grey modes. Each change clears before the current bar, restarts
// Block, and makes the first adaptive frame fit and write its palette first.
func TestAltPTogglesAndRecomposesBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state func(*testing.T) (*State, *image.RGBA)
		count func(*State) int
	}{
		{
			name: "RGB",
			state: func(t *testing.T) (*State, *image.RGBA) {
				st, _ := paletteTestState(t, true)
				return st, blockFrame(t, st)
			},
			count: func(st *State) int { return st.paletteEngine.RequantCount },
		},
		{
			name: "grey",
			state: func(t *testing.T) (*State, *image.RGBA) {
				st := greyTestState(true)
				return st, greyTestImage(t, st)
			},
			count: func(st *State) int { return st.greyEngine.RequantCount },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, src := tc.state(t)
			if err := st.RenderStreamFrame(render.Frame{Img: src}, io.Discard); err != nil {
				t.Fatalf("seed the adaptive frame: %v", err)
			}
			beforeRefit := tc.count(st)
			eng := &switchOpEngine{}
			done := make(chan struct{})
			defer close(done)
			out := &bytes.Buffer{}
			s := &streamSession{eng: eng, st: st, tty: out, ctl: newControl(eng, done), diff: st.diff}

			s.handleKey(keyEvent{act: actionTogglePalette})
			if !st.staticPaletteSelected {
				t.Fatal("Alt+P did not select the static palette")
			}
			prefix := sgrReset + clearScreen + palette.OSC104
			got := out.String()
			if !strings.HasPrefix(got, prefix) {
				t.Fatalf("static toggle output = %q, want prefix %q", got, prefix)
			}
			barAt := strings.Index(got, fmt.Sprintf("\x1b[%d;1H", st.Rows))
			if barAt < len(prefix) {
				t.Fatalf("static toggle bar precedes the palette release: %q", got)
			}
			wantCalls := []string{"SetBackend(block)"}
			if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), wantCalls) }) {
				t.Fatalf("calls after the static toggle = %v, want %v", eng.calls(), wantCalls)
			}

			var static bytes.Buffer
			if err := st.RenderStreamFrame(render.Frame{Img: src}, &static); err != nil {
				t.Fatalf("render the static frame: %v", err)
			}
			if static.Len() == 0 {
				t.Fatal("the static toggle did not reset the Block differ")
			}

			out.Reset()
			s.handleKey(keyEvent{act: actionTogglePalette})
			if st.staticPaletteSelected {
				t.Fatal("the second Alt+P did not select the adaptive palette")
			}
			got = out.String()
			if !strings.HasPrefix(got, sgrReset+clearScreen) || strings.Contains(got, palette.OSC104) {
				t.Fatalf("adaptive toggle output = %q, want reset and clear without OSC 104", got)
			}
			wantCalls = append(wantCalls, "SetBackend(block)")
			if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), wantCalls) }) {
				t.Fatalf("calls after both toggles = %v, want %v", eng.calls(), wantCalls)
			}

			var adaptive bytes.Buffer
			if err := st.RenderStreamFrame(render.Frame{Img: src}, &adaptive); err != nil {
				t.Fatalf("render the first adaptive frame: %v", err)
			}
			if !strings.HasPrefix(adaptive.String(), osc4Introducer) {
				t.Fatalf("first adaptive frame did not write its palette before cells: %q", adaptive.String())
			}
			if gotCount := tc.count(st); gotCount != beforeRefit+1 {
				t.Fatalf("adaptive fit count = %d, want %d", gotCount, beforeRefit+1)
			}
		})
	}
}

// TestAltPRapidDoubleToggle keeps the second press when the first restart is
// still pending. The final policy is adaptive and each change repaints once.
func TestAltPRapidDoubleToggle(t *testing.T) {
	st, _ := paletteTestState(t, true)
	if err := st.RenderStreamFrame(render.Frame{Img: blockFrame(t, st)}, io.Discard); err != nil {
		t.Fatalf("seed the adaptive frame: %v", err)
	}
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)
	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: st, tty: out, ctl: newControl(eng, done), diff: st.diff}

	s.handleKey(keyEvent{act: actionTogglePalette})
	s.handleKey(keyEvent{act: actionTogglePalette})

	if st.staticPaletteSelected {
		t.Fatal("a rapid double toggle did not return to adaptive")
	}
	if got := strings.Count(out.String(), sgrReset+clearScreen); got != 2 {
		t.Fatalf("rapid double toggle repaints = %d, want 2", got)
	}
	if got := strings.Count(out.String(), palette.OSC104); got != 1 {
		t.Fatalf("rapid double toggle palette releases = %d, want 1", got)
	}
	if !waitFor(2*time.Second, func() bool { return len(eng.calls()) > 0 }) {
		t.Fatal("rapid double toggle scheduled no Block restart")
	}
}

// TestAltPPaletteToggleUnsupportedIsSilent checks every runtime gate. A
// rejected toggle changes no policy, emits no bytes, and schedules no restart.
func TestAltPPaletteToggleUnsupportedIsSilent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*State)
	}{
		{"without OSC 4", func(st *State) { st.paletteQueryOK = false }},
		{"outside Block", func(st *State) { st.Backend = BackendSixel }},
		{"in truecolour", func(st *State) { st.Colour = ColourTrue }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(8, 4)
			st.Backend = BackendBlock
			st.Colour = ColourXTerm256
			st.paletteQueryOK = true
			tc.prepare(&st)
			eng := &switchOpEngine{}
			done := make(chan struct{})
			defer close(done)
			out := &bytes.Buffer{}
			s := &streamSession{eng: eng, st: &st, tty: out, ctl: newControl(eng, done)}

			s.handleKey(keyEvent{act: actionTogglePalette})

			if st.staticPaletteSelected || out.Len() != 0 || len(eng.calls()) != 0 {
				t.Fatalf("unsupported Alt+P changed state or output: static %t, output %q, calls %v",
					st.staticPaletteSelected, out.String(), eng.calls())
			}
		})
	}
}
