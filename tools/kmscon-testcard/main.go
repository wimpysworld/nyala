// Command kmscon-testcard prints a quadrant test card to the terminal for the
// owner to eyeball inter-row bleed on a real KMSCON TTY. It is owner-verified,
// not a CI gate: it draws to stdout and asks the operator to look, so it cannot
// run headless. Run it via `just kmscon-testcard` on a KMSCON console.
//
// The card draws the ten quadrant block glyphs U+2596..U+259F, each on its own
// pair of contrasting backgrounds, then a half-block floor row U+2584. On a clean
// terminal each glyph fills its quadrants exactly with the foreground colour and
// the cell rows abut with no gap; bleed shows as a bright or dark seam between
// rows. The owner reads the card and confirms the rows do not bleed.
package main

import (
	"bufio"
	"fmt"
	"os"
)

// quadrantGlyphs is the contiguous quadrant block set U+2596..U+259F the card
// draws, in code-point order. These are the glyphs the quadrant scheme emits, so
// the card exercises the exact set a real render uses.
var quadrantGlyphs = []rune{
	0x2596, // quadrant lower left
	0x2597, // quadrant lower right
	0x2598, // quadrant upper left
	0x2599, // quadrant upper left, lower left, lower right
	0x259A, // quadrant upper left and lower right
	0x259B, // quadrant upper left, upper right, lower left
	0x259C, // quadrant upper left, upper right, lower right
	0x259D, // quadrant upper right
	0x259E, // quadrant upper right and lower left
	0x259F, // quadrant upper right, lower left, lower right
}

// lowerHalfBlock is the U+2584 lower-half-block glyph the floor row draws, the
// fallback the renderer uses when a font lacks the full quadrant set.
const lowerHalfBlock = 0x2584

// rgb is one 24-bit colour for the truecolour SGR sequences the card emits.
type rgb struct{ r, g, b uint8 }

// pairs lists the foreground/background colour pairs each glyph row is drawn on.
// The contrasting pairs make a seam between rows show as a visible bright or dark
// line, which is what the owner checks for.
var pairs = []struct {
	name   string
	fg, bg rgb
}{
	{"white on black", rgb{0xFF, 0xFF, 0xFF}, rgb{0x00, 0x00, 0x00}},
	{"black on white", rgb{0x00, 0x00, 0x00}, rgb{0xFF, 0xFF, 0xFF}},
	{"yellow on blue", rgb{0xFF, 0xFF, 0x00}, rgb{0x00, 0x00, 0xCC}},
	{"cyan on red", rgb{0x00, 0xFF, 0xFF}, rgb{0xCC, 0x00, 0x00}},
}

// fgSGR returns the truecolour foreground SGR sequence for c.
func fgSGR(c rgb) string {
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
}

// bgSGR returns the truecolour background SGR sequence for c.
func bgSGR(c rgb) string {
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.r, c.g, c.b)
}

// reset clears all SGR attributes so a row never leaks colour into the next.
const reset = "\x1b[0m"

// cellsPerGlyph is how many adjacent cells each glyph repeats across, so a seam
// runs long enough to spot. A single cell is too short to read a faint bleed.
const cellsPerGlyph = 8

func main() {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	fmt.Fprintln(w, "KMSCON quadrant test card. Look for a bright or dark seam between rows.")
	fmt.Fprintln(w, "Clean rows abut with no gap; a seam is inter-row bleed.")
	fmt.Fprintln(w)

	for _, p := range pairs {
		fmt.Fprintf(w, "%s:\n", p.name)
		// Two abutting rows of each glyph so a seam between them is visible.
		for range 2 {
			fmt.Fprint(w, fgSGR(p.fg), bgSGR(p.bg))
			for _, g := range quadrantGlyphs {
				for range cellsPerGlyph {
					fmt.Fprint(w, string(g))
				}
			}
			fmt.Fprintln(w, reset)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "half-block floor (U+2584):")
	floor := pairs[0]
	for range 2 {
		fmt.Fprint(w, fgSGR(floor.fg), bgSGR(floor.bg))
		for range len(quadrantGlyphs) * cellsPerGlyph {
			fmt.Fprint(w, string(rune(lowerHalfBlock)))
		}
		fmt.Fprintln(w, reset)
	}
}
