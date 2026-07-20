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
