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

// rgb555Key packs an RGBA colour into the 15-bit RGB555 key both the generated
// perceptual table and an adaptive palette table are indexed by: the top five
// bits of red, then green, then blue. The alpha channel is ignored.
func rgb555Key(c color.RGBA) uint16 {
	return uint16(c.R>>3)<<10 | uint16(c.G>>3)<<5 | uint16(c.B>>3)
}

// rgb555Entries is the entry count of a full RGB555 lookup table, one per
// 15-bit key.
const rgb555Entries = 1 << 15

// XTerm256 converts an RGBA colour to a perceptually nearest canonical
// xterm-256 index through a generated 15-bit lookup table. The result always
// lies in indices 16..255, avoiding the terminal-configurable system colours.
//
// The alpha channel is ignored; only R, G, and B drive the index.
func XTerm256(c color.RGBA) uint8 {
	return xterm256PerceptualLUT[rgb555Key(c)]
}

// PaletteIndex converts an RGBA colour to a terminal register through an
// adaptive palette's RGB555 lookup table. The table must hold absolute register
// numbers, which is what the shared palette engine builds. A table of any other
// length, including a nil one, falls back to XTerm256, so a renderer that
// carries no palette yet still draws. The alpha channel is ignored.
func PaletteIndex(lut []uint8, c color.RGBA) uint8 {
	if len(lut) != rgb555Entries {
		return XTerm256(c)
	}
	return lut[rgb555Key(c)]
}

// XTermGrey converts an RGBA colour to the nearest fixed xterm-256 greyscale
// ramp entry. The result always lies in indices 232..255. Alpha is ignored.
func XTermGrey(c color.RGBA) uint8 {
	const (
		base = 8 * 256
		step = 10 * 256
	)

	n := (luma(c) - base + step/2) / step
	n = max(0, min(n, 23))
	return uint8(232 + n) //nolint:gosec // n is clamped to 0..23.
}

// XTerm256RGBA returns the canonical RGBA value of an xterm-256 palette index.
// Indices 0..15 return the canonical ANSI colours through ANSI16RGBA, 16..231
// the 6x6x6 colour cube on levels 0, 95, 135, 175, 215, 255, and 232..255 the
// greyscale ramp 8..238 in steps of 10. Terminals can redefine indices 0..15;
// the canonical values are the reference model.
func XTerm256RGBA(idx uint8) color.RGBA {
	if idx < 16 {
		return ANSI16RGBA(idx)
	}
	if idx >= 232 {
		v := 8 + 10*(idx-232)
		return color.RGBA{R: v, G: v, B: v, A: 0xff}
	}
	n := int(idx) - 16
	return color.RGBA{R: cubeLevel(n / 36), G: cubeLevel(n / 6 % 6), B: cubeLevel(n % 6), A: 0xff}
}

// cubeLevel maps a 0..5 xterm colour-cube step to its channel value.
func cubeLevel(i int) uint8 {
	if i == 0 {
		return 0
	}
	return uint8(55 + 40*i) //nolint:gosec // cube steps are 0..5, so the value is 95..255
}

// ansi16RGBA holds the canonical RGBA values of the 16 ANSI indices on the
// xterm reference palette, the model XTerm256RGBA serves indices 0..15 from.
var ansi16RGBA = [16]color.RGBA{
	{A: 0xff},
	{R: 128, A: 0xff},
	{G: 128, A: 0xff},
	{R: 128, G: 128, A: 0xff},
	{B: 128, A: 0xff},
	{R: 128, B: 128, A: 0xff},
	{G: 128, B: 128, A: 0xff},
	{R: 192, G: 192, B: 192, A: 0xff},
	{R: 128, G: 128, B: 128, A: 0xff},
	{R: 255, A: 0xff},
	{G: 255, A: 0xff},
	{R: 255, G: 255, A: 0xff},
	{B: 255, A: 0xff},
	{R: 255, B: 255, A: 0xff},
	{G: 255, B: 255, A: 0xff},
	{R: 255, G: 255, B: 255, A: 0xff},
}

// ANSI16RGBA returns the canonical RGBA value of an ANSI index 0..15. An index
// past 15 returns opaque black, the palette's own index 0.
func ANSI16RGBA(idx uint8) color.RGBA {
	if idx >= 16 {
		return color.RGBA{A: 0xff}
	}
	return ansi16RGBA[idx]
}

// XTermGreyRGBA returns the canonical RGBA value of an xterm greyscale ramp
// index 232..255. An index outside the ramp returns opaque black.
func XTermGreyRGBA(idx uint8) color.RGBA {
	if idx < 232 {
		return color.RGBA{A: 0xff}
	}
	v := 8 + 10*(idx-232)
	return color.RGBA{R: v, G: v, B: v, A: 0xff}
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
