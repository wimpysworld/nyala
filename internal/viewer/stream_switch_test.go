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
func (e *switchOpEngine) SendNav(NavAction)           {}
func (e *switchOpEngine) CurrentURL() string          { return "" }
func (e *switchOpEngine) OpenURL(string)              {}
func (e *switchOpEngine) BrowserVersion() string      { return "" }
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
