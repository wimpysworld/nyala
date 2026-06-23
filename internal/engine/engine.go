// Package engine drives a web page and emits rendered frames.
//
// Engine is the seam between the frame source and the terminal viewer. Stage 3
// fills it with ChromiumEngine, which streams a live screencast from headless
// Chromium over the Chrome DevTools Protocol and folds the crisp DOM text layer
// onto each frame. Future engines implement the same interface with no other
// code change.
package engine

import "github.com/wimpysworld/nyala/internal/render"

// Engine drives one page and emits decoded frames. Frames carries the bitmap
// with its crisp text already synced on, so one channel delivers both.
type Engine interface {
	// Navigate loads url and starts emitting frames for it.
	Navigate(url string) error
	// Resize sets the terminal grid in cells; the engine recomputes the
	// capture size and restarts the stream.
	Resize(cols, rows int)
	// Frames returns the channel of decoded frames, each an RGBA bitmap with
	// Text already synced on.
	Frames() <-chan render.Frame
	// SendKey forwards a key event to the page.
	SendKey(k Key)
	// SendMouse forwards a mouse event to the page.
	SendMouse(m MouseEvent)
	// Close tears down the engine and the browser process.
	Close() error
}

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
