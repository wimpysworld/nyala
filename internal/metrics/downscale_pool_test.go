package metrics

import (
	"bytes"
	"image"
	"testing"

	"github.com/wimpysworld/nyala/internal/rowband"
)

// noiseRGBA returns a w x h RGBA filled from a deterministic 64-bit LCG seeded by
// seed, so the downscale reads a full noisy plane rather than a smooth gradient
// without pulling in math/rand. Different seeds give independent planes.
func noiseRGBA(seed uint64, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	state := seed
	for i := range img.Pix {
		state = state*6364136223846793005 + 1442695040888963407
		img.Pix[i] = byte(state >> 56)
	}
	return img
}

// TestParallelDownscaleByteIdentical proves the pool-split downscale is
// byte-for-byte identical to the serial DownscaleLiveInto across random sources and
// several grid and cell sizes, including destination heights that do not divide
// evenly into bands and a single-row destination. Byte identity is the correctness
// contract: the pool must never shift a single output byte from the serial path.
func TestParallelDownscaleByteIdentical(t *testing.T) {
	// Force eight workers so any destination tall enough splits into several bands,
	// exercising the parallel path even on a low-core test runner.
	pool := rowband.NewSized(8)
	defer pool.Close()
	pd := NewParallelDownscale()

	cases := []struct{ srcW, srcH, dstW, dstH int }{
		{1920, 1320, 240, 160}, // 120x40 quadrant, even split into 8
		{3840, 2838, 480, 344}, // 240x86 quadrant, 344 rows: uneven across 8 bands
		{1920, 1320, 240, 161}, // odd destination height, uneven remainder
		{800, 600, 120, 17},    // 17 rows: one band above the min, one below
		{640, 480, 100, 1},     // single-row destination: serial fallback
		{640, 480, 100, 15},    // below the band minimum: serial fallback
		{300, 200, 300, 200},   // no-op size: every tile is one source pixel
		{100, 100, 200, 240},   // upscale: empty tiles fall back to nearest
	}
	for i, c := range cases {
		src := noiseRGBA(uint64(i)+1, c.srcW, c.srcH)

		want := image.NewRGBA(image.Rect(0, 0, c.dstW, c.dstH))
		DownscaleLiveInto(want, src)

		got := image.NewRGBA(image.Rect(0, 0, c.dstW, c.dstH))
		pd.DownscaleInto(got, src, pool)

		if !bytes.Equal(want.Pix, got.Pix) {
			t.Errorf("src %dx%d -> dst %dx%d: parallel output differs from serial",
				c.srcW, c.srcH, c.dstW, c.dstH)
		}
	}
}

// TestParallelDownscaleReuse proves one driver and pool stay byte-identical across
// repeated calls, since the live path reuses both every frame. A stale WaitGroup or
// a leaked band would surface as drift on a later call.
func TestParallelDownscaleReuse(t *testing.T) {
	pool := rowband.NewSized(8)
	defer pool.Close()
	pd := NewParallelDownscale()

	for i := range 20 {
		src := noiseRGBA(uint64(i)+100, 1920, 1320)
		want := image.NewRGBA(image.Rect(0, 0, 240, 160))
		DownscaleLiveInto(want, src)
		got := image.NewRGBA(image.Rect(0, 0, 240, 160))
		pd.DownscaleInto(got, src, pool)
		if !bytes.Equal(want.Pix, got.Pix) {
			t.Fatalf("call %d: parallel output differs from serial", i)
		}
	}
}

// TestParallelDownscaleNilPool proves a nil pool runs the serial kernel, the
// capture and static-image fallback the live path drops to when it holds no pool.
func TestParallelDownscaleNilPool(t *testing.T) {
	pd := NewParallelDownscale()
	src := noiseRGBA(7, 1920, 1320)
	want := image.NewRGBA(image.Rect(0, 0, 240, 160))
	DownscaleLiveInto(want, src)
	got := image.NewRGBA(image.Rect(0, 0, 240, 160))
	pd.DownscaleInto(got, src, nil)
	if !bytes.Equal(want.Pix, got.Pix) {
		t.Fatal("nil-pool output differs from serial")
	}
}
