// Command shaded-testcard prints the Shaded Block test card to the terminal
// for the owner to record real-terminal evidence for the Symbols for Legacy
// Computing shade glyphs. It is owner-verified, not a CI gate: it draws to
// stdout and asks the operator to look, so it cannot run headless. Run it via
// `just shaded-testcard`.
//
// The card prints the U+2584 half-block control, then every Shaded candidate
// glyph as a repeated run with an ASCII sentinel cell after it, so a width
// error shows as a shifted sentinel column. Rows repeat under xterm-256, ANSI
// tonal, ANSI greyscale, and monochrome colour, on high-contrast foreground
// and background pairs, followed by diagonal, corner, and flat-cell rows built
// from the same glyphs. Recording which terminals draw the card cleanly is the
// owner's job; this tool only makes the evidence visible.
package main

import (
	"bufio"
	"fmt"
	"os"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// reset clears all SGR attributes so a row never leaks colour into the next.
const reset = "\x1b[0m"

// runLen is how many adjacent cells each candidate repeats across. The
// sentinel after a long run makes a fractional width error accumulate into a
// visible column shift.
const runLen = 8

// colourMode is one card colour section: a name and the SGR pair builder for
// a high-contrast foreground and background. Monochrome returns empty SGR, so
// its rows draw in the terminal's default colours.
type colourMode struct {
	name string
	sgr  func(bright bool) string
}

var colourModes = []colourMode{
	{"xterm-256", func(bright bool) string {
		if bright {
			return "\x1b[38;5;231m\x1b[48;5;16m"
		}
		return "\x1b[38;5;16m\x1b[48;5;231m"
	}},
	{"ansi-tonal", func(bright bool) string {
		if bright {
			return "\x1b[97m\x1b[40m"
		}
		return "\x1b[30m\x1b[107m"
	}},
	{"ansi-grey", func(bright bool) string {
		if bright {
			return "\x1b[38;5;255m\x1b[48;5;232m"
		}
		return "\x1b[38;5;232m\x1b[48;5;255m"
	}},
	{"monochrome", func(bool) string { return "" }},
}

func main() {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	fmt.Fprintln(w, "Shaded Block test card. Check three things per row:")
	fmt.Fprintln(w, "  1. every glyph draws (no blank or replacement box),")
	fmt.Fprintln(w, "  2. the X| sentinel column lines up on every row (one-cell width),")
	fmt.Fprintln(w, "  3. shade rows read as an even 50 percent tone with no seams.")
	fmt.Fprintln(w)

	candidates := block.ShadedCandidates()
	for _, mode := range colourModes {
		fmt.Fprintf(w, "== %s ==\n", mode.name)
		fmt.Fprintf(w, "control U+2584: %s", mode.sgr(true))
		for range runLen {
			fmt.Fprint(w, "▄")
		}
		fmt.Fprintln(w, reset+"X|")
		for _, g := range candidates {
			fmt.Fprintf(w, "glyph  %-7s: ", fmt.Sprintf("U+%04X", g))
			for _, bright := range []bool{true, false} {
				fmt.Fprint(w, mode.sgr(bright))
				for range runLen {
					fmt.Fprint(w, string(g))
				}
				fmt.Fprint(w, reset, "X|")
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}

	// Composite rows: a diagonal edge from the triangular shades, the four
	// corner triangles, and a flat row (foreground equal to background), the
	// cell classes the Shaded compose emits on real pages.
	fmt.Fprintln(w, "== composite rows (xterm-256) ==")
	bright := "\x1b[38;5;231m\x1b[48;5;16m"
	fmt.Fprint(w, "diagonal: ", bright)
	for range 4 {
		fmt.Fprint(w, "\U0001FB9C\U0001FB9E")
	}
	fmt.Fprintln(w, reset+"X|")
	fmt.Fprint(w, "corners:  ", bright)
	for range 2 {
		fmt.Fprint(w, "\U0001FB9C\U0001FB9D\U0001FB9F\U0001FB9E")
	}
	fmt.Fprintln(w, reset+"X|")
	fmt.Fprint(w, "flat:     ", "\x1b[38;5;244m\x1b[48;5;244m")
	for range runLen {
		fmt.Fprint(w, "█")
	}
	fmt.Fprintln(w, reset+"X|")
}
