package block

import (
	"image"
	"image/color"

	"github.com/wimpysworld/nyala/internal/render"
)

// ycbcrSampler maps the decoder's full-size native YCbCr planes onto the
// renderer's sub-cell sample grid. Each sample averages its complete source
// region in YCbCr, then converts that one average to RGBA. This produces the
// same samples as materialising an intermediate RGBA image.
type ycbcrSampler struct {
	src           *image.YCbCr
	dstW, dstH    int
	xSpans        []sampleSpan
	ySpans        []sampleSpan
	yRowOffsets   []int
	cRowOffsets   []int
	chromaColumns []int
	xChromaRanges []sampleSpan
	yChromaRanges []sampleSpan
	xChromaGroups []weightedMapping
	yChromaGroups []weightedMapping
	x420Spans     []chromaSpan420
	y420Spans     []chromaSpan420
	fast420       bool
}

type sampleSpan struct {
	start int
	end   int
	size  uint64
}

type weightedMapping struct {
	offset int
	weight uint64
}

type chromaSpan420 struct {
	offset      int
	groups      int
	firstWeight uint64
	lastWeight  uint64
}

func newYCbCrSampler(src *image.YCbCr, dstW, dstH int) ycbcrSampler {
	var s ycbcrSampler
	s.prepare(src, dstW, dstH)
	return s
}

// prepare maps every destination sample and source plane coordinate once. The
// resulting slices are immutable until the row-band join completes, so workers
// can share them safely. Reusing a sampler reuses every slice backing array.
func (s *ycbcrSampler) prepare(src *image.YCbCr, dstW, dstH int) {
	s.src, s.dstW, s.dstH = src, dstW, dstH
	s.fast420 = false
	if src == nil {
		return
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	s.xSpans = reuseSlice(s.xSpans, dstW)
	s.ySpans = reuseSlice(s.ySpans, dstH)
	for dx := range dstW {
		s.xSpans[dx] = scaledSampleSpan(dx, sw, dstW)
	}
	for dy := range dstH {
		s.ySpans[dy] = scaledSampleSpan(dy, sh, dstH)
	}

	ratH, ratV := ycbcrSubsampleShift(src.SubsampleRatio)
	minXc := b.Min.X / ratH
	minYc := b.Min.Y / ratV
	s.chromaColumns = reuseSlice(s.chromaColumns, sw)
	for px := range sw {
		s.chromaColumns[px] = (b.Min.X+px)/ratH - minXc
	}
	s.yRowOffsets = reuseSlice(s.yRowOffsets, sh)
	s.cRowOffsets = reuseSlice(s.cRowOffsets, sh)
	for py := range sh {
		s.yRowOffsets[py] = py * src.YStride
		s.cRowOffsets[py] = ((b.Min.Y+py)/ratV - minYc) * src.CStride
	}
	s.xChromaRanges, s.xChromaGroups = prepareWeightedMappings(
		s.xSpans,
		s.chromaColumns,
		s.xChromaRanges,
		s.xChromaGroups,
	)
	s.yChromaRanges, s.yChromaGroups = prepareWeightedMappings(
		s.ySpans,
		s.cRowOffsets,
		s.yChromaRanges,
		s.yChromaGroups,
	)
	if src.SubsampleRatio == image.YCbCrSubsampleRatio420 && dstW <= sw && dstH <= sh {
		s.x420Spans, s.fast420 = prepare420Spans(
			s.xChromaRanges,
			s.xChromaGroups,
			1,
			s.x420Spans,
		)
		if s.fast420 {
			s.y420Spans, s.fast420 = prepare420Spans(
				s.yChromaRanges,
				s.yChromaGroups,
				src.CStride,
				s.y420Spans,
			)
		}
	}
}

func reuseSlice[T any](buf []T, n int) []T {
	if cap(buf) < n {
		return make([]T, n)
	}
	return buf[:n]
}

func scaledSampleSpan(dst, srcSize, dstSize int) sampleSpan {
	start := dst * srcSize / dstSize
	end := (dst + 1) * srcSize / dstSize
	if end <= start {
		end = start + 1
	}
	return sampleSpan{start: start, end: end}
}

// prepareWeightedMappings clips equal adjacent plane mappings to each sample
// span. A group weight is the exact number of luma pixels represented by one
// chroma coordinate along that axis. It also records each span's luma size
// without an integer-width conversion in the hot path.
func prepareWeightedMappings(
	spans []sampleSpan,
	mappings []int,
	ranges []sampleSpan,
	groups []weightedMapping,
) ([]sampleSpan, []weightedMapping) {
	ranges = reuseSlice(ranges, len(spans))
	groups = groups[:0]
	for i := range spans {
		span := &spans[i]
		start := len(groups)
		span.size = 0
		if span.start < 0 || span.start >= len(mappings) || span.end <= span.start {
			ranges[i] = sampleSpan{start: start, end: start}
			continue
		}
		end := min(span.end, len(mappings))
		current := weightedMapping{offset: mappings[span.start], weight: 1}
		span.size++
		for pos := span.start + 1; pos < end; pos++ {
			span.size++
			if mappings[pos] == current.offset {
				current.weight++
				continue
			}
			groups = append(groups, current)
			current = weightedMapping{offset: mappings[pos], weight: 1}
		}
		groups = append(groups, current)
		ranges[i] = sampleSpan{start: start, end: len(groups)}
	}
	return ranges, groups
}

// prepare420Spans condenses the common 4:2:0 mapping into contiguous plane
// spans. Crops that cross a negative odd coordinate can produce a three-pixel
// chroma run under Go's signed division rules; those retain the generic path.
func prepare420Spans(
	ranges []sampleSpan,
	groups []weightedMapping,
	step int,
	spans []chromaSpan420,
) ([]chromaSpan420, bool) {
	spans = reuseSlice(spans, len(ranges))
	for i, r := range ranges {
		mapping := groups[r.start:r.end]
		if len(mapping) == 0 || mapping[0].weight > 2 {
			return spans, false
		}
		for j := 1; j < len(mapping); j++ {
			if mapping[j].offset != mapping[j-1].offset+step || mapping[j].weight > 2 {
				return spans, false
			}
		}
		spans[i] = chromaSpan420{
			offset:      mapping[0].offset,
			groups:      len(mapping),
			firstWeight: mapping[0].weight,
			lastWeight:  mapping[len(mapping)-1].weight,
		}
	}
	return spans, true
}

// pixelAt returns one opaque RGB sub-cell sample. Coordinates outside the
// destination grid read as black, matching the RGBA block pixel helper.
func (s *ycbcrSampler) pixelAt(dx, dy int) (r, g, b uint8) {
	if s == nil || s.src == nil || s.dstW <= 0 || s.dstH <= 0 ||
		dx < 0 || dy < 0 || dx >= s.dstW || dy >= s.dstH ||
		len(s.yRowOffsets) == 0 || len(s.chromaColumns) == 0 {
		return 0, 0, 0
	}

	xSpan := s.xSpans[dx]
	ySpan := s.ySpans[dy]
	n := xSpan.size * ySpan.size
	if n == 0 {
		return 0, 0, 0
	}
	if s.fast420 {
		return s.pixelAt420(dx, dy, n)
	}
	var sumY uint64
	for py := ySpan.start; py < ySpan.end; py++ {
		yStart := s.yRowOffsets[py] + xSpan.start
		for _, value := range s.src.Y[yStart : yStart+xSpan.end-xSpan.start] {
			sumY += uint64(value)
		}
	}

	xRange := s.xChromaRanges[dx]
	yRange := s.yChromaRanges[dy]
	xGroups := s.xChromaGroups[xRange.start:xRange.end]
	yGroups := s.yChromaGroups[yRange.start:yRange.end]
	var sumCb, sumCr uint64
	for _, yGroup := range yGroups {
		for _, xGroup := range xGroups {
			weight := xGroup.weight * yGroup.weight
			offset := yGroup.offset + xGroup.offset
			sumCb += uint64(s.src.Cb[offset]) * weight
			sumCr += uint64(s.src.Cr[offset]) * weight
		}
	}
	y := byte(sumY / n & 0xff)
	cb := byte(sumCb / n & 0xff)
	cr := byte(sumCr / n & 0xff)
	return color.YCbCrToRGB(y, cb, cr)
}

func (s *ycbcrSampler) pixelAt420(dx, dy int, n uint64) (r, g, b uint8) {
	xLuma := s.xSpans[dx]
	yLuma := s.ySpans[dy]
	var sumY uint64
	if xLuma.end-xLuma.start == 8 {
		for py := yLuma.start; py < yLuma.end; py++ {
			offset := s.yRowOffsets[py] + xLuma.start
			row := s.src.Y[offset : offset+8]
			sumY += uint64(row[0]) + uint64(row[1]) + uint64(row[2]) + uint64(row[3]) +
				uint64(row[4]) + uint64(row[5]) + uint64(row[6]) + uint64(row[7])
		}
	} else {
		for py := yLuma.start; py < yLuma.end; py++ {
			yStart := s.yRowOffsets[py] + xLuma.start
			for _, value := range s.src.Y[yStart : yStart+xLuma.end-xLuma.start] {
				sumY += uint64(value)
			}
		}
	}

	xSpan := s.x420Spans[dx]
	ySpan := s.y420Spans[dy]
	var sumCb, sumCr uint64
	for row := range ySpan.groups {
		yWeight := uint64(2)
		if row == 0 {
			yWeight = ySpan.firstWeight
		}
		if row == ySpan.groups-1 {
			yWeight = ySpan.lastWeight
		}

		offset := ySpan.offset + row*s.src.CStride + xSpan.offset
		if xSpan.groups == 1 {
			sumCb += uint64(s.src.Cb[offset]) * xSpan.firstWeight * yWeight
			sumCr += uint64(s.src.Cr[offset]) * xSpan.firstWeight * yWeight
			continue
		}

		rowCb := uint64(s.src.Cb[offset]) * xSpan.firstWeight
		rowCr := uint64(s.src.Cr[offset]) * xSpan.firstWeight
		for col := 1; col < xSpan.groups-1; col++ {
			rowCb += uint64(s.src.Cb[offset+col]) * 2
			rowCr += uint64(s.src.Cr[offset+col]) * 2
		}
		last := offset + xSpan.groups - 1
		rowCb += uint64(s.src.Cb[last]) * xSpan.lastWeight
		rowCr += uint64(s.src.Cr[last]) * xSpan.lastWeight
		sumCb += rowCb * yWeight
		sumCr += rowCr * yWeight
	}
	y := byte(sumY / n & 0xff)
	cb := byte(sumCb / n & 0xff)
	cr := byte(sumCr / n & 0xff)
	return color.YCbCrToRGB(y, cb, cr)
}

func ycbcrSubsampleShift(r image.YCbCrSubsampleRatio) (h, v int) {
	switch r {
	case image.YCbCrSubsampleRatio420:
		return 2, 2
	case image.YCbCrSubsampleRatio422:
		return 2, 1
	case image.YCbCrSubsampleRatio440:
		return 1, 2
	case image.YCbCrSubsampleRatio411:
		return 4, 1
	case image.YCbCrSubsampleRatio410:
		return 4, 2
	default:
		return 1, 1
	}
}

func (br *Renderer) ycbcrSampleSize() (width, height int) {
	switch br.Scheme {
	case SchemeSextant:
		return br.Cols * sextCellPxW, br.Rows * sextCellPxH
	default:
		return br.Cols * cellPxW, br.Rows * cellPxH
	}
}

// ComposeYCbCr composites a full-size native YCbCr frame directly onto a fresh
// grid. It averages source regions into final sub-cell colours and never creates
// an intermediate downsampled RGBA image. A non-YCbCr frame uses Compose.
func (br *Renderer) ComposeYCbCr(f render.Frame) Grid {
	src, ok := f.Img.(*image.YCbCr)
	if !ok {
		return br.Compose(f)
	}
	var g Grid
	reuseGrid(&g, br.Cols, br.Rows)
	width, height := br.ycbcrSampleSize()
	sampler := newYCbCrSampler(src, width, height)
	br.composeYCbCrRows(&g, &sampler, 0, g.Rows)
	applyOverlayYCbCr(&g, f, br.Scheme, &sampler)
	return g
}

// ComposeYCbCrInto is the allocation-reusing twin of ComposeYCbCr. It writes
// directly into dst and returns it. A non-YCbCr frame uses ComposeInto.
func (br *Renderer) ComposeYCbCrInto(dst *Grid, f render.Frame) *Grid {
	src, ok := f.Img.(*image.YCbCr)
	if !ok {
		return br.ComposeInto(dst, f)
	}
	reuseGrid(dst, br.Cols, br.Rows)
	width, height := br.ycbcrSampleSize()
	sampler := newYCbCrSampler(src, width, height)
	br.composeYCbCrRows(dst, &sampler, 0, dst.Rows)
	applyOverlayYCbCr(dst, f, br.Scheme, &sampler)
	return dst
}

func (br *Renderer) composeYCbCrRows(g *Grid, src *ycbcrSampler, row0, row1 int) {
	switch br.Scheme {
	case SchemeSextant:
		composeYCbCrSextantRows(g, src, row0, row1)
	default:
		composeYCbCrQuadrantRows(g, src, row0, row1)
	}
}

func composeYCbCrQuadrantRows(g *Grid, src *ycbcrSampler, row0, row1 int) {
	for row := row0; row < row1; row++ {
		for col := range g.Cols {
			x0, y0 := col*cellPxW, row*cellPxH
			x := sampledColour(src, x0, y0, 2)
			y := sampledColour(src, x0+1, y0, 2)
			z := sampledColour(src, x0, y0+2, 2)
			w := sampledColour(src, x0+1, y0+2, 2)
			glyph, fg, bg := binarizeQuadrant(x, y, z, w)
			*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
		}
	}
}

func composeYCbCrSextantRows(g *Grid, src *ycbcrSampler, row0, row1 int) {
	for row := row0; row < row1; row++ {
		for col := range g.Cols {
			x0, y0 := col*sextCellPxW, row*sextCellPxH
			tl := sampledColour(src, x0, y0, 1)
			tr := sampledColour(src, x0+1, y0, 1)
			ml := sampledColour(src, x0, y0+1, 1)
			mr := sampledColour(src, x0+1, y0+1, 1)
			bl := sampledColour(src, x0, y0+2, 1)
			botRight := sampledColour(src, x0+1, y0+2, 1)
			glyph, fg, bg := binarizeSextant(tl, tr, ml, mr, bl, botRight)
			*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
		}
	}
}

func sampledColour(src *ycbcrSampler, x, y, rows int) color.RGBA {
	var rs, gs, bs int
	for dy := range rows {
		r, g, b := src.pixelAt(x, y+dy)
		rs += int(r)
		gs += int(g)
		bs += int(b)
	}
	return color.RGBA{R: clampByte(rs / rows), G: clampByte(gs / rows), B: clampByte(bs / rows), A: 0xff}
}
