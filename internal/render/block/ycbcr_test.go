package block

import (
	"fmt"
	"image"
	"image/color"
	"reflect"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/rowband"
)

func patternedYCbCr(rect image.Rectangle, ratio image.YCbCrSubsampleRatio) *image.YCbCr {
	src := image.NewYCbCr(rect, ratio)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			src.Y[src.YOffset(x, y)] = uint8((x*17 + y*29 + x*y) & 0xff)
			ci := src.COffset(x, y)
			src.Cb[ci] = uint8((x*11 + y*7 + 43) & 0xff)
			src.Cr[ci] = uint8((x*5 + y*19 + 97) & 0xff)
		}
	}
	return src
}

func patternedPaddedYCbCr420(rect image.Rectangle, padding int) *image.YCbCr {
	chromaWidth := (rect.Max.X+1)/2 - rect.Min.X/2
	chromaHeight := (rect.Max.Y+1)/2 - rect.Min.Y/2
	src := &image.YCbCr{
		Y:              make([]byte, (rect.Dx()+padding)*rect.Dy()),
		Cb:             make([]byte, (chromaWidth+padding)*chromaHeight),
		Cr:             make([]byte, (chromaWidth+padding)*chromaHeight),
		YStride:        rect.Dx() + padding,
		CStride:        chromaWidth + padding,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           rect,
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			src.Y[src.YOffset(x, y)] = uint8((x*17 + y*29 + x*y) & 0xff)
			ci := src.COffset(x, y)
			src.Cb[ci] = uint8((x*11 + y*7 + 43) & 0xff)
			src.Cr[ci] = uint8((x*5 + y*19 + 97) & 0xff)
		}
	}
	return src
}

// referenceYCbCrAverage materialises a sub-cell RGBA image as an independent
// test oracle. It uses the standard library's coordinate lookup rather than the
// direct composer's plane-index arithmetic.
func referenceYCbCrAverage(src *image.YCbCr, dw, dh int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	for dy := range dh {
		sy0 := dy * sh / dh
		sy1 := (dy + 1) * sh / dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := range dw {
			sx0 := dx * sw / dw
			sx1 := (dx + 1) * sw / dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sumY, sumCb, sumCr, n uint64
			for py := sy0; py < sy1; py++ {
				for px := sx0; px < sx1; px++ {
					c := src.YCbCrAt(sb.Min.X+px, sb.Min.Y+py)
					sumY += uint64(c.Y)
					sumCb += uint64(c.Cb)
					sumCr += uint64(c.Cr)
					n++
				}
			}
			r, g, b := color.YCbCrToRGB(
				byte(sumY/n&0xff),
				byte(sumCb/n&0xff),
				byte(sumCr/n&0xff),
			)
			o := dst.PixOffset(dx, dy)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = r, g, b, 0xff
		}
	}
	return dst
}

func ycbcrTestRuns() []render.GlyphRun {
	return []render.GlyphRun{
		{
			Col:          2,
			Row:          1,
			Text:         "A界",
			FG:           color.RGBA{R: 230, G: 240, B: 250, A: 0xff},
			X:            19,
			W:            28,
			CellW:        10,
			DisplayCells: 3,
		},
	}
}

// TestComposeYCbCrMatchesIntermediateRGBA proves the direct path produces the
// same fused grid for every Block scheme, every YCbCr subsampling ratio,
// odd dimensions, non-zero bounds, and overlay boundary recomposition.
func TestComposeYCbCrMatchesIntermediateRGBA(t *testing.T) {
	ratios := []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444,
		image.YCbCrSubsampleRatio422,
		image.YCbCrSubsampleRatio420,
		image.YCbCrSubsampleRatio440,
		image.YCbCrSubsampleRatio411,
		image.YCbCrSubsampleRatio410,
	}
	schemes := []Scheme{SchemeQuadrant, SchemeSextant}
	sources := []struct {
		name string
		full image.Rectangle
		crop image.Rectangle
	}{
		{name: "positive", full: image.Rect(0, 0, 97, 91), crop: image.Rect(3, 5, 92, 86)},
		{name: "negative", full: image.Rect(-17, -13, 80, 78), crop: image.Rect(-14, -8, 75, 73)},
	}
	for _, ratio := range ratios {
		for _, source := range sources {
			base := patternedYCbCr(source.full, ratio)
			src := base.SubImage(source.crop).(*image.YCbCr)
			for _, scheme := range schemes {
				name := fmt.Sprintf("ratio-%d/%s/scheme-%d", ratio, source.name, scheme)
				t.Run(name, func(t *testing.T) {
					br := Renderer{Cols: 7, Rows: 4, Scheme: scheme}
					dw, dh := br.ycbcrSampleSize()
					frame := render.Frame{Img: src, Text: ycbcrTestRuns()}
					want := br.Compose(render.Frame{Img: referenceYCbCrAverage(src, dw, dh), Text: frame.Text})
					got := br.ComposeYCbCr(frame)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("direct YCbCr grid differs from intermediate RGBA reference\n got: %#v\nwant: %#v", got, want)
					}
				})
			}
		}
	}
}

// TestYCbCrChromaGroupsClipPartialRuns targets destination sample boundaries
// that split both horizontal and vertical 4:2:0 chroma runs.
func TestYCbCrChromaGroupsClipPartialRuns(t *testing.T) {
	const dstW, dstH = 7, 6
	src := patternedYCbCr(image.Rect(-5, -5, 26, 24), image.YCbCrSubsampleRatio420)
	sampler := newYCbCrSampler(src, dstW, dstH)
	if !mappingRunCut(sampler.xSpans, sampler.chromaColumns) {
		t.Fatal("test geometry does not split a horizontal chroma run")
	}
	if !mappingRunCut(sampler.ySpans, sampler.cRowOffsets) {
		t.Fatal("test geometry does not split a vertical chroma run")
	}
	assertWeightedRanges(t, sampler.xSpans, sampler.xChromaRanges, sampler.xChromaGroups)
	assertWeightedRanges(t, sampler.ySpans, sampler.yChromaRanges, sampler.yChromaGroups)
	assertSamplerMatchesReference(t, &sampler, referenceYCbCrAverage(src, dstW, dstH))
}

// TestYCbCrChromaGroups444 keeps the no-subsampling case explicit: every chroma
// mapping represents one luma pixel along its axis.
func TestYCbCrChromaGroups444(t *testing.T) {
	const dstW, dstH = 7, 6
	src := patternedYCbCr(image.Rect(-5, -5, 26, 24), image.YCbCrSubsampleRatio444)
	sampler := newYCbCrSampler(src, dstW, dstH)
	for _, group := range sampler.xChromaGroups {
		if group.weight != 1 {
			t.Fatalf("horizontal 4:4:4 group weight = %d, want 1", group.weight)
		}
	}
	for _, group := range sampler.yChromaGroups {
		if group.weight != 1 {
			t.Fatalf("vertical 4:4:4 group weight = %d, want 1", group.weight)
		}
	}
	assertSamplerMatchesReference(t, &sampler, referenceYCbCrAverage(src, dstW, dstH))
}

// TestYCbCr420NegativeOddCropFallback covers signed division around zero, where
// one chroma coordinate can represent three luma pixels and the fast path must
// retain the generic weighted mapping.
func TestYCbCr420NegativeOddCropFallback(t *testing.T) {
	const dstW, dstH = 7, 6
	base := patternedYCbCr(image.Rect(-9, -9, 14, 14), image.YCbCrSubsampleRatio420)
	src := base.SubImage(image.Rect(-1, -3, 12, 12)).(*image.YCbCr)
	sampler := newYCbCrSampler(src, dstW, dstH)
	if sampler.fast420 {
		t.Fatal("negative odd crop selected the 4:2:0 fast path")
	}
	assertSamplerMatchesReference(t, &sampler, referenceYCbCrAverage(src, dstW, dstH))
}

// TestYCbCr420NegativeOddCropFastPath covers a cropped source that starts at
// negative odd coordinates but stays below zero, so its chroma runs remain
// two pixels wide and the specialised path must preserve signed plane offsets.
func TestYCbCr420NegativeOddCropFastPath(t *testing.T) {
	const dstW, dstH = 7, 6
	base := patternedYCbCr(image.Rect(-31, -29, -2, -2), image.YCbCrSubsampleRatio420)
	src := base.SubImage(image.Rect(-25, -23, -4, -4)).(*image.YCbCr)
	sampler := newYCbCrSampler(src, dstW, dstH)
	if !sampler.fast420 {
		t.Fatal("negative odd crop did not select the 4:2:0 fast path")
	}
	assertSamplerMatchesReference(t, &sampler, referenceYCbCrAverage(src, dstW, dstH))
}

// TestYCbCr420PaddedStrideFastPath checks direct plane offsets when chroma rows
// contain padding beyond the visible source width.
func TestYCbCr420PaddedStrideFastPath(t *testing.T) {
	const dstW, dstH = 9, 7
	src := patternedPaddedYCbCr420(image.Rect(3, 5, 40, 34), 11)
	sampler := newYCbCrSampler(src, dstW, dstH)
	if !sampler.fast420 {
		t.Fatal("padded 4:2:0 source did not select the fast path")
	}
	assertSamplerMatchesReference(t, &sampler, referenceYCbCrAverage(src, dstW, dstH))
}

// TestYCbCr420FastPathReuseAcrossRatios checks that ratio changes reset the
// dispatch flag even when every mapping slice keeps the same length and backing
// array.
func TestYCbCr420FastPathReuseAcrossRatios(t *testing.T) {
	const dstW, dstH = 11, 9
	rect := image.Rect(3, 5, 100, 96)
	sources := []struct {
		ratio image.YCbCrSubsampleRatio
		fast  bool
	}{
		{ratio: image.YCbCrSubsampleRatio420, fast: true},
		{ratio: image.YCbCrSubsampleRatio422},
		{ratio: image.YCbCrSubsampleRatio444},
		{ratio: image.YCbCrSubsampleRatio420, fast: true},
	}
	var sampler ycbcrSampler
	for i, tc := range sources {
		src := patternedYCbCr(rect, tc.ratio)
		sampler.prepare(src, dstW, dstH)
		if sampler.fast420 != tc.fast {
			t.Fatalf("step %d ratio %d fast420 = %t, want %t", i, tc.ratio, sampler.fast420, tc.fast)
		}
		assertSamplerMatchesReference(t, &sampler, referenceYCbCrAverage(src, dstW, dstH))
	}
}

// TestYCbCr420UpscaleFallback keeps repeated one-pixel source spans on the
// generic path.
func TestYCbCr420UpscaleFallback(t *testing.T) {
	const dstW, dstH = 11, 9
	src := patternedYCbCr(image.Rect(3, 5, 10, 10), image.YCbCrSubsampleRatio420)
	sampler := newYCbCrSampler(src, dstW, dstH)
	if sampler.fast420 {
		t.Fatal("upscaled 4:2:0 source selected the fast path")
	}
	assertSamplerMatchesReference(t, &sampler, referenceYCbCrAverage(src, dstW, dstH))
}

func mappingRunCut(spans []sampleSpan, mappings []int) bool {
	for _, span := range spans {
		if span.start > 0 && span.start < len(mappings) && mappings[span.start-1] == mappings[span.start] {
			return true
		}
		if span.end > 0 && span.end < len(mappings) && mappings[span.end-1] == mappings[span.end] {
			return true
		}
	}
	return false
}

func assertWeightedRanges(t *testing.T, spans, ranges []sampleSpan, groups []weightedMapping) {
	t.Helper()
	for i, span := range spans {
		r := ranges[i]
		var total uint64
		for _, group := range groups[r.start:r.end] {
			total += group.weight
		}
		if total != span.size {
			t.Fatalf("span %d grouped weight = %d, want %d", i, total, span.size)
		}
	}
}

func assertSamplerMatchesReference(t *testing.T, sampler *ycbcrSampler, want *image.RGBA) {
	t.Helper()
	for y := range sampler.dstH {
		for x := range sampler.dstW {
			r, g, b := sampler.pixelAt(x, y)
			c := want.RGBAAt(x, y)
			if r != c.R || g != c.G || b != c.B {
				t.Fatalf("sample (%d,%d) = rgb(%d,%d,%d), want rgb(%d,%d,%d)", x, y, r, g, b, c.R, c.G, c.B)
			}
		}
	}
}

// TestParallelComposeYCbCrMatchesIntermediateRGBA proves the row-band path is
// exact and reuses its destination across direct YCbCr frames.
func TestParallelComposeYCbCrMatchesIntermediateRGBA(t *testing.T) {
	br := Renderer{Cols: 23, Rows: 17, Scheme: SchemeSextant}
	dw, dh := br.ycbcrSampleSize()
	src := patternedYCbCr(image.Rect(0, 0, 319, 271), image.YCbCrSubsampleRatio420)
	frame := render.Frame{Img: src, Text: ycbcrTestRuns()}
	want := br.Compose(render.Frame{Img: referenceYCbCrAverage(src, dw, dh), Text: frame.Text})
	serial := br.ComposeYCbCr(frame)
	if !reflect.DeepEqual(serial, want) {
		t.Fatal("serial direct YCbCr grid differs from intermediate RGBA reference")
	}
	pool := rowband.NewSized(4)
	defer pool.Close()
	pc := NewParallelCompose()
	var got Grid
	for range 3 {
		pc.ComposeYCbCrInto(&br, &got, frame, pool)
		if !reflect.DeepEqual(got, serial) {
			t.Fatal("parallel direct YCbCr grid differs from serial composition")
		}
	}
}

// TestParallelComposeReuseTransitions covers the state changes a live renderer
// makes while one compose driver survives resizes and backend scheme changes.
func TestParallelComposeReuseTransitions(t *testing.T) {
	pool := rowband.NewSized(4)
	defer pool.Close()
	pc := NewParallelCompose()
	var got Grid

	directCases := []struct {
		name    string
		br      Renderer
		src     *image.YCbCr
		overlay []render.GlyphRun
	}{
		{
			name: "quadrant",
			br:   Renderer{Cols: 7, Rows: 4, Scheme: SchemeQuadrant},
			src:  patternedYCbCr(image.Rect(0, 0, 97, 91), image.YCbCrSubsampleRatio420),
		},
		{
			name:    "resized-sextant",
			br:      Renderer{Cols: 23, Rows: 17, Scheme: SchemeSextant},
			src:     patternedYCbCr(image.Rect(3, 5, 322, 276), image.YCbCrSubsampleRatio422),
			overlay: ycbcrTestRuns(),
		},
		{
			name: "resized-quadrant",
			br:   Renderer{Cols: 11, Rows: 9, Scheme: SchemeQuadrant},
			src:  patternedYCbCr(image.Rect(0, 0, 143, 131), image.YCbCrSubsampleRatio444),
		},
	}
	for _, tc := range directCases {
		dw, dh := tc.br.ycbcrSampleSize()
		frame := render.Frame{Img: tc.src, Text: tc.overlay}
		want := tc.br.Compose(render.Frame{Img: referenceYCbCrAverage(tc.src, dw, dh), Text: tc.overlay})
		pc.ComposeYCbCrInto(&tc.br, &got, frame, pool)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s transition differs from intermediate RGBA reference", tc.name)
		}
	}

	rgbaBR := Renderer{Cols: 5, Rows: 6, Scheme: SchemeQuadrant}
	rgba := image.NewRGBA(image.Rect(0, 0, rgbaBR.Cols*cellPxW, rgbaBR.Rows*cellPxH))
	for i := range rgba.Pix {
		rgba.Pix[i] = clampByte(i * 31)
	}
	rgbaFrame := render.Frame{Img: rgba}
	pc.ComposeYCbCrInto(&rgbaBR, &got, rgbaFrame, pool)
	if want := rgbaBR.Compose(rgbaFrame); !reflect.DeepEqual(got, want) {
		t.Fatal("direct YCbCr to RGBA fallback transition differs")
	}

	first := directCases[0]
	dw, dh := first.br.ycbcrSampleSize()
	pc.ComposeYCbCrInto(&first.br, &got, render.Frame{Img: first.src}, pool)
	if want := first.br.Compose(render.Frame{Img: referenceYCbCrAverage(first.src, dw, dh)}); !reflect.DeepEqual(got, want) {
		t.Fatal("RGBA fallback to direct YCbCr transition differs")
	}
}

// TestComposeYCbCrNonYCbCrFallback keeps the existing RGBA compose behaviour.
func TestComposeYCbCrNonYCbCrFallback(t *testing.T) {
	br := Renderer{Cols: 2, Rows: 2, Scheme: SchemeQuadrant}
	img := image.NewRGBA(image.Rect(0, 0, 4, 8))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	f := render.Frame{Img: img}
	if got, want := br.ComposeYCbCr(f), br.Compose(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("non-YCbCr fallback differs: got %#v, want %#v", got, want)
	}
}

// BenchmarkComposeYCbCrDirect compares the removed intermediate image with the
// direct grid path on a representative 160x90 sextant surface.
func BenchmarkComposeYCbCrDirect(b *testing.B) {
	br := Renderer{Cols: 160, Rows: 90, Scheme: SchemeSextant}
	dw, dh := br.ycbcrSampleSize()
	src := patternedYCbCr(image.Rect(0, 0, 1280, 1440), image.YCbCrSubsampleRatio420)
	frame := render.Frame{Img: src}
	b.Run("intermediate-rgba", func(b *testing.B) {
		pool := rowband.New()
		defer pool.Close()
		pc := NewParallelCompose()
		var dst Grid
		pc.ComposeInto(&br, &dst, render.Frame{Img: referenceYCbCrAverage(src, dw, dh)}, pool)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			rgba := referenceYCbCrAverage(src, dw, dh)
			pc.ComposeInto(&br, &dst, render.Frame{Img: rgba}, pool)
		}
	})
	b.Run("direct-grid", func(b *testing.B) {
		pool := rowband.New()
		defer pool.Close()
		pc := NewParallelCompose()
		var dst Grid
		pc.ComposeYCbCrInto(&br, &dst, frame, pool)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			pc.ComposeYCbCrInto(&br, &dst, frame, pool)
		}
	})
}

var benchmarkYCbCrSample uint64

// BenchmarkYCbCrSampler separates reusable map preparation from the source-plane
// aggregation loop on the live 2560x2880 to 320x270 sampling geometry.
func BenchmarkYCbCrSampler(b *testing.B) {
	const dstW, dstH = 320, 270
	src := patternedYCbCr(image.Rect(0, 0, 2560, 2880), image.YCbCrSubsampleRatio420)
	b.Run("prepare-reused", func(b *testing.B) {
		sampler := newYCbCrSampler(src, dstW, dstH)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			sampler.prepare(src, dstW, dstH)
		}
	})
	b.Run("aggregate-sample-grid", func(b *testing.B) {
		sampler := newYCbCrSampler(src, dstW, dstH)
		var sink uint64
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			for y := range dstH {
				for x := range dstW {
					r, g, blue := sampler.pixelAt(x, y)
					sink += uint64(r) + uint64(g) + uint64(blue)
				}
			}
		}
		benchmarkYCbCrSample = sink
	})
}
