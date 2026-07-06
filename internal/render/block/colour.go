// Package block implements the terminal block-cell renderer and its colour
// conversion. It maps RGBA pixels to xterm-256 indices in closed form and emits
// truecolour SGR sequences, ported from Carbonyl's xterm.rs.
//
// Channel order is RGBA throughout: the R, G, and B accessors of color.RGBA map
// to the red, green, and blue terminal channels in that order. CDP decodes to
// RGBA, so no BGRA swap happens here.
package block

import (
	"image/color"
	"strconv"
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
		step := min((int(r)-8)/10, 23)
		return clampByte(232 + step)
	}

	ri := cubeIndex(r)
	gi := cubeIndex(g)
	bi := cubeIndex(b)

	return clampByte(16 + 36*ri + 6*gi + bi)
}

// trueFGPrefix and trueBGPrefix are the truecolour foreground and background SGR
// prefixes (ESC [ 38;2; and ESC [ 48;2;). The RGB triple and trailing "m" follow.
const (
	trueFGPrefix = "\x1b[38;2;"
	trueBGPrefix = "\x1b[48;2;"
)

// appendTrueColor appends prefix + "R;G;Bm" to dst with strconv.AppendInt and
// returns the extended slice, building the SGR with no fmt reflection and no
// intermediate string. The alpha channel is ignored; channel order is RGBA.
func appendTrueColor(dst []byte, prefix string, c color.RGBA) []byte {
	dst = append(dst, prefix...)
	dst = strconv.AppendInt(dst, int64(c.R), 10)
	dst = append(dst, ';')
	dst = strconv.AppendInt(dst, int64(c.G), 10)
	dst = append(dst, ';')
	dst = strconv.AppendInt(dst, int64(c.B), 10)
	return append(dst, 'm')
}

// AppendTrueColorFG appends the foreground SGR that sets an exact 24-bit RGBA
// colour ("\x1b[38;2;R;G;Bm") to dst and returns the extended slice. It is the
// allocation-free build the differ folds into its reused per-frame buffer; the
// alpha channel is ignored.
func AppendTrueColorFG(dst []byte, c color.RGBA) []byte {
	return appendTrueColor(dst, trueFGPrefix, c)
}

// AppendTrueColorBG appends the background SGR that sets an exact 24-bit RGBA
// colour ("\x1b[48;2;R;G;Bm") to dst and returns the extended slice. It is the
// allocation-free build the differ folds into its reused per-frame buffer; the
// alpha channel is ignored.
func AppendTrueColorBG(dst []byte, c color.RGBA) []byte {
	return appendTrueColor(dst, trueBGPrefix, c)
}

// TrueColorFG returns the SGR sequence that sets the foreground to an exact
// 24-bit RGBA colour. The alpha channel is ignored. The form is
// "\x1b[38;2;R;G;Bm".
func TrueColorFG(c color.RGBA) string {
	return string(AppendTrueColorFG(nil, c))
}

// TrueColorBG returns the SGR sequence that sets the background to an exact
// 24-bit RGBA colour. The alpha channel is ignored. The form is
// "\x1b[48;2;R;G;Bm".
func TrueColorBG(c color.RGBA) string {
	return string(AppendTrueColorBG(nil, c))
}
