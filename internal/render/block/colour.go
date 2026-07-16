// Package block implements the terminal block-cell renderer and its colour
// conversion. It maps RGBA pixels to xterm-256 indices through a generated
// perceptual lookup table and emits truecolour SGR sequences.
//
// Channel order is RGBA throughout: the R, G, and B accessors of color.RGBA map
// to the red, green, and blue terminal channels in that order. CDP decodes to
// RGBA, so no BGRA swap happens here.
package block

import (
	_ "embed"
	"image/color"
	"strconv"
)

//go:generate go run generate_xterm256_lut.go

// xterm256PerceptualLUT maps a 15-bit RGB key to the nearest canonical xterm
// colour from indices 16..255 by CIEDE2000 distance. The generated table is
// immutable, adds no start-up work, and avoids configurable indices 0..15.
//
//go:embed xterm256_perceptual_lut.bin
var xterm256PerceptualLUT string

// decByte holds the decimal text of each byte value 0..255, with no leading
// zeros, so a truecolour SGR component appends as a direct string copy rather
// than a per-call strconv digit build.
var decByte = buildDecByte()

func buildDecByte() [256]string {
	var t [256]string
	for i := range t {
		t[i] = strconv.Itoa(i)
	}
	return t
}

// XTerm256 converts an RGBA colour to a perceptually nearest canonical
// xterm-256 index through a generated 15-bit lookup table. The result always
// lies in indices 16..255, avoiding the terminal-configurable system colours.
//
// The alpha channel is ignored; only R, G, and B drive the index.
func XTerm256(c color.RGBA) uint8 {
	key := uint16(c.R>>3)<<10 | uint16(c.G>>3)<<5 | uint16(c.B>>3)
	return xterm256PerceptualLUT[key]
}

// trueFGPrefix and trueBGPrefix are the truecolour foreground and background SGR
// prefixes (ESC [ 38;2; and ESC [ 48;2;). The RGB triple and trailing "m" follow.
const (
	trueFGPrefix = "\x1b[38;2;"
	trueBGPrefix = "\x1b[48;2;"
)

// appendTrueColor appends prefix + "R;G;Bm" to dst from the precomputed decByte
// table and returns the extended slice, building the SGR with no fmt reflection,
// no per-call digit build, and no intermediate string. The alpha channel is
// ignored; channel order is RGBA.
func appendTrueColor(dst []byte, prefix string, c color.RGBA) []byte {
	dst = append(dst, prefix...)
	dst = append(dst, decByte[c.R]...)
	dst = append(dst, ';')
	dst = append(dst, decByte[c.G]...)
	dst = append(dst, ';')
	dst = append(dst, decByte[c.B]...)
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

// appendTrueColors appends one SGR that sets both exact 24-bit colours. The
// foreground and background parameter groups share one CSI introducer and one
// final byte. Alpha is ignored.
func appendTrueColors(dst []byte, fg, bg color.RGBA) []byte {
	dst = append(dst, trueFGPrefix...)
	dst = append(dst, decByte[fg.R]...)
	dst = append(dst, ';')
	dst = append(dst, decByte[fg.G]...)
	dst = append(dst, ';')
	dst = append(dst, decByte[fg.B]...)
	dst = append(dst, ";48;2;"...)
	dst = append(dst, decByte[bg.R]...)
	dst = append(dst, ';')
	dst = append(dst, decByte[bg.G]...)
	dst = append(dst, ';')
	dst = append(dst, decByte[bg.B]...)
	return append(dst, 'm')
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
