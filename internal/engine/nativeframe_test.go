package engine

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gen2brain/jpegn"
	woozypng "github.com/woozymasta/png"

	"github.com/wimpysworld/nyala/internal/pngdecode"
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
	unsupportedPNG := unsupportedFramePNG(t)
	corruptJPEG := append([]byte{0xff, 0xd8, 0xff, 0xdb, 0x00}, payloadMarker...)
	unsupportedJPEG := unsupportedBaselineScanJPEG(t)
	e := &ChromiumEngine{backend: backendSixel}
	_, corruptPNGError := pngdecode.Decode(bytes.NewReader(corruptPNG))
	_, unsupportedPNGError := pngdecode.Decode(bytes.NewReader(unsupportedPNG))
	var pngUnsupported woozypng.UnsupportedError
	if !errors.As(unsupportedPNGError, &pngUnsupported) {
		t.Fatalf("unsupported PNG fixture error = %T %v, want UnsupportedError", unsupportedPNGError, unsupportedPNGError)
	}

	tests := []struct {
		name       string
		wantPrefix string
		wantCause  error
		decode     func() (image.Image, error)
	}{
		{
			name:       "frame png",
			wantPrefix: "png decode:",
			wantCause:  corruptPNGError,
			decode: func() (image.Image, error) {
				return decodeFrameImage(corruptPNG)
			},
		},
		{
			name:       "frame unsupported png",
			wantPrefix: "png decode:",
			wantCause:  unsupportedPNGError,
			decode: func() (image.Image, error) {
				return decodeFrameImage(unsupportedPNG)
			},
		},
		{
			name:       "frame jpeg",
			wantPrefix: "native image decode: jpeg decode:",
			wantCause:  jpegn.ErrSyntax,
			decode: func() (image.Image, error) {
				return decodeFrameImage(corruptJPEG)
			},
		},
		{
			name:       "frame unsupported jpeg",
			wantPrefix: "native image decode: jpeg decode:",
			wantCause:  jpegn.ErrUnsupported,
			decode: func() (image.Image, error) {
				return decodeFrameImage(unsupportedJPEG)
			},
		},
		{
			name:       "fast jpeg",
			wantPrefix: "native image decode: jpeg decode:",
			wantCause:  jpegn.ErrSyntax,
			decode: func() (image.Image, error) {
				return decodeJPEGFast(corruptJPEG)
			},
		},
		{
			name:       "live base64",
			wantPrefix: "engine: live frame base64 decode:",
			decode: func() (image.Image, error) {
				return e.decodeLiveFrame(payloadMarker + "*")
			},
		},
		{
			name:       "live png",
			wantPrefix: "engine: live frame decode: png decode:",
			wantCause:  corruptPNGError,
			decode: func() (image.Image, error) {
				return e.decodeLiveFrame(base64.StdEncoding.EncodeToString(corruptPNG))
			},
		},
		{
			name:       "live unsupported png",
			wantPrefix: "engine: live frame decode: png decode:",
			wantCause:  unsupportedPNGError,
			decode: func() (image.Image, error) {
				return e.decodeLiveFrame(base64.StdEncoding.EncodeToString(unsupportedPNG))
			},
		},
		{
			name:       "live jpeg",
			wantPrefix: "engine: live frame decode: native image decode: jpeg decode:",
			wantCause:  jpegn.ErrSyntax,
			decode: func() (image.Image, error) {
				return e.decodeLiveFrame(base64.StdEncoding.EncodeToString(corruptJPEG))
			},
		},
		{
			name:       "live unsupported jpeg",
			wantPrefix: "engine: live frame decode: native image decode: jpeg decode:",
			wantCause:  jpegn.ErrUnsupported,
			decode: func() (image.Image, error) {
				return e.decodeLiveFrame(base64.StdEncoding.EncodeToString(unsupportedJPEG))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			img, err := test.decode()
			if err == nil {
				t.Fatal("decode returned no error")
			}
			if img != nil {
				t.Fatalf("decode returned fallback image %T with error %v", img, err)
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

// TestDecodePoolDropsUnsupportedJPEGWithoutFallbackFrame proves an unsupported
// JPEG produces one reported drop and no frame, even though stdlib can decode
// the same fixture.
func TestDecodePoolDropsUnsupportedJPEGWithoutFallbackFrame(t *testing.T) {
	e := &ChromiumEngine{
		backend: backendSixel,
		ingest:  &ingestCounter{on: true, window: ingestWindow},
	}
	rawQ, frames, stop := drivePool(t, e, 1)
	defer stop()
	payload := base64.StdEncoding.EncodeToString(unsupportedBaselineScanJPEG(t))
	e.ingest.record(time.Unix(0, 0))
	rawQ.put(seqRawFrame{seq: 1, data: payload})
	waitDecodeDrop(t, e.ingest)

	stats := e.ingest.summary()
	if stats.DecodeDrops != 1 {
		t.Fatalf("decode drops = %d, want 1", stats.DecodeDrops)
	}
	if !strings.HasPrefix(stats.DecodeLatestError, "engine: live frame decode: native image decode: jpeg decode:") {
		t.Fatalf("latest decode error = %q, want authoritative jpegn context", stats.DecodeLatestError)
	}
	if strings.Contains(stats.DecodeLatestError, payload) {
		t.Fatalf("latest decode error exposed payload bytes: %q", stats.DecodeLatestError)
	}
	select {
	case frame := <-frames:
		t.Fatalf("decode pool emitted fallback frame: %+v", frame)
	default:
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
		if direct, err := e.directKittyPayload(data); err != nil {
			b.Fatal(err)
		} else if !direct {
			b.Fatal("valid PNG rejected by direct Kitty selector")
		}
		if err := enc.EncodePNGID(data, 1, 0, 0, io.Discard); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if direct, err := e.directKittyPayload(data); err != nil {
				b.Fatal(err)
			} else if !direct {
				b.Fatal("valid PNG rejected by direct Kitty selector")
			}
			if err := enc.EncodePNGID(data, 1, 0, 0, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
}
