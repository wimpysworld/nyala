package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/pngdecode"
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

func TestDecodeKittyPlaneErrorsExposeStableContext(t *testing.T) {
	const payloadMarker = "WW19"
	e := &ChromiumEngine{}
	unsupportedPNG := unsupportedPNGPayload(t)
	badBase64 := payloadMarker + "*"
	_, _, base64Err := decodeBase64Pooled(badBase64)
	if base64Err == nil {
		t.Fatal("malformed Base64 fixture returned no error")
	}
	rawPNG, err := base64.StdEncoding.DecodeString(pngPayload(t))
	if err != nil {
		t.Fatalf("decode PNG fixture Base64: %v", err)
	}
	corruptPNG := bytes.Clone(rawPNG)
	idat := bytes.Index(corruptPNG, []byte("IDAT"))
	if idat < 0 || len(corruptPNG) < idat+4+len(payloadMarker) {
		t.Fatal("PNG fixture has no usable IDAT payload")
	}
	copy(corruptPNG[idat+4:], payloadMarker)
	corruptJPEG := append([]byte{0xff, 0xd8, 0xff, 0xdb, 0x00}, payloadMarker...)

	tests := []struct {
		name       string
		data       string
		wantPrefix string
		wantCause  error
	}{
		{
			name:       "base64",
			data:       badBase64,
			wantPrefix: "engine: kitty plane base64 decode:",
			wantCause:  base64Err,
		},
		{
			name:       "png",
			data:       base64.StdEncoding.EncodeToString(corruptPNG),
			wantPrefix: "engine: kitty plane png decode:",
		},
		{
			name:       "unsupported PNG",
			data:       unsupportedPNG,
			wantPrefix: "engine: kitty plane png decode:",
			wantCause:  pngdecode.ErrUnsupportedPNG,
		},
		{
			name:       "jpeg",
			data:       base64.StdEncoding.EncodeToString(corruptJPEG),
			wantPrefix: "engine: kitty plane jpeg decode:",
		},
		{
			name:       "unknown format",
			data:       base64.StdEncoding.EncodeToString([]byte(payloadMarker)),
			wantPrefix: errUnknownPlaneFormat.Error(),
			wantCause:  errUnknownPlaneFormat,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			img, release, err := e.decodeKittyPlane(test.data)
			if err == nil {
				if release != nil {
					release()
				}
				t.Fatalf("decodeKittyPlane returned %T, want error", img)
			}
			if img != nil {
				if release != nil {
					release()
				}
				t.Fatalf("decodeKittyPlane returned %T with an error, want no image", img)
			}
			if release != nil {
				release()
				t.Fatal("decodeKittyPlane returned a release hook with an error")
			}
			if !strings.HasPrefix(err.Error(), test.wantPrefix) {
				t.Fatalf("error = %q, want prefix %q", err, test.wantPrefix)
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf("error = %v, want wrapped cause %v", err, test.wantCause)
			}
			if strings.Contains(err.Error(), payloadMarker) {
				t.Fatalf("error exposed payload bytes: %q", err)
			}
		})
	}
}

// TestBackendSwitchLatestErrorReplacesInFlightJPEGValidation proves a JPEG
// captured before a switch to direct Kitty records a transient validation drop,
// then a real pixel decode failure after switching back replaces that reason.
func TestBackendSwitchLatestErrorReplacesInFlightJPEGValidation(t *testing.T) {
	t.Parallel()
	const payloadMarker = "WW190-private-payload"
	payload := base64.StdEncoding.EncodeToString(append([]byte{0xff, 0xd8, 0xff, 0xdb, 0x00}, payloadMarker...))
	laterErr := errors.New("engine: live frame decode after backend switch: invalid JPEG marker")
	decodeCalls := atomic.Uint64{}
	ingest := &ingestCounter{on: true, window: ingestWindow}
	ingest.record(time.Now())
	e := &ChromiumEngine{
		backend: backendBlock,
		ingest:  ingest,
		decode: func(string) (*image.RGBA, error) {
			decodeCalls.Add(1)
			return nil, laterErr
		},
	}
	e.SetBackend(backendKitty)
	if e.directKittyPNG(payload) {
		t.Fatal("JPEG payload passed the direct PNG validation")
	}
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	rawQ.put(seqRawFrame{seq: 1, data: payload})

	waitDecodeDrops(t, ingest, 1)
	if got := decodeCalls.Load(); got != 0 {
		t.Fatalf("pixel decoder calls = %d, want 0", got)
	}
	select {
	case got := <-frames:
		t.Fatalf("invalid in-flight JPEG emitted frame %+v", got)
	default:
	}
	stats := ingest.summary()
	if stats.DecodeDrops != 1 {
		t.Fatalf("decode drops = %d, want 1", stats.DecodeDrops)
	}
	const wantPrefix = "engine: direct Kitty PNG validation:"
	if !strings.HasPrefix(stats.DecodeLatestError, wantPrefix) {
		t.Fatalf("latest decode error = %q, want prefix %q", stats.DecodeLatestError, wantPrefix)
	}
	if strings.Contains(stats.DecodeLatestError, payloadMarker) || strings.Contains(stats.DecodeLatestError, payload) {
		t.Fatalf("latest decode error retained payload: %q", stats.DecodeLatestError)
	}

	const laterPayload = "later-private-payload"
	e.SetBackend(backendBlock)
	rawQ.put(seqRawFrame{seq: 2, data: laterPayload})
	waitDecodeDrops(t, ingest, 2)
	if got := decodeCalls.Load(); got != 1 {
		t.Fatalf("pixel decoder calls = %d, want 1", got)
	}
	stats = ingest.summary()
	if stats.DecodeDrops != 2 {
		t.Fatalf("decode drops = %d, want 2", stats.DecodeDrops)
	}
	if stats.DecodeLatestError != laterErr.Error() {
		t.Fatalf("latest decode error = %q, want %q", stats.DecodeLatestError, laterErr)
	}
	if strings.Contains(stats.DecodeLatestError, payload) || strings.Contains(stats.DecodeLatestError, laterPayload) {
		t.Fatalf("latest decode error retained payload: %q", stats.DecodeLatestError)
	}
	select {
	case got := <-frames:
		t.Fatalf("failed decode emitted frame %+v", got)
	default:
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
