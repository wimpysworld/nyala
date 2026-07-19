// Package engine drives Chromium and emits rendered frames.
//
// ChromiumEngine streams a live screencast from headless Chromium over the
// Chrome DevTools Protocol and folds the crisp DOM text layer onto each frame.
package engine

// Key is a key event forwarded to the page. The engine maps it to a CDP input
// event; the modifier bits follow the CDP convention (Alt=1, Ctrl=2, Meta=4,
// Shift=8).
type Key struct {
	Text string // the text the key produces, if printable
	Code string // the physical key code
	Mod  int    // modifier bitmask
}

// MouseEvent is a mouse event forwarded to the page. The engine maps it to a
// CDP input event. Scroll uses the wheel deltas in CSS pixels.
type MouseEvent struct {
	X, Y           int // cursor position in CSS pixels
	DeltaX, DeltaY int // wheel deltas in CSS pixels
	Buttons        int // pressed-buttons mask (Left=1, Right=2, Middle=4)
	Mod            int // modifier bitmask
}
