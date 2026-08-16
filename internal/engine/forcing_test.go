package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/chromedp/cdproto/cdp"
)

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

// cdpCapture is a cdp.Executor that records the marshalled parameters of every
// command sent through it and answers each one successfully without a browser. It
// is the seam the isolated-world parameter tests use: the builder chain the
// production enable path composes is replayed against it, so the assertions read
// the wire payload Chromium would receive rather than a hand-copied struct.
type cdpCapture struct {
	method string
	params map[string]any
}

// Execute records the command method and its parameters as generic JSON, then
// reports success. It satisfies cdp.Executor.
func (c *cdpCapture) Execute(_ context.Context, method string, params, _ any) error {
	c.method = method
	c.params = nil
	if params == nil {
		return nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &c.params)
}

// TestLayoutShiftBindingScopesToIsolatedWorld proves the layout-shift binding
// registration carries the isolated world as its execution context name, so the
// binding is exposed only on that world's global and the page can never call or
// enumerate it. A dropped executionContextName would put the binding back on the
// page's own window, which is the exposure WW-223 removes. It runs the production
// builder enableLayoutShift itself uses, so the drop fails here.
func TestLayoutShiftBindingScopesToIsolatedWorld(t *testing.T) {
	rec := &cdpCapture{}
	ctx := cdp.WithExecutor(context.Background(), rec)
	if err := layoutShiftBindingParams().Do(ctx); err != nil {
		t.Fatalf("Runtime.addBinding: %v", err)
	}
	if rec.method != "Runtime.addBinding" {
		t.Errorf("method = %q, want Runtime.addBinding", rec.method)
	}
	if got := rec.params["name"]; got != layoutShiftBinding {
		t.Errorf("name = %v, want %q", got, layoutShiftBinding)
	}
	if got := rec.params["executionContextName"]; got != layoutShiftWorld {
		t.Errorf("executionContextName = %v, want %q", got, layoutShiftWorld)
	}
}

// TestLayoutShiftScriptInstallsIntoIsolatedWorld proves the observer's
// on-new-document registration carries the world name and runImmediately. The
// world name keeps the observer, its binding reference, and the generation counter
// off the page's window; runImmediately covers the already-loaded document, which
// is what replaced the old page-world live evaluate, so dropping it would leave the
// current page without an observer until the next navigation. It runs the
// production builder enableLayoutShift itself uses, so either drop fails here.
func TestLayoutShiftScriptInstallsIntoIsolatedWorld(t *testing.T) {
	rec := &cdpCapture{}
	ctx := cdp.WithExecutor(context.Background(), rec)
	script := buildLayoutShiftScript()
	if _, err := layoutShiftScriptParams(script).Do(ctx); err != nil {
		t.Fatalf("Page.addScriptToEvaluateOnNewDocument: %v", err)
	}
	if rec.method != "Page.addScriptToEvaluateOnNewDocument" {
		t.Errorf("method = %q, want Page.addScriptToEvaluateOnNewDocument", rec.method)
	}
	if got := rec.params["source"]; got != script {
		t.Errorf("source is not the built observer script; got %v", got)
	}
	if got := rec.params["worldName"]; got != layoutShiftWorld {
		t.Errorf("worldName = %v, want %q", got, layoutShiftWorld)
	}
	if got := rec.params["runImmediately"]; got != true {
		t.Errorf("runImmediately = %v, want true", got)
	}
}

// TestLayoutShiftDisableClearsHeldContext proves disableLayoutShift clears the held
// isolated-world execution context id, so a cycle back to the block backend cannot
// evaluate the walk against a dead context from the previous session. It drives the
// engine with no CDP context, the degraded path where every transport call fails:
// the id must still be cleared, because a stale id is worse than none (the walk
// guard turns a zero id into a clean error, while a stale one raises "Cannot find
// context" on every read).
func TestLayoutShiftDisableClearsHeldContext(t *testing.T) {
	e := &ChromiumEngine{}
	e.mu.Lock()
	e.layoutShiftCtxID = 42
	e.mu.Unlock()

	e.disableLayoutShift(context.Background())

	e.mu.Lock()
	got := e.layoutShiftCtxID
	e.mu.Unlock()
	if got != 0 {
		t.Errorf("layoutShiftCtxID = %d after disable, want 0", got)
	}
}
