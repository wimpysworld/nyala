package pngdecode

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// encodePNG encodes img through the stdlib encoder, the reference producer for
// the fixture shapes (stdlib is acceptable in tests).
func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// rgbFixture is an opaque gradient the stdlib encoder writes as 8-bit
// truecolour (colour type 2).
func rgbFixture(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 5), B: uint8(x + y), A: 255})
		}
	}
	return img
}

// rgbaFixture is a translucent gradient the stdlib encoder writes as 8-bit
// truecolour with alpha (colour type 6).
func rgbaFixture(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			// #nosec G115 -- the fixture alpha wraps modulo 256 by design.
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 9), G: uint8(y * 3), B: uint8(x ^ y), A: uint8(255 - x - y)})
		}
	}
	return img
}

// noisyFixture is a pseudo-random opaque image that compresses badly, so its
// IDAT stream spans several chunks and exercises the multi-IDAT reader.
func noisyFixture(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	state := uint32(1)
	for i := 0; i < len(img.Pix); i += 4 {
		state = state*1664525 + 1013904223
		img.Pix[i+0] = uint8(state >> 24)
		img.Pix[i+1] = uint8(state >> 16 & 0xFF)
		img.Pix[i+2] = uint8(state >> 8 & 0xFF)
		img.Pix[i+3] = 255
	}
	return img
}

// writeChunk appends one PNG chunk with its CRC to b.
func writeChunk(b *bytes.Buffer, typ string, data []byte) {
	var head [8]byte
	// #nosec G115 -- the test chunks are a few bytes long.
	binary.BigEndian.PutUint32(head[:4], uint32(len(data)))
	copy(head[4:], typ)
	b.Write(head[:])
	b.Write(data)
	crc := crc32.NewIEEE()
	crc.Write(head[4:])
	crc.Write(data)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc.Sum32())
	b.Write(tail[:])
}

// interlacedPNG hand-builds a 1x1 Adam7-interlaced truecolour PNG, a shape the
// stdlib encoder cannot produce, to drive the DecodeInto fallback.
func interlacedPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(pngHeader)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 1)
	binary.BigEndian.PutUint32(ihdr[4:8], 1)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 2  // truecolour
	ihdr[12] = 1 // Adam7
	writeChunk(&b, "IHDR", ihdr)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	// Pass 1 holds the single pixel: filter none plus one RGB pixel.
	if _, err := zw.Write([]byte{0, 0x12, 0x34, 0x56}); err != nil {
		t.Fatalf("compress interlaced payload: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zlib writer: %v", err)
	}
	writeChunk(&b, "IDAT", z.Bytes())
	writeChunk(&b, "IEND", nil)
	return b.Bytes()
}

// TestDecodeIntoMatchesStdlib decodes each fixture with DecodeInto and asserts
// every pixel byte-for-byte against the stdlib decode of the same bytes, in
// straight-alpha RGBA order.
func TestDecodeIntoMatchesStdlib(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"rgb":                 encodePNG(t, rgbFixture(23, 17)),
		"rgba":                encodePNG(t, rgbaFixture(23, 17)),
		"multi idat rgb":      encodePNG(t, noisyFixture(160, 120)),
		"gray fallback":       encodePNG(t, image.NewGray(image.Rect(0, 0, 9, 7))),
		"interlaced fallback": interlacedPNG(t),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ref, err := png.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("stdlib decode: %v", err)
			}
			b := ref.Bounds()
			wantW, wantH := b.Dx(), b.Dy()
			dst := make([]byte, 4*wantW*wantH)
			w, h, err := DecodeInto(bytes.NewReader(raw), dst)
			if err != nil {
				t.Fatalf("DecodeInto: %v", err)
			}
			if w != wantW || h != wantH {
				t.Fatalf("DecodeInto size = %dx%d, want %dx%d", w, h, wantW, wantH)
			}
			for y := range h {
				for x := range w {
					want := color.NRGBAModel.Convert(ref.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
					i := 4 * (y*w + x)
					got := color.NRGBA{R: dst[i], G: dst[i+1], B: dst[i+2], A: dst[i+3]}
					if got != want {
						t.Fatalf("pixel (%d,%d) = %+v, want %+v (RGBA order)", x, y, got, want)
					}
				}
			}
		})
	}
}

// TestDecodeIntoDstTooSmall proves an undersized plane fails cleanly on both
// the fast path and the fallback.
func TestDecodeIntoDstTooSmall(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"fast":     encodePNG(t, rgbaFixture(8, 8)),
		"fallback": encodePNG(t, image.NewGray(image.Rect(0, 0, 8, 8))),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dst := make([]byte, 4*8*8-1)
			if _, _, err := DecodeInto(bytes.NewReader(raw), dst); !errors.Is(err, ErrDstTooSmall) {
				t.Fatalf("DecodeInto error = %v, want ErrDstTooSmall", err)
			}
		})
	}
}

// TestDimensions checks the IHDR size peek and its non-PNG rejection.
func TestDimensions(t *testing.T) {
	t.Parallel()
	raw := encodePNG(t, rgbFixture(23, 17))
	w, h, ok := Dimensions(raw)
	if !ok || w != 23 || h != 17 {
		t.Fatalf("Dimensions = %d, %d, %v; want 23, 17, true", w, h, ok)
	}
	if _, _, ok := Dimensions([]byte("not a png, far too short")); ok {
		t.Fatal("Dimensions accepted a non-PNG payload")
	}
}

// TestDecodeIntoSteadyStateAllocs proves the fast path allocates nothing per
// decode once its pools are warm: no pixel plane, no row buffers, no inflater.
func TestDecodeIntoSteadyStateAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("race-detector instrumentation inflates allocation counts")
	}
	raw := encodePNG(t, noisyFixture(160, 120))
	dst := make([]byte, 4*160*120)
	br := bytes.NewReader(raw)
	var decodeErr error
	run := func() {
		br.Reset(raw)
		if _, _, err := DecodeInto(br, dst); err != nil {
			decodeErr = err
		}
	}
	run() // warm the row, inflater, and state pools
	allocs := testing.AllocsPerRun(50, run)
	if decodeErr != nil {
		t.Fatalf("DecodeInto: %v", decodeErr)
	}
	if allocs != 0 {
		t.Fatalf("DecodeInto steady state allocates %.1f objects per frame, want 0", allocs)
	}
}
