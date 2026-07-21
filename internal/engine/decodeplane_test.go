package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestKittyShmDecodePlane proves the kitty-shm decode mode: a direct-eligible
// PNG payload decodes into a pooled RGBA plane carried on Frame.Img with a
// Release hook, instead of passing through as EncodedPNG. The pixel assertions
// pin the project RGBA byte order against the pngPayload gradient.
func TestKittyShmDecodePlane(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{backend: backendKitty}
	e.SetKittyDecode(true)
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	rawQ.put(seqRawFrame{seq: 1, data: pngPayload(t)})

	got := recvFrame(t, frames)
	if got.EncodedPNG != "" {
		t.Fatal("kitty-shm frame retained a direct PNG payload")
	}
	rgba, ok := got.Img.(*image.RGBA)
	if !ok {
		t.Fatalf("kitty-shm frame image is %T, want *image.RGBA", got.Img)
	}
	if rgba.Rect.Dx() != 8 || rgba.Rect.Dy() != 6 {
		t.Fatalf("plane size = %v, want 8x6", rgba.Rect)
	}
	// pngPayload writes R=x*8, G=y*8, B=x+y, A=255 (see decodepool_test.go).
	for y := range 6 {
		for x := range 8 {
			i := y*rgba.Stride + 4*x
			r, g, b, a := rgba.Pix[i], rgba.Pix[i+1], rgba.Pix[i+2], rgba.Pix[i+3]
			if r != uint8(x*8) || g != uint8(y*8) || b != uint8(x+y) || a != 255 {
				t.Fatalf("pixel (%d,%d) = %d,%d,%d,%d; want %d,%d,%d,255 (RGBA order)",
					x, y, r, g, b, a, x*8, y*8, x+y)
			}
		}
	}
	if got.Release == nil {
		t.Fatal("kitty-shm frame carries no Release hook")
	}
	got.Release()
	got.Release() // exactly-once: the second call must be a safe no-op
}

// TestKittyShmDecodeOffKeepsDirectPath proves the toggle default: with the
// mode off, the direct Kitty pass-through is byte-identical and carries no
// Release hook, so Block, Sixel, and direct-Kitty paths are unaffected.
func TestKittyShmDecodeOffKeepsDirectPath(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{backend: backendKitty}
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	payload := pngPayload(t)
	rawQ.put(seqRawFrame{seq: 1, data: payload})

	got := recvFrame(t, frames)
	if got.EncodedPNG != payload {
		t.Fatal("direct Kitty payload changed with the shm mode off")
	}
	if got.Img != nil || got.Release != nil {
		t.Fatal("direct Kitty frame gained a plane or a Release hook")
	}
}

// TestKittyShmDecodeJPEGPlane proves the shm-mode JPEG capture path: a JPEG
// payload decodes through the pool into an *image.RGBA in RGBA byte order with
// no Release hook (the JPEG plane is not pooled, so the GC owns it), and its
// pixels match the stdlib JPEG decode of the same fixture within JPEG
// tolerance.
func TestKittyShmDecodeJPEGPlane(t *testing.T) {
	t.Parallel()
	payload := encodeBase64JPEG(t)
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode fixture base64: %v", err)
	}
	stdImg, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("stdlib decode fixture: %v", err)
	}
	want := imageToRGBA(stdImg)

	e := &ChromiumEngine{backend: backendKitty}
	e.SetKittyDecode(true)
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	rawQ.put(seqRawFrame{seq: 1, data: payload})

	got := recvFrame(t, frames)
	if got.EncodedPNG != "" {
		t.Fatal("kitty-shm JPEG frame retained a direct payload")
	}
	if got.Release != nil {
		t.Fatal("kitty-shm JPEG frame carries a Release hook; the JPEG plane is not pooled")
	}
	rgba, ok := got.Img.(*image.RGBA)
	if !ok {
		t.Fatalf("kitty-shm JPEG frame image is %T, want *image.RGBA", got.Img)
	}
	if !rgba.Rect.Eq(want.Rect) {
		t.Fatalf("plane bounds = %v, want %v", rgba.Rect, want.Rect)
	}
	// The fixture is solid R=200 G=10 B=20, so the distinct channel values pin
	// the RGBA byte order; the tolerance absorbs decoder rounding differences.
	const tol = 3
	for y := range rgba.Rect.Dy() {
		for x := range rgba.Rect.Dx() {
			i := y*rgba.Stride + 4*x
			j := y*want.Stride + 4*x
			for c := range 4 {
				d := int(rgba.Pix[i+c]) - int(want.Pix[j+c])
				if d < -tol || d > tol {
					t.Fatalf("pixel (%d,%d) channel %d = %d, stdlib %d; beyond JPEG tolerance %d (RGBA order)",
						x, y, c, rgba.Pix[i+c], want.Pix[j+c], tol)
				}
			}
			if rgba.Pix[i+3] != 255 {
				t.Fatalf("pixel (%d,%d) alpha = %d, want 255", x, y, rgba.Pix[i+3])
			}
		}
	}
}

// TestDecodeKittyPlaneRejectsUnknownFormat proves the signature sniff: a
// payload that is neither PNG nor JPEG fails with errUnknownPlaneFormat so the
// worker drops the frame.
func TestDecodeKittyPlaneRejectsUnknownFormat(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{}
	payload := base64.StdEncoding.EncodeToString([]byte("neither a PNG signature nor a JPEG start-of-image marker"))
	if _, _, err := e.decodeKittyPlane(payload); !errors.Is(err, errUnknownPlaneFormat) {
		t.Fatalf("decodeKittyPlane error = %v, want errUnknownPlaneFormat", err)
	}
}

// TestKittyDemotionInFlightJPEGDrops proves the shm-to-direct demotion safety:
// a JPEG frame still in flight after the format restarts to PNG never reaches
// the direct pass-through. ValidDirectPNGData rejects it (directKittyPNG is
// false), so the frame emerges with no EncodedPNG and the direct Kitty writer
// emits nothing for it.
func TestKittyDemotionInFlightJPEGDrops(t *testing.T) {
	t.Parallel()
	payload := encodeBase64JPEG(t)
	e := &ChromiumEngine{backend: backendKitty}
	e.SetKittyDecode(false)
	if e.directKittyPNG(payload) {
		t.Fatal("JPEG payload passed the direct PNG validation")
	}
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	rawQ.put(seqRawFrame{seq: 1, data: payload})

	got := recvFrame(t, frames)
	if got.EncodedPNG != "" {
		t.Fatal("in-flight JPEG reached the direct Kitty pass-through")
	}
	if got.Release != nil {
		t.Fatal("in-flight JPEG frame gained a Release hook off the plane path")
	}
}

// TestCollectorReleasesStalePlane proves the seq-gate drop point: a plane
// frame that arrives after a newer sequence has published is released exactly
// once, and the published frame's plane is left for its final owner.
func TestCollectorReleasesStalePlane(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{}
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan decodedFrame)
	frames := make(chan render.Frame, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.runDecodeCollector(ctx, in, frames, &textSync{}, make(chan struct{}, 1), make(chan struct{}, 1))
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	var freshReleases, staleReleases atomic.Int32
	fresh := decodedFrame{seq: 2, pending: pendingFrame{
		img:     seqImage(2),
		release: func() { freshReleases.Add(1) },
	}}
	stale := decodedFrame{seq: 1, pending: pendingFrame{
		img:     seqImage(1),
		release: func() { staleReleases.Add(1) },
	}}
	in <- fresh
	published := recvFrame(t, frames)
	in <- stale

	deadline := time.Now().Add(2 * time.Second)
	for staleReleases.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stale plane frame was not released by the seq gate")
		}
		time.Sleep(time.Millisecond)
	}
	if n := staleReleases.Load(); n != 1 {
		t.Fatalf("stale plane released %d times, want 1", n)
	}
	if freshReleases.Load() != 0 {
		t.Fatal("published plane was released by the engine; the viewer owns it")
	}
	if published.Release == nil {
		t.Fatal("published plane frame carries no Release hook")
	}
}

// TestPlanePoolReuseAndExactlyOnce proves the pool recycles a released plane,
// shapes it to the requested size, and makes a double release a no-op so one
// plane can never reach two frames.
func TestPlanePoolReuseAndExactlyOnce(t *testing.T) {
	t.Parallel()
	p := newPlanePool(2)
	fp := p.get(4, 3)
	if len(fp.img.Pix) != 4*4*3 || fp.img.Stride != 16 || fp.img.Rect != image.Rect(0, 0, 4, 3) {
		t.Fatalf("plane shape = len %d stride %d rect %v", len(fp.img.Pix), fp.img.Stride, fp.img.Rect)
	}
	fp.release()
	reused := p.get(2, 2)
	if reused != fp {
		t.Fatal("pool did not reuse the released plane")
	}
	if len(reused.img.Pix) != 4*2*2 || reused.img.Stride != 8 {
		t.Fatalf("reused plane not reshaped: len %d stride %d", len(reused.img.Pix), reused.img.Stride)
	}
	reused.release()
	reused.release() // second release must not double-insert
	first, second := p.get(1, 1), p.get(1, 1)
	if first == second {
		t.Fatal("double release handed the same plane out twice")
	}
}

// BenchmarkDecodeKittyPlane measures the kitty-shm plane decode on a
// screencast-sized frame; allocs/op must read zero at steady state.
func BenchmarkDecodeKittyPlane(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	state := uint32(1)
	for i := 0; i < len(img.Pix); i += 4 {
		state = state*1664525 + 1013904223
		img.Pix[i+0] = uint8(state >> 24)
		img.Pix[i+3] = 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		b.Fatalf("encode png: %v", err)
	}
	payload := base64.StdEncoding.EncodeToString(buf.Bytes())
	e := &ChromiumEngine{}
	// Warm the base64, reader, decode-state, and plane pools so the loop
	// measures steady state, not the one-time pool fills.
	if _, release, err := e.decodeKittyPlane(payload); err == nil {
		release()
	}
	b.ReportAllocs()
	for b.Loop() {
		_, release, err := e.decodeKittyPlane(payload)
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
}

// TestKittyPlaneDecodeSteadyStateAllocs proves the whole kitty-shm decode
// (base64 into pooled scratch, PNG into pooled plane) allocates no plane per
// frame at steady state.
func TestKittyPlaneDecodeSteadyStateAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("race-detector instrumentation inflates allocation counts")
	}
	e := &ChromiumEngine{}
	payload := pngPayload(t)
	var decodeErr error
	run := func() {
		_, release, err := e.decodeKittyPlane(payload)
		if err != nil {
			decodeErr = err
			return
		}
		release()
	}
	run() // warm the base64, reader, decode-state, and plane pools
	allocs := testing.AllocsPerRun(50, run)
	if decodeErr != nil {
		t.Fatalf("decodeKittyPlane: %v", decodeErr)
	}
	if allocs != 0 {
		t.Fatalf("kitty-shm decode allocates %.1f objects per frame at steady state, want 0", allocs)
	}
}
