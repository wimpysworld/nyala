// Package block implements the terminal block-cell renderer and its colour
// conversion. It maps RGBA pixels to xterm-256 indices in closed form and emits
// truecolour SGR sequences, ported from Carbonyl's xterm.rs.
//
// Channel order is RGBA throughout: the R, G, and B accessors of color.RGBA map
// to the red, green, and blue terminal channels in that order. CDP decodes to
// RGBA, so no BGRA swap happens here.
package block

import (
	"fmt"
	"image/color"
)

// cubeIndex projects one 8-bit channel onto the 0..5 cube axis. It mirrors
// Carbonyl's xterm.rs: the first break sits below the 0/95 midpoint, then each
// further step lands on the midpoint between adjacent cube levels.
func cubeIndex(c uint8) int {
	switch {
	case c < 48:
		return 0
	case c < 115:
		return 1
	default:
		return int((c - 35) / 40)
	}
}

// XTerm256 converts an RGBA colour to its closest xterm-256 index in closed
// form, O(1) per pixel with no nearest-colour search. Pure-grey inputs
// (R == G == B) map onto the 24-step grey ramp (indices 232..255), not the
// colour cube, except the ramp endpoints that the cube renders exactly. Every
// other colour projects onto the 6x6x6 cube (indices 16..231).
//
// The alpha channel is ignored; only R, G, and B drive the index.
func XTerm256(c color.RGBA) uint8 {
	r, g, b := c.R, c.G, c.B

	if r == g && g == b {
		// Grey ramp branch. The ramp runs 8, 18, ... 238 in steps of 10 over
		// indices 232..255. Values outside the ramp fall back to the nearest
		// cube grey (black 16, white 231).
		if r < 8 {
			return 16
		}
		if r > 248 {
			return 231
		}
		step := (int(r) - 8) / 10
		if step > 23 {
			step = 23
		}
		return clampByte(232 + step)
	}

	ri := cubeIndex(r)
	gi := cubeIndex(g)
	bi := cubeIndex(b)

	return clampByte(16 + 36*ri + 6*gi + bi)
}

// TrueColorFG returns the SGR sequence that sets the foreground to an exact
// 24-bit RGBA colour. The alpha channel is ignored. The form is
// "\x1b[38;2;R;G;Bm".
func TrueColorFG(c color.RGBA) string {
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.R, c.G, c.B)
}

// TrueColorBG returns the SGR sequence that sets the background to an exact
// 24-bit RGBA colour. The alpha channel is ignored. The form is
// "\x1b[48;2;R;G;Bm".
func TrueColorBG(c color.RGBA) string {
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.R, c.G, c.B)
}
