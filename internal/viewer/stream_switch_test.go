package viewer

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
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
