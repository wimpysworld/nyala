package engine

import (
	"bytes"
	"image"
	stdpng "image/png"
	"os"
	"testing"

	woozypng "github.com/woozymasta/png"
)

// TestWoozymastaPNGByteIdenticalToStdlib asserts the woozymasta PNG decoder is a
// byte-identical drop-in for stdlib image/png on a committed 8-bit RGBA fixture. It
// is deliberately not build-tagged so it runs on every arch, including arm64 CI: the
// SIMD filter paths differ per arch and native arm64 byte-identity could not be
// verified locally, so this test is the gate that catches any divergence.
func TestWoozymastaPNGByteIdenticalToStdlib(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/linuxmatters.png")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	stdImg, err := stdpng.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("stdlib decode: %v", err)
	}
	wooImg, err := woozypng.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("woozymasta decode: %v", err)
	}

	// Convert both the same way the render path does (imageToRGBA), keeping RGBA byte
	// order, then compare the exact pixel bytes and geometry.
	std := imageToRGBA(stdImg)
	woo := imageToRGBA(wooImg)
	assertRGBAEqual(t, "stdlib vs woozymasta", std, woo)

	// decodeFrameImage sniffs the PNG signature and must route to the same decode.
	frameImg, err := decodeFrameImage(raw)
	if err != nil {
		t.Fatalf("decodeFrameImage: %v", err)
	}
	assertRGBAEqual(t, "stdlib vs decodeFrameImage", std, imageToRGBA(frameImg))
}

// assertRGBAEqual fails the test unless a and b share bounds, stride, and pixel bytes.
func assertRGBAEqual(t *testing.T, label string, a, b *image.RGBA) {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("%s: bounds differ: %v vs %v", label, a.Bounds(), b.Bounds())
	}
	if a.Stride != b.Stride {
		t.Fatalf("%s: stride differ: %d vs %d", label, a.Stride, b.Stride)
	}
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatalf("%s: pixel bytes differ", label)
	}
}
