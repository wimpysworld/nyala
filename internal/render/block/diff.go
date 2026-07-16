package block

import (
	"bytes"
	"image/color"
	"io"
	"unicode/utf8"
)

// SGRFunc appends the SGR escape that sets a cell colour for the chosen colour
// mode to dst and returns the extended slice. The caller owns the colour mode, so
// the differ stays mode-neutral: truecolour, xterm-256, and monochrome (which
// appends nothing) all flow through the same compare and coalescing. Appending
// into the differ's reused buffer keeps the per-cell colour build allocation-free.
type SGRFunc func(dst []byte, c color.RGBA) []byte

// GridDiff retains the last drawn grid and the last emitted foreground and
// background SGR, so Emit writes only the cells that changed since the previous
// frame. It is the live paint path's flicker fix: a full grid rewrite per frame
// is replaced by a cell diff with CUP and SGR coalescing. The zero value is a
// fresh differ that full-repaints its first frame. The capture path may reuse it
// once that path unifies.
type GridDiff struct {
	prev    Grid       // last drawn grid; empty Cells means no previous frame
	lastFG  []byte     // last emitted foreground SGR this frame, empty until set
	lastBG  []byte     // last emitted background SGR this frame, empty until set
	fgCol   color.RGBA // colour that built lastFG; zero (A==0) until set
	bgCol   color.RGBA // colour that built lastBG; zero (A==0) until set
	fgSet   bool       // truecolour foreground RGB state is seeded this frame
	bgSet   bool       // truecolour background RGB state is seeded this frame
	sgrBuf  []byte     // reused scratch for building an SGR before its compare
	runeBuf []byte     // reused scratch for encoding a bitmap glyph rune
	cupBuf  [16]byte   // reused scratch for building a CUP cursor move
}

// Emit writes grid to w as the minimal escape stream that turns the previously
// drawn grid into grid, then retains grid as the new previous. fg and bg map a
// cell colour to its SGR for the active colour mode.
//
// A cell is emitted only when its (Text, Wide, FG, BG) differ from the previous
// grid. The cursor moves with a CUP (1-based) only when the next emitted cell is
// not where the virtual cursor already sits, so a contiguous changed run costs
// one CUP. After a cell the virtual cursor advances by the cell's column width
// (2 for a Wide lead cell, 1 otherwise). The foreground and background SGR are
// cached and re-emitted only on a colour change, so a same-colour run sends one
// SGR pair.
//
// A cell whose overlay grapheme may render at a terminal width that differs from
// our count (a non-ASCII grapheme or a wide lead cell) marks the virtual cursor
// unknown, so the next emitted cell re-anchors with a fresh CUP. This bounds a
// width mismatch to one cell instead of skewing the rest of the row. Pure-ASCII
// overlay text and width-1 block glyphs keep the contiguous fast path.
//
// On the first frame, and whenever the grid dimensions change (a resize), Emit
// full-repaints every cell and reseeds the previous grid, because no prior cell
// maps to the new geometry.
//
// SGR colour state persists across cursor moves in a terminal, so the colour
// cache is kept across a CUP jump; it is reset only at the start of each frame,
// where the virtual cursor is also marked unknown so the first emitted cell
// always carries a CUP and its colour.
func (d *GridDiff) Emit(w io.Writer, grid Grid, fg, bg SGRFunc) error {
	return d.emit(w, grid, fg, bg, false)
}

// EmitTrueColour writes grid using the exact 24-bit RGB appenders. It preserves
// Emit's bytes and cursor policy while avoiding the generic formatter and encoded
// SGR comparison path. Callers must select it only for truecolour output.
func (d *GridDiff) EmitTrueColour(w io.Writer, grid Grid) error {
	return d.emit(w, grid, nil, nil, true)
}

func (d *GridDiff) emit(w io.Writer, grid Grid, fg, bg SGRFunc, trueColour bool) error {
	full := len(d.prev.Cells) == 0 || d.prev.Cols != grid.Cols || d.prev.Rows != grid.Rows

	// Reset the colour cache and the virtual cursor for this frame. A cursor row
	// of -1 means unknown, so the first emitted cell always moves with a CUP. The
	// zero colour (alpha 0) is the unset sentinel: pipeline cells are always
	// opaque, so the first emitted cell always builds and emits its colour.
	d.lastFG, d.lastBG = d.lastFG[:0], d.lastBG[:0]
	d.fgCol, d.bgCol = color.RGBA{}, color.RGBA{}
	d.fgSet, d.bgSet = false, false
	curRow, curCol := -1, -1

	for row := range grid.Rows {
		for col := range grid.Cols {
			c := grid.At(col, row)
			// A Wide trailing cell emits nothing (WriteCell owns that rule) and the
			// lead cell already advanced the cursor across it, so skip it here. Its
			// prev copy still updates below via the full grid retain.
			if c.Wide {
				continue
			}
			if !full && cellUnchanged(c, d.prev.At(col, row)) {
				continue
			}
			if curRow != row || curCol != col {
				if err := d.writeCUP(w, row, col); err != nil {
					return err
				}
				curRow, curCol = row, col
			}
			var err error
			if trueColour {
				err = d.writeTrueColour(w, c)
			} else {
				err = d.writeColour(w, c, fg, bg)
			}
			if err != nil {
				return err
			}
			if err := d.writeCellGlyph(w, c); err != nil {
				return err
			}
			adv := cellAdvance(c, grid, col, row)
			curCol += adv
			if overlayWidthUntrusted(c, adv) {
				// A terminal whose width for this grapheme differs from ours would
				// shift every later cell on the row, since the contiguous run streams
				// with no CUP. Mark the virtual cursor unknown so the next emitted cell
				// re-anchors with a fresh CUP, bounding the error to this one cell.
				curCol = -1
			}
		}
	}

	d.retainPrev(grid)
	return nil
}

func sameRGB(a, b color.RGBA) bool {
	return a.R == b.R && a.G == b.G && a.B == b.B
}

// writeTrueColour emits exact RGB changes directly. Alpha is absent from the SGR
// bytes, so it is intentionally absent from the retained colour comparison.
func (d *GridDiff) writeTrueColour(w io.Writer, c *Cell) error {
	fgChanged := (!d.fgSet && c.FG != (color.RGBA{})) || (d.fgSet && !sameRGB(c.FG, d.fgCol))
	bgChanged := (!d.bgSet && c.BG != (color.RGBA{})) || (d.bgSet && !sameRGB(c.BG, d.bgCol))
	if fgChanged && bgChanged {
		d.sgrBuf = appendTrueColors(d.sgrBuf[:0], c.FG, c.BG)
		if _, err := w.Write(d.sgrBuf); err != nil {
			return err
		}
		d.fgCol, d.fgSet = c.FG, true
		d.bgCol, d.bgSet = c.BG, true
		return nil
	}
	if fgChanged {
		d.sgrBuf = AppendTrueColorFG(d.sgrBuf[:0], c.FG)
		if _, err := w.Write(d.sgrBuf); err != nil {
			return err
		}
		d.fgCol, d.fgSet = c.FG, true
	}
	if bgChanged {
		d.sgrBuf = AppendTrueColorBG(d.sgrBuf[:0], c.BG)
		if _, err := w.Write(d.sgrBuf); err != nil {
			return err
		}
		d.bgCol, d.bgSet = c.BG, true
	}
	return nil
}

// Reset drops the retained previous grid, so the next Emit full-repaints. The
// live path calls it after a full screen clear (the first draw and every resize),
// because the cleared terminal no longer matches the retained grid, so a diff
// would skip cells the clear blanked.
func (d *GridDiff) Reset() {
	d.prev = Grid{}
}

// writeColour emits the cell's foreground and background SGR, but only when the
// emitted bytes changed since the last emitted cell. It updates the cache so a
// same-colour run sends one SGR pair. A same-colour cell (RGBA equal to the last
// colour built) skips the rebuild entirely, the common run fast path; on a colour
// change it rebuilds into the reused buffer and compares the bytes, so two colours
// that collapse to the same escape (an xterm-256 palette collision, or monochrome
// which emits nothing) still coalesce exactly as the old string cache did.
func (d *GridDiff) writeColour(w io.Writer, c *Cell, fg, bg SGRFunc) error {
	if c.FG != d.fgCol {
		d.sgrBuf = fg(d.sgrBuf[:0], c.FG)
		if !bytes.Equal(d.sgrBuf, d.lastFG) {
			if _, err := w.Write(d.sgrBuf); err != nil {
				return err
			}
			d.lastFG = append(d.lastFG[:0], d.sgrBuf...)
		}
		d.fgCol = c.FG
	}
	if c.BG != d.bgCol {
		d.sgrBuf = bg(d.sgrBuf[:0], c.BG)
		if !bytes.Equal(d.sgrBuf, d.lastBG) {
			if _, err := w.Write(d.sgrBuf); err != nil {
				return err
			}
			d.lastBG = append(d.lastBG[:0], d.sgrBuf...)
		}
		d.bgCol = c.BG
	}
	return nil
}

// writeCellGlyph writes the cell's drawable text alone: the overlay grapheme when
// Text is set, otherwise the bitmap glyph rune. It mirrors WriteCell's Text-over-
// Glyph rule without re-emitting the SGR, because writeColour already owns the
// colour with caching. The bitmap rune encodes into the reused buffer with
// utf8.AppendRune, so a bitmap cell costs no per-cell string allocation.
func (d *GridDiff) writeCellGlyph(w io.Writer, c *Cell) error {
	if c.Text != "" {
		_, err := io.WriteString(w, c.Text)
		return err
	}
	d.runeBuf = utf8.AppendRune(d.runeBuf[:0], c.Glyph)
	_, err := w.Write(d.runeBuf)
	return err
}

// cellAdvance is the column width the virtual cursor moves after a cell: 2 when
// the cell leads a wide (width-2) overlay grapheme, 1 otherwise. The lead cell is
// the one whose following cell on the same row carries Wide.
func cellAdvance(c *Cell, g Grid, col, row int) int {
	if c.Text != "" && col+1 < g.Cols && g.At(col+1, row).Wide {
		return 2
	}
	return 1
}

// overlayWidthUntrusted reports whether a cell's rendered width may differ from
// the virtual cursor advance, so the cursor can no longer be trusted after it. A
// cell is untrusted when it carries overlay Text whose first rune is non-ASCII
// (an ambiguous- or wide-width grapheme, or a Nerd Font glyph, whose terminal
// width can differ from our go-runewidth count), or when it is a wide lead cell
// (advance 2). Pure-ASCII overlay text (0x20..0x7E) and width-1 block glyphs stay
// trusted, so the diff keeps its contiguous fast path and colour coalescing.
func overlayWidthUntrusted(c *Cell, advance int) bool {
	if c.Text == "" {
		return false
	}
	if advance == 2 {
		return true
	}
	r, _ := utf8.DecodeRuneInString(c.Text)
	return r > 0x7F
}

// cellUnchanged reports whether two cells draw the same bytes: matching Text,
// Wide, Glyph, foreground, and background. A match lets Emit skip the cell. Glyph
// joins the proposal's (Text, Wide, fg, bg) tuple because a bitmap cell whose
// block glyph changes while its colours hold still draws differently, so omitting
// Glyph would leave a stale glyph on the diff path.
func cellUnchanged(a, b *Cell) bool {
	return a.Text == b.Text && a.Wide == b.Wide && a.Glyph == b.Glyph &&
		a.FG == b.FG && a.BG == b.BG
}

// writeCUP moves the cursor to a 0-based (col, row), emitting the 1-based CUP
// escape ESC [ row ; col H.
func (d *GridDiff) writeCUP(w io.Writer, row, col int) error {
	b := d.cupBuf[:0]
	b = append(b, '\x1b', '[')
	b = appendInt(b, row+1)
	b = append(b, ';')
	b = appendInt(b, col+1)
	b = append(b, 'H')
	_, err := w.Write(b)
	return err
}

// appendInt appends the base-10 digits of a non-negative int to b.
func appendInt(b []byte, v int) []byte {
	if v == 0 {
		return append(b, '0')
	}
	var tmp [12]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	return append(b, tmp[i:]...)
}

// retainPrev copies grid into d.prev so the retained previous frame is
// independent of the caller's grid buffer, which the live path reuses across
// frames. It reuses d.prev's backing array when it is long enough, allocating
// only when the grid grows, so the steady state retains with no per-frame
// allocation.
func (d *GridDiff) retainPrev(g Grid) {
	if cap(d.prev.Cells) < len(g.Cells) {
		d.prev.Cells = make([]Cell, len(g.Cells))
	} else {
		d.prev.Cells = d.prev.Cells[:len(g.Cells)]
	}
	d.prev.Cols, d.prev.Rows = g.Cols, g.Rows
	copy(d.prev.Cells, g.Cells)
}
