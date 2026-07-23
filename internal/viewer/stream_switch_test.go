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

// TestSwitchBackendOpOrder pins the Tab restart closure: when the engine row
// count moves the grid resize lands in the same closure, before SetBackend so
// the backend restart runs last at the final grid, and when the row count is
// unchanged no resize is issued.
func TestSwitchBackendOpOrder(t *testing.T) {
	t.Run("changed rows resize before backend switch", func(t *testing.T) {
		eng := &switchOpEngine{}
		switchBackendOp(BackendSixel, 80, 24, 23)(eng)
		want := []string{"Resize(80,23)", "SetBackend(sixel)"}
		if !reflect.DeepEqual(eng.calls(), want) {
			t.Fatalf("calls = %v; want %v", eng.calls(), want)
		}
	})
	t.Run("unchanged rows switch backend only", func(t *testing.T) {
		eng := &switchOpEngine{}
		switchBackendOp(BackendBlock, 80, 24, 24)(eng)
		want := []string{"SetBackend(block)"}
		if !reflect.DeepEqual(eng.calls(), want) {
			t.Fatalf("calls = %v; want %v", eng.calls(), want)
		}
	})
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
func (e *switchOpEngine) SendMouse(MouseEvent)        {}
func (e *switchOpEngine) Close() error                { return nil }

// TestTabResizesEngineGridUnderMultiplexer drives handleKey through Tab under
// tmux with Sixel active and asserts the engine grid drops the bottom row on
// the switch back to Sixel and returns to the full grid on the switch to
// Block, while the viewer State keeps the full terminal grid throughout.
func TestTabResizesEngineGridUnderMultiplexer(t *testing.T) {
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)
	ctl := newControl(eng, done)

	st := NewState(80, 24)
	st.mux = muxTmux
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	s := &streamSession{eng: eng, st: &st, ctl: ctl}

	// Sixel to Block: the engine grid returns to the full 24 rows.
	s.handleKey(keyEvent{b: keyTab})
	if st.Backend != BackendBlock {
		t.Fatalf("backend after first Tab = %v, want Block", st.Backend)
	}
	wantFirst := []string{"Resize(80,24)", "SetBackend(block)"}
	if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), wantFirst) }) {
		t.Fatalf("calls after Sixel-to-Block Tab = %v; want %v", eng.calls(), wantFirst)
	}

	// Block back to Sixel: the engine grid reserves the bottom row again.
	s.handleKey(keyEvent{b: keyTab})
	if st.Backend != BackendSixel {
		t.Fatalf("backend after second Tab = %v, want Sixel", st.Backend)
	}
	wantBoth := append(append([]string(nil), wantFirst...), "Resize(80,23)", "SetBackend(sixel)")
	if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), wantBoth) }) {
		t.Fatalf("calls after Block-to-Sixel Tab = %v; want %v", eng.calls(), wantBoth)
	}

	// The viewer's own grid (compose, overlay, badge) never shrinks.
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
	s.handleKey(keyEvent{b: keyTab})
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

// TestTabSwitchToSixelUnderMultiplexerClears pins the clear on the transition
// that flips the Sixel bottom-row reservation on: a Block-to-Sixel Tab under
// tmux must erase the screen so the reserved bottom row does not keep the last
// Block-rendered row beneath the rows-1 Sixel frame.
func TestTabSwitchToSixelUnderMultiplexerClears(t *testing.T) {
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
	s.firstDrawn.Store(true)
	s.handleKey(keyEvent{b: keyTab})
	if st.Backend != BackendSixel {
		t.Fatalf("backend after Tab = %v, want Sixel", st.Backend)
	}
	if !strings.Contains(out.String(), sgrReset+clearScreen) {
		t.Fatalf("Block-to-Sixel switch emitted no clear: %q", out.String())
	}
	// The engine grid drops the reserved bottom row in the same restart.
	want := []string{"Resize(80,23)", "SetBackend(sixel)"}
	if !waitFor(2*time.Second, func() bool { return reflect.DeepEqual(eng.calls(), want) }) {
		t.Fatalf("calls after Block-to-Sixel Tab = %v; want %v", eng.calls(), want)
	}
}

// TestTabSwitchEmitsNoClearWhenBackendUnchanged pins the shared rule's guard:
// a Tab that lands on the same backend (a one-backend cycle) and a Tab before
// the first frame (the loading cue is still up) emit no clear.
func TestTabSwitchEmitsNoClearWhenBackendUnchanged(t *testing.T) {
	t.Run("one-backend cycle", func(t *testing.T) {
		eng := &switchOpEngine{}
		done := make(chan struct{})
		defer close(done)
		ctl := newControl(eng, done)
		st := NewState(80, 24)
		st.SetEnabled([]Backend{BackendBlock})
		out := &bytes.Buffer{}
		s := &streamSession{eng: eng, st: &st, tty: out, ctl: ctl}
		s.firstDrawn.Store(true)
		s.handleKey(keyEvent{b: keyTab})
		if out.Len() != 0 {
			t.Fatalf("same-backend Tab wrote %q, want nothing", out.String())
		}
	})
	t.Run("before first frame", func(t *testing.T) {
		eng := &switchOpEngine{}
		done := make(chan struct{})
		defer close(done)
		ctl := newControl(eng, done)
		st := NewState(80, 24)
		st.SetEnabled([]Backend{BackendSixel, BackendBlock})
		out := &bytes.Buffer{}
		s := &streamSession{eng: eng, st: &st, tty: out, ctl: ctl}
		s.handleKey(keyEvent{b: keyTab})
		if out.Len() != 0 {
			t.Fatalf("pre-frame Tab wrote %q, want nothing", out.String())
		}
	})
}
