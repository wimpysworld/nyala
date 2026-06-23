package block

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

func TestXTerm256GreyRamp(t *testing.T) {
	cases := []struct {
		name string
		grey uint8
		want uint8
	}{
		{"black below ramp", 0, 16},
		{"ramp low edge", 8, 232},
		{"mid grey 128", 128, 244},
		{"ramp high", 238, 255},
		{"white above ramp", 255, 231},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := XTerm256(color.RGBA{R: c.grey, G: c.grey, B: c.grey, A: 0xff})
			if got != c.want {
				t.Fatalf("grey %d: got index %d, want %d", c.grey, got, c.want)
			}
			// Pure grey must land on the ramp (232..255) or a cube grey
			// endpoint (16 black, 231 white), never a chromatic cube cell.
			isGrey := got == 16 || got == 231 || got >= 232
			if !isGrey {
				t.Fatalf("grey %d mapped to non-grey cube index %d", c.grey, got)
			}
		})
	}
}

func TestXTerm256SaturatedPrimaries(t *testing.T) {
	cases := []struct {
		name string
		in   color.RGBA
		want uint8
	}{
		// Cube index = 16 + 36*r + 6*g + b, axis 5 for a saturated 255 channel.
		{"red", color.RGBA{R: 255, A: 0xff}, 196},
		{"green", color.RGBA{G: 255, A: 0xff}, 46},
		{"blue", color.RGBA{B: 255, A: 0xff}, 21},
		{"yellow", color.RGBA{R: 255, G: 255, A: 0xff}, 226},
		{"magenta", color.RGBA{R: 255, B: 255, A: 0xff}, 201},
		{"cyan", color.RGBA{G: 255, B: 255, A: 0xff}, 51},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := XTerm256(c.in)
			if got != c.want {
				t.Fatalf("%s: got index %d, want %d", c.name, got, c.want)
			}
		})
	}
}

// TestXTerm256ChannelOrder asserts RGBA byte order. A pixel saturated only on
// red must select the red cube axis (index 196), not green (46) or blue (21).
// A channel swap would surface as the wrong index here.
func TestXTerm256ChannelOrder(t *testing.T) {
	redOnly := color.RGBA{R: 255, G: 0, B: 0, A: 0xff}
	if got := XTerm256(redOnly); got != 196 {
		t.Fatalf("RGBA red pixel: got index %d, want 196 (channel order wrong?)", got)
	}
	blueOnly := color.RGBA{R: 0, G: 0, B: 255, A: 0xff}
	if got := XTerm256(blueOnly); got != 21 {
		t.Fatalf("RGBA blue pixel: got index %d, want 21 (channel order wrong?)", got)
	}
}

func TestTrueColorSGR(t *testing.T) {
	c := color.RGBA{R: 18, G: 52, B: 86, A: 0xff}
	if got, want := TrueColorFG(c), "\x1b[38;2;18;52;86m"; got != want {
		t.Fatalf("foreground SGR: got %q, want %q", got, want)
	}
	if got, want := TrueColorBG(c), "\x1b[48;2;18;52;86m"; got != want {
		t.Fatalf("background SGR: got %q, want %q", got, want)
	}
}

var (
	bright = color.RGBA{R: 240, G: 240, B: 240, A: 0xff}
	dark   = color.RGBA{R: 10, G: 10, B: 10, A: 0xff}
)

// TestBinarizeQuadrantSplit checks the luma-midpoint split and the resulting
// glyph and fg/bg pair for known sub-cell colour layouts. Each row sets the four
// quadrants (x top-left, y top-right, z bottom-left, w bottom-right) and asserts
// the glyph and the (fg, bg) the Carbonyl-ported algorithm selects.
func TestBinarizeQuadrantSplit(t *testing.T) {
	cases := []struct {
		name           string
		x, y, z, w     color.RGBA
		wantGlyph      rune
		wantFG, wantBG color.RGBA
	}{
		{
			// Top bright, bottom dark: mask 1100, upper half block. fg averages
			// the bright top pair, bg the dark bottom pair.
			name: "top bright bottom dark", x: bright, y: bright, z: dark, w: dark,
			wantGlyph: 0x2580, wantFG: avgRGBA(bright, bright), wantBG: avgRGBA(dark, dark),
		},
		{
			// Only top-left bright: mask 1000, upper-left quadrant. fg is the lone
			// bright top-left, bg the average of the three dark cells.
			name: "top-left bright only", x: bright, y: dark, z: dark, w: dark,
			wantGlyph: 0x2598, wantFG: bright, wantBG: avgRGBA(dark, dark, dark),
		},
		{
			// Left column bright: mask 1010, left half block. fg averages the
			// bright left pair, bg the dark right pair.
			name: "left column bright", x: bright, y: dark, z: bright, w: dark,
			wantGlyph: 0x258C, wantFG: avgRGBA(bright, bright), wantBG: avgRGBA(dark, dark),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			glyph, fg, bg := binarizeQuadrant(c.x, c.y, c.z, c.w)
			if glyph != c.wantGlyph {
				t.Fatalf("glyph: got %#U, want %#U", glyph, c.wantGlyph)
			}
			if fg != c.wantFG {
				t.Fatalf("fg: got %v, want %v", fg, c.wantFG)
			}
			if bg != c.wantBG {
				t.Fatalf("bg: got %v, want %v", bg, c.wantBG)
			}
		})
	}
}

// TestComposeQuadrantNotInverted catches the inversion bug directly. For each
// single-bright-quadrant case it builds a 2x4 image with one sub-cell white and
// the other three black, composes a 1x1 quadrant grid, and asserts the glyph is
// the single-quadrant rune for that position, the foreground is the bright white
// (each channel above the midpoint), and the background is the dark black. An
// inverted cell would paint the bright quadrant dark, so the foreground would
// come back black; this test fails on that.
func TestComposeQuadrantNotInverted(t *testing.T) {
	white := color.RGBA{R: 255, G: 255, B: 255, A: 0xff}
	black := color.RGBA{R: 0, G: 0, B: 0, A: 0xff}

	// Sub-cell pixel origins in the 2x4 block. Each sub-cell is 1 px wide, 2 tall.
	type quad struct {
		name      string
		px, py    int // top pixel of the bright sub-cell column
		wantGlyph rune
	}
	cases := []quad{
		{"top-left", 0, 0, 0x2598},
		{"top-right", 1, 0, 0x259D},
		{"bottom-left", 0, 2, 0x2596},
		{"bottom-right", 1, 2, 0x2597},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := fillRGBA(cellPxW, cellPxH, black)
			set := func(x, y int, col color.RGBA) {
				i := img.PixOffset(x, y)
				img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = col.R, col.G, col.B, col.A
			}
			set(c.px, c.py, white)
			set(c.px, c.py+1, white)

			br := &Renderer{Cols: 1, Rows: 1}
			g := br.Compose(render.Frame{Img: img})
			cell := g.At(0, 0)
			if cell.Glyph != c.wantGlyph {
				t.Fatalf("glyph: got %#U, want %#U", cell.Glyph, c.wantGlyph)
			}
			if cell.FG.R <= 127 || cell.FG.G <= 127 || cell.FG.B <= 127 {
				t.Fatalf("fg: got %v, want bright white (inverted?)", cell.FG)
			}
			if cell.BG != black {
				t.Fatalf("bg: got %v, want dark black %v", cell.BG, black)
			}
		})
	}
}

// TestComposeQuadrantTwoBright checks a two-quadrant split keeps fg and bg
// unswapped. Top-left and top-right white, bottom pair black: mask 1100, upper
// half block, foreground white, background black.
func TestComposeQuadrantTwoBright(t *testing.T) {
	white := color.RGBA{R: 255, G: 255, B: 255, A: 0xff}
	black := color.RGBA{R: 0, G: 0, B: 0, A: 0xff}
	img := fillRGBA(cellPxW, cellPxH, black)
	set := func(x, y int, col color.RGBA) {
		i := img.PixOffset(x, y)
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = col.R, col.G, col.B, col.A
	}
	// Both top sub-cells (rows 0 and 1, columns 0 and 1) white.
	set(0, 0, white)
	set(0, 1, white)
	set(1, 0, white)
	set(1, 1, white)

	br := &Renderer{Cols: 1, Rows: 1}
	g := br.Compose(render.Frame{Img: img})
	cell := g.At(0, 0)
	if cell.Glyph != 0x2580 {
		t.Fatalf("glyph: got %#U, want U+2580 upper half", cell.Glyph)
	}
	if cell.FG != white {
		t.Fatalf("fg: got %v, want white (swapped?)", cell.FG)
	}
	if cell.BG != black {
		t.Fatalf("bg: got %v, want black (swapped?)", cell.BG)
	}
}

// fillRGBA returns a w x h opaque image filled with one colour.
func fillRGBA(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = c.R
		img.Pix[i+1] = c.G
		img.Pix[i+2] = c.B
		img.Pix[i+3] = c.A
	}
	return img
}

// TestComposeTwoColourCell builds a synthetic one-cell frame whose top 2 rows
// are bright and bottom 2 rows are dark, then asserts the composed cell carries
// the upper-half-block glyph and the bright/dark fg/bg pair. The bright top set
// is the foreground, so the glyph fills the upper half.
func TestComposeTwoColourCell(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, cellPxW, cellPxH))
	for y := 0; y < cellPxH; y++ {
		c := bright
		if y >= cellPxH/2 {
			c = dark
		}
		for x := 0; x < cellPxW; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
		}
	}
	br := &Renderer{Cols: 1, Rows: 1}
	g := br.Compose(render.Frame{Img: img})
	cell := g.At(0, 0)
	if cell.Glyph != 0x2580 {
		t.Fatalf("glyph: got %#U, want U+2580", cell.Glyph)
	}
	if cell.FG != bright {
		t.Fatalf("fg: got %v, want bright %v", cell.FG, bright)
	}
	if cell.BG != dark {
		t.Fatalf("bg: got %v, want dark %v", cell.BG, dark)
	}
}

// TestComposeGridDimensions asserts the composed grid matches the requested
// cols x rows and stores exactly cols*rows cells.
func TestComposeGridDimensions(t *testing.T) {
	br := &Renderer{Cols: 7, Rows: 3}
	g := br.Compose(render.Frame{Img: fillRGBA(7*cellPxW, 3*cellPxH, bright)})
	if g.Cols != 7 || g.Rows != 3 {
		t.Fatalf("dimensions: got %dx%d, want 7x3", g.Cols, g.Rows)
	}
	if len(g.Cells) != 21 {
		t.Fatalf("cell count: got %d, want 21", len(g.Cells))
	}
}

// TestRenderDeterministic asserts the same Frame renders to byte-identical
// output across runs.
func TestRenderDeterministic(t *testing.T) {
	img := fillRGBA(4*cellPxW, 2*cellPxH, color.RGBA{R: 200, G: 120, B: 40, A: 0xff})
	br := &Renderer{Cols: 4, Rows: 2}
	var a, b bytes.Buffer
	if err := br.Render(render.Frame{Img: img}, &a); err != nil {
		t.Fatalf("render a: %v", err)
	}
	if err := br.Render(render.Frame{Img: img}, &b); err != nil {
		t.Fatalf("render b: %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("non-deterministic output: %q vs %q", a.String(), b.String())
	}
	if a.Len() == 0 {
		t.Fatal("empty render output")
	}
}

// TestRenderEmitsText asserts Render writes the overlay grapheme, not the bitmap
// rune, for a text cell, and emits nothing for a Wide trailing cell.
func TestRenderEmitsText(t *testing.T) {
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff}
	g := Grid{Cols: 2, Rows: 1, Cells: []Cell{
		{Glyph: 0x2588, FG: fg, BG: bg, Text: "Q"},
		{Glyph: 0x2588, FG: fg, BG: bg, Wide: true},
	}}
	var buf bytes.Buffer
	br := &Renderer{Cols: 2, Rows: 1}
	if err := br.renderGrid(g, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Q") {
		t.Fatalf("output %q does not contain the overlay grapheme Q", out)
	}
	if strings.ContainsRune(out, 0x2588) {
		t.Fatalf("output %q contains the bitmap rune; overlay must win", out)
	}
}

// TestRenderWideTrailingCellEmitsNothing places a width-2 grapheme so its lead
// cell holds the grapheme and the next cell is the Wide trailing cell. The
// rendered bytes must contain the grapheme exactly once and the trailing cell
// must contribute no printable character: in a real terminal the lead cell's
// wide grapheme already paints both columns, so a space would add a third column
// and misalign the row.
func TestRenderWideTrailingCellEmitsNothing(t *testing.T) {
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff}
	g := Grid{Cols: 2, Rows: 1, Cells: []Cell{
		{Glyph: 0x2588, FG: fg, BG: bg, Text: "世"},
		{Glyph: 0x2588, FG: fg, BG: bg, Wide: true},
	}}
	var buf bytes.Buffer
	br := &Renderer{Cols: 2, Rows: 1}
	if err := br.renderGrid(g, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, "世"); got != 1 {
		t.Fatalf("grapheme count = %d, want 1", got)
	}
	// The Wide trailing cell emits no SGR and no rune, so a single fg/bg SGR run
	// (the lead cell's) precedes the grapheme and the row carries no space.
	if strings.ContainsRune(out, ' ') {
		t.Fatalf("output %q contains a space; the Wide trailing cell must emit nothing", out)
	}
}

// TestRendererName asserts the backend identifier.
func TestRendererName(t *testing.T) {
	br := &Renderer{}
	if got := br.Name(); got != "block" {
		t.Fatalf("Name() = %q, want %q", got, "block")
	}
}

// TestComposeHalfBlockCell builds a synthetic one-cell half-block frame whose top
// source pixel is bright and bottom pixel is dark, then asserts the cell carries
// the lower-half-block glyph with fg = bottom pixel and bg = top pixel.
func TestComposeHalfBlockCell(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, halfCellPxW, halfCellPxH))
	setPx := func(x, y int, c color.RGBA) {
		i := img.PixOffset(x, y)
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	setPx(0, 0, bright) // top pixel -> background
	setPx(0, 1, dark)   // bottom pixel -> foreground
	br := &Renderer{Cols: 1, Rows: 1, Scheme: SchemeHalfBlock}
	g := br.Compose(render.Frame{Img: img})
	cell := g.At(0, 0)
	if cell.Glyph != 0x2584 {
		t.Fatalf("glyph: got %#U, want U+2584", cell.Glyph)
	}
	if cell.FG != dark {
		t.Fatalf("fg: got %v, want bottom pixel dark %v", cell.FG, dark)
	}
	if cell.BG != bright {
		t.Fatalf("bg: got %v, want top pixel bright %v", cell.BG, bright)
	}
}

// TestToggleScheme asserts the toggle flips between the two schemes and never
// touches anything but the concrete Renderer field.
func TestToggleScheme(t *testing.T) {
	br := &Renderer{}
	if br.Scheme != SchemeQuadrant {
		t.Fatalf("zero value: got scheme %d, want SchemeQuadrant", br.Scheme)
	}
	if got := br.ToggleScheme(); got != SchemeHalfBlock || br.Scheme != SchemeHalfBlock {
		t.Fatalf("first toggle: got %d, want SchemeHalfBlock", got)
	}
	if got := br.ToggleScheme(); got != SchemeQuadrant || br.Scheme != SchemeQuadrant {
		t.Fatalf("second toggle: got %d, want SchemeQuadrant", got)
	}
}

// validGrid checks a composed grid matches the requested cols x rows, stores
// exactly cols*rows cells, and every cell carries a non-zero glyph.
func validGrid(t *testing.T, g Grid, cols, rows int) {
	t.Helper()
	if g.Cols != cols || g.Rows != rows {
		t.Fatalf("dimensions: got %dx%d, want %dx%d", g.Cols, g.Rows, cols, rows)
	}
	if len(g.Cells) != cols*rows {
		t.Fatalf("cell count: got %d, want %d", len(g.Cells), cols*rows)
	}
	for i := range g.Cells {
		if g.Cells[i].Glyph == 0 {
			t.Fatalf("cell %d has a zero glyph", i)
		}
	}
}

// TestComposeBothSchemes renders the same Frame in both schemes and asserts each
// produces a valid grid. The Frame is sized to the quadrant footprint, the larger
// of the two, so the half-block sampler stays in bounds too.
func TestComposeBothSchemes(t *testing.T) {
	const cols, rows = 4, 3
	img := fillRGBA(cols*cellPxW, rows*cellPxH, color.RGBA{R: 200, G: 120, B: 40, A: 0xff})
	f := render.Frame{Img: img}

	quad := &Renderer{Cols: cols, Rows: rows, Scheme: SchemeQuadrant}
	validGrid(t, quad.Compose(f), cols, rows)

	half := &Renderer{Cols: cols, Rows: rows, Scheme: SchemeHalfBlock}
	hg := half.Compose(f)
	validGrid(t, hg, cols, rows)
	for i := range hg.Cells {
		if hg.Cells[i].Glyph != 0x2584 {
			t.Fatalf("half-block cell %d: got %#U, want U+2584", i, hg.Cells[i].Glyph)
		}
	}
}

// TestSampleHalfBlockChannelOrder asserts RGBA channel order through the half-
// block sampler. A cell whose source pixels are pure red must surface red fg and
// bg, never green or blue. A channel swap would surface here.
func TestSampleHalfBlockChannelOrder(t *testing.T) {
	red := color.RGBA{R: 255, G: 0, B: 0, A: 0xff}
	img := fillRGBA(halfCellPxW, halfCellPxH, red)
	top, bottom := sampleHalfBlock(img, 0, 0)
	for _, q := range []color.RGBA{top, bottom} {
		if q.R != 255 || q.G != 0 || q.B != 0 {
			t.Fatalf("RGBA order: got %v, want pure red (255,0,0)", q)
		}
	}
}

// TestSampleQuadrantChannelOrder asserts RGBA channel order through the sampler.
// A cell whose source pixels are pure red must surface as a red fg/bg, never
// green or blue. A channel swap would surface here.
func TestSampleQuadrantChannelOrder(t *testing.T) {
	red := color.RGBA{R: 255, G: 0, B: 0, A: 0xff}
	img := fillRGBA(cellPxW, cellPxH, red)
	x, y, z, w := sampleQuadrant(img, 0, 0)
	for _, q := range []color.RGBA{x, y, z, w} {
		if q.R != 255 || q.G != 0 || q.B != 0 {
			t.Fatalf("RGBA order: got %v, want pure red (255,0,0)", q)
		}
	}
	_, fg, bg := binarizeQuadrant(x, y, z, w)
	if fg.R != 255 || fg.G != 0 || fg.B != 0 {
		t.Fatalf("fg channel order: got %v, want red", fg)
	}
	if bg.R != 255 || bg.G != 0 || bg.B != 0 {
		t.Fatalf("bg channel order: got %v, want red", bg)
	}
}

// TestRenderRowsEndCRLF asserts Render terminates every row with CRLF, not a
// bare LF. With auto-wrap off in a raw terminal, the carriage return is needed
// to return the cursor to column 0 each row; a bare LF leaves it interlaced.
func TestRenderRowsEndCRLF(t *testing.T) {
	br := Renderer{Cols: 2, Rows: 2}
	img := image.NewRGBA(image.Rect(0, 0, 4, 8))
	var buf bytes.Buffer
	if err := br.Render(render.Frame{Img: img}, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.Bytes()
	if got := bytes.Count(out, []byte("\n")); got != br.Rows {
		t.Fatalf("line feed count = %d, want %d (one per row)", got, br.Rows)
	}
	for i, b := range out {
		if b == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("line feed at byte %d not preceded by carriage return", i)
		}
	}
}
