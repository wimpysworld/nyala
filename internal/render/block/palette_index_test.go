package block

import (
	"image/color"
	"testing"
)

// adaptiveTestLUT builds a full RGB555 table whose register is a function of the
// key, so a lookup through it is distinguishable from a lookup through the
// static perceptual table. The registers stay in the 16..255 budget range, which
// is the range the shared palette engine writes in its budget mode.
func adaptiveTestLUT() []uint8 {
	lut := make([]uint8, rgb555Entries)
	for key := range lut {
		lut[key] = uint8(16 + key%240) //nolint:gosec // the modulus bounds the value to 255.
	}
	return lut
}

// TestPaletteIndexMapsThroughTheAdaptiveTable proves an attached table decides
// the register and that the key is the same RGB555 key XTerm256 uses, so a table
// the shared palette engine built indexes correctly with no conversion.
func TestPaletteIndexMapsThroughTheAdaptiveTable(t *testing.T) {
	lut := adaptiveTestLUT()
	for _, c := range []color.RGBA{
		{A: 0xff},
		{R: 255, G: 255, B: 255, A: 0xff},
		{R: 12, G: 200, B: 91, A: 0xff},
		{R: 200, G: 7, B: 43, A: 0xff},
	} {
		key := uint16(c.R>>3)<<10 | uint16(c.G>>3)<<5 | uint16(c.B>>3)
		if got, want := PaletteIndex(lut, c), lut[key]; got != want {
			t.Errorf("PaletteIndex(%v) = %d, want %d", c, got, want)
		}
	}
}

// TestPaletteIndexFallsBackWithoutATable proves a nil or wrong-length table
// draws through the static perceptual table, which is the correct draw before
// the first quantisation.
func TestPaletteIndexFallsBackWithoutATable(t *testing.T) {
	short := make([]uint8, rgb555Entries-1)
	for _, c := range []color.RGBA{
		{A: 0xff},
		{R: 255, G: 255, B: 255, A: 0xff},
		{R: 12, G: 200, B: 91, A: 0xff},
	} {
		want := XTerm256(c)
		if got := PaletteIndex(nil, c); got != want {
			t.Errorf("nil table PaletteIndex(%v) = %d, want %d", c, got, want)
		}
		if got := PaletteIndex(short, c); got != want {
			t.Errorf("short table PaletteIndex(%v) = %d, want %d", c, got, want)
		}
	}
}
