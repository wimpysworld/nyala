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
	// MouseActionMove is pointer motion (SGR bit 32). Button says which kind:
	// a held button makes it a drag, the reports a drag is made of, and the
	// engine reports that button as the drag's pressed button;
	// MouseButtonNone makes it a hover, which mode 1003 any-event tracking
	// delivers whenever the pointer moves with nothing held.
	MouseActionMove
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
	// MouseButtonBack is a side back button (SGR buttons 8 and 10). The
	// viewer dispatches it as history navigation and never forwards it.
	MouseButtonBack
	// MouseButtonForward is a side forward button (SGR buttons 9 and 11).
	// The viewer dispatches it as history navigation and never forwards it.
	MouseButtonForward
	// MouseButtonNone is the no-button value hover motion carries: the
	// buttonless SGR report (b&3 == 3) with the motion bit 32 set, which mode
	// 1003 any-event tracking delivers. It comes last because MouseButtonLeft
	// is the iota zero value a bare MouseEvent{} relies on. No press or
	// release ever carries it.
	MouseButtonNone
)

// SGR mouse report bits on the button number b. Modifiers are bitmasks on top
// of the button bits, never exact values, so a modified press still decodes.
// sgrExtra marks buttons 8-11 (back, forward), which the viewer dispatches
// as history navigation.
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
// modifiers decode by bitmask on b. Motion (bit 32) decodes as
// MouseActionMove carrying the modifiers and either the held button, the
// reports a drag is made of, or MouseButtonNone for the buttonless report
// (b&3 == 3) mode 1003 any-event tracking delivers as a hover. Motion with a
// side button held is dropped, as no drag of the page. The side buttons 8-11
// (bit 128) decode as MouseButtonBack (even) and MouseButtonForward (odd) for
// history navigation. ok is false for a body that is not a well-formed SGR
// mouse report the viewer handles: wrong shape, non-numeric parameters, a
// wheel release, or a buttonless b&3 == 3 report without the motion bit,
// which is a press or release of no button and so malformed. Terminals can
// emit zero or negative coordinates; the decode clamps them non-negative and
// the engine clamps to the grid.
func decodeSGRMouse(body []byte) (ev MouseEvent, ok bool) {
	n := len(body)
	if n < 2 || body[0] != '<' {
		return MouseEvent{}, false
	}
	final := body[n-1]
	if final != 'M' && final != 'm' {
		return MouseEvent{}, false
	}
	parts := strings.Split(string(body[1:n-1]), ";")
	if len(parts) != 3 {
		return MouseEvent{}, false
	}
	b, errB := strconv.Atoi(parts[0])
	x, errX := strconv.Atoi(parts[1])
	y, errY := strconv.Atoi(parts[2])
	if errB != nil || errX != nil || errY != nil || b < 0 {
		return MouseEvent{}, false
	}
	motion := b&sgrMotion != 0
	ev = MouseEvent{Col: max(x-1, 0), Row: max(y-1, 0), Mod: cdpModifiers(b)}
	if b&sgrExtra != 0 {
		// A side button never drags the page: it is a history request, taken
		// on its press alone.
		if motion {
			return MouseEvent{}, false
		}
		ev.Button = MouseButtonBack
		if b&1 != 0 {
			ev.Button = MouseButtonForward
		}
		ev.Action = MouseActionRelease
		if final == 'M' {
			ev.Action = MouseActionPress
		}
		return ev, true
	}
	if b&sgrWheel != 0 {
		if final != 'M' || motion {
			return MouseEvent{}, false
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
		return ev, true
	}
	switch b & 3 {
	case 0:
		ev.Button = MouseButtonLeft
	case 1:
		ev.Button = MouseButtonMiddle
	case 2:
		ev.Button = MouseButtonRight
	default:
		// No button. Only motion can mean it, as a hover under mode 1003; a
		// press or release of no button is malformed.
		if !motion {
			return MouseEvent{}, false
		}
		ev.Button = MouseButtonNone
	}
	switch {
	case motion:
		ev.Action = MouseActionMove
	case final == 'M':
		ev.Action = MouseActionPress
	default:
		ev.Action = MouseActionRelease
	}
	return ev, true
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
// press, release, and drag motion), then any-event tracking (mode 1003, which
// adds buttonless motion so hover reports arrive and CSS hover reaches the
// page), with SGR encoding (mode 1006: decimal coordinates, distinct release
// finals). 1002 comes before 1003 deliberately: a terminal holds one
// mouse-target state, so the last target it accepts wins, and one that
// ignores 1003 keeps drag rather than losing mouse reporting altogether.
// PuTTY before 0.79 and Chromebook hterm drop mouse reporting entirely when
// asked for 1003 alone. Stream writes the set beside enterFullScreen, after
// the startup handshake, so mouse traffic cannot race the probe replies. The
// escapes are identical on every terminal; multiplexers re-encode SGR cell
// reports pane-relative, so cells stay the one coordinate currency.
const enableMouseReporting = "\x1b[?1002h\x1b[?1003h\x1b[?1006h"

// disableMouseReporting is the unconditional exit set for
// enableMouseReporting. exitTerminal emits it on every teardown, so a leaked
// mouse mode cannot spray SGR reports into the shell. On a terminal holding
// one mouse-target state, resetting either target turns reporting off and the
// second reset is a harmless no-op; both go out to keep the enable and
// disable pair symmetric.
const disableMouseReporting = "\x1b[?1002l\x1b[?1003l\x1b[?1006l"
