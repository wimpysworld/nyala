package pngdecode

import (
	"bytes"
	"image"
	"image/color"
	stdpng "image/png"
	"sync"
	"testing"
	"time"

	woozypng "github.com/woozymasta/png"
)

func TestBufferPoolReusesBuffer(t *testing.T) {
	p := make(bufferPool, 1)
	want := make([]byte, 4, 8)
	p.Put(want)

	got := p.Get(8)
	if &got[0] != &want[0] {
		t.Fatal("Get did not reuse the pooled buffer")
	}
}

func TestBufferPoolEmptyGetAllocates(t *testing.T) {
	p := make(bufferPool, 1)
	done := make(chan []byte)
	go func() { done <- p.Get(8) }()

	select {
	case got := <-done:
		if len(got) != 8 {
			t.Fatalf("Get returned %d bytes, want 8", len(got))
		}
	case <-time.After(time.Second):
		t.Fatal("Get blocked when the pool was empty")
	}
}

func TestBufferPoolOverflowDoesNotBlock(t *testing.T) {
	p := make(bufferPool, 1)
	p.Put(make([]byte, 1))
	done := make(chan struct{})
	go func() {
		p.Put(make([]byte, 1))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Put blocked when the pool was full")
	}
}

func TestProductionPoolRetentionBound(t *testing.T) {
	p, ok := decoder.BufferPool.(bufferPool)
	if !ok {
		t.Fatalf("production pool type = %T, want bufferPool", decoder.BufferPool)
	}
	for len(p) > 0 {
		<-p
	}
	t.Cleanup(func() {
		for len(p) > 0 {
			<-p
		}
	})
	for range poolDepth + 1 {
		p.Put(make([]byte, 1))
	}
	if len(p) != poolDepth {
		t.Fatalf("production pool retains %d buffers, want %d", len(p), poolDepth)
	}
}

func TestDecodeMatchesUnpooled(t *testing.T) {
	raw := testPNG(t)
	want, err := woozypng.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unpooled decode: %v", err)
	}
	got, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("pooled decode: %v", err)
	}
	if !imagesEqual(want, got) {
		t.Fatal("pooled decode pixels differ from unpooled decode")
	}
}

func TestDecodeConcurrent(t *testing.T) {
	raw := testPNG(t)
	want, err := woozypng.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("reference decode: %v", err)
	}
	const workers = 16
	var wg sync.WaitGroup
	results := make(chan struct {
		equal bool
		err   error
	}, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Decode(bytes.NewReader(raw))
			results <- struct {
				equal bool
				err   error
			}{equal: err == nil && imagesEqual(want, got), err: err}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(results)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent decodes did not complete")
	}
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent decode: %v", result.err)
		}
		if !result.equal {
			t.Fatal("concurrent decode pixels differ from reference")
		}
	}
}

func TestDecoderReturnsBuffersAfterError(t *testing.T) {
	raw := testPNG(t)
	p := make(bufferPool, 2)
	dec := woozypng.Decoder{BufferPool: p}
	if _, err := dec.Decode(bytes.NewReader(raw[:len(raw)-8])); err == nil {
		t.Fatal("truncated PNG decode succeeded")
	}
	if len(p) != 2 {
		t.Fatalf("pool contains %d buffers after error, want 2", len(p))
	}
	want, err := woozypng.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("reference decode: %v", err)
	}
	got, err := dec.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("valid decode after error: %v", err)
	}
	if !imagesEqual(want, got) {
		t.Fatal("valid decode after error differs from reference")
	}
}

func BenchmarkDecode(b *testing.B) {
	raw := testPNG(b)
	for _, tc := range []struct {
		name   string
		decode func() (image.Image, error)
	}{
		{name: "unpooled", decode: func() (image.Image, error) { return woozypng.Decode(bytes.NewReader(raw)) }},
		{name: "pooled", decode: func() (image.Image, error) { return Decode(bytes.NewReader(raw)) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			if _, err := tc.decode(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := tc.decode(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func testPNG(tb testing.TB) []byte {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2560, 4))
	for y := range img.Bounds().Dy() {
		for x := range img.Bounds().Dx() {
			img.SetRGBA(x, y, color.RGBA{R: byte(x), G: byte(y), B: byte(x + y), A: 255})
		}
	}
	var raw bytes.Buffer
	if err := stdpng.Encode(&raw, img); err != nil {
		tb.Fatalf("encode PNG: %v", err)
	}
	return raw.Bytes()
}

func imagesEqual(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}
