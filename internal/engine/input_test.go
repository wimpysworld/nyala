package engine

import (
	"math"
	"testing"
)

// snapshotCellHeight is the CSS-pixel cell height the quantiser snaps against,
// recomputed here so the whole-cell tests assert against the same divisor the
// transport uses rather than a hand-copied literal.
func snapshotCellHeight(e *ChromiumEngine) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, cellH := e.snapshotCellPx()
	return cellH
}

// TestWheelSnapsVerticalToWholeCellBlock asserts a fractional-cell line scroll on
// the block backend lands the held vertical offset on a whole multiple of the CSS
// cell height. At 158x86 with cell 16x33 the block density scale is Z = 2, so the
// CSS cell height is fractional (16.5 px) and a raw 40 px line step must snap to
// the nearest whole cell, not to the raw 40. The DOM scroll reported by the
// snapshot and screencast metadata must always be a whole number of cells.
func TestWheelSnapsVerticalToWholeCellBlock(t *testing.T) {
	e := New("")
	e.SetCellPx(16, 33)
	e.Resize(158, 86) // cell 16 over the 8 px density anchor: Z = 2; fractional CSS cell

	cellH := snapshotCellHeight(e)
	want := int(math.Round(math.Round(float64(wheelLineStepPx)/cellH) * cellH))
	e.SendKey(Key{Code: "ArrowDown"})

	e.mu.Lock()
	got := e.scrollY
	e.mu.Unlock()
	if got != want {
		t.Errorf("ArrowDown scrollY = %d, want %d (nearest whole CSS cell)", got, want)
	}
	if want == wheelLineStepPx {
		t.Fatalf("test geometry is not fractional: snapped step equals raw %d", wheelLineStepPx)
	}
}

// TestWheelSnapAccumulatesWithoutDrift asserts repeated small scrolls snap the
// accumulated target, not each raw delta, so the held offset tracks the true intent
// and does not drift. Snapping each 40 px step and summing would drift from the
// accumulated-target snap; this asserts the transport uses the accumulated target.
func TestWheelSnapAccumulatesWithoutDrift(t *testing.T) {
	e := New("")
	e.SetCellPx(16, 33)
	e.Resize(158, 86)

	cellH := snapshotCellHeight(e)
	const steps = 5
	for range steps {
		e.SendKey(Key{Code: "ArrowDown"})
	}

	target := steps * wheelLineStepPx
	want := int(math.Round(math.Round(float64(target)/cellH) * cellH))
	e.mu.Lock()
	got := e.scrollY
	e.mu.Unlock()
	if got != want {
		t.Errorf("accumulated scrollY = %d, want %d (accumulated-target snap)", got, want)
	}

	perStepDrift := steps * int(math.Round(math.Round(float64(wheelLineStepPx)/cellH)*cellH))
	if perStepDrift == want {
		t.Fatalf("test geometry cannot distinguish accumulated snap from per-step snap")
	}
}

// TestWheelHorizontalUnquantised asserts horizontal scroll is left untouched by the
// vertical whole-cell quantiser: a raw horizontal delta advances scrollX by the full
// amount on the block backend.
func TestWheelHorizontalUnquantised(t *testing.T) {
	e := New("")
	e.SetCellPx(16, 33)
	e.Resize(158, 86)

	e.SendKey(Key{Code: "ArrowRight"})

	e.mu.Lock()
	gotX, gotY := e.scrollX, e.scrollY
	e.mu.Unlock()
	if gotX != wheelLineStepPx {
		t.Errorf("ArrowRight scrollX = %d, want %d (horizontal unquantised)", gotX, wheelLineStepPx)
	}
	if gotY != 0 {
		t.Errorf("ArrowRight scrollY = %d, want 0 (horizontal must not move vertical)", gotY)
	}
}

// TestWheelNonBlockAlignsTarget asserts a scroll off the block backend keeps
// scrollYTarget aligned with scrollY, so no stale whole-cell snap residue survives
// to correct a later block scroll.
func TestWheelNonBlockAlignsTarget(t *testing.T) {
	e := New("")
	e.SetCellPx(16, 33)
	e.Resize(158, 86)
	e.SetBackend(backendSixel)

	e.SendKey(Key{Code: "ArrowDown"})

	e.mu.Lock()
	gotY, gotTarget := e.scrollY, e.scrollYTarget
	e.mu.Unlock()
	if gotY != wheelLineStepPx {
		t.Errorf("non-block ArrowDown scrollY = %d, want %d (unquantised)", gotY, wheelLineStepPx)
	}
	if gotTarget != gotY {
		t.Errorf("non-block scrollYTarget = %d, want %d (aligned with scrollY)", gotTarget, gotY)
	}
}

// TestWheelBackendSwitchNoCorrectiveJump asserts a block, non-block, block sequence
// dispatches only the new delta on block re-entry, not a corrective jump back to a
// stale snap. The non-block scroll realigns the target with scrollY, so the first
// re-entry block scroll accumulates from the current offset and its held step equals
// one snapped whole cell, not that plus a residual correction.
func TestWheelBackendSwitchNoCorrectiveJump(t *testing.T) {
	e := New("")
	e.SetCellPx(16, 33)
	e.Resize(158, 86)

	cellH := snapshotCellHeight(e)
	step := int(math.Round(math.Round(float64(wheelLineStepPx)/cellH) * cellH))

	// Scroll on block, snapping to a whole cell that differs from the raw step.
	e.SendKey(Key{Code: "ArrowDown"})

	// Switch to a graphics backend and scroll unquantised.
	e.SetBackend(backendSixel)
	e.SendKey(Key{Code: "ArrowDown"})

	e.mu.Lock()
	beforeY := e.scrollY
	e.mu.Unlock()

	// Return to block and scroll once more.
	e.SetBackend(backendBlock)
	e.SendKey(Key{Code: "ArrowDown"})

	e.mu.Lock()
	afterY := e.scrollY
	e.mu.Unlock()

	delta := afterY - beforeY
	// The re-entry step accumulates a raw wheelLineStepPx onto the aligned target,
	// then snaps, so the held delta is within one cell of the snapped line step, never
	// a large corrective jump.
	if delta < step-int(math.Ceil(cellH)) || delta > step+int(math.Ceil(cellH)) {
		t.Errorf("block re-entry delta = %d, want near %d (no corrective jump)", delta, step)
	}
}

// TestWheelCentreCSSPixels asserts the wheel dispatch position is in CSS
// pixels: the device-pixel viewport centre divides by the layout scale Z, the
// same divide the page-step delta takes. At the Ghostty graphics geometry
// (cell 26x53, Z = 1.625) the raw device centre would land outside the CSS
// viewport, so the divide is load-bearing there; at Z 1 it is a no-op.
func TestWheelCentreCSSPixels(t *testing.T) {
	cx, cy := wheelCentre(156, 43, 26, 53, 1.625)
	if wantCx := 2028.0 / 1.625; math.Abs(cx-wantCx) > layoutScaleEps {
		t.Errorf("wheelCentre cx = %g, want %g (device centre / Z)", cx, wantCx)
	}
	if wantCy := 1139.5 / 1.625; math.Abs(cy-wantCy) > layoutScaleEps {
		t.Errorf("wheelCentre cy = %g, want %g (device centre / Z)", cy, wantCy)
	}

	cx, cy = wheelCentre(80, 24, 8, 16, 1)
	if cx != 320 || cy != 192 {
		t.Errorf("wheelCentre at Z 1 = (%g, %g), want (320, 192) unchanged", cx, cy)
	}
}

// TestSendKeyPageStepCSSPixels asserts a Page key steps one CSS-pixel viewport
// height, not one device-pixel height. The wheel transport speaks CSS pixels, so
// on a wide block grid where the layout scale Z exceeds 1 the raw rows*cellH device
// height must be divided by Z. With ctx nil dispatchWheel advances the engine's held
// scroll offset without a browser, so reading e.scrollY after SendKey observes the
// step the transport applied.
func TestSendKeyPageStepCSSPixels(t *testing.T) {
	e := New("")
	e.SetCellPx(16, 32)
	e.Resize(240, 48) // cell 16 over the 8 px density anchor, so Z = 2

	// rows*cellH = 48*32 = 1536 device px; CSS step = round(1536 / 2) = 768.
	const wantStep = 768
	e.SendKey(Key{Code: "PageDown"})

	e.mu.Lock()
	got := e.scrollY
	e.mu.Unlock()
	if got != wantStep {
		t.Errorf("PageDown scrollY = %d, want %d (rows*cellH / Z in CSS px)", got, wantStep)
	}
}

// TestSendKeyPageStepUnscaled asserts the page step equals the raw device height
// when Z is 1 (an 8 px cell matches the density anchor), so the CSS-pixel divide
// is a no-op there.
func TestSendKeyPageStepUnscaled(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(120, 40) // cell 8 matches the 8 px density anchor, so Z = 1

	const wantStep = 40 * 16 // rows*cellH, no divide
	e.SendKey(Key{Code: "PageDown"})

	e.mu.Lock()
	got := e.scrollY
	e.mu.Unlock()
	if got != wantStep {
		t.Errorf("PageDown scrollY = %d, want %d (rows*cellH at Z=1)", got, wantStep)
	}
}
