// This file derives from mattn/go-sixel v0.0.12, sixel.go.
// Copyright (c) 2021 Yasuhiro Matsumoto. See LICENSE.go-sixel.

package sixel

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"runtime"
	"strconv"
	"sync"

	"github.com/soniakeys/quant/median"
)

const (
	specialChNr = byte(0x6d)
	specialChCr = byte(0x64)
	repeatLimit = 8192

	parallelBandWorkThreshold = 39_000_000
	parallelMapPixelThreshold = 500_000
)

type bandScratch struct {
	out    []byte
	bitset []byte
	seen   []uint16
	right  []int
	gen    uint16
}

// opaqueEncoder is the subset of mattn/go-sixel's encoder used by Nyala.
// CDP frames are composited and opaque, so it omits alpha handling.
// It is stateful and must not be used concurrently.
type opaqueEncoder struct {
	w io.Writer

	outScratch []byte
	band       []bandScratch
	rgba       *image.RGBA
	sample     *image.RGBA
	paletted   *image.Paletted
	palette    color.Palette
	lut        []uint8
}

func newOpaqueEncoder(w io.Writer) *opaqueEncoder { return &opaqueEncoder{w: w} }

func (e *opaqueEncoder) encode(img image.Image) error {
	const colours = 256
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return nil
	}
	e.dropResizedScratch(bounds)
	var paletted *image.Paletted
	var rgba *image.RGBA
	var exact bool
	switch p := img.(type) {
	case *image.Paletted:
		if len(p.Palette) <= colours {
			paletted = p
			exact = true
		} else {
			rgba = e.toRGBA(p)
		}
	case *image.NRGBA:
		paletted, exact = palettedFromOpaquePixelsInto(e.paletted, p.Bounds(), p.Pix, p.PixOffset, colours-1)
		e.paletted = paletted
		if !exact {
			rgba = e.toRGBA(p)
		}
	case *image.RGBA:
		paletted, exact = palettedFromOpaquePixelsInto(e.paletted, p.Bounds(), p.Pix, p.PixOffset, colours-1)
		e.paletted = paletted
		rgba = p
	case *image.YCbCr:
		rgba = e.toRGBA(p)
	default:
		return fmt.Errorf("unsupported image type %T", img)
	}
	if !exact {
		e.palette, e.sample = samplePaletteInto(rgba, colours-1, e.palette, e.sample)
		palette := e.palette
		if paletted == nil {
			paletted = reusablePaletted(e.paletted, rgba.Bounds(), palette)
			e.paletted = paletted
		} else {
			paletted.Palette = palette
		}
		e.lut = paletteLUT(e.lut, palette)
		mapPaletted(paletted, rgba, e.lut)
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

	out = e.appendBands(out, paletted)
	out = append(out, 0x1b, 0x5c)
	e.outScratch = out[:0]
	_, err := e.w.Write(out)
	return err
}

func (e *opaqueEncoder) dropResizedScratch(bounds image.Rectangle) {
	if e.rgba != nil && e.rgba.Bounds() != bounds {
		e.rgba = nil
	}
	if e.paletted != nil && e.paletted.Bounds() != bounds {
		e.paletted = nil
	}
}

func (e *opaqueEncoder) appendBands(dst []byte, paletted *image.Paletted) []byte {
	bounds := paletted.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	bandCount := (height + 5) / 6
	workers := sixelWorkerCount()
	if !parallelBands(len(paletted.Palette), width, bandCount, workers) ||
		!parallelBands(presentColourCount(paletted), width, bandCount, workers) {
		e.growBandScratch(1)
		return appendBandRange(dst, paletted, &e.band[0], 0, bandCount)
	}
	return e.appendBandsParallel(dst, paletted, workers)
}

func (e *opaqueEncoder) appendBandsParallel(dst []byte, paletted *image.Paletted, workers int) []byte {
	height := paletted.Bounds().Dy()
	bandCount := (height + 5) / 6
	completeBands := height / 6
	workers = min(workers, completeBands)
	e.growBandScratch(workers)
	var wg sync.WaitGroup
	for worker := range workers {
		lo, hi := completeBands*worker/workers, completeBands*(worker+1)/workers
		wg.Go(func() {
			scratch := &e.band[worker]
			scratch.out = appendBandRange(scratch.out[:0], paletted, scratch, lo, hi)
		})
	}
	wg.Wait()
	for worker := range workers {
		dst = append(dst, e.band[worker].out...)
	}
	if completeBands < bandCount {
		dst = appendBandRange(dst, paletted, &e.band[0], completeBands, bandCount)
	}
	return dst
}

func (e *opaqueEncoder) growBandScratch(count int) {
	if cap(e.band) < count {
		e.band = append(e.band[:cap(e.band)], make([]bandScratch, count-cap(e.band))...)
	}
	e.band = e.band[:count]
}

func parallelBands(presentColours, width, bandCount, workers int) bool {
	return workers > 1 && bandCount >= 120 && presentColours*width*bandCount >= parallelBandWorkThreshold
}

func presentColourCount(paletted *image.Paletted) int {
	var present [256]bool
	count := 0
	bounds := paletted.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		offset := paletted.PixOffset(bounds.Min.X, y)
		for _, pix := range paletted.Pix[offset : offset+bounds.Dx()] {
			if !present[pix] {
				present[pix] = true
				count++
			}
		}
	}
	return count
}

func appendBandRange(dst []byte, paletted *image.Paletted, scratch *bandScratch, first, last int) []byte {
	srcBounds := paletted.Bounds()
	width := srcBounds.Dx()
	paletteSize := len(paletted.Palette)
	bufSize := width * paletteSize
	buf := scratch.bitset
	if cap(buf) < bufSize {
		buf = make([]byte, bufSize)
	} else {
		buf = buf[:bufSize]
		clear(buf)
	}
	seen := scratch.seen
	if cap(seen) < paletteSize {
		seen = make([]uint16, paletteSize)
	} else {
		seen = seen[:paletteSize]
	}
	right := scratch.right
	if cap(right) < paletteSize {
		right = make([]int, paletteSize)
	} else {
		right = right[:paletteSize]
		clear(right)
	}
	ch0 := specialChCr
	if first == 0 {
		ch0 = specialChNr
	}
	for z := first; z < last; z++ {
		scratch.gen++
		if scratch.gen == 0 {
			clear(seen)
			scratch.gen = 1
		}
		gen := scratch.gen
		if z > 0 {
			dst = append(dst, '-')
			ch0 = specialChNr
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
				if x >= right[idx] {
					right[idx] = x + 1
				}
			}
		}
		for n := range paletteSize {
			if seen[n] != gen {
				continue
			}
			if ch0 == specialChCr {
				dst = append(dst, '$')
			}
			dst = appendColourSelect(dst, n)
			count := 0
			base := width * n
			for x := range right[n] {
				ch := buf[base+x]
				buf[base+x] = 0
				if ch0 < 0x40 && ch != ch0 {
					dst = appendRun(dst, ch0, count)
					count = 0
				}
				ch0 = ch
				count++
			}
			right[n] = 0
			if ch0 != 0 {
				dst = appendRun(dst, ch0, count)
			}
			ch0 = specialChCr
		}
	}
	scratch.bitset = buf
	scratch.seen = seen
	scratch.right = right
	return dst
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
	for ; count > repeatLimit; count -= repeatLimit {
		dst = append(dst, '!')
		dst = strconv.AppendInt(dst, int64(repeatLimit), 10)
		dst = append(dst, sixel)
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

func (e *opaqueEncoder) toRGBA(img image.Image) *image.RGBA {
	bounds := img.Bounds()
	if e.rgba == nil || e.rgba.Bounds() != bounds || e.rgba.Stride != bounds.Dx()*4 || len(e.rgba.Pix) != bounds.Dx()*bounds.Dy()*4 {
		e.rgba = image.NewRGBA(bounds)
	}
	draw.Draw(e.rgba, bounds, img, bounds.Min, draw.Src)
	return e.rgba
}

func samplePalette(rgba *image.RGBA, maxColours int) color.Palette { //nolint:unparam // The fixed production limit remains explicit for test helpers.
	palette, _ := samplePaletteInto(rgba, maxColours, nil, nil)
	return palette
}

func samplePaletteInto(rgba *image.RGBA, maxColours int, dst color.Palette, sample *image.RGBA) (color.Palette, *image.RGBA) {
	const budget = 1 << 16
	if cap(dst) < maxColours {
		dst = make(color.Palette, 0, maxColours)
	}
	bounds := rgba.Bounds()
	src := image.Image(rgba)
	if bounds.Dx()*bounds.Dy() > budget {
		step := 2
		width, height := max(1, bounds.Dx()/step), max(1, bounds.Dy()/step)
		for width > budget/height {
			step++
			width, height = max(1, bounds.Dx()/step), max(1, bounds.Dy()/step)
		}
		sampleBounds := image.Rect(0, 0, width, height)
		if sample == nil || sample.Bounds() != sampleBounds {
			sample = image.NewRGBA(sampleBounds)
		}
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
	return median.Quantizer(0).Quantize(dst[:0], src), sample
}

func newPaletteLUT(palette color.Palette) []uint8 {
	return paletteLUT(nil, palette)
}

func paletteLUT(lut []uint8, palette color.Palette) []uint8 {
	var pr, pg, pb [256]int32
	for i, colour := range palette {
		r, g, b, _ := colour.RGBA()
		pr[i], pg[i], pb[i] = int32(r>>8), int32(g>>8), int32(b>>8)
	}
	if len(lut) != 1<<15 || cap(lut) != 1<<15 {
		lut = make([]uint8, 1<<15)
	}
	workers := sixelWorkerCount()
	var wg sync.WaitGroup
	for worker := range workers {
		lo, hi := len(lut)*worker/workers, len(lut)*(worker+1)/workers
		wg.Go(func() {
			for i := lo; i < hi; i++ {
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
		})
	}
	wg.Wait()
	return lut
}

func sixelWorkerCount() int { return min(4, runtime.GOMAXPROCS(0)) }

func mapPaletted(dst *image.Paletted, src *image.RGBA, lut []uint8) {
	bounds := src.Bounds()
	workers := sixelWorkerCount()
	if !parallelMapping(bounds.Dx()*bounds.Dy(), workers) {
		mapPalettedRows(dst, src, lut, bounds.Min.Y, bounds.Max.Y)
		return
	}
	workers = min(workers, bounds.Dy())
	var wg sync.WaitGroup
	for worker := range workers {
		lo := bounds.Min.Y + bounds.Dy()*worker/workers
		hi := bounds.Min.Y + bounds.Dy()*(worker+1)/workers
		wg.Go(func() {
			mapPalettedRows(dst, src, lut, lo, hi)
		})
	}
	wg.Wait()
}

func parallelMapping(pixels, workers int) bool {
	return workers > 1 && pixels >= parallelMapPixelThreshold
}

func mapPalettedRows(dst *image.Paletted, src *image.RGBA, lut []uint8, first, last int) {
	bounds := src.Bounds()
	for y := first; y < last; y++ {
		srcOffset := src.PixOffset(bounds.Min.X, y)
		dstOffset := dst.PixOffset(bounds.Min.X, y)
		for range bounds.Dx() {
			dst.Pix[dstOffset] = lut[int(src.Pix[srcOffset]>>3)<<10|int(src.Pix[srcOffset+1]>>3)<<5|int(src.Pix[srcOffset+2]>>3)]
			srcOffset += 4
			dstOffset++
		}
	}
}

func palettedFromRGBA(img *image.RGBA, maxColours int) (*image.Paletted, bool) {
	return palettedFromOpaquePixels(img.Bounds(), img.Pix, img.PixOffset, maxColours)
}

func palettedFromOpaquePixels(bounds image.Rectangle, pix []byte, offset func(int, int) int, maxColours int) (*image.Paletted, bool) {
	return palettedFromOpaquePixelsInto(nil, bounds, pix, offset, maxColours)
}

func palettedFromOpaquePixelsInto(dst *image.Paletted, bounds image.Rectangle, pix []byte, offset func(int, int) int, maxColours int) (*image.Paletted, bool) {
	palette := make(color.Palette, 1, maxColours+1)
	palette[0] = color.RGBA{}
	indexes := make(map[uint32]uint8, maxColours)
	dst = reusablePaletted(dst, bounds, palette)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		srcOffset := offset(bounds.Min.X, y)
		dstOffset := dst.PixOffset(bounds.Min.X, y)
		row := pix[srcOffset : srcOffset+bounds.Dx()*4]
		for x := range bounds.Dx() {
			base := x * 4
			key := uint32(row[base])<<24 | uint32(row[base+1])<<16 | uint32(row[base+2])<<8 | 0xff
			idx, ok := indexes[key]
			if !ok {
				if len(palette) > maxColours {
					return dst, false
				}
				idx = uint8(len(palette)) //nolint:gosec // The 255-colour limit bounds the index.
				indexes[key] = idx
				palette = append(palette, color.RGBA{R: row[base], G: row[base+1], B: row[base+2], A: 0xff})
			}
			dst.Pix[dstOffset+x] = idx
		}
	}
	dst.Palette = palette
	return dst, true
}

func reusablePaletted(dst *image.Paletted, bounds image.Rectangle, palette color.Palette) *image.Paletted {
	if dst == nil || dst.Bounds() != bounds || dst.Stride != bounds.Dx() || len(dst.Pix) != bounds.Dx()*bounds.Dy() {
		return image.NewPaletted(bounds, palette)
	}
	dst.Palette = palette
	return dst
}
