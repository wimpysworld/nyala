package viewer

import (
	"fmt"
	"image"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestEngineRows pins the pure engine-grid row decision: Sixel under tmux or
// zellij reserves the bottom terminal row, every other backend-multiplexer
// pair keeps the full grid, and a grid below two rows never shrinks.
func TestEngineRows(t *testing.T) {
	cases := []struct {
		name    string
		backend Backend
		mux     multiplexerKind
		rows    int
		want    int
	}{
		{"sixel under tmux reserves bottom row", BackendSixel, muxTmux, 24, 23},
		{"sixel under zellij reserves bottom row", BackendSixel, muxZellij, 24, 23},
		{"block under tmux keeps full grid", BackendBlock, muxTmux, 24, 24},
		{"block under zellij keeps full grid", BackendBlock, muxZellij, 24, 24},
		{"kitty under tmux keeps full grid", BackendKitty, muxTmux, 24, 24},
		{"sixel on native terminal keeps full grid", BackendSixel, muxNone, 24, 24},
		{"block on native terminal keeps full grid", BackendBlock, muxNone, 24, 24},
		{"sixel under screen keeps full grid", BackendSixel, muxScreen, 24, 24},
		{"sixel under herdr keeps full grid", BackendSixel, muxHerdr, 24, 24},
		{"two rows reserve to one", BackendSixel, muxTmux, 2, 1},
		{"one row never shrinks", BackendSixel, muxTmux, 1, 1},
		{"zero rows pass through", BackendSixel, muxZellij, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineRows(tc.backend, tc.mux, tc.rows); got != tc.want {
				t.Fatalf("engineRows(%v, %v, %d) = %d, want %d",
					tc.backend, tc.mux, tc.rows, got, tc.want)
			}
		})
	}
}

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

// TestFitSixelToGridHonoursBottomRowReservation pins the reconciliation
// between fitSixelToGrid and the engine-grid reservation: an engine-sized
// (rows-1) frame under tmux passes through unchanged rather than being
// upscaled back over the reserved row, and a stale full-height frame is
// rescaled down to the reserved extent.
func TestFitSixelToGridHonoursBottomRowReservation(t *testing.T) {
	st := NewState(8, 4)
	st.mux = muxTmux
	st.Backend = BackendSixel
	gridW, gridH := 8*st.W, 3*st.H // engine grid: rows-1

	engineSized := image.NewRGBA(image.Rect(0, 0, gridW, gridH))
	if got := st.fitSixelToGrid(engineSized); got != engineSized {
		t.Fatalf("engine-sized frame was rescaled: bounds %v, want unchanged %v",
			got.Bounds(), engineSized.Bounds())
	}

	fullHeight := image.NewRGBA(image.Rect(0, 0, gridW, 4*st.H))
	got := st.fitSixelToGrid(fullHeight)
	if got.Bounds().Dx() != gridW || got.Bounds().Dy() != gridH {
		t.Fatalf("full-height frame fit = %v, want %dx%d (reserved bottom row)",
			got.Bounds(), gridW, gridH)
	}
}
