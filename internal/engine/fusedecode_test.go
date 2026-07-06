package engine

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"testing"
)

// pageLikeJPEG builds a screencast-sized JPEG with page-like content: a flat light
// background with periodic dark strokes and coloured blocks, so the YCbCr planes
// carry real high-frequency detail and chroma variation rather than a trivially
// compressible flat fill. Returned as an *image.YCbCr, the layout the JPEG decoder
// produces and the fused kernel consumes.
func pageLikeJPEG(tb testing.TB, w, h, q int) *image.YCbCr {
	tb.Helper()
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			r, g, b := uint8(240), uint8(240), uint8(238)
			if (x/3+y/5)%7 == 0 {
				r, g, b = 20, 20, 30
			}
			if (x/64)%3 == 0 && (y/48)%2 == 0 {
				r, g, b = uint8(60+x%160), uint8(x%200), uint8(y%200)
			}
			o := src.PixOffset(x, y)
			src.Pix[o], src.Pix[o+1], src.Pix[o+2], src.Pix[o+3] = r, g, b, 0xff
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: q}); err != nil {
		tb.Fatalf("encode JPEG: %v", err)
	}
	img, err := jpeg.Decode(&buf)
	if err != nil {
		tb.Fatalf("decode JPEG: %v", err)
	}
	yc, ok := img.(*image.YCbCr)
	if !ok {
		tb.Fatalf("decoded image is %T, want *image.YCbCr", img)
	}
	return yc
}

// referenceSubCell is the current pipeline the fuse replaces: convert the whole
// YCbCr surface to RGBA (draw.Draw), then area-average that RGBA down to the sub-cell
// grid (the production areaAverageBand kernel). It is the reference the fused output
// is measured against.
func referenceSubCell(yc *image.YCbCr, dw, dh int) *image.RGBA {
	b := yc.Bounds()
	full := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(full, full.Bounds(), yc, b.Min, draw.Src)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// Mirror internal/metrics areaAverageBand: unweighted box mean per destination cell.
	sw, sh := b.Dx(), b.Dy()
	for dy := range dh {
		sy0, sy1 := dy*sh/dh, (dy+1)*sh/dh
		for dx := range dw {
			sx0, sx1 := dx*sw/dw, (dx+1)*sw/dw
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				row := full.PixOffset(sx0, sy)
				for sx := sx0; sx < sx1; sx++ {
					r += uint64(full.Pix[row])
					g += uint64(full.Pix[row+1])
					bl += uint64(full.Pix[row+2])
					a += uint64(full.Pix[row+3])
					row += 4
					n++
				}
			}
			o := dst.PixOffset(dx, dy)
			dst.Pix[o] = byte(r / n & 0xFF)
			dst.Pix[o+1] = byte(g / n & 0xFF)
			dst.Pix[o+2] = byte(bl / n & 0xFF)
			dst.Pix[o+3] = byte(a / n & 0xFF)
		}
	}
	return dst
}

// TestAverageYCbCrToRGBATolerance is the correctness guardrail for the fuse: it has
// no golden (no golden gates the JPEG->block path), so it asserts the fused sub-cell
// output stays within a small bounded per-channel delta of the reference
// (convert-whole-surface then area-average). The delta is only chroma-subsampling
// replication and the YCbCrToRGB clamp, so it is a handful of levels, not a redraw.
func TestAverageYCbCrToRGBATolerance(t *testing.T) {
	cases := []struct{ w, h, dw, dh int }{
		{2528, 2838, 316, 258}, // aquarium 158x86, sextant sub-cell
		{1920, 1536, 316, 258},
		{1280, 800, 240, 160}, // quadrant-ish sub-cell
	}
	// The delta is dominated by chroma replication at box edges plus the convert-order
	// difference; measured max sits at a few levels, so 8 leaves headroom without
	// masking a real regression (a swapped channel or wrong scale would blow past it).
	const tol = 8
	for _, c := range cases {
		yc := pageLikeJPEG(t, c.w, c.h, movingScreencastQuality)
		ref := referenceSubCell(yc, c.dw, c.dh)
		got := image.NewRGBA(image.Rect(0, 0, c.dw, c.dh))
		averageYCbCrToRGBA(got, yc)

		var maxR, maxG, maxB int
		for i := 0; i < len(ref.Pix); i += 4 {
			maxR = maxInt(maxR, absI(int(ref.Pix[i])-int(got.Pix[i])))
			maxG = maxInt(maxG, absI(int(ref.Pix[i+1])-int(got.Pix[i+1])))
			maxB = maxInt(maxB, absI(int(ref.Pix[i+2])-int(got.Pix[i+2])))
			if got.Pix[i+3] != 0xff {
				t.Fatalf("%dx%d: alpha at pixel %d = %d, want 255 (opaque)", c.w, c.h, i/4, got.Pix[i+3])
			}
		}
		t.Logf("%dx%d -> %dx%d: max per-channel delta R=%d G=%d B=%d (tol %d)", c.w, c.h, c.dw, c.dh, maxR, maxG, maxB, tol)
		if maxR > tol || maxG > tol || maxB > tol {
			t.Errorf("%dx%d: fused vs reference max delta R=%d G=%d B=%d exceeds tol %d", c.w, c.h, maxR, maxG, maxB, tol)
		}
	}
}

// TestAverageYCbCrToRGBAOpaqueAndSized checks the kernel writes an opaque,
// fully-sized result and never panics on a degenerate 1x1 target.
func TestAverageYCbCrToRGBAOpaqueAndSized(t *testing.T) {
	yc := pageLikeJPEG(t, 256, 256, movingScreencastQuality)
	for _, dh := range []int{1, 3, 17} {
		dw := dh
		dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
		averageYCbCrToRGBA(dst, yc)
		for i := 3; i < len(dst.Pix); i += 4 {
			if dst.Pix[i] != 0xff {
				t.Fatalf("%dx%d: non-opaque alpha at %d", dw, dh, i/4)
			}
		}
	}
}

// BenchmarkDecodeFullVsFused measures the decode path before (image.Decode +
// full-surface convert + full-surface area-average to sub-cell) versus after
// (image.Decode + fused YCbCr sub-cell average + small convert) at the aquarium
// surface, so the per-frame decode-worker cut is measurable. The two sub-benchmarks
// share the encoded JPEG; the "before" also runs the production area-average so the
// comparison is the whole pipeline the fuse folds into the worker.
func BenchmarkDecodeFullVsFused(b *testing.B) {
	const w, h, dw, dh = 2528, 2838, 316, 258
	yc := pageLikeJPEG(b, w, h, movingScreencastQuality)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, yc, &jpeg.Options{Quality: movingScreencastQuality}); err != nil {
		b.Fatalf("re-encode: %v", err)
	}
	raw := buf.Bytes()

	b.Run("before_decode+convert+downscale", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			img, _, err := image.Decode(bytes.NewReader(raw))
			if err != nil {
				b.Fatal(err)
			}
			full := imageToRGBA(img)
			_ = referenceAreaAverage(full, dw, dh)
		}
	})
	b.Run("after_decode+fused", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			img, _, err := image.Decode(bytes.NewReader(raw))
			if err != nil {
				b.Fatal(err)
			}
			ycc := img.(*image.YCbCr)
			dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
			averageYCbCrToRGBA(dst, ycc)
		}
	})
}

// referenceAreaAverage box-averages a full RGBA down to dw x dh, the production
// downscale the benchmark's "before" arm folds in.
func referenceAreaAverage(full *image.RGBA, dw, dh int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sw, sh := full.Bounds().Dx(), full.Bounds().Dy()
	for dy := range dh {
		sy0, sy1 := dy*sh/dh, (dy+1)*sh/dh
		for dx := range dw {
			sx0, sx1 := dx*sw/dw, (dx+1)*sw/dw
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				row := full.PixOffset(sx0, sy)
				for sx := sx0; sx < sx1; sx++ {
					r += uint64(full.Pix[row])
					g += uint64(full.Pix[row+1])
					bl += uint64(full.Pix[row+2])
					a += uint64(full.Pix[row+3])
					row += 4
					n++
				}
			}
			o := dst.PixOffset(dx, dy)
			dst.Pix[o] = byte(r / n & 0xFF)
			dst.Pix[o+1] = byte(g / n & 0xFF)
			dst.Pix[o+2] = byte(bl / n & 0xFF)
			dst.Pix[o+3] = byte(a / n & 0xFF)
		}
	}
	return dst
}

func absI(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
