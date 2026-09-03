package block

import (
	"bufio"
	"image"
	"image/color"
	"image/draw"
	"io"
	"math/bits"
	"unicode/utf8"

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

// lowerHalfBlock is the U+2584 lower-half-block glyph. The glyph fills the lower
// half of the cell with the foreground colour and leaves the upper half showing
// the background colour.
const lowerHalfBlock = 0x2584

// Scheme selects how Renderer maps source pixels to cells. SchemeQuadrant
// is the 2x2 binarisation default; SchemeSextant is the 2x3 sextant
// binarisation.
type Scheme int

const (
	// SchemeQuadrant maps one cell to a 2x4 source block via the quadrant (2x2)
	// binarisation. It is the default scheme.
	SchemeQuadrant Scheme = iota
	// SchemeSextant maps one cell to a 2x3 source block via the sextant (2x3)
	// binarisation, raising vertical resolution over the quadrant scheme.
	SchemeSextant
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
	// Link is the OSC 8 hyperlink this cell sits inside, nil when the cell
	// carries no link. Every cell of one run shares the same pointer, so the
	// link is a cell attribute like FG, and the writers bracket runs of equal
	// links with one open and one close.
	Link *Hyperlink
}

// Grid is the Block backend's owned cols x rows array of cells, stored
// row-major. Production writers and diagnostic rasterisation consume the same
// composed grid.
type Grid struct {
	Cols  int    // grid width in cells
	Rows  int    // grid height in cells
	Cells []Cell // row-major, len == Cols*Rows
	// runOrder is the overlay fold's scratch buffer for the front-to-back run
	// order (orderedRuns). It hangs off the grid because the live draw path reuses
	// one grid per frame (reuseGrid keeps it), so the reordering costs no
	// allocation per frame after the first contended frame.
	runOrder []render.GlyphRun
	// boundaryClaimed is the overlay fold's first-writer-wins bitset for the
	// boundary recompose (recomposeBoundaryCellsFrom), one bit per cell in the
	// same row-major order as Cells. The glyph write is gated by claimed(), which
	// reads the cell itself; a background-only boundary recompose leaves no such
	// mark, so it needs its own record of which cells the fold has already
	// repainted. Like runOrder it hangs off the grid, so the live draw path's
	// reused grid pays no allocation per frame.
	boundaryClaimed []uint64
}

// resetBoundaryClaims clears the boundary recompose bitset and sizes it to the
// grid, reusing the backing array when it is already long enough. The fold calls
// it once per frame, before the first run, and unconditionally: a reused grid
// must not carry a text-free frame's claims over from the last frame that had
// text, and the reset makes the bitset a pure function of the frame, so a reused
// grid compares equal to a freshly composed one.
func (g *Grid) resetBoundaryClaims() {
	n := (len(g.Cells) + 63) / 64
	if cap(g.boundaryClaimed) < n {
		g.boundaryClaimed = make([]uint64, n)
		return
	}
	g.boundaryClaimed = g.boundaryClaimed[:n]
	clear(g.boundaryClaimed)
}

// claimBoundaryCell claims the cell at (col, row) for the boundary recompose and
// reports whether the claim succeeded. It fails once the cell is already claimed,
// which makes the recompose first-writer-wins: fed front-to-back, the front-most
// run keeps a boundary cell two runs both spill into. The claim is separate from
// claimed(), so a later run's glyph still writes over a recomposed cell.
func (g *Grid) claimBoundaryCell(col, row int) bool {
	i := row*g.Cols + col
	word, bit := i/64, uint64(1)<<uint(i%64)
	if g.boundaryClaimed[word]&bit != 0 {
		return false
	}
	g.boundaryClaimed[word] |= bit
	return true
}

// At returns a pointer to the cell at (col, row). It panics on an out-of-range
// index, matching slice semantics.
func (g *Grid) At(col, row int) *Cell {
	return &g.Cells[row*g.Cols+col]
}

// Renderer composites a Frame onto an owned cell grid, then writes one
// truecolour SGR cell per grid cell. Scheme selects quadrant or sextant
// binarisation. The binarisation schemes fill bright
// sub-cells with the foreground and dark sub-cells with the background. The
// scheme is a backend-specific knob on this concrete type, not on the
// render.Renderer interface. Renderer satisfies render.Renderer.
type Renderer struct {
	Cols   int    // requested grid width in cells
	Rows   int    // requested grid height in cells
	Scheme Scheme // pixel-to-cell mapping; zero value is SchemeQuadrant
	// PaletteLUT is the adaptive palette's RGB555 lookup table, or nil when the
	// renderer draws through the static xterm-256 table. It holds absolute
	// terminal register numbers and is keyed exactly like XTerm256, so a
	// consumer writes an entry straight into an SGR with no arithmetic of its
	// own. PaletteIndex is the one reader.
	//
	// The binarise pass never reads the table: a cell keeps its averaged RGBA
	// colour through compose and the SGR writer maps that colour when it emits
	// the cell. That is why attaching a palette leaves the serial and the
	// parallel compose byte-identical, and why one field on the renderer covers
	// both compose entries, ComposeInto and ComposeYCbCrInto, which each take
	// the renderer by value.
	PaletteLUT []uint8
}

// Compile-time assertion that Renderer satisfies render.Renderer.
var _ render.Renderer = (*Renderer)(nil)

// Name returns the backend identifier "block".
func (*Renderer) Name() string { return "block" }

// Render composites the frame's image onto a Cols x Rows cell grid and writes
// the grid to w as truecolour SGR sequences. Bitmap cells emit a block glyph;
// overlay cells emit their text, and wide trailing cells emit nothing. Each row
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
	// curLink is the hyperlink the terminal currently has open. OSC 8 is
	// global terminal state like SGR, so one open brackets a whole run of
	// equal-link cells and a state compare decides each transition.
	var curLink *Hyperlink
	var linkBuf []byte
	for row := range grid.Rows {
		for col := range grid.Cols {
			c := grid.At(col, row)
			// A Wide trailing cell emits nothing, so it never changes link state.
			if !c.Wide && !sameHyperlink(curLink, c.Link) {
				linkBuf = linkBuf[:0]
				if curLink != nil {
					linkBuf = appendLinkClose(linkBuf)
				}
				if c.Link != nil {
					linkBuf = appendLinkOpen(linkBuf, c.Link)
				}
				if _, err := bw.Write(linkBuf); err != nil {
					return err
				}
				curLink = c.Link
			}
			if err := WriteCell(bw, c, TrueColorFG(c.FG), TrueColorBG(c.BG)); err != nil {
				return err
			}
		}
		// Close an open link before the row terminator, so no link ever spans
		// the SGR reset and CRLF.
		if curLink != nil {
			if _, err := bw.Write(appendLinkClose(linkBuf[:0])); err != nil {
				return err
			}
			curLink = nil
		}
		if _, err := bw.WriteString("\x1b[0m\r\n"); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// WriteCell writes one composed cell to w: its foreground and background SGR
// (the caller supplies the strings, so it owns the colour mode), then the cell's
// drawable text, the overlay grapheme when Text is set, otherwise the bitmap
// glyph rune. It emits nothing for a Wide trailing cell, because the lead cell's
// width-2 grapheme already paints both columns; this Wide-skip rule lives here
// alone, so every grid emitter shares it.
func WriteCell(w io.Writer, c *Cell, fgSGR, bgSGR string) error {
	if c.Wide {
		return nil
	}
	if _, err := io.WriteString(w, fgSGR); err != nil {
		return err
	}
	if _, err := io.WriteString(w, bgSGR); err != nil {
		return err
	}
	if c.Text != "" {
		_, err := io.WriteString(w, c.Text)
		return err
	}
	var glyph [utf8.UTFMax]byte
	_, err := w.Write(utf8.AppendRune(glyph[:0], c.Glyph))
	return err
}

// Compose composites the frame image onto a fresh Cols x Rows grid, dispatching
// on the renderer's Scheme, then folds any text overlay onto the grid. The
// result is deterministic for a given Frame.
func (br *Renderer) Compose(f render.Frame) Grid {
	f = rgbaFrame(f)
	var g Grid
	switch br.Scheme {
	case SchemeSextant:
		g = br.composeSextant(f)
	default:
		g = br.composeQuadrant(f)
	}
	applyOverlay(&g, f, br.Scheme)
	return g
}

// ComposeInto is the allocation-reusing twin of Compose for the live draw path.
// It composes f into dst, reusing dst's Cells backing array when it already holds
// at least Cols*Rows cells, and grows it only on a size change. Each scheme
// fully overwrites every cell, so a reused buffer carries no stale content.
// It returns the same dst.
func (br *Renderer) ComposeInto(dst *Grid, f render.Frame) *Grid {
	f = rgbaFrame(f)
	reuseGrid(dst, br.Cols, br.Rows)
	switch br.Scheme {
	case SchemeSextant:
		br.composeSextantInto(dst, f)
	default:
		br.composeQuadrantInto(dst, f)
	}
	applyOverlay(dst, f, br.Scheme)
	return dst
}

// reuseGrid sizes g to cols x rows, reusing its Cells backing array when it is
// long enough and allocating only when it must grow. It is the shared resize
// helper for the *Into compose and fit paths, which reuse one grid per frame.
func reuseGrid(g *Grid, cols, rows int) {
	n := cols * rows
	if cap(g.Cells) < n {
		g.Cells = make([]Cell, n)
	} else {
		g.Cells = g.Cells[:n]
	}
	g.Cols = cols
	g.Rows = rows
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
	br.composeQuadrantInto(&g, f)
	return g
}

// composeQuadrantInto binarises the frame image onto g, which the caller has
// already sized to Cols x Rows. It is the shared body of composeQuadrant (fresh
// grid) and ComposeInto (reused grid), so both fill cells the same way.
func (br *Renderer) composeQuadrantInto(g *Grid, f render.Frame) {
	br.composeQuadrantRows(g, f, 0, g.Rows)
}

// composeQuadrantRows binarises grid rows [row0, row1) of g from the frame image.
// It is the per-band body of composeQuadrantInto: each cell samples its own 2x4
// source block and writes only its own cell, so a call over a disjoint row range
// writes byte-for-byte the same cells the whole-grid pass writes for those rows.
// The row split is purely which rows a call fills, so the parallel band-join is
// byte-identical to the serial pass.
func (br *Renderer) composeQuadrantRows(g *Grid, f render.Frame, row0, row1 int) {
	img, _ := f.Img.(*image.RGBA)
	if exactQuadrantSource(img, g.Cols, g.Rows) {
		br.composeQuadrantRowsExact(g, img, row0, row1)
		return
	}
	for row := row0; row < row1; row++ {
		for col := range g.Cols {
			x, y, z, wq := sampleQuadrant(img, col, row)
			glyph, fg, bg := binarizeQuadrant(x, y, z, wq)
			*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
		}
	}
}

// exactQuadrantSource reports whether img has the packed, zero-origin, exact
// dimensions produced for live Quadrant composition. Every other layout stays
// on the bounds-safe generic sampler.
func exactQuadrantSource(img *image.RGBA, cols, rows int) bool {
	if img == nil || cols <= 0 || rows <= 0 {
		return false
	}
	maxInt := int(^uint(0) >> 1)
	if cols > maxInt/cellPxW || rows > maxInt/cellPxH {
		return false
	}
	w, h := cols*cellPxW, rows*cellPxH
	if w > maxInt/4 {
		return false
	}
	stride := w * 4
	if h > maxInt/stride {
		return false
	}
	return img.Rect == image.Rect(0, 0, w, h) && img.Stride == stride && len(img.Pix) == stride*h
}

func (br *Renderer) composeQuadrantRowsExact(g *Grid, img *image.RGBA, row0, row1 int) {
	for row := row0; row < row1; row++ {
		for col := range g.Cols {
			x, y, z, w := sampleQuadrantExact(img, col, row)
			glyph, fg, bg := binarizeQuadrant(x, y, z, w)
			*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
		}
	}
}

// rgbaFrame converts a decoder-native image at the block consumer boundary.
// The live viewer already supplies its downscaled RGBA plane, so the hot path is
// one type assertion and no copy.
func rgbaFrame(f render.Frame) render.Frame {
	if f.Img == nil {
		return f
	}
	if _, ok := f.Img.(*image.RGBA); ok {
		return f
	}
	b := f.Img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), f.Img, b.Min, draw.Src)
	f.Img = rgba
	return f
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

func averagePair(a, b uint8) uint8 {
	return uint8((uint16(a) + uint16(b)) >> 1) //nolint:gosec // byte average stays in range
}

func verticalPair(pix []byte, top, bottom int) color.RGBA {
	return color.RGBA{
		R: averagePair(pix[top], pix[bottom]),
		G: averagePair(pix[top+1], pix[bottom+1]),
		B: averagePair(pix[top+2], pix[bottom+2]),
		A: 0xff,
	}
}

// sampleQuadrantExact reads one cell from a source accepted by
// exactQuadrantSource. Callers must establish that predicate first.
func sampleQuadrantExact(img *image.RGBA, col, row int) (x, y, z, w color.RGBA) {
	top := row*cellPxH*img.Stride + col*cellPxW*4
	middle := top + 2*img.Stride
	x = verticalPair(img.Pix, top, top+img.Stride)
	y = verticalPair(img.Pix, top+4, top+img.Stride+4)
	z = verticalPair(img.Pix, middle, middle+img.Stride)
	w = verticalPair(img.Pix, middle+4, middle+img.Stride+4)
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
	for dy := range quadColPxH {
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

// colourMaskMeta keeps the mask membership and both group counts once per mask.
// The two schemes share the representation while retaining their documented bit
// order in their own tables.
type colourMaskMeta struct {
	bright  uint8
	brightN uint8
	darkN   uint8
}

func makeColourMaskMeta(mask, cells int) colourMaskMeta {
	boundedMask := uint8(mask) //nolint:gosec // table builders bound masks to six bits
	brightN := bits.OnesCount8(boundedMask)
	return colourMaskMeta{
		bright:  boundedMask,
		brightN: uint8(brightN),         //nolint:gosec // at most six set bits
		darkN:   uint8(cells - brightN), //nolint:gosec // cells is four or six
	}
}

func buildQuadrantMaskMeta() [16]colourMaskMeta {
	var table [16]colourMaskMeta
	for mask := range table {
		table[mask] = makeColourMaskMeta(mask, 4)
	}
	return table
}

var quadrantMaskMeta = buildQuadrantMaskMeta()

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
	mid := lo + (hi-lo)/2

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
	// Accumulate the bright and dark channel sums in scalars rather than two
	// heap-allocated slices: the compose path runs this per non-flat cell, so the
	// slices were the largest allocation source. The arithmetic matches avgRGBA
	// summed over the same sub-cells in cell order, so the result is byte-identical.
	cells := [4]color.RGBA{x, y, z, w}
	meta := quadrantMaskMeta[mask]
	var brs, bgs, bbs int
	var drs, dgs, dbs int
	for i, c := range cells {
		if meta.bright&(1<<(3-i)) != 0 {
			brs += int(c.R)
			bgs += int(c.G)
			bbs += int(c.B)
		} else {
			drs += int(c.R)
			dgs += int(c.G)
			dbs += int(c.B)
		}
	}
	return avgChannelsCount(brs, bgs, bbs, meta.brightN), avgChannelsCount(drs, dgs, dbs, meta.darkN)
}

// avgRGBA averages a set of RGBA colours per channel, with alpha forced opaque.
// Channel order is RGBA. An empty set returns opaque black. It averages any
// number of sub-cells, mirroring Carbonyl's avg_with for two.
func avgRGBA(cs ...color.RGBA) color.RGBA {
	var rs, gs, bs int
	for _, c := range cs {
		rs += int(c.R)
		gs += int(c.G)
		bs += int(c.B)
	}
	return avgChannels(rs, gs, bs, len(cs))
}

// avgChannels averages pre-summed R, G, B channel sums over n sub-cells, with
// alpha forced opaque and channel order RGBA. It is the shared tail of avgRGBA
// and the inlined quadrant/sextant colour sums, so all three round identically.
// A zero count returns opaque black, matching an empty avgRGBA set.
func avgChannels(rs, gs, bs, n int) color.RGBA {
	if n == 0 {
		return color.RGBA{A: 0xff}
	}
	return color.RGBA{
		R: clampByte(rs / n),
		G: clampByte(gs / n),
		B: clampByte(bs / n),
		A: 0xff,
	}
}

// avgChannelsCount averages valid sub-cell sums using a count-specific constant
// divisor. Its callers provide counts from mask metadata and sums in 0..255*n.
func avgChannelsCount(rs, gs, bs int, n uint8) color.RGBA {
	var r, g, b int
	switch n {
	case 1:
		r, g, b = rs, gs, bs
	case 2:
		r, g, b = rs>>1, gs>>1, bs>>1
	case 3:
		r, g, b = rs/3, gs/3, bs/3
	case 4:
		r, g, b = rs>>2, gs>>2, bs>>2
	case 5:
		r, g, b = rs/5, gs/5, bs/5
	case 6:
		r, g, b = rs/6, gs/6, bs/6
	default:
		return color.RGBA{A: 0xff}
	}
	return color.RGBA{
		R: uint8(r), //nolint:gosec // valid sums divided by their count stay in 0..255
		G: uint8(g), //nolint:gosec // valid sums divided by their count stay in 0..255
		B: uint8(b), //nolint:gosec // valid sums divided by their count stay in 0..255
		A: 0xff,
	}
}

// lumaWeightR, lumaWeightG, and lumaWeightB are the fixed-point 8-bit Rec.601
// luma weights: the Carbonyl float constants 0.299, 0.587, 0.114 scaled to 256
// and rounded (77, 150, 29), summing to 256. Integer weights let luma stay in
// integer arithmetic, which benches faster than the float multiply-add and than
// a pre-scaled lookup table. The luma is an ordering key only: the binarise
// splits sub-cells at the luma midpoint, so the absolute scale does not matter,
// only the relative order. The integer luma tracks the float luma's ordering
// closely enough that the two-colour split differs on only a handful of near-tie
// cells (recorded in the golden archives).
const (
	lumaWeightR = 77
	lumaWeightG = 150
	lumaWeightB = 29
)

// luma converts an RGBA colour to an integer Rec.601 luma ordering key.
// Channel order is RGBA.
func luma(c color.RGBA) int {
	return lumaWeightR*int(c.R) + lumaWeightG*int(c.G) + lumaWeightB*int(c.B)
}

// boolBit maps a flag to 0 or 1 for mask assembly.
func boolBit(v bool) int {
	if v {
		return 1
	}
	return 0
}
