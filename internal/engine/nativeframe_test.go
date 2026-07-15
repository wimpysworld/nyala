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

// TestDecodeLiveFrameSixelJPEGWorkerBoundary pins the memory-policy boundary. A
// four-worker Sixel session uses fused RGBA output, while a manual five-worker
// session keeps native YCbCr.
func TestDecodeLiveFrameSixelJPEGWorkerBoundary(t *testing.T) {
	if maxSixelRGBAWorkers != 4 {
		t.Fatalf("maxSixelRGBAWorkers = %d, want 4", maxSixelRGBAWorkers)
	}
	src := loadSourceRGBA(t)
	data := base64.StdEncoding.EncodeToString(encodeBaseline420(t, src))
	cases := []struct {
		workers  int
		wantRGBA bool
	}{
		{workers: maxSixelRGBAWorkers, wantRGBA: true},
		{workers: maxSixelRGBAWorkers + 1, wantRGBA: false},
	}
	for _, tc := range cases {
		t.Run(itoa(tc.workers), func(t *testing.T) {
			e := &ChromiumEngine{backend: "sixel", decodeWorkers: tc.workers}
			got, err := e.decodeLiveFrame(data)
			if err != nil {
				t.Fatalf("decodeLiveFrame() = %v", err)
			}
			_, rgba := got.(*image.RGBA)
			if rgba != tc.wantRGBA {
				t.Fatalf("decodeLiveFrame() type = %T, want RGBA=%v", got, tc.wantRGBA)
			}
			if !tc.wantRGBA {
				if _, ok := got.(*image.YCbCr); !ok {
					t.Fatalf("decodeLiveFrame() type = %T, want native *image.YCbCr", got)
				}
			}
		})
	}
}

// TestSixelJPEGToRGBABackendSwitch proves both switch directions use the current
// backend without changing the recorded session count. Block stays native, Sixel
// takes fused RGBA, and the return to Block restores native YCbCr.
func TestSixelJPEGToRGBABackendSwitch(t *testing.T) {
	src := loadSourceRGBA(t)
	data := base64.StdEncoding.EncodeToString(encodeBaseline420(t, src))
	e := &ChromiumEngine{backend: "block", decodeWorkers: maxSixelRGBAWorkers}

	assertType := func(wantRGBA bool) {
		t.Helper()
		got, err := e.decodeLiveFrame(data)
		if err != nil {
			t.Fatalf("decodeLiveFrame() = %v", err)
		}
		_, rgba := got.(*image.RGBA)
		if rgba != wantRGBA {
			t.Fatalf("backend %q decoded %T, want RGBA=%v", e.backend, got, wantRGBA)
		}
	}

	assertType(false)
	e.SetBackend("sixel")
	assertType(true)
	e.SetBackend("block")
	assertType(false)
	if e.decodeWorkers != maxSixelRGBAWorkers {
		t.Fatalf("backend cycle changed session workers to %d, want %d", e.decodeWorkers, maxSixelRGBAWorkers)
	}
}

// TestSixelJPEGToRGBAActivation pins the paths that must stay native or lossless.
func TestSixelJPEGToRGBAActivation(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		workers int
		capture bool
		enabled bool
	}{
		{name: "sixel four", backend: "sixel", workers: 4, enabled: true},
		{name: "sixel five", backend: "sixel", workers: 5},
		{name: "block four", backend: "block", workers: 4},
		{name: "kitty four", backend: "kitty", workers: 4},
		{name: "capture", backend: "sixel", workers: 1, capture: true},
		{name: "no session", backend: "sixel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &ChromiumEngine{backend: tc.backend, decodeWorkers: tc.workers, captureOn: tc.capture}
			if got := e.sixelJPEGToRGBA(); got != tc.enabled {
				t.Fatalf("sixelJPEGToRGBA() = %v, want %v", got, tc.enabled)
			}
		})
	}
}

// TestSixelJPEGToRGBASessionReset proves teardown clears the worker count, so a
// dead session cannot enable the larger RGBA decode in its replacement window.
func TestSixelJPEGToRGBASessionReset(t *testing.T) {
	e := &ChromiumEngine{backend: "sixel", decodeWorkers: 4}
	if !e.sixelJPEGToRGBA() {
		t.Fatal("live four-worker Sixel session did not enable RGBA")
	}
	e.teardownSession()
	if e.sixelJPEGToRGBA() {
		t.Fatal("torn-down session retained Sixel RGBA activation")
	}
	if e.decodeWorkers != 0 {
		t.Fatalf("torn-down session workers = %d, want 0", e.decodeWorkers)
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
	e := &ChromiumEngine{backend: "sixel", decodeWorkers: 4}
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
		if !e.directKittyPNG(data) {
			b.Fatal("valid PNG rejected by direct Kitty selector")
		}
		if err := enc.EncodePNGID(data, 1, io.Discard); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if !e.directKittyPNG(data) {
				b.Fatal("valid PNG rejected by direct Kitty selector")
			}
			if err := enc.EncodePNGID(data, 1, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
}
