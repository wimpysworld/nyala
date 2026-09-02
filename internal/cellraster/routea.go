// Package cellraster turns a Block grid back into an image so correctness
// metrics can score it. Route A is the block backend's path: it rasterises the
// owned cell grid (glyph, foreground, background per cell) to an *image.RGBA,
// emulating a fixed-cell terminal.
//
// The cell raster is pinned at the Route A size of 8x16 pixels (1:2 aspect).
// The output is deterministic: the same grid rasterises to a
// byte-identical *image.RGBA across runs, and PNG writes are timestamp-free, so
// golden frames stay stable. Channel order is RGBA throughout.
package cellraster

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"

	xfont "golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/font"
)

// CellPxW and CellPxH are the pinned Route A cell raster size in pixels: 8 wide
// by 16 tall, a 1:2 aspect that matches the assumed terminal cell shape.
const (
	CellPxW = 8
	CellPxH = 16
)

// baselineY is the glyph baseline, measured in pixels down from the cell top.
// It sits 3 pixels above the cell bottom so descenders fit inside the 16-pixel
// cell. faceSize is the font size in points; at 72 DPI one point is one pixel,
// so the size is chosen to fill the 8x16 cell without spilling past it.
const (
	baselineY = CellPxH - 3
	faceSize  = 16.0
	faceDPI   = 72.0
)

// Rasteriser rasterises a block.Grid to an *image.RGBA at the pinned 8x16 cell
// size. It holds one font.Face per embedded font so glyph drawing reuses a
// parsed, sized face. Construct it with NewRasteriser; the zero value is not
// usable.
type Rasteriser struct {
	atlas      *font.Atlas
	faceByFont map[*sfnt.Font]xfont.Face
}

// NewRasteriser parses the embedded Route A fonts and builds a sized face for
// each. It fails if a font does not load or a face cannot be built at the pinned
// cell size.
func NewRasteriser() (*Rasteriser, error) {
	a, err := font.Load()
	if err != nil {
		return nil, fmt.Errorf("load atlas: %w", err)
	}
	opts := &opentype.FaceOptions{Size: faceSize, DPI: faceDPI, Hinting: xfont.HintingFull}
	primaryFace, err := opentype.NewFace(a.Primary, opts)
	if err != nil {
		return nil, fmt.Errorf("primary face: %w", err)
	}
	fallbackFace, err := opentype.NewFace(a.Fallback, opts)
	if err != nil {
		return nil, fmt.Errorf("fallback face: %w", err)
	}
	return &Rasteriser{
		atlas: a,
		faceByFont: map[*sfnt.Font]xfont.Face{
			a.Primary:  primaryFace,
			a.Fallback: fallbackFace,
		},
	}, nil
}

// Rasterise renders the grid to a fresh *image.RGBA of width Cols*8 and height
// Rows*16. Each cell is filled with its background colour, then its overlay
// text or bitmap glyph is drawn in the foreground colour. The result is
// deterministic: the same grid yields a byte-identical image across runs.
// Channel order is RGBA.
func (r *Rasteriser) Rasterise(g block.Grid) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, g.Cols*CellPxW, g.Rows*CellPxH))
	for row := range g.Rows {
		for col := range g.Cols {
			c := g.At(col, row)
			r.drawCell(img, col, row, *c)
		}
	}
	return img
}

// drawCell fills cell (col, row) with its background, then draws its text in the
// foreground colour. The text is the overlay grapheme when c.Text is set, else
// the bitmap glyph rune. A Wide trailing cell draws background only and returns,
// because the lead cell already drew the wide glyph clipped to its own cell. The
// baseline sits at baselineY within the cell; the text is clipped to the cell
// rectangle so it never bleeds into a neighbour.
func (r *Rasteriser) drawCell(img *image.RGBA, col, row int, c block.Cell) {
	rect := image.Rect(col*CellPxW, row*CellPxH, (col+1)*CellPxW, (row+1)*CellPxH)
	draw.Draw(img, rect, &image.Uniform{C: c.BG}, image.Point{}, draw.Src)
	if c.Wide {
		return
	}
	s := c.Text
	if s == "" {
		s = string(c.Glyph)
	}
	r.drawText(img, rect, s, c.FG)
}

// drawText draws s in the foreground colour fg over the already-filled rect, on
// the baseline at baselineY, clipped to rect so it never bleeds into a
// neighbouring cell. It resolves the face from the first rune of s through the
// same path drawCell and RenderGlyphCell share, so the rasterised cells and the
// matcher templates draw through one face. An empty s or a rune neither font
// covers leaves the rect as its background.
func (r *Rasteriser) drawText(img *image.RGBA, rect image.Rectangle, s string, fg color.RGBA) {
	if s == "" {
		return
	}
	runes := []rune(s)
	if len(runes) == 1 && drawShadeCell(img, rect, runes[0], fg) {
		return
	}
	face := r.faceFor(runes[0])
	if face == nil {
		return
	}
	d := xfont.Drawer{
		Dst:  clip{img: img, rect: rect},
		Src:  &image.Uniform{C: fg},
		Face: face,
		Dot: fixed.Point26_6{
			X: fixed.I(rect.Min.X),
			Y: fixed.I(rect.Min.Y + baselineY),
		},
	}
	d.DrawString(s)
}

// RenderGlyphCell draws one grapheme to a fresh CellPxW x CellPxH tile through
// the same face path drawCell uses, filling the background with bg and the text
// with fg. The matcher uses it to build per-grapheme templates that match the
// graded PNG exactly. The returned image is a single cell tile in RGBA order.
func (r *Rasteriser) RenderGlyphCell(text string, fg, bg color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, CellPxW, CellPxH))
	rect := img.Bounds()
	draw.Draw(img, rect, &image.Uniform{C: bg}, image.Point{}, draw.Src)
	r.drawText(img, rect, text, fg)
	return img
}

// drawShadeCell draws a Symbols for Legacy Computing shade candidate
// procedurally and reports whether it did. Neither embedded font covers
// U+1FB8C..U+1FB94, so the font path would leave those cells blank; the
// procedural path reads the same block.ShadedOpacity geometry the compose
// coverage tables are built from, one lattice point per raster pixel on the
// 8x16 cell, so the rasterised proof and the scored model agree. Medium shade
// draws as a rect-relative checkerboard, the deterministic model of a
// terminal's 50 percent dot pattern; Unicode fixes no dot phase. BMP glyphs
// (below U+1FB8C) stay on the font path, so every existing golden render is
// unchanged.
func drawShadeCell(img *image.RGBA, rect image.Rectangle, glyph rune, fg color.RGBA) bool {
	if glyph < 0x1FB8C {
		return false
	}
	if _, ok := block.ShadedOpacity(glyph, 0, 0); !ok {
		return false
	}
	for y := range CellPxH {
		for x := range CellPxW {
			op, _ := block.ShadedOpacity(glyph, x, y)
			if op == 2 || (op == 1 && (x+y)%2 == 0) {
				img.SetRGBA(rect.Min.X+x, rect.Min.Y+y, fg)
			}
		}
	}
	return true
}

// faceFor returns the face that covers glyph, preferring the primary. It returns
// nil when neither font covers glyph, so drawCell leaves the cell as its
// background.
func (r *Rasteriser) faceFor(glyph rune) xfont.Face {
	f, _, ok := r.atlas.Resolve(glyph)
	if !ok {
		return nil
	}
	return r.faceByFont[f]
}

// clip wraps an *image.RGBA so glyph drawing cannot write outside one cell. Set
// composites onto img only for points inside rect; reads pass through. This
// stops a tall or wide glyph from bleeding into a neighbouring cell.
type clip struct {
	img  *image.RGBA
	rect image.Rectangle
}

func (c clip) ColorModel() color.Model { return c.img.ColorModel() }
func (c clip) Bounds() image.Rectangle { return c.img.Bounds() }
func (c clip) At(x, y int) color.Color { return c.img.At(x, y) }
func (c clip) Set(x, y int, col color.Color) {
	if (image.Point{X: x, Y: y}).In(c.rect) {
		c.img.Set(x, y, col)
	}
}

// EncodePNG writes img to w as a timestamp-free PNG. The PNG encoder embeds no
// timestamp, so encoding the same image yields a byte-identical stream across
// runs, keeping golden frames stable.
func EncodePNG(w io.Writer, img image.Image) error {
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	return enc.Encode(w, img)
}
