package block

import (
	"bufio"
	"image"
	"image/color"
	"io"

	"github.com/wimpysworld/nyala/internal/render"
)

// cellPxW and cellPxH are the source-pixel footprint of one cell in the quadrant
// scheme. The quadrant scheme maps one cell to a 2x4 source block: two columns
// and four rows. Each quadrant of the 2x2 binarisation averages a 1x2 column of
// that block.
const (
	cellPxW = 2
	cellPxH = 4
)

// halfCellPxW and halfCellPxH are the source-pixel footprint of one cell in the
// half-block scheme. The half-block floor maps one cell to a 1x2 source column:
// the top pixel becomes the cell background, the bottom pixel its foreground,
// drawn with the lower-half-block glyph U+2584.
const (
	halfCellPxW = 1
	halfCellPxH = 2
)

// lowerHalfBlock is the U+2584 lower-half-block glyph. The glyph fills the lower
// half of the cell with the foreground colour and leaves the upper half showing
// the background colour.
const lowerHalfBlock = 0x2584

// Scheme selects how Renderer maps source pixels to cells. SchemeQuadrant
// is the 2x2 binarisation default; SchemeHalfBlock is the half-block floor.
type Scheme int

const (
	// SchemeQuadrant maps one cell to a 2x4 source block via the quadrant (2x2)
	// binarisation. It is the default scheme.
	SchemeQuadrant Scheme = iota
	// SchemeHalfBlock maps one cell to a 1x2 source column, emitting the lower-
	// half-block glyph U+2584 with the top pixel as background and the bottom
	// pixel as foreground.
	SchemeHalfBlock
)

// Cell is one terminal cell on the owned grid. Glyph is the block rune to draw,
// FG is its foreground colour, and BG is its background colour. The quadrant
// binarisation fills these from a 2x2 sub-cell of source colours. Text and Wide
// carry the optional crisp-text overlay: when Text is set the consumers prefer
// it over Glyph; the zero value (Text == "", Wide == false) is a plain bitmap
// cell.
type Cell struct {
	Glyph rune       // block glyph for this cell
	FG    color.RGBA // foreground colour
	BG    color.RGBA // background colour
	Text  string     // overlay grapheme; "" means a bitmap cell
	Wide  bool       // trailing cell of a wide (width-2) overlay glyph
}

// Grid is an owned cols x rows array of cells, stored row-major. It is the
// backend-neutral product of compositing a Frame: every renderer reads the same
// grid, then emits it in its own representation.
type Grid struct {
	Cols  int    // grid width in cells
	Rows  int    // grid height in cells
	Cells []Cell // row-major, len == Cols*Rows
}

// At returns a pointer to the cell at (col, row). It panics on an out-of-range
// index, matching slice semantics.
func (g *Grid) At(col, row int) *Cell {
	return &g.Cells[row*g.Cols+col]
}

// Renderer composites a Frame onto an owned cell grid, then writes one
// truecolour SGR cell per grid cell. Scheme selects the pixel-to-cell mapping:
// the quadrant (2x2) binarisation, or the half-block floor. The quadrant scheme
// is self-consistent: the glyph fills the bright quadrants with the foreground,
// the dark quadrants with the background. The scheme is a backend-specific knob
// on this concrete type, not on the render.Renderer interface. It satisfies
// render.Renderer.
type Renderer struct {
	Cols   int    // requested grid width in cells
	Rows   int    // requested grid height in cells
	Scheme Scheme // pixel-to-cell mapping; zero value is SchemeQuadrant
}

// ToggleScheme flips between the quadrant and half-block schemes and returns the
// new scheme. It mutates the receiver, so it touches only this concrete type and
// never the render.Renderer interface.
func (br *Renderer) ToggleScheme() Scheme {
	if br.Scheme == SchemeQuadrant {
		br.Scheme = SchemeHalfBlock
	} else {
		br.Scheme = SchemeQuadrant
	}
	return br.Scheme
}

// Compile-time assertion that Renderer satisfies render.Renderer.
var _ render.Renderer = (*Renderer)(nil)

// Name returns the backend identifier "block".
func (*Renderer) Name() string { return "block" }

// Render composites the frame's image onto a Cols x Rows cell grid and writes
// the grid to w as truecolour SGR sequences, one block glyph per cell. Each row
// ends with an SGR reset and a CRLF; the carriage return returns the cursor to
// column 0 when auto-wrap is off in a raw terminal. Output is deterministic: the
// same Frame yields byte-identical output across runs.
func (br *Renderer) Render(f render.Frame, w io.Writer) error {
	return br.renderGrid(br.Compose(f), w)
}

// renderGrid writes an already-composed grid to w as truecolour SGR cells, one
// glyph per cell, preferring a cell's overlay Text over its bitmap Glyph. It is
// the shared body of Render, split out so a hand-built grid renders without a
// compose pass.
func (br *Renderer) renderGrid(grid Grid, w io.Writer) error {
	bw := bufio.NewWriter(w)
	for row := 0; row < grid.Rows; row++ {
		for col := 0; col < grid.Cols; col++ {
			c := grid.At(col, row)
			if _, err := bw.WriteString(TrueColorFG(c.FG)); err != nil {
				return err
			}
			if _, err := bw.WriteString(TrueColorBG(c.BG)); err != nil {
				return err
			}
			if err := writeCellText(bw, c); err != nil {
				return err
			}
		}
		if _, err := bw.WriteString("\x1b[0m\r\n"); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// writeCellText writes a cell's drawable text to bw: the overlay grapheme when
// Text is set, otherwise the bitmap glyph rune. A Wide trailing cell holds an
// empty Text, so it writes a single space over its background, occupying its one
// column without re-emitting the wide grapheme.
func writeCellText(bw *bufio.Writer, c *Cell) error {
	if c.Wide {
		_, err := bw.WriteRune(' ')
		return err
	}
	if c.Text != "" {
		_, err := bw.WriteString(c.Text)
		return err
	}
	_, err := bw.WriteRune(c.Glyph)
	return err
}

// Compose composites the frame image onto a fresh Cols x Rows grid, dispatching
// on the renderer's Scheme, then folds any text overlay onto the grid. The
// result is deterministic for a given Frame.
func (br *Renderer) Compose(f render.Frame) Grid {
	var g Grid
	if br.Scheme == SchemeHalfBlock {
		g = br.composeHalfBlock(f)
	} else {
		g = br.composeQuadrant(f)
	}
	applyOverlay(&g, f, br.Scheme)
	return g
}

// composeQuadrant binarises the frame image onto a fresh Cols x Rows quadrant
// grid. Each cell samples a 2x4 source block; an empty or nil image yields an
// all-black grid (every quadrant black, a flat 0000 mask, so the full-block
// glyph with black fg and bg).
func (br *Renderer) composeQuadrant(f render.Frame) Grid {
	g := Grid{
		Cols:  br.Cols,
		Rows:  br.Rows,
		Cells: make([]Cell, br.Cols*br.Rows),
	}
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			x, y, z, wq := sampleQuadrant(f.Img, col, row)
			glyph, fg, bg := binarizeQuadrant(x, y, z, wq)
			*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
		}
	}
	return g
}

// composeHalfBlock maps the frame image onto a fresh Cols x Rows half-block grid.
// Each cell samples a 1x2 source column: the top pixel becomes the cell
// background, the bottom pixel its foreground, and the glyph is the lower-half-
// block U+2584. An empty or nil image yields an all-black grid.
func (br *Renderer) composeHalfBlock(f render.Frame) Grid {
	g := Grid{
		Cols:  br.Cols,
		Rows:  br.Rows,
		Cells: make([]Cell, br.Cols*br.Rows),
	}
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			top, bottom := sampleHalfBlock(f.Img, col, row)
			*g.At(col, row) = Cell{Glyph: lowerHalfBlock, FG: bottom, BG: top}
		}
	}
	return g
}

// sampleHalfBlock reads the two source pixels of cell (col, row) for the half-
// block scheme. The cell maps to a 1x2 source column; top is the upper pixel,
// bottom the lower. Pixels outside the image read as opaque black.
func sampleHalfBlock(img *image.RGBA, col, row int) (top, bottom color.RGBA) {
	x0 := col * halfCellPxW
	y0 := row * halfCellPxH
	tr, tg, tb := pixel(img, x0, y0)
	br, bg, bb := pixel(img, x0, y0+1)
	top = color.RGBA{R: tr, G: tg, B: tb, A: 0xff}
	bottom = color.RGBA{R: br, G: bg, B: bb, A: 0xff}
	return top, bottom
}

// sampleQuadrant box-averages the 2x4 source block for cell (col, row) into the
// four quadrant colours (top-left, top-right, bottom-left, bottom-right). Each
// quadrant covers one source column and two source rows. Pixels outside the
// image bound are treated as opaque black.
func sampleQuadrant(img *image.RGBA, col, row int) (x, y, z, w color.RGBA) {
	x0 := col * cellPxW
	y0 := row * cellPxH
	// Quadrant origins within the 2x4 block: left/right column, top/bottom pair.
	x = avgBlock(img, x0, y0)     // top-left
	y = avgBlock(img, x0+1, y0)   // top-right
	z = avgBlock(img, x0, y0+2)   // bottom-left
	w = avgBlock(img, x0+1, y0+2) // bottom-right
	return x, y, z, w
}

// quadColPxH is the source-pixel height one quadrant averages: a quadrant covers
// a 1x2 source column, so avgBlock averages two rows.
const quadColPxH = 2

// avgBlock averages the 1x2 source column anchored at (x0, y0) into one RGBA
// colour. Channel order is RGBA: R, G, B average independently, alpha is forced
// opaque. Out-of-bound or nil pixels contribute opaque black.
func avgBlock(img *image.RGBA, x0, y0 int) color.RGBA {
	var rs, gs, bs int
	for dy := 0; dy < quadColPxH; dy++ {
		r, g, b := pixel(img, x0, y0+dy)
		rs += int(r)
		gs += int(g)
		bs += int(b)
	}
	return color.RGBA{
		R: clampByte(rs / quadColPxH),
		G: clampByte(gs / quadColPxH),
		B: clampByte(bs / quadColPxH),
		A: 0xff,
	}
}

// clampByte clamps v to the [0, 255] range and returns it as a uint8. Channel
// averages are already within that range, so the clamp never changes a valid
// result; it makes the conversion provably safe.
func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// pixel reads the R, G, B channels at (x, y). Pixels outside the image or a nil
// image read as black. The RGBA pixel layout is R, G, B, A; this honours that
// order.
func pixel(img *image.RGBA, x, y int) (r, g, b uint8) {
	if img == nil {
		return 0, 0, 0
	}
	if !(image.Point{X: x, Y: y}).In(img.Bounds()) {
		return 0, 0, 0
	}
	i := img.PixOffset(x, y)
	return img.Pix[i], img.Pix[i+1], img.Pix[i+2]
}

// quadrant block glyphs keyed by the 4-bit mask TL<<3 | TR<<2 | BL<<1 | BR,
// where each bit is set when that sub-cell's luma sits above the cell midpoint
// (the bright sub-cells). The glyph is the Unicode quadrant block whose filled
// quadrants are exactly the bright set, so the foreground colour paints the
// bright quadrants. The two flat masks (0000 all dark, 1111 all bright) emit a
// full block; their foreground equals their background, so the cell paints one
// colour either way.
var quadrantGlyphs = [16]rune{
	0x2588, // 0000 full block (flat cell, fg == bg)
	0x2597, // 0001 quadrant lower right (BR)
	0x2596, // 0010 quadrant lower left (BL)
	0x2584, // 0011 lower half block (BL, BR)
	0x259D, // 0100 quadrant upper right (TR)
	0x2590, // 0101 right half block (TR, BR)
	0x259E, // 0110 quadrant upper right and lower left (TR, BL)
	0x259F, // 0111 quadrant upper right, lower left, lower right (TR, BL, BR)
	0x2598, // 1000 quadrant upper left (TL)
	0x259A, // 1001 quadrant upper left and lower right (TL, BR)
	0x258C, // 1010 left half block (TL, BL)
	0x2599, // 1011 quadrant upper left, lower left, lower right (TL, BL, BR)
	0x2580, // 1100 upper half block (TL, TR)
	0x259C, // 1101 quadrant upper left, upper right, lower right (TL, TR, BR)
	0x259B, // 1110 quadrant upper left, upper right, lower left (TL, TR, BL)
	0x2588, // 1111 full block (flat cell, fg == bg)
}

// binarizeQuadrant turns the four sub-cell colours (x top-left, y top-right,
// z bottom-left, w bottom-right) into one block glyph and two averaged colours.
// It converts each sub-cell to Rec.601 luma, splits at the luma midpoint, and
// builds the 4-bit mask TL<<3 | TR<<2 | BL<<1 | BR of the bright sub-cells. The
// glyph is the quadrant block whose filled quadrants are the bright set; the
// foreground is the average of the bright sub-cells, the background the average
// of the dark ones. A flat cell (mask 0000 or 1111) emits a full block with
// foreground equal to background.
func binarizeQuadrant(x, y, z, w color.RGBA) (glyph rune, fg, bg color.RGBA) {
	a := luma(x)
	b := luma(y)
	c := luma(z)
	d := luma(w)

	lo := min(min(a, b), min(c, d))
	hi := max(max(a, b), max(c, d))
	mid := lo + (hi-lo)/2.0

	mask := boolBit(a > mid)<<3 | boolBit(b > mid)<<2 | boolBit(c > mid)<<1 | boolBit(d > mid)

	glyph = quadrantGlyphs[mask]
	fg, bg = quadrantColours(mask, x, y, z, w)
	return glyph, fg, bg
}

// quadrantColours returns the (foreground, background) colour pair for a mask.
// The four sub-cell colours are x (top-left), y (top-right), z (bottom-left),
// and w (bottom-right). A sub-cell is bright when its mask bit is set: bit3
// top-left, bit2 top-right, bit1 bottom-left, bit0 bottom-right. The foreground
// is the average of the bright sub-cells, the background the average of the dark
// ones. The two flat masks (0000 and 1111) have no split, so both colours are
// the average of all four sub-cells.
func quadrantColours(mask int, x, y, z, w color.RGBA) (fg, bg color.RGBA) {
	if mask == 0b0000 || mask == 0b1111 {
		all := avgRGBA(x, y, z, w)
		return all, all
	}
	cells := [4]color.RGBA{x, y, z, w}
	var bright, dark []color.RGBA
	for i, c := range cells {
		if mask&(1<<(3-i)) != 0 {
			bright = append(bright, c)
		} else {
			dark = append(dark, c)
		}
	}
	return avgRGBA(bright...), avgRGBA(dark...)
}

// avgRGBA averages a set of RGBA colours per channel, with alpha forced opaque.
// Channel order is RGBA. An empty set returns opaque black. It averages any
// number of sub-cells, mirroring Carbonyl's avg_with for two.
func avgRGBA(cs ...color.RGBA) color.RGBA {
	if len(cs) == 0 {
		return color.RGBA{A: 0xff}
	}
	var rs, gs, bs int
	for _, c := range cs {
		rs += int(c.R)
		gs += int(c.G)
		bs += int(c.B)
	}
	n := len(cs)
	return color.RGBA{
		R: clampByte(rs / n),
		G: clampByte(gs / n),
		B: clampByte(bs / n),
		A: 0xff,
	}
}

// luma converts an RGBA colour to Rec.601 luma. The weights are the Carbonyl
// constants 0.299 R, 0.587 G, 0.114 B; channel order is RGBA.
func luma(c color.RGBA) float64 {
	return 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
}

// boolBit maps a flag to 0 or 1 for mask assembly.
func boolBit(v bool) int {
	if v {
		return 1
	}
	return 0
}
