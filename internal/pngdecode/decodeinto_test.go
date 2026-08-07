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

// insertChunkAfterIHDR returns raw with one complete chunk inserted after IHDR.
func insertChunkAfterIHDR(raw []byte, typ string, data []byte) []byte {
	var b bytes.Buffer
	b.Write(raw[:33])
	writeChunk(&b, typ, data)
	b.Write(raw[33:])
	return b.Bytes()
}

// interlacedPNG hand-builds a 1x1 Adam7-interlaced truecolour PNG, a shape the
// stdlib encoder cannot produce.
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
		"rgb":            encodePNG(t, rgbFixture(23, 17)),
		"rgba":           encodePNG(t, rgbaFixture(23, 17)),
		"multi idat rgb": encodePNG(t, noisyFixture(160, 120)),
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

// TestDecodeIntoRejectsUnsupportedPNG checks the public error taxonomy for
// valid PNG features and chunk sequences outside the direct decoder.
func TestDecodeIntoRejectsUnsupportedPNG(t *testing.T) {
	t.Parallel()
	rgb := encodePNG(t, rgbFixture(2, 2))
	rgb16Image := image.NewRGBA64(image.Rect(0, 0, 2, 2))
	red := [...]uint16{0, 0x1111}
	green := [...]uint16{0, 0x2222}
	for y := range 2 {
		for x := range 2 {
			rgb16Image.SetRGBA64(x, y, color.RGBA64{R: red[x], G: green[y], B: 0x3333, A: 0xFFFF})
		}
	}
	rgb16 := encodePNG(t, rgb16Image)
	if rgb16[24] != 16 {
		t.Fatalf("16-bit fixture depth = %d, want 16", rgb16[24])
	}
	withIHDRByte := func(offset int, value byte) []byte {
		raw := append([]byte(nil), rgb...)
		raw[offset] = value
		binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
		return raw
	}
	cases := map[string][]byte{
		"bit depth":              rgb16,
		"greyscale colour type":  encodePNG(t, image.NewGray(image.Rect(0, 0, 2, 2))),
		"palette colour type":    encodePNG(t, image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})),
		"compression method":     withIHDRByte(26, 1),
		"filter method":          withIHDRByte(27, 1),
		"Adam7":                  interlacedPNG(t),
		"tRNS":                   insertChunkAfterIHDR(rgb, "tRNS", []byte{0, 0, 0, 0, 0, 0}),
		"unknown critical chunk": insertChunkAfterIHDR(rgb, "ABCD", nil),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := DecodeInto(bytes.NewReader(raw), make([]byte, 16)); !errors.Is(err, ErrUnsupportedPNG) {
				t.Fatalf("DecodeInto error = %v, want ErrUnsupportedPNG", err)
			} else if errors.Is(err, ErrDstTooSmall) {
				t.Fatalf("DecodeInto error = %v, must differ from ErrDstTooSmall", err)
			}
		})
	}
	if _, _, err := DecodeInto(bytes.NewReader(cases["tRNS"]), nil); !errors.Is(err, ErrUnsupportedPNG) {
		t.Fatalf("DecodeInto tRNS with empty destination error = %v, want ErrUnsupportedPNG", err)
	}
}

// nonContiguousIDATPNG returns a PNG whose compressed stream resumes after an
// ancillary chunk.
func nonContiguousIDATPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(pngHeader)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 1)
	binary.BigEndian.PutUint32(ihdr[4:8], 1)
	ihdr[8] = 8
	ihdr[9] = ctTrueColor
	writeChunk(&b, "IHDR", ihdr)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write([]byte{0, 0x12, 0x34, 0x56}); err != nil {
		t.Fatalf("compress payload: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zlib writer: %v", err)
	}
	compressed := z.Bytes()
	writeChunk(&b, "IDAT", compressed[:2])
	writeChunk(&b, "tEXt", nil)
	writeChunk(&b, "IDAT", compressed[2:])
	writeChunk(&b, "IEND", nil)
	return b.Bytes()
}

// truncatedPixelPNG returns a PNG whose sole IDAT ends part-way through its
// compressed pixel stream.
func truncatedPixelPNG(t *testing.T, raw []byte) []byte {
	t.Helper()
	length := int(binary.BigEndian.Uint32(raw[33:37]))
	if string(raw[37:41]) != "IDAT" || length < 4 {
		t.Fatal("fixture does not start with a usable IDAT chunk")
	}
	var b bytes.Buffer
	b.Write(raw[:33])
	writeChunk(&b, "IDAT", raw[41:41+length/2])
	writeChunk(&b, "IEND", nil)
	return b.Bytes()
}

// TestDecodeIntoMalformedIsNotUnsupported checks structural and pixel-stream
// errors stay separate from unsupported but valid input.
func TestDecodeIntoMalformedIsNotUnsupported(t *testing.T) {
	t.Parallel()
	rgb := encodePNG(t, rgbFixture(2, 2))
	badSignature := append([]byte(nil), rgb...)
	badSignature[0] = 0
	cases := map[string]struct {
		raw []byte
		dst []byte
	}{
		"bad signature":       {raw: badSignature, dst: make([]byte, 16)},
		"duplicate IHDR":      {raw: insertChunkAfterIHDR(rgb, "IHDR", rgb[16:29]), dst: make([]byte, 16)},
		"IEND before IDAT":    {raw: insertChunkAfterIHDR(rgb, "IEND", nil), dst: make([]byte, 16)},
		"non-contiguous IDAT": {raw: nonContiguousIDATPNG(t), dst: make([]byte, 4)},
		"truncated pixels":    {raw: truncatedPixelPNG(t, rgb), dst: make([]byte, 16)},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := DecodeInto(bytes.NewReader(test.raw), test.dst)
			if !errors.Is(err, errFormat) {
				t.Fatalf("DecodeInto error = %v, want errFormat", err)
			}
			if errors.Is(err, ErrUnsupportedPNG) || errors.Is(err, ErrDstTooSmall) {
				t.Fatalf("DecodeInto error = %v, want a distinct malformed-data error", err)
			}
		})
	}
}

// TestDecodeIntoDstTooSmall proves an undersized plane fails cleanly for both
// supported colour types.
func TestDecodeIntoDstTooSmall(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"rgb":  encodePNG(t, rgbFixture(8, 8)),
		"rgba": encodePNG(t, rgbaFixture(8, 8)),
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

// TestDecodeIntoRejectsOverflowingDimensions proves the fast path refuses
// IHDR dimensions whose 4*w*h would overflow int and slip past the size
// check, admitting a far-too-small dst.
func TestDecodeIntoRejectsOverflowingDimensions(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	b.WriteString(pngHeader)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 0xFFFFFFF0) // width
	binary.BigEndian.PutUint32(ihdr[4:8], 0xFFFFFFF0) // height
	ihdr[8] = 8                                       // bit depth
	ihdr[9] = 2                                       // truecolour: the fast-path shape
	writeChunk(&b, "IHDR", ihdr)
	dst := make([]byte, 64)
	if _, _, err := DecodeInto(bytes.NewReader(b.Bytes()), dst); !errors.Is(err, ErrDstTooSmall) {
		t.Fatalf("DecodeInto error = %v, want ErrDstTooSmall", err)
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
