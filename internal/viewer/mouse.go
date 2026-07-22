package viewer

import (
	"strconv"
	"strings"
)

// MouseAction classifies a decoded SGR mouse report the viewer forwards to
// the engine.
type MouseAction int

const (
	// MouseActionPress is a button press (SGR final M).
	MouseActionPress MouseAction = iota
	// MouseActionRelease is a button release (SGR final m).
	MouseActionRelease
	// MouseActionWheel is one wheel step; WheelDX and WheelDY carry the
	// direction. Wheel reports are press-only.
	MouseActionWheel
)

// MouseButton identifies the pointer button of a decoded press or release.
type MouseButton int

const (
	// MouseButtonLeft is the primary button (SGR button 0).
	MouseButtonLeft MouseButton = iota
	// MouseButtonMiddle is the wheel button (SGR button 1).
	MouseButtonMiddle
	// MouseButtonRight is the secondary button (SGR button 2).
	MouseButtonRight
)

// SGR mouse report bits on the button number b. Modifiers are bitmasks on top
// of the button bits, never exact values, so a modified press still decodes.
// sgrExtra marks buttons 8-11 (back, forward), which Nyala drops.
const (
	sgrModShift = 4
	sgrModAlt   = 8
	sgrModCtrl  = 16
	sgrMotion   = 32
	sgrWheel    = 64
	sgrExtra    = 128
)

// CDP modifier bits, the currency MouseEvent.Mod carries: Alt=1, Ctrl=2,
// Meta=4, Shift=8.
const (
	cdpModAlt   = 1
	cdpModCtrl  = 2
	cdpModShift = 8
)

// MouseEvent is one decoded SGR mouse report the viewer forwards to the
// engine over the Engine seam. The viewer forwards terminal-native cells; the
// engine owns the cell-to-CSS conversion.
type MouseEvent struct {
	// Action is press, release, or wheel.
	Action MouseAction
	// Button is the pressed or released button; ignored for wheel events.
	Button MouseButton
	// Mod carries CDP modifier bits (Alt=1, Ctrl=2, Meta=4, Shift=8),
	// translated from the SGR bits at decode.
	Mod int
	// Col and Row are 0-based cell coordinates, clamped non-negative at
	// decode; the engine clamps them to its grid.
	Col int
	Row int
	// WheelDX and WheelDY are the unit wheel step direction (-1, 0, or +1
	// per axis) for MouseActionWheel; up and left are negative.
	WheelDX int
	WheelDY int
}

// decodeSGRMouse parses one SGR mouse CSI body: '<' then "b;x;y" with
// 1-based coordinates, then final 'M' (press) or 'm' (release). Buttons and
// modifiers decode by bitmask on b. motion reports a recognised motion event
// (bit 32); the caller drops it (motion dispatch is deferred) so its bytes
// never leak as key input. ok is false for a body that is not a well-formed
// SGR mouse report the viewer handles: wrong shape, non-numeric parameters,
// buttons 8-11 (bit 128), a wheel release, or the buttonless b&3 == 3.
// Terminals can emit zero or negative coordinates; the decode clamps them
// non-negative and the engine clamps to the grid.
func decodeSGRMouse(body []byte) (ev MouseEvent, motion, ok bool) {
	n := len(body)
	if n < 2 || body[0] != '<' {
		return MouseEvent{}, false, false
	}
	final := body[n-1]
	if final != 'M' && final != 'm' {
		return MouseEvent{}, false, false
	}
	parts := strings.Split(string(body[1:n-1]), ";")
	if len(parts) != 3 {
		return MouseEvent{}, false, false
	}
	b, errB := strconv.Atoi(parts[0])
	x, errX := strconv.Atoi(parts[1])
	y, errY := strconv.Atoi(parts[2])
	if errB != nil || errX != nil || errY != nil || b < 0 {
		return MouseEvent{}, false, false
	}
	if b&sgrMotion != 0 {
		return MouseEvent{}, true, true
	}
	if b&sgrExtra != 0 {
		return MouseEvent{}, false, false
	}
	ev = MouseEvent{Col: max(x-1, 0), Row: max(y-1, 0), Mod: cdpModifiers(b)}
	if b&sgrWheel != 0 {
		if final != 'M' {
			return MouseEvent{}, false, false
		}
		ev.Action = MouseActionWheel
		switch b & 3 {
		case 0:
			ev.WheelDY = -1
		case 1:
			ev.WheelDY = 1
		case 2:
			ev.WheelDX = -1
		default:
			ev.WheelDX = 1
		}
		return ev, false, true
	}
	switch b & 3 {
	case 0:
		ev.Button = MouseButtonLeft
	case 1:
		ev.Button = MouseButtonMiddle
	case 2:
		ev.Button = MouseButtonRight
	default:
		return MouseEvent{}, false, false
	}
	ev.Action = MouseActionRelease
	if final == 'M' {
		ev.Action = MouseActionPress
	}
	return ev, false, true
}

// cdpModifiers translates the SGR modifier bits on b (Shift 4, Alt 8,
// Ctrl 16) into the CDP modifier bitmask (Alt=1, Ctrl=2, Meta=4, Shift=8).
func cdpModifiers(b int) int {
	mod := 0
	if b&sgrModShift != 0 {
		mod |= cdpModShift
	}
	if b&sgrModAlt != 0 {
		mod |= cdpModAlt
	}
	if b&sgrModCtrl != 0 {
		mod |= cdpModCtrl
	}
	return mod
}

// enableMouseReporting turns on button-event mouse tracking (mode 1002:
// press, release, and drag motion) with SGR encoding (mode 1006: decimal
// coordinates, distinct release finals). Stream writes it beside
// enterFullScreen, after the startup handshake, so mouse traffic cannot race
// the probe replies. The pair is identical on every terminal; multiplexers
// re-encode SGR cell reports pane-relative, so cells stay the one coordinate
// currency.
const enableMouseReporting = "\x1b[?1002h\x1b[?1006h"

// disableMouseReporting is the unconditional exit pair for
// enableMouseReporting. exitTerminal emits it on every teardown, so a leaked
// mouse mode cannot spray SGR reports into the shell.
const disableMouseReporting = "\x1b[?1002l\x1b[?1006l"
