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
// cell height. At 158x86 with cell 16x33 and layout width 1280 the CSS cell height
// is fractional (~16.7 px), so a raw 40 px line step must snap to the nearest whole
// cell, not to the raw 40. This is the R2 fix: the DOM scroll the snapshot and the
// screencast metadata report is always a whole number of cells.
func TestWheelSnapsVerticalToWholeCellBlock(t *testing.T) {
	e := New("")
	e.SetCellPx(16, 33)
	e.SetLayoutWidth(1280)
	e.Resize(158, 86) // cols*cellW = 2528, Z = 2528/1280 = 1.975; fractional CSS cell

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
	e.SetLayoutWidth(1280)
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
	e.SetLayoutWidth(1280)
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

// TestSendKeyPageStepCSSPixels asserts a Page key steps one CSS-pixel viewport
// height, not one device-pixel height. The wheel transport speaks CSS pixels, so
// on a wide block grid where the layout scale Z exceeds 1 the raw rows*cellH device
// height must be divided by Z. With ctx nil dispatchWheel advances the engine's held
// scroll offset without a browser, so reading e.scrollY after SendKey observes the
// step the transport applied.
func TestSendKeyPageStepCSSPixels(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.SetLayoutWidth(1280)
	e.Resize(240, 48) // cols*cellW = 1920, so Z = 1920/1280 = 1.5

	// rows*cellH = 48*16 = 768 device px; CSS step = round(768 / 1.5) = 512.
	const wantStep = 512
	e.SendKey(Key{Code: "PageDown"})

	e.mu.Lock()
	got := e.scrollY
	e.mu.Unlock()
	if got != wantStep {
		t.Errorf("PageDown scrollY = %d, want %d (rows*cellH / Z in CSS px)", got, wantStep)
	}
}

// TestSendKeyPageStepUnscaled asserts the page step equals the raw device height
// when Z is 1 (a grid at or under the target layout width), so the CSS-pixel divide
// is a no-op there.
func TestSendKeyPageStepUnscaled(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.SetLayoutWidth(1280)
	e.Resize(120, 40) // cols*cellW = 960 <= 1280, so Z = 1

	const wantStep = 40 * 16 // rows*cellH, no divide
	e.SendKey(Key{Code: "PageDown"})

	e.mu.Lock()
	got := e.scrollY
	e.mu.Unlock()
	if got != wantStep {
		t.Errorf("PageDown scrollY = %d, want %d (rows*cellH at Z=1)", got, wantStep)
	}
}
