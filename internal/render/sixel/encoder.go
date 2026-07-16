// This file derives from mattn/go-sixel v0.0.12, sixel.go.
// Copyright (c) 2021 Yasuhiro Matsumoto. See LICENSE.go-sixel.

package sixel

import (
	"image"
	"image/color"
	"image/draw"
	"io"
	"strconv"

	"github.com/soniakeys/quant/median"
)

const (
	specialChNr = byte(0x6d)
	specialChCr = byte(0x64)
)

// opaqueEncoder is the subset of mattn/go-sixel's encoder used by Nyala.
// CDP frames are composited and opaque, so it omits alpha handling.
type opaqueEncoder struct {
	w io.Writer

	outScratch    []byte
	bitsetScratch []byte
	seenScratch   []uint16
	seenGen       uint16
}

func newOpaqueEncoder(w io.Writer) *opaqueEncoder { return &opaqueEncoder{w: w} }

func (e *opaqueEncoder) encode(img image.Image) error {
	const colours = 256
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	if width == 0 || height == 0 {
		return nil
	}
	srcBounds := img.Bounds()
	var paletted *image.Paletted
	switch p := img.(type) {
	case *image.Paletted:
		if len(p.Palette) <= colours {
			paletted = p
		}
	case *image.NRGBA:
		paletted = palettedFromNRGBA(p, colours-1)
	case *image.RGBA:
		paletted = palettedFromRGBA(p, colours-1)
	}
	if paletted == nil {
		rgba := toRGBA(img)
		palette := samplePalette(rgba, colours-1)
		paletted = image.NewPaletted(rgba.Bounds(), palette)
		mapPaletted(paletted, rgba, newPaletteLUT(palette))
	}

	paletteSize := len(paletted.Palette)
	out := e.outScratch[:0]
	if outCap := width*height/2 + paletteSize*16 + 64; cap(out) < outCap {
		out = make([]byte, 0, outCap)
	}
	out = append(out, "\033P0;0;8q\"1;1;"...)
	out = strconv.AppendInt(out, int64(width), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(height), 10)
	for n, colour := range paletted.Palette {
		r, g, b, _ := colour.RGBA()
		out = appendColourRegister(out, n, r*100/0xffff, g*100/0xffff, b*100/0xffff)
	}

	bufSize := width * paletteSize
	buf := e.bitsetScratch
	if cap(buf) < bufSize {
		buf = make([]byte, bufSize)
	} else {
		buf = buf[:bufSize]
		clear(buf)
	}
	seen := e.seenScratch
	if cap(seen) < paletteSize {
		seen = make([]uint16, paletteSize)
	} else {
		seen = seen[:paletteSize]
	}
	ch0 := specialChNr
	for z := range (height + 5) / 6 {
		e.seenGen++
		if e.seenGen == 0 {
			clear(seen)
			e.seenGen = 1
		}
		gen := e.seenGen
		if z > 0 {
			out = append(out, '-')
		}
		for p := range 6 {
			y := z*6 + p
			if y >= srcBounds.Dy() {
				continue
			}
			rowMask := byte(1 << uint(p))
			offset := paletted.PixOffset(srcBounds.Min.X, srcBounds.Min.Y+y)
			row := paletted.Pix[offset : offset+srcBounds.Dx()]
			for x, pix := range row {
				idx := int(pix)
				seen[idx] = gen
				buf[width*idx+x] |= rowMask
			}
		}
		for n := range paletteSize {
			if seen[n] != gen {
				continue
			}
			if ch0 == specialChCr {
				out = append(out, '$')
			}
			out = appendColourSelect(out, n)
			count := 0
			base := width * n
			for x := range width {
				ch := buf[base+x]
				buf[base+x] = 0
				if ch0 < 0x40 && ch != ch0 {
					out = appendRun(out, ch0, count)
					count = 0
				}
				ch0 = ch
				count++
			}
			if ch0 != 0 {
				out = appendRun(out, ch0, count)
			}
			ch0 = specialChCr
		}
	}
	out = append(out, 0x1b, 0x5c)
	e.outScratch = out[:0]
	e.bitsetScratch = buf
	e.seenScratch = seen
	_, err := e.w.Write(out)
	return err
}

func appendColourRegister(dst []byte, n int, r, g, b uint32) []byte {
	dst = append(dst, '#')
	dst = strconv.AppendInt(dst, int64(n), 10)
	dst = append(dst, ';', '2', ';')
	dst = strconv.AppendInt(dst, int64(r), 10)
	dst = append(dst, ';')
	dst = strconv.AppendInt(dst, int64(g), 10)
	dst = append(dst, ';')
	dst = strconv.AppendInt(dst, int64(b), 10)
	return dst
}

func appendColourSelect(dst []byte, n int) []byte {
	dst = append(dst, '#')
	return strconv.AppendInt(dst, int64(n), 10)
}

func appendRun(dst []byte, ch byte, count int) []byte {
	if count == 0 {
		return dst
	}
	sixel := 63 + ch
	for ; count > 255; count -= 255 {
		dst = append(dst, '!', '2', '5', '5', sixel)
	}
	switch count {
	case 1:
		return append(dst, sixel)
	case 2:
		return append(dst, sixel, sixel)
	case 3:
		return append(dst, sixel, sixel, sixel)
	default:
		dst = append(dst, '!')
		dst = strconv.AppendInt(dst, int64(count), 10)
		return append(dst, sixel)
	}
}

func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	return rgba
}

func samplePalette(rgba *image.RGBA, maxColours int) color.Palette {
	const budget = 1 << 18
	bounds := rgba.Bounds()
	src := image.Image(rgba)
	if bounds.Dx()*bounds.Dy() > budget {
		step := 2
		for (bounds.Dx()/step)*(bounds.Dy()/step) > budget {
			step++
		}
		width, height := bounds.Dx()/step, bounds.Dy()/step
		sample := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := range height {
			dstOffset := sample.PixOffset(0, y)
			for x := range width {
				srcOffset := rgba.PixOffset(bounds.Min.X+x*step, bounds.Min.Y+y*step)
				copy(sample.Pix[dstOffset:dstOffset+4], rgba.Pix[srcOffset:srcOffset+4])
				dstOffset += 4
			}
		}
		src = sample
	}
	return median.Quantizer(0).Quantize(make(color.Palette, 0, maxColours), src)
}

func newPaletteLUT(palette color.Palette) []uint8 {
	var pr, pg, pb [256]int32
	for i, colour := range palette {
		r, g, b, _ := colour.RGBA()
		pr[i], pg[i], pb[i] = int32(r>>8), int32(g>>8), int32(b>>8)
	}
	lut := make([]uint8, 1<<15)
	for i := range lut {
		r := int32(i>>10&31<<3 | 4) //nolint:gosec // A 15-bit LUT index bounds this to 252.
		g := int32(i>>5&31<<3 | 4)  //nolint:gosec // A 15-bit LUT index bounds this to 252.
		b := int32(i&31<<3 | 4)     //nolint:gosec // A 15-bit LUT index bounds this to 252.
		best, bestDistance := 0, int32(1<<31-1)
		for j := range palette {
			dr, dg, db := r-pr[j], g-pg[j], b-pb[j]
			if distance := dr*dr + dg*dg + db*db; distance < bestDistance {
				best, bestDistance = j, distance
			}
		}
		lut[i] = uint8(best)
	}
	return lut
}

func mapPaletted(dst *image.Paletted, src *image.RGBA, lut []uint8) {
	bounds := src.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		srcOffset := src.PixOffset(bounds.Min.X, y)
		dstOffset := dst.PixOffset(bounds.Min.X, y)
		for range bounds.Dx() {
			dst.Pix[dstOffset] = lut[int(src.Pix[srcOffset]>>3)<<10|int(src.Pix[srcOffset+1]>>3)<<5|int(src.Pix[srcOffset+2]>>3)]
			srcOffset += 4
			dstOffset++
		}
	}
}

func palettedFromNRGBA(img *image.NRGBA, maxColours int) *image.Paletted {
	return palettedFromOpaquePixels(img.Bounds(), img.Pix, img.Stride, img.PixOffset, maxColours)
}

func palettedFromRGBA(img *image.RGBA, maxColours int) *image.Paletted {
	return palettedFromOpaquePixels(img.Bounds(), img.Pix, img.Stride, img.PixOffset, maxColours)
}

func palettedFromOpaquePixels(bounds image.Rectangle, pix []byte, stride int, offset func(int, int) int, maxColours int) *image.Paletted {
	palette := make(color.Palette, 1, maxColours+1)
	palette[0] = color.NRGBA{}
	indexes := make(map[uint32]uint8, maxColours)
	dst := image.NewPaletted(bounds, palette)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		srcOffset := offset(bounds.Min.X, y)
		dstOffset := dst.PixOffset(bounds.Min.X, y)
		row := pix[srcOffset : srcOffset+bounds.Dx()*4 : srcOffset+stride]
		for x := range bounds.Dx() {
			base := x * 4
			key := uint32(row[base])<<24 | uint32(row[base+1])<<16 | uint32(row[base+2])<<8 | 0xff
			idx, ok := indexes[key]
			if !ok {
				if len(palette) > maxColours {
					return nil
				}
				idx = uint8(len(palette)) //nolint:gosec // The 255-colour limit bounds the index.
				indexes[key] = idx
				palette = append(palette, color.NRGBA{R: row[base], G: row[base+1], B: row[base+2], A: 0xff})
			}
			dst.Pix[dstOffset+x] = idx
		}
	}
	dst.Palette = palette
	return dst
}
