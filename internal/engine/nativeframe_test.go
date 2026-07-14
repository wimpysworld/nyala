package engine

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"reflect"
	"testing"

	renderkitty "github.com/wimpysworld/nyala/internal/render/kitty"
)

var nativeFrameBenchImage image.Image

func TestDecodeLiveFrameKeepsNativeJPEGForGraphics(t *testing.T) {
	src := loadSourceRGBA(t)
	raw := encodeBaseline420(t, src)
	e := &ChromiumEngine{backend: "kitty"}
	got, err := e.decodeLiveFrame(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("decodeLiveFrame() = %v", err)
	}
	if _, ok := got.(*image.YCbCr); !ok {
		t.Fatalf("decodeLiveFrame() type = %T, want *image.YCbCr", got)
	}
}

func TestDecodeLiveFrameKeepsNativePNGForPixelConsumer(t *testing.T) {
	data := pngPayload(t)
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("decode test payload: %v", err)
	}
	want, err := decodeFrameImage(raw)
	if err != nil {
		t.Fatalf("decodeFrameImage() = %v", err)
	}
	e := &ChromiumEngine{backend: "sixel"}
	got, err := e.decodeLiveFrame(data)
	if err != nil {
		t.Fatalf("decodeLiveFrame() = %v", err)
	}
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		t.Fatalf("decodeLiveFrame() type = %T, want decoder-native %T", got, want)
	}
}

func TestDecodePoolRetainsDirectKittyPNG(t *testing.T) {
	data := pngPayload(t)
	e := &ChromiumEngine{backend: "kitty"}
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	rawQ.put(seqRawFrame{seq: 1, data: data})

	got := recvFrame(t, frames)
	if got.Img != nil {
		t.Fatalf("direct Kitty PNG decoded to %T, want no pixel image", got.Img)
	}
	if got.EncodedPNG != data {
		t.Fatal("direct Kitty PNG payload differs from the owned CDP string")
	}
	if !got.HasImage() {
		t.Fatal("direct Kitty PNG frame is not marked usable")
	}
}

func TestDecodePoolDecodesPNGForSixel(t *testing.T) {
	data := pngPayload(t)
	e := &ChromiumEngine{backend: "sixel"}
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	rawQ.put(seqRawFrame{seq: 1, data: data})

	got := recvFrame(t, frames)
	if got.Img == nil {
		t.Fatal("Sixel PNG frame has no decoded pixels")
	}
	if got.EncodedPNG != "" {
		t.Fatal("Sixel PNG frame retained a direct Kitty payload")
	}
}

// BenchmarkNativeSharedJPEG measures the full-surface RGBA conversion removed
// from the shared graphics frame seam. Both arms use the same q90 JPEG payload
// and live jpegn decoder; current-rgba adds the former shared conversion.
func BenchmarkNativeSharedJPEG(b *testing.B) {
	src := tileTo(loadSourceRGBA(b), 2560, 2880)
	data := base64.StdEncoding.EncodeToString(encodeBaseline420(b, src))
	e := &ChromiumEngine{backend: "kitty"}
	b.Run("current-rgba", func(b *testing.B) {
		if img, err := e.decodeLiveFrame(data); err != nil {
			b.Fatal(err)
		} else {
			nativeFrameBenchImage = imageToRGBA(img)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			img, err := e.decodeLiveFrame(data)
			if err != nil {
				b.Fatal(err)
			}
			nativeFrameBenchImage = imageToRGBA(img)
		}
	})
	b.Run("native", func(b *testing.B) {
		if img, err := e.decodeLiveFrame(data); err != nil {
			b.Fatal(err)
		} else {
			nativeFrameBenchImage = img
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			img, err := e.decodeLiveFrame(data)
			if err != nil {
				b.Fatal(err)
			}
			nativeFrameBenchImage = img
		}
	})
}

// BenchmarkDirectPNGKitty compares the former decode, RGBA conversion, RGB
// pack, zlib, and base64 path with direct framing of the same CDP base64 PNG.
func BenchmarkDirectPNGKitty(b *testing.B) {
	src := tileTo(loadSourceRGBA(b), 2560, 2880)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		b.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(raw.Bytes())
	e := &ChromiumEngine{backend: "kitty"}
	b.Run("current-decoded", func(b *testing.B) {
		enc := &renderkitty.LiveEncoder{}
		if img, err := e.decodeLiveFrame(data); err != nil {
			b.Fatal(err)
		} else if err := enc.EncodeID(imageToRGBA(img), 1, io.Discard); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			img, err := e.decodeLiveFrame(data)
			if err != nil {
				b.Fatal(err)
			}
			if err := enc.EncodeID(imageToRGBA(img), 1, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("direct", func(b *testing.B) {
		enc := &renderkitty.LiveEncoder{}
		if err := enc.EncodePNGID(data, 1, io.Discard); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if err := enc.EncodePNGID(data, 1, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
}
