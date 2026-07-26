package viewer

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestResizeCellPx pins the pure resize-time cell-pair decision: a changed
// winsize derivation moves the pair, an unchanged one reports no change, a
// failed derivation keeps the old pair (fail closed), and a forced
// NYALA_CELL_PX override never re-derives.
func TestResizeCellPx(t *testing.T) {
	current := cellPx{W: 8, H: 16}
	zoomed := cellPx{W: 10, H: 20}
	cases := []struct {
		name        string
		forced      bool
		derived     cellPx
		derivedOK   bool
		wantPx      cellPx
		wantChanged bool
	}{
		{name: "changed pair moves", derived: zoomed, derivedOK: true, wantPx: zoomed, wantChanged: true},
		{name: "unchanged pair reports no change", derived: current, derivedOK: true, wantPx: current, wantChanged: false},
		{name: "failed derivation keeps old pair", derived: cellPx{}, derivedOK: false, wantPx: current, wantChanged: false},
		{name: "forced override never re-derives", forced: true, derived: zoomed, derivedOK: true, wantPx: current, wantChanged: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			px, changed := resizeCellPx(tc.forced, current, tc.derived, tc.derivedOK)
			if px != tc.wantPx || changed != tc.wantChanged {
				t.Fatalf("resizeCellPx(%v, %v, %v, %v) = %v, %v; want %v, %v",
					tc.forced, current, tc.derived, tc.derivedOK, px, changed, tc.wantPx, tc.wantChanged)
			}
		})
	}
}

// resizeOpEngine records the engine calls resizeEngineOp makes, in order, so
// the test can assert a changed cell pair lands before the grid restart.
type resizeOpEngine struct {
	calls []string
}

func (e *resizeOpEngine) SetBackend(render.Backend) {}

func (e *resizeOpEngine) SetCellPx(w, h int) {
	e.calls = append(e.calls, fmt.Sprintf("SetCellPx(%d,%d)", w, h))
}

func (e *resizeOpEngine) Resize(cols, rows int) {
	e.calls = append(e.calls, fmt.Sprintf("Resize(%d,%d)", cols, rows))
}

func (e *resizeOpEngine) Navigate(string) error       { return nil }
func (e *resizeOpEngine) Frames() <-chan render.Frame { return nil }
func (e *resizeOpEngine) SendKey(string)              {}
func (e *resizeOpEngine) SendText(string)             {}
func (e *resizeOpEngine) SendPaste(string) bool       { return false }
func (e *resizeOpEngine) SendNav(NavAction)           {}
func (e *resizeOpEngine) CurrentURL() string          { return "" }
func (e *resizeOpEngine) OpenURL(string)              {}
func (e *resizeOpEngine) BrowserVersion() string      { return "" }
func (e *resizeOpEngine) Find(string)                 {}
func (e *resizeOpEngine) FindNext()                   {}
func (e *resizeOpEngine) FindPrev()                   {}
func (e *resizeOpEngine) FindClear()                  {}
func (e *resizeOpEngine) FindState() FindState        { return FindState{} }
func (e *resizeOpEngine) Selection() (string, bool)   { return "", false }
func (e *resizeOpEngine) SendMouse(MouseEvent)        {}
func (e *resizeOpEngine) Close() error                { return nil }

// TestResizeEngineOpOrder pins the restart closure: with a changed pair the
// engine sees SetCellPx before Resize inside one closure (so the one-slot
// drop-on-full control channel can never drop one without the other), and with
// an unchanged pair SetCellPx is not called.
func TestResizeEngineOpOrder(t *testing.T) {
	t.Run("changed pair sets cell px before resize", func(t *testing.T) {
		eng := &resizeOpEngine{}
		resizeEngineOp(cellPx{W: 10, H: 20}, true, 80, 24)(eng)
		want := []string{"SetCellPx(10,20)", "Resize(80,24)"}
		if !reflect.DeepEqual(eng.calls, want) {
			t.Fatalf("calls = %v; want %v", eng.calls, want)
		}
	})
	t.Run("unchanged pair resizes only", func(t *testing.T) {
		eng := &resizeOpEngine{}
		resizeEngineOp(cellPx{W: 8, H: 16}, false, 80, 24)(eng)
		want := []string{"Resize(80,24)"}
		if !reflect.DeepEqual(eng.calls, want) {
			t.Fatalf("calls = %v; want %v", eng.calls, want)
		}
	})
}
