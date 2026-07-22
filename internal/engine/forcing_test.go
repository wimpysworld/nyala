package engine

import "testing"

// TestReconcileForcingAction pins the pure font-forcing reconcile decision,
// including the refresh arm the stale-line-height fix adds: forcing that stays
// active across a cell-pair change must replace the stylesheet, not keep it.
func TestReconcileForcingAction(t *testing.T) {
	cases := []struct {
		name                   string
		desired, active, stale bool
		want                   forcingAction
	}{
		{"fresh block enable", true, false, false, forcingEnable},
		{"switch off block disables", false, true, false, forcingDisable},
		{"active with a fresh sheet keeps", true, true, false, forcingKeep},
		{"active with a stale sheet refreshes", true, true, true, forcingRefresh},
		{"inactive graphics keeps", false, false, false, forcingKeep},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reconcileForcingAction(c.desired, c.active, c.stale); got != c.want {
				t.Errorf("reconcileForcingAction(%v, %v, %v) = %v, want %v", c.desired, c.active, c.stale, got, c.want)
			}
		})
	}
}

// TestFontForcingStaleOnCellPairChange drives the forcingReconcileInputs seam
// through the trigger the stale-line-height fault needs: a mid-session cell-pair
// change (a terminal font change from cell 8x16 to 16x33). Under the density
// design Z moves with cellW (1 to 2), so the CSS cell width stays at the 8 px
// anchor while the CSS cell height moves 16 to 16.5, exactly the baked
// line-height the registered stylesheet would otherwise keep while calibration
// rewrites only the font-size. The inputs must report the sheet stale and the
// reconcile must refresh it.
func TestFontForcingStaleOnCellPairChange(t *testing.T) {
	e := &ChromiumEngine{backend: backendBlock, cols: 158, rows: 86, cellW: 8, cellH: 16}
	e.fontForcingID = "sheet-1"
	e.mu.Lock()
	e.forcedCellW, e.forcedCellH = e.snapshotCellPx()
	e.mu.Unlock()

	if _, _, _, _, stale := e.forcingReconcileInputs(); stale {
		t.Fatal("unchanged cell pair reported stale; the guard would refresh every sync")
	}

	e.SetCellPx(16, 33)
	desired, active, cellW, cellH, stale := e.forcingReconcileInputs()
	if !desired || !active {
		t.Fatalf("desired = %v, active = %v, want both true (block backend, sheet registered)", desired, active)
	}
	if cellW != 8 {
		t.Errorf("CSS cell width = %g, want 8 (the density anchor holds it across the font change)", cellW)
	}
	if cellH != 16.5 {
		t.Errorf("CSS cell height = %g, want 16.5 (cellH 33 over Z 2)", cellH)
	}
	if !stale {
		t.Fatal("cell-pair change not reported stale; the baked line-height would trail Z")
	}
	if got := reconcileForcingAction(desired, active, stale); got != forcingRefresh {
		t.Errorf("reconcile action = %v, want forcingRefresh", got)
	}
}
