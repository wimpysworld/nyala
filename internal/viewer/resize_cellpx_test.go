package viewer

import "testing"

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
