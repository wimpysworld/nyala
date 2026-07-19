//go:build live

package engine

import (
	"fmt"
	"image"
	"io"
	"testing"

	xsixel "github.com/charmbracelet/x/ansi/sixel"
	"github.com/wimpysworld/nyala/internal/render"
	renderkitty "github.com/wimpysworld/nyala/internal/render/kitty"
	rendersixel "github.com/wimpysworld/nyala/internal/render/sixel"
)

const (
	benchSixelIntro = "\x1bPq"
	benchSixelST    = "\x1b\\"

	// JPEG conversion can differ by one RGB level when draw.Draw and a direct
	// YCbCr-to-RGB pack round an intermediate value in a different order.
	benchJPEGMaxRGBDelta = 1
)

// BenchmarkCDPNativeFrame compares the current shared full-frame RGBA path with
// a native-image design. Both arms consume the same real Chromium screencast
// payload. The proposed shared and Sixel arms keep JPEG as YCbCr and PNG in the
// PNG decoder's native layout. The Kitty PNG arm rechunks CDP's base64 PNG
// directly into Kitty APC frames and performs no image decode.
//
// The setup captures and validates pixels before ResetTimer. PNG pixels are exact.
// JPEG pixels may differ by benchJPEGMaxRGBDelta per RGB channel. Run with:
//
//	go test -tags live -run '^$' -bench '^BenchmarkCDPNativeFrame$' \
//		-benchmem -benchtime=1s -count=5 ./internal/engine/
func BenchmarkCDPNativeFrame(b *testing.B) {
	payloads := collectScreencastBenchPayloads(b)
	validateNativeFrameOutputs(b, payloads)
	for _, payload := range payloads {
		payload := payload
		benchmarkSharedDecode(b, payload)
		benchmarkKittyFrame(b, payload)
		benchmarkSixelFrame(b, payload)
	}
}

func benchmarkSharedDecode(b *testing.B, payload screencastBenchPayload) {
	b.Run(payload.name+"/shared/current-rgba", func(b *testing.B) {
		e := &ChromiumEngine{backend: "kitty"}
		if _, err := e.decodeLiveFrame(payload.data); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			img, err := e.decodeLiveFrame(payload.data)
			if err != nil {
				b.Fatal(err)
			}
			screencastBenchImage = img
		}
		reportNativeFrameMetrics(b, payload, 0)
	})

	b.Run(payload.name+"/shared/proposed-native", func(b *testing.B) {
		if _, err := decodeNativeScreencastImage(payload.data); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			img, err := decodeNativeScreencastImage(payload.data)
			if err != nil {
				b.Fatal(err)
			}
			screencastBenchImage = img
		}
		reportNativeFrameMetrics(b, payload, 0)
	})
}

func benchmarkKittyFrame(b *testing.B, payload screencastBenchPayload) {
	if payload.name != "png" {
		return
	}
	b.Run(payload.name+"/kitty/direct-png", func(b *testing.B) {
		enc := &renderkitty.LiveEncoder{}
		var count byteCountingWriter
		if err := enc.EncodePNGID(payload.data, 1, &count); err != nil {
			b.Fatal(err)
		}
		outputBytes := count.n
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if err := enc.EncodePNGID(payload.data, 1, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
		reportNativeFrameMetrics(b, payload, outputBytes)
	})
}

func benchmarkSixelFrame(b *testing.B, payload screencastBenchPayload) {
	b.Run(payload.name+"/sixel/current-rgba", func(b *testing.B) {
		e := &ChromiumEngine{backend: "sixel"}
		r := &rendersixel.Renderer{}
		img, err := e.decodeLiveFrame(payload.data)
		if err != nil {
			b.Fatal(err)
		}
		var count byteCountingWriter
		if err := r.Render(render.Frame{Img: img}, &count); err != nil {
			b.Fatal(err)
		}
		outputBytes := count.n
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			img, err := e.decodeLiveFrame(payload.data)
			if err != nil {
				b.Fatal(err)
			}
			if err := r.Render(render.Frame{Img: img}, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
		reportNativeFrameMetrics(b, payload, outputBytes)
	})

	b.Run(payload.name+"/sixel/proposed-native", func(b *testing.B) {
		img, err := decodeNativeScreencastImage(payload.data)
		if err != nil {
			b.Fatal(err)
		}
		var count byteCountingWriter
		if err := encodeNativeSixel(&count, img); err != nil {
			b.Fatal(err)
		}
		outputBytes := count.n
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			img, err := decodeNativeScreencastImage(payload.data)
			if err != nil {
				b.Fatal(err)
			}
			if err := encodeNativeSixel(io.Discard, img); err != nil {
				b.Fatal(err)
			}
		}
		reportNativeFrameMetrics(b, payload, outputBytes)
	})
}

func decodeNativeScreencastImage(data string) (image.Image, error) {
	raw, release, err := decodeBase64Pooled(data)
	if err != nil {
		return nil, fmt.Errorf("engine: base64-decode native frame: %w", err)
	}
	defer release()
	img, err := decodeJPEGFast(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode native frame image: %w", err)
	}
	return img, nil
}

func encodeNativeSixel(w io.Writer, img image.Image) error {
	if _, err := io.WriteString(w, benchSixelIntro); err != nil {
		return err
	}
	var enc xsixel.Encoder
	if err := enc.Encode(w, img); err != nil {
		return err
	}
	_, err := io.WriteString(w, benchSixelST)
	return err
}

func reportNativeFrameMetrics(b *testing.B, payload screencastBenchPayload, terminalBytes int64) {
	b.Helper()
	b.ReportMetric(float64(len(payload.raw)), "input-B")
	if terminalBytes > 0 {
		b.ReportMetric(float64(terminalBytes), "terminal-B")
	}
	b.SetBytes(int64(len(payload.raw)))
}

type byteCountingWriter struct{ n int64 }

func (w *byteCountingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func validateNativeFrameOutputs(tb testing.TB, payloads []screencastBenchPayload) {
	tb.Helper()
	for _, payload := range payloads {
		e := &ChromiumEngine{backend: "kitty"}
		current, err := e.decodeLiveFrame(payload.data)
		if err != nil {
			tb.Fatalf("%s current decode: %v", payload.name, err)
		}
		native, err := decodeNativeScreencastImage(payload.data)
		if err != nil {
			tb.Fatalf("%s native decode: %v", payload.name, err)
		}
		tolerance := 0
		if payload.name != "png" {
			tolerance = benchJPEGMaxRGBDelta
		}
		assertBenchImagesClose(tb, payload.name+" shared", current, native, tolerance)

		if payload.name == "png" {
			if err := (&renderkitty.LiveEncoder{}).EncodePNGID(payload.data, 1, io.Discard); err != nil {
				tb.Fatalf("%s direct Kitty PNG: %v", payload.name, err)
			}
		}
	}
}

func assertBenchImagesClose(tb testing.TB, label string, a, b image.Image, tolerance int) {
	tb.Helper()
	if !a.Bounds().Eq(b.Bounds()) {
		tb.Fatalf("%s bounds differ: %v vs %v", label, a.Bounds(), b.Bounds())
	}
	bounds := a.Bounds()
	maxDelta := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			ar, ag, ab, aa := a.At(x, y).RGBA()
			br, bg, bb, ba := b.At(x, y).RGBA()
			if aa != ba {
				tb.Fatalf("%s alpha differs at (%d,%d): %d vs %d", label, x, y, aa, ba)
			}
			for _, d := range []int{absI(int(ar>>8) - int(br>>8)), absI(int(ag>>8) - int(bg>>8)), absI(int(ab>>8) - int(bb>>8))} {
				if d > maxDelta {
					maxDelta = d
				}
			}
		}
	}
	if maxDelta > tolerance {
		tb.Fatalf("%s max RGB delta = %d, want <= %d", label, maxDelta, tolerance)
	}
}

func absI(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
