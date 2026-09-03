package palette

import (
	"image/color"
	"strconv"
)

// OSC104 is the bare OSC 104 sequence, which restores every terminal palette
// entry to its configured value. Bare means no parameters, so one write undoes
// every AppendOSC4 write whatever entries it set. Writing it when nothing was
// set is harmless. The terminator is ST, never BEL.
const OSC104 = "\x1b]104\x1b\\"

// osc4Introducer opens a palette-set sequence. Every entry follows as a
// ";index;spec" pair, so a chunk of N entries needs one introducer, not N.
const osc4Introducer = "\x1b]4"

// MaxOSC4Entries is the largest number of palette entries one OSC 4 sequence
// carries. Seven entries give fifteen OSC parameters: one for the leading "4"
// and two per entry. Fifteen is the largest odd count that fits the cap of
// sixteen parameters that the vte parser behind Alacritty and Rio applies.
// That parser truncates the parameter list at the cap and then rejects an even
// count outright, so one oversized sequence sets no register at all rather than
// setting the first few. A short final chunk stays safe, because every chunk of
// N entries gives an odd count of 2N+1. Do not raise this value.
const MaxOSC4Entries = 7

// hexDigits indexes the lower-case hex digit for a nibble. Terminals accept
// either case; lower-case matches the form terminals themselves report.
const hexDigits = "0123456789abcdef"

// AppendOSC4 appends the OSC 4 sequences that set terminal palette entry base+i
// to entry i of the palette, and returns the extended slice. It writes one
// sequence per MaxOSC4Entries entries, in ascending register order, so a
// 240-entry palette becomes 35 sequences rather than one. The form of each is
// "OSC 4 ; c1 ; spec1 ; c2 ; spec2 ; ... ST", where each spec is "rgb:RR/GG/BB"
// with two hex digits per channel. The terminator is ST, never BEL. The caller
// supplies the buffer, so a buffer with spare capacity makes the write
// allocation-free. An empty palette appends nothing, so a frame that reuses its
// palette writes no palette bytes at all. The caller keeps base and
// base+len(entries)-1 within 0..255; the emitter writes the index it is given.
func AppendOSC4(dst []byte, entries color.Palette, base int) []byte {
	for start := 0; start < len(entries); start += MaxOSC4Entries {
		end := min(start+MaxOSC4Entries, len(entries))
		dst = append(dst, osc4Introducer...)
		for i, entry := range entries[start:end] {
			r, g, b, _ := entry.RGBA()
			dst = append(dst, ';')
			dst = strconv.AppendInt(dst, int64(base+start+i), 10)
			dst = append(dst, ";rgb:"...)
			dst = appendHex8(dst, r>>8)
			dst = append(dst, '/')
			dst = appendHex8(dst, g>>8)
			dst = append(dst, '/')
			dst = appendHex8(dst, b>>8)
		}
		dst = append(dst, "\x1b\\"...)
	}
	return dst
}

// appendHex8 appends the low 8 bits of v as two lower-case hex digits and
// returns the extended slice. It takes a uint32 because color.Color reports
// 16-bit channels, whose top byte the caller drops.
func appendHex8(dst []byte, v uint32) []byte {
	return append(dst, hexDigits[v>>4&0x0f], hexDigits[v&0x0f])
}
