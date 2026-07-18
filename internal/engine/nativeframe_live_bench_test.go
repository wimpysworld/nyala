//go:build live

package engine

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/kitty"
	xsixel "github.com/charmbracelet/x/ansi/sixel"
	emmb64 "github.com/emmansun/base64"
	"github.com/klauspost/compress/zlib"
	"github.com/wimpysworld/nyala/internal/render"
	renderkitty "github.com/wimpysworld/nyala/internal/render/kitty"
	rendersixel "github.com/wimpysworld/nyala/internal/render/sixel"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

const (
	benchKittyAPCIntro = "\x1b_G"
	benchKittyST       = "\x1b\\"
	benchKittyMore     = "m=1"
	benchKittyLast     = "m=0"
	benchSixelIntro    = "\x1bPq"
	benchSixelST       = "\x1b\\"

	// JPEG conversion can differ by one RGB level when draw.Draw and a direct
	// YCbCr-to-RGB pack round an intermediate value in a different order.
	benchJPEGMaxRGBDelta = 1
)

// BenchmarkCDPNativeFrame compares the current shared full-frame RGBA path with
// a native-image design. Both arms consume the same real Chromium screencast
// payload. The proposed shared and Sixel arms keep JPEG as YCbCr and PNG in the
// PNG decoder's native layout. The proposed Kitty JPEG arm converts YCbCr
// straight into its final RGB pack. The proposed Kitty PNG arm rechunks CDP's
// base64 PNG directly into Kitty APC frames and performs no image decode.
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
	b.Run(payload.name+"/kitty/current-rgba-rgb", func(b *testing.B) {
		e := &ChromiumEngine{backend: "kitty"}
		enc := &renderkitty.LiveEncoder{}
		img, err := e.decodeLiveFrame(payload.data)
		if err != nil {
			b.Fatal(err)
		}
		var count byteCountingWriter
		if err := enc.EncodeID(imageToRGBA(img), 1, &count); err != nil {
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
			if err := enc.EncodeID(imageToRGBA(img), 1, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
		reportNativeFrameMetrics(b, payload, outputBytes)
	})

	b.Run(payload.name+"/kitty/proposed-native", func(b *testing.B) {
		var outputBytes int64
		if payload.name == "png" {
			enc := &benchKittyPNGEncoder{}
			var count byteCountingWriter
			if err := enc.EncodeBase64PNG(payload.data, screencastBenchWidth, screencastBenchHeight, 1, &count); err != nil {
				b.Fatal(err)
			}
			outputBytes = count.n
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := enc.EncodeBase64PNG(payload.data, screencastBenchWidth, screencastBenchHeight, 1, io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		} else {
			enc := &benchKittyNativeEncoder{}
			img, err := decodeNativeScreencastImage(payload.data)
			if err != nil {
				b.Fatal(err)
			}
			var count byteCountingWriter
			if err := enc.EncodeID(img, 1, &count); err != nil {
				b.Fatal(err)
			}
			outputBytes = count.n
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				img, err := decodeNativeScreencastImage(payload.data)
				if err != nil {
					b.Fatal(err)
				}
				if err := enc.EncodeID(img, 1, io.Discard); err != nil {
					b.Fatal(err)
				}
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

type benchSliceWriter struct{ buf []byte }

func (w *benchSliceWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

type benchKittyNativeEncoder struct {
	rgb   []byte
	zbuf  benchSliceWriter
	zw    *zlib.Writer
	b64   []byte
	frame []byte
}

func (e *benchKittyNativeEncoder) EncodeID(img image.Image, id int, w io.Writer) error {
	if err := e.packRGB(img); err != nil {
		return err
	}
	e.zbuf.buf = e.zbuf.buf[:0]
	if e.zw == nil {
		zw, err := zlib.NewWriterLevel(&e.zbuf, zlib.BestSpeed)
		if err != nil {
			return err
		}
		e.zw = zw
	} else {
		e.zw.Reset(&e.zbuf)
	}
	if _, err := e.zw.Write(e.rgb); err != nil {
		return err
	}
	if err := e.zw.Close(); err != nil {
		return err
	}
	need := emmb64.StdEncoding.EncodedLen(len(e.zbuf.buf))
	if cap(e.b64) < need {
		e.b64 = make([]byte, need)
	}
	e.b64 = e.b64[:need]
	emmb64.StdEncoding.Encode(e.b64, e.zbuf.buf)
	b := img.Bounds()
	opts := kitty.Options{
		Format:       kitty.RGB,
		Compression:  kitty.Zlib,
		Transmission: kitty.Direct,
		Action:       kitty.TransmitAndPut,
		Quite:        2,
		ImageWidth:   b.Dx(),
		ImageHeight:  b.Dy(),
		ID:           id,
	}
	e.frame = appendBenchKittyByteFrames(e.frame[:0], e.b64, opts.Options())
	_, err := w.Write(e.frame)
	return err
}

func (e *benchKittyNativeEncoder) packRGB(img image.Image) error {
	switch src := img.(type) {
	case *image.YCbCr:
		e.packYCbCr(src)
		return nil
	case *image.RGBA:
		e.packRGBA(src)
		return nil
	default:
		return fmt.Errorf("unsupported native Kitty image type %T", img)
	}
}

func (e *benchKittyNativeEncoder) packRGBA(src *image.RGBA) {
	b := src.Bounds()
	n := b.Dx() * b.Dy() * 3
	e.growRGB(n)
	di := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		o := src.PixOffset(b.Min.X, y)
		row := src.Pix[o : o+b.Dx()*4]
		for i := 0; i < len(row); i += 4 {
			e.rgb[di], e.rgb[di+1], e.rgb[di+2] = row[i], row[i+1], row[i+2]
			di += 3
		}
	}
}

func (e *benchKittyNativeEncoder) packYCbCr(src *image.YCbCr) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	e.growRGB(w * h * 3)
	ratH, ratV := benchSubsampleShift(src.SubsampleRatio)
	minXc := b.Min.X / ratH
	minYc := b.Min.Y / ratV
	di := 0
	for py := range h {
		yRow := src.Y[py*src.YStride:]
		cBase := ((b.Min.Y+py)/ratV - minYc) * src.CStride
		cb := src.Cb[cBase:]
		cr := src.Cr[cBase:]
		for px := range w {
			ci := (b.Min.X+px)/ratH - minXc
			r, g, bl := color.YCbCrToRGB(yRow[px], cb[ci], cr[ci])
			e.rgb[di], e.rgb[di+1], e.rgb[di+2] = r, g, bl
			di += 3
		}
	}
}

func (e *benchKittyNativeEncoder) growRGB(n int) {
	if cap(e.rgb) < n {
		e.rgb = make([]byte, n)
	}
	e.rgb = e.rgb[:n]
}

type benchKittyPNGEncoder struct{ frame []byte }

func (e *benchKittyPNGEncoder) EncodeBase64PNG(data string, width, height, id int, w io.Writer) error {
	opts := kitty.Options{
		Format:       kitty.PNG,
		Transmission: kitty.Direct,
		Action:       kitty.TransmitAndPut,
		Quite:        2,
		ImageWidth:   width,
		ImageHeight:  height,
		ID:           id,
	}
	e.frame = appendBenchKittyStringFrames(e.frame[:0], data, opts.Options())
	_, err := w.Write(e.frame)
	return err
}

func appendBenchKittyByteFrames(dst, payload []byte, opts []string) []byte {
	control := strings.Join(opts, ",")
	if len(payload) <= kitty.MaxChunkSize {
		return appendBenchKittyFrame(dst, control, payload)
	}
	control += "," + benchKittyMore
	for len(payload) > kitty.MaxChunkSize {
		dst = appendBenchKittyFrame(dst, control, payload[:kitty.MaxChunkSize])
		payload = payload[kitty.MaxChunkSize:]
		control = benchKittyMore
	}
	return appendBenchKittyFrame(dst, benchKittyLast, payload)
}

func appendBenchKittyStringFrames(dst []byte, payload string, opts []string) []byte {
	control := strings.Join(opts, ",")
	if len(payload) <= kitty.MaxChunkSize {
		return appendBenchKittyStringFrame(dst, control, payload)
	}
	control += "," + benchKittyMore
	for len(payload) > kitty.MaxChunkSize {
		dst = appendBenchKittyStringFrame(dst, control, payload[:kitty.MaxChunkSize])
		payload = payload[kitty.MaxChunkSize:]
		control = benchKittyMore
	}
	return appendBenchKittyStringFrame(dst, benchKittyLast, payload)
}

func appendBenchKittyFrame(dst []byte, control string, payload []byte) []byte {
	dst = append(dst, benchKittyAPCIntro...)
	dst = append(dst, control...)
	dst = append(dst, ';')
	dst = append(dst, payload...)
	dst = append(dst, benchKittyST...)
	return dst
}

func appendBenchKittyStringFrame(dst []byte, control, payload string) []byte {
	dst = append(dst, benchKittyAPCIntro...)
	dst = append(dst, control...)
	dst = append(dst, ';')
	dst = append(dst, payload...)
	dst = append(dst, benchKittyST...)
	return dst
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

		var currentKitty bytes.Buffer
		if err := (&renderkitty.LiveEncoder{}).EncodeID(imageToRGBA(current), 1, &currentKitty); err != nil {
			tb.Fatalf("%s current Kitty encode: %v", payload.name, err)
		}
		var proposedKitty bytes.Buffer
		if payload.name == "png" {
			if err := (&benchKittyPNGEncoder{}).EncodeBase64PNG(payload.data, screencastBenchWidth, screencastBenchHeight, 1, &proposedKitty); err != nil {
				tb.Fatalf("%s proposed Kitty PNG: %v", payload.name, err)
			}
		} else if err := (&benchKittyNativeEncoder{}).EncodeID(native, 1, &proposedKitty); err != nil {
			tb.Fatalf("%s proposed Kitty JPEG: %v", payload.name, err)
		}
		if payload.name != "png" && !bytes.Equal(currentKitty.Bytes(), proposedKitty.Bytes()) {
			tb.Fatalf("%s Kitty output differs, want exact bytes", payload.name)
		}
		currentKittyImg, err := renderdecode.DecodeKitty(currentKitty.Bytes())
		if err != nil {
			tb.Fatalf("%s decode current Kitty output: %v", payload.name, err)
		}
		proposedKittyImg, err := renderdecode.DecodeKitty(proposedKitty.Bytes())
		if err != nil {
			tb.Fatalf("%s decode proposed Kitty output: %v", payload.name, err)
		}
		assertBenchImagesClose(tb, payload.name+" Kitty", currentKittyImg, proposedKittyImg, tolerance)

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

func benchSubsampleShift(r image.YCbCrSubsampleRatio) (h, v int) {
	switch r {
	case image.YCbCrSubsampleRatio420:
		return 2, 2
	case image.YCbCrSubsampleRatio422:
		return 2, 1
	case image.YCbCrSubsampleRatio440:
		return 1, 2
	case image.YCbCrSubsampleRatio411:
		return 4, 1
	case image.YCbCrSubsampleRatio410:
		return 4, 2
	default:
		return 1, 1
	}
}

func absI(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
