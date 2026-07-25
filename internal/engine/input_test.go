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

// TestKeyStrokeParams asserts the full-stroke builder: Tab emits rawKeyDown
// then keyUp with the Tab key and virtual key code 9 and no char event, a
// shifted Tab carries the CDP Shift modifier on every event, Enter emits
// rawKeyDown, a char with text "\r", then keyUp with virtual key code 13, and
// an unmapped code returns nil so it stays on the KeyChar path.
func TestKeyStrokeParams(t *testing.T) {
	tab := keyStrokeParams("Tab", 0)
	if len(tab) != 2 {
		t.Fatalf("Tab stroke = %d events, want 2 (rawKeyDown, keyUp)", len(tab))
	}
	if tab[0].Type != "rawKeyDown" || tab[1].Type != "keyUp" {
		t.Fatalf("Tab stroke types = %q, %q, want rawKeyDown, keyUp", tab[0].Type, tab[1].Type)
	}
	for i, p := range tab {
		if p.Key != "Tab" || p.Code != "Tab" || p.WindowsVirtualKeyCode != 9 || p.NativeVirtualKeyCode != 9 {
			t.Fatalf("Tab event %d = key %q code %q vk %d/%d, want Tab/Tab/9/9",
				i, p.Key, p.Code, p.WindowsVirtualKeyCode, p.NativeVirtualKeyCode)
		}
		if p.Modifiers != 0 {
			t.Fatalf("Tab event %d modifiers = %d, want 0", i, p.Modifiers)
		}
	}

	shiftTab := keyStrokeParams("Tab", 8)
	for i, p := range shiftTab {
		if p.Modifiers != 8 {
			t.Fatalf("Shift+Tab event %d modifiers = %d, want 8 (CDP Shift)", i, p.Modifiers)
		}
	}

	enter := keyStrokeParams("Enter", 0)
	if len(enter) != 3 {
		t.Fatalf("Enter stroke = %d events, want 3 (rawKeyDown, char, keyUp)", len(enter))
	}
	if enter[0].Type != "rawKeyDown" || enter[1].Type != "char" || enter[2].Type != "keyUp" {
		t.Fatalf("Enter stroke types = %q, %q, %q, want rawKeyDown, char, keyUp",
			enter[0].Type, enter[1].Type, enter[2].Type)
	}
	if enter[1].Text != "\r" || enter[1].UnmodifiedText != "\r" {
		t.Fatalf("Enter char text = %q/%q, want \"\\r\"", enter[1].Text, enter[1].UnmodifiedText)
	}
	for i, p := range enter {
		if p.Key != "Enter" || p.WindowsVirtualKeyCode != 13 {
			t.Fatalf("Enter event %d = key %q vk %d, want Enter/13", i, p.Key, p.WindowsVirtualKeyCode)
		}
	}

	if got := keyStrokeParams("KeyA", 0); got != nil {
		t.Fatalf("unmapped code built a stroke: %v", got)
	}
}

// TestKeyStrokeParamsEditKeys asserts Backspace and Escape stroke as
// rawKeyDown then keyUp with their virtual key codes and no char event, so
// Chromium runs its own default action from the key down: Backspace edits the
// focused field and Escape dismisses page state.
func TestKeyStrokeParamsEditKeys(t *testing.T) {
	cases := []struct {
		code string
		vkey int64
	}{
		{"Backspace", 8},
		{"Escape", 27},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			got := keyStrokeParams(c.code, 0)
			if len(got) != 2 {
				t.Fatalf("%s stroke = %d events, want 2 (rawKeyDown, keyUp)", c.code, len(got))
			}
			if got[0].Type != "rawKeyDown" || got[1].Type != "keyUp" {
				t.Fatalf("%s stroke types = %q, %q, want rawKeyDown, keyUp", c.code, got[0].Type, got[1].Type)
			}
			for i, p := range got {
				if p.Key != c.code || p.Code != c.code {
					t.Fatalf("%s event %d = key %q code %q, want %s/%s", c.code, i, p.Key, p.Code, c.code, c.code)
				}
				if p.WindowsVirtualKeyCode != c.vkey || p.NativeVirtualKeyCode != c.vkey {
					t.Fatalf("%s event %d vk = %d/%d, want %d", c.code, i, p.WindowsVirtualKeyCode, p.NativeVirtualKeyCode, c.vkey)
				}
				if p.Text != "" {
					t.Fatalf("%s event %d carries text %q, want none", c.code, i, p.Text)
				}
			}
		})
	}
}

// TestPrintableStroke asserts the typed-text split: one printable ASCII
// character builds a keyDown/keyUp pair whose down carries the character as
// both text and unmodified text, so the page sees a real key press and
// Chromium generates the keypress that types it, while a multi-byte rune, a
// longer string, a control byte, and DEL build nothing, so those insert
// atomically through Input.insertText instead. The keyDown type is
// load-bearing: rawKeyDown would suppress Chromium's char synthesis and the
// character would never type.
func TestPrintableStroke(t *testing.T) {
	for _, text := range []string{"a", "Z", " ", "~", "/", "7"} {
		got := printableStroke(text)
		if len(got) != 2 {
			t.Fatalf("printableStroke(%q) = %d events, want 2 (keyDown, keyUp)", text, len(got))
		}
		if got[0].Type != "keyDown" || got[1].Type != "keyUp" {
			t.Fatalf("printableStroke(%q) types = %q, %q, want keyDown, keyUp", text, got[0].Type, got[1].Type)
		}
		if got[0].Text != text || got[0].UnmodifiedText != text {
			t.Fatalf("printableStroke(%q) down text = %q/%q, want %q", text, got[0].Text, got[0].UnmodifiedText, text)
		}
		for i, p := range got {
			if p.Key != text {
				t.Fatalf("printableStroke(%q) event %d key = %q, want %q", text, i, p.Key, text)
			}
		}
		if got[1].Text != "" {
			t.Fatalf("printableStroke(%q) keyUp carries text %q, want none", text, got[1].Text)
		}
	}
	for _, text := range []string{"", "é", "日", "ab", "\x00", "\x7f", "\n"} {
		if got := printableStroke(text); got != nil {
			t.Fatalf("printableStroke(%q) built a stroke, want nil for the insertText path", text)
		}
	}
}

// TestPrintableKeyIdentity asserts the layout-independent key identities the
// typed stroke reports: Space, the digits, and the letters carry their physical
// code and virtual key code (the upper-case ASCII value for a letter, so a
// lower-case a reports KeyA and 65), while layout-specific punctuation reports
// none rather than inventing a US-layout key the user may not have pressed.
func TestPrintableKeyIdentity(t *testing.T) {
	cases := []struct {
		c    byte
		code string
		vkey int64
	}{
		{' ', "Space", 32},
		{'0', "Digit0", 48},
		{'7', "Digit7", 55},
		{'a', "KeyA", 65},
		{'z', "KeyZ", 90},
		{'A', "KeyA", 65},
		{'Z', "KeyZ", 90},
		{'/', "", 0},
		{'~', "", 0},
		{'!', "", 0},
	}
	for _, c := range cases {
		code, vkey := printableKeyIdentity(c.c)
		if code != c.code || vkey != c.vkey {
			t.Fatalf("printableKeyIdentity(%q) = (%q, %d), want (%q, %d)", c.c, code, vkey, c.code, c.vkey)
		}
	}
}

// TestSendTextWithoutSessionIsInert asserts SendText drops the text when no
// browser session is attached rather than dispatching or panicking, matching
// the SendKey contract that a wedged or absent browser never parks the caller.
func TestSendTextWithoutSessionIsInert(t *testing.T) {
	e := New("")
	e.SendText("a")
	e.SendText("日本語")
	e.SendText("")

	e.mu.Lock()
	failures := e.inputFailures
	e.mu.Unlock()
	if failures != 0 {
		t.Fatalf("inputFailures = %d, want 0 (no dispatch without a session)", failures)
	}
}

// TestSendKeyEndThenHomeEdges asserts the Home and End edge scroll rides the
// held wheel-transport offset: End advances the target by edgeScrollPx (the
// page clamps at its real bottom), and a following Home negates the whole
// held target, landing it at exactly 0 beside the page's own top clamp. With
// ctx nil the dispatch is skipped and the held state alone moves, so the test
// runs without a browser.
func TestSendKeyEndThenHomeEdges(t *testing.T) {
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(120, 40) // Z = 1, whole-cell CSS height 16 divides edgeScrollPx

	e.SendKey(Key{Code: "End"})
	e.mu.Lock()
	afterEnd := e.scrollYTarget
	e.mu.Unlock()
	if afterEnd != edgeScrollPx {
		t.Fatalf("End scrollYTarget = %d, want %d", afterEnd, edgeScrollPx)
	}

	e.SendKey(Key{Code: "Home"})
	e.mu.Lock()
	y, target := e.scrollY, e.scrollYTarget
	e.mu.Unlock()
	if y != 0 || target != 0 {
		t.Fatalf("Home left scroll at (%d, %d), want (0, 0)", y, target)
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
