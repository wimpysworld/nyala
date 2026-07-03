package engine

import "testing"

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
