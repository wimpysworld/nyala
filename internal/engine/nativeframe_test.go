package engine

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"reflect"
	"strings"
	"testing"

	renderkitty "github.com/wimpysworld/nyala/internal/render/kitty"
)

var nativeFrameBenchImage image.Image

func TestDecodeLiveFrameKeepsNativeJPEGForBlock(t *testing.T) {
	src := loadSourceRGBA(t)
	raw := encodeBaseline420(t, src)
	e := &ChromiumEngine{backend: backendBlock}
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
	e := &ChromiumEngine{backend: backendSixel}
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
	e := &ChromiumEngine{backend: backendKitty}
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
	for _, workers := range []int{1, defaultDecodeWorkers(), maxDecodeWorkers} {
		t.Run(itoa(workers), func(t *testing.T) {
			e := &ChromiumEngine{backend: backendSixel}
			rawQ, frames, stop := drivePool(t, e, workers)
			defer stop()
			rawQ.put(seqRawFrame{seq: 1, data: data})

			got := recvFrame(t, frames)
			if got.Img == nil {
				t.Fatal("Sixel PNG frame has no decoded pixels")
			}
			if got.EncodedPNG != "" {
				t.Fatal("Sixel PNG frame retained a direct Kitty payload")
			}
		})
	}
}

func TestFrameDecodeErrorsExposeStableContext(t *testing.T) {
	const payloadMarker = "WW190_PAYLOAD_BYTES"
	corruptPNG := append(append([]byte(nil), pngSignature...), payloadMarker...)
	corruptJPEG := append([]byte{0xff, 0xd8, 0xff, 0xdb, 0x00}, payloadMarker...)
	e := &ChromiumEngine{backend: backendSixel}

	tests := []struct {
		name       string
		wantPrefix string
		decode     func() error
	}{
		{
			name:       "frame png",
			wantPrefix: "png decode:",
			decode: func() error {
				_, err := decodeFrameImage(corruptPNG)
				return err
			},
		},
		{
			name:       "frame jpeg",
			wantPrefix: "jpeg decode:",
			decode: func() error {
				_, err := decodeFrameImage(corruptJPEG)
				return err
			},
		},
		{
			name:       "fast png fallback",
			wantPrefix: "native image decode: png decode:",
			decode: func() error {
				_, err := decodeJPEGFast(corruptPNG)
				return err
			},
		},
		{
			name:       "fast jpeg fallback",
			wantPrefix: "native image decode: jpeg decode:",
			decode: func() error {
				_, err := decodeJPEGFast(corruptJPEG)
				return err
			},
		},
		{
			name:       "live base64",
			wantPrefix: "engine: live frame base64 decode:",
			decode: func() error {
				_, err := e.decodeLiveFrame(payloadMarker + "*")
				return err
			},
		},
		{
			name:       "live png",
			wantPrefix: "engine: live frame decode: native image decode: png decode:",
			decode: func() error {
				_, err := e.decodeLiveFrame(base64.StdEncoding.EncodeToString(corruptPNG))
				return err
			},
		},
		{
			name:       "live jpeg",
			wantPrefix: "engine: live frame decode: native image decode: jpeg decode:",
			decode: func() error {
				_, err := e.decodeLiveFrame(base64.StdEncoding.EncodeToString(corruptJPEG))
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.decode()
			if err == nil {
				t.Fatal("decode returned no error")
			}
			if !strings.HasPrefix(err.Error(), test.wantPrefix) {
				t.Fatalf("error = %q, want prefix %q", err, test.wantPrefix)
			}
			if strings.Contains(err.Error(), payloadMarker) {
				t.Fatalf("error exposed payload bytes: %q", err)
			}
		})
	}
}

// BenchmarkNativeSharedJPEG measures the full-surface RGBA conversion removed
// from the shared graphics frame seam. Both arms use the same q90 JPEG payload
// and live jpegn decoder; current-rgba adds the former shared conversion.
func BenchmarkNativeSharedJPEG(b *testing.B) {
	src := tileTo(loadSourceRGBA(b), 2560, 2880)
	data := base64.StdEncoding.EncodeToString(encodeBaseline420(b, src))
	e := &ChromiumEngine{backend: backendKitty}
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

// BenchmarkDirectPNGKitty measures direct validation and framing of a CDP PNG.
func BenchmarkDirectPNGKitty(b *testing.B) {
	src := tileTo(loadSourceRGBA(b), 2560, 2880)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		b.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(raw.Bytes())
	e := &ChromiumEngine{backend: backendKitty}
	b.Run("direct", func(b *testing.B) {
		enc := &renderkitty.LiveEncoder{}
		if !e.directKittyPNG(data) {
			b.Fatal("valid PNG rejected by direct Kitty selector")
		}
		if err := enc.EncodePNGID(data, 1, 0, 0, io.Discard); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if !e.directKittyPNG(data) {
				b.Fatal("valid PNG rejected by direct Kitty selector")
			}
			if err := enc.EncodePNGID(data, 1, 0, 0, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
}
