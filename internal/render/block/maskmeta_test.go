package block

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"reflect"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

func legacyMaskAverage(rs, gs, bs, n int) color.RGBA {
	if n == 0 {
		return color.RGBA{A: 0xff}
	}
	return color.RGBA{
		R: uint8(rs / n), //nolint:gosec // test inputs are valid channel sums
		G: uint8(gs / n), //nolint:gosec // test inputs are valid channel sums
		B: uint8(bs / n), //nolint:gosec // test inputs are valid channel sums
		A: 0xff,
	}
}

func legacyQuadrantColours(mask int, cells [4]color.RGBA) (fg, bg color.RGBA) {
	if mask == 0 || mask == 15 {
		all := legacyMaskAverage(
			int(cells[0].R)+int(cells[1].R)+int(cells[2].R)+int(cells[3].R),
			int(cells[0].G)+int(cells[1].G)+int(cells[2].G)+int(cells[3].G),
			int(cells[0].B)+int(cells[1].B)+int(cells[2].B)+int(cells[3].B), 4)
		return all, all
	}
	var brs, bgs, bbs, bn, drs, dgs, dbs, dn int
	for i, c := range cells {
		if mask&(1<<(3-i)) != 0 {
			brs, bgs, bbs, bn = brs+int(c.R), bgs+int(c.G), bbs+int(c.B), bn+1
		} else {
			drs, dgs, dbs, dn = drs+int(c.R), dgs+int(c.G), dbs+int(c.B), dn+1
		}
	}
	return legacyMaskAverage(brs, bgs, bbs, bn), legacyMaskAverage(drs, dgs, dbs, dn)
}

func legacySextantColours(mask int, cells [6]color.RGBA) (fg, bg color.RGBA) {
	if mask == 0 || mask == 63 {
		var rs, gs, bs int
		for _, c := range cells {
			rs, gs, bs = rs+int(c.R), gs+int(c.G), bs+int(c.B)
		}
		all := legacyMaskAverage(rs, gs, bs, 6)
		return all, all
	}
	var brs, bgs, bbs, bn, drs, dgs, dbs, dn int
	for i, c := range cells {
		if mask&(1<<i) != 0 {
			brs, bgs, bbs, bn = brs+int(c.R), bgs+int(c.G), bbs+int(c.B), bn+1
		} else {
			drs, dgs, dbs, dn = drs+int(c.R), dgs+int(c.G), dbs+int(c.B), dn+1
		}
	}
	return legacyMaskAverage(brs, bgs, bbs, bn), legacyMaskAverage(drs, dgs, dbs, dn)
}

func TestAvgChannelsCountExact(t *testing.T) {
	for n := 1; n <= 6; n++ {
		for sum := 0; sum <= 255*n; sum++ {
			got := avgChannelsCount(sum, 255*n-sum, sum, uint8(n))
			want := legacyMaskAverage(sum, 255*n-sum, sum, n)
			if got != want {
				t.Fatalf("n=%d sum=%d: got %#v, want %#v", n, sum, got, want)
			}
		}
	}
}

func TestMaskColoursExact(t *testing.T) {
	sets := [][6]color.RGBA{
		{},
		{{R: 255, G: 255, B: 255}, {R: 1, G: 254, B: 0}, {R: 254, G: 1, B: 255}, {R: 0, G: 255, B: 1}, {R: 255, G: 0, B: 254}, {R: 1, G: 1, B: 1}},
		{{R: 17, G: 99, B: 201}, {R: 231, G: 8, B: 71}, {R: 44, G: 155, B: 3}, {R: 127, G: 254, B: 88}, {R: 9, G: 67, B: 245}, {R: 199, G: 101, B: 32}},
	}
	for _, cs := range sets {
		quad := [4]color.RGBA{cs[0], cs[1], cs[2], cs[3]}
		for mask := range 16 {
			gotFG, gotBG := quadrantColours(mask, quad[0], quad[1], quad[2], quad[3])
			wantFG, wantBG := legacyQuadrantColours(mask, quad)
			if gotFG != wantFG || gotBG != wantBG {
				t.Fatalf("quadrant mask %d: got %#v/%#v, want %#v/%#v", mask, gotFG, gotBG, wantFG, wantBG)
			}
		}
		for mask := range 64 {
			gotFG, gotBG := sextantColours(mask, cs[0], cs[1], cs[2], cs[3], cs[4], cs[5])
			wantFG, wantBG := legacySextantColours(mask, cs)
			if gotFG != wantFG || gotBG != wantBG {
				t.Fatalf("sextant mask %d: got %#v/%#v, want %#v/%#v", mask, gotFG, gotBG, wantFG, wantBG)
			}
		}
	}
}

func legacyBinarizeQuadrant(c [4]color.RGBA) (rune, color.RGBA, color.RGBA) {
	a, b, z, d := luma(c[0]), luma(c[1]), luma(c[2]), luma(c[3])
	lo, hi := min(min(a, b), min(z, d)), max(max(a, b), max(z, d))
	mid := lo + (hi-lo)/2
	mask := boolBit(a > mid)<<3 | boolBit(b > mid)<<2 | boolBit(z > mid)<<1 | boolBit(d > mid)
	fg, bg := legacyQuadrantColours(mask, c)
	return quadrantGlyphs[mask], fg, bg
}

func legacyBinarizeSextant(c [6]color.RGBA) (rune, color.RGBA, color.RGBA) {
	l := [6]int{luma(c[0]), luma(c[1]), luma(c[2]), luma(c[3]), luma(c[4]), luma(c[5])}
	lo, hi := l[0], l[0]
	for _, v := range l[1:] {
		lo, hi = min(lo, v), max(hi, v)
	}
	mid := lo + (hi-lo)/2
	mask := 0
	for i, v := range l {
		mask |= boolBit(v > mid) << i
	}
	fg, bg := legacySextantColours(mask, c)
	return sextantGlyphs[mask], fg, bg
}

func legacyCompose(br Renderer, frame render.Frame) Grid {
	frame = rgbaFrame(frame)
	g := Grid{Cols: br.Cols, Rows: br.Rows, Cells: make([]Cell, br.Cols*br.Rows)}
	img, _ := frame.Img.(*image.RGBA)
	for row := range br.Rows {
		for col := range br.Cols {
			switch br.Scheme {
			case SchemeSextant:
				c := [6]color.RGBA{}
				c[0], c[1], c[2], c[3], c[4], c[5] = sampleSextant(img, col, row)
				glyph, fg, bg := legacyBinarizeSextant(c)
				*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
			default:
				c := [4]color.RGBA{}
				c[0], c[1], c[2], c[3] = sampleQuadrant(img, col, row)
				glyph, fg, bg := legacyBinarizeQuadrant(c)
				*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
			}
		}
	}
	applyOverlay(&g, frame, br.Scheme)
	return g
}

func TestMaskMetadataCompleteGridExact(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test fixture
	makeImage := func(rect image.Rectangle) *image.RGBA {
		img := image.NewRGBA(rect)
		if _, err := rng.Read(img.Pix); err != nil {
			t.Fatal(err)
		}
		return img
	}
	base := makeImage(image.Rect(0, 0, 12, 12))
	inputs := []render.Frame{
		{},
		{Img: image.NewRGBA(image.Rect(0, 0, 0, 0))},
		{Img: makeImage(image.Rect(0, 0, 3, 3))},
		{Img: makeImage(image.Rect(0, 0, 10, 16))},
		{Img: makeImage(image.Rect(0, 0, 7, 5))},
		{Img: base.SubImage(image.Rect(2, 1, 9, 8))},
	}
	for range 16 {
		inputs = append(inputs, render.Frame{Img: makeImage(image.Rect(0, 0, 1+rng.Intn(13), 1+rng.Intn(13)))})
	}
	for i := range inputs {
		inputs[i].Text = []render.GlyphRun{{Col: 0, Row: 0, Text: "A界", FG: color.RGBA{R: 254, G: 1, B: 255, A: 0xff}, X: 1, W: 15, CellW: 8, DisplayCells: 3}}
	}
	for _, scheme := range []Scheme{SchemeQuadrant, SchemeSextant} {
		br := Renderer{Cols: 5, Rows: 4, Scheme: scheme}
		for i, frame := range inputs {
			got, want := br.Compose(frame), legacyCompose(br, frame)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scheme %d input %d grid differs", scheme, i)
			}
			var gotBytes, wantBytes bytes.Buffer
			if err := (&GridDiff{}).EmitTrueColour(&gotBytes, got); err != nil {
				t.Fatal(err)
			}
			if err := (&GridDiff{}).EmitTrueColour(&wantBytes, want); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotBytes.Bytes(), wantBytes.Bytes()) {
				t.Fatalf("scheme %d input %d terminal bytes differ", scheme, i)
			}
		}
	}
}
