package sixel

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	gosixel "github.com/mattn/go-sixel"
	xdraw "golang.org/x/image/draw"

	"github.com/wimpysworld/nyala/internal/render"
)

// Compile-time assertion that Renderer satisfies render.Renderer.
var _ render.Renderer = (*Renderer)(nil)

// knownFrame builds a small two-colour RGBA frame for the payload tests.
func knownFrame(t *testing.T) render.Frame {
	t.Helper()
	const w, h = 8, 12
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}
	blue := color.RGBA{R: 0x00, G: 0x00, B: 0xff, A: 0xff}
	for y := range h {
		for x := range w {
			if y < h/2 {
				img.SetRGBA(x, y, red)
			} else {
				img.SetRGBA(x, y, blue)
			}
		}
	}
	return render.Frame{Img: img}
}

func TestName(t *testing.T) {
	var r Renderer
	if got := r.Name(); got != "sixel" {
		t.Fatalf("Name() = %q, want %q", got, "sixel")
	}
}

func TestRenderEmitsFramedDCSPayload(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(knownFrame(t), &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "\x1bP0;0;8q") {
		t.Errorf("payload does not start with parameterised Sixel DCS introducer: %q", out[:min(len(out), 12)])
	}
	if !strings.HasSuffix(out, "\x1b\\") {
		t.Errorf("payload does not end with String Terminator ESC backslash")
	}

	body := strings.TrimSuffix(strings.TrimPrefix(out, "\x1bP0;0;8q"), "\x1b\\")
	if len(body) == 0 {
		t.Fatalf("payload body is empty, want non-empty Sixel data")
	}
	// The encoder body opens with the raster attribute introducer.
	if body[0] != '"' {
		t.Errorf("body does not start with raster attribute %q, got %q", '"', body[0])
	}
	// A palette colour introducer must appear somewhere in the body.
	if !strings.ContainsRune(body, '#') {
		t.Errorf("body has no colour introducer %q", '#')
	}
}

func TestRenderNilImageEmitsEmptyFramedPayload(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(render.Frame{}, &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	if got := buf.String(); got != "\x1bP0;0;8q\x1b\\" {
		t.Fatalf("nil-image payload = %q, want framed-but-empty %q", got, "\x1bP0;0;8q\x1b\\")
	}
}

var errTestWriter = errors.New("test writer failure")

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errTestWriter }

func TestRenderReusesEncoderAndRecoversFromWriterError(t *testing.T) {
	var r Renderer
	if err := r.Render(knownFrame(t), errorWriter{}); !errors.Is(err, errTestWriter) {
		t.Fatalf("Render() error = %v, want %v", err, errTestWriter)
	}
	if r.dst.dst != nil {
		t.Fatal("renderer retained the failed writer")
	}
	enc := r.enc
	if enc == nil {
		t.Fatal("renderer did not initialise its persistent encoder")
	}

	var first, second bytes.Buffer
	if err := r.Render(knownFrame(t), &first); err != nil {
		t.Fatalf("Render() after writer error: %v", err)
	}
	if err := r.Render(knownFrame(t), &second); err != nil {
		t.Fatalf("second Render(): %v", err)
	}
	if r.enc != enc {
		t.Fatal("renderer replaced its persistent encoder")
	}
	if r.dst.dst != nil {
		t.Fatal("renderer retained the successful writer")
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("repeated render output differs")
	}
}

func TestRenderConcurrentCallsAreDeterministic(t *testing.T) {
	var r Renderer
	var want bytes.Buffer
	if err := r.Render(knownFrame(t), &want); err != nil {
		t.Fatalf("reference Render(): %v", err)
	}

	const workers = 8
	outputs := make([][]byte, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			if err := r.Render(knownFrame(t), &out); err != nil {
				errs <- err
				return
			}
			outputs[worker] = append([]byte(nil), out.Bytes()...)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for worker, output := range outputs {
		if !bytes.Equal(output, want.Bytes()) {
			t.Fatalf("worker %d output differs", worker)
		}
	}
}

func TestOpaqueEncoderMatchesGoSixel(t *testing.T) {
	t.Parallel()
	bounds := image.Rect(3, 5, 83, 53)
	rgba := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			rgba.SetRGBA(x, y, color.RGBA{
				R: uint8(x*17 + y*3), //nolint:gosec // Overflow creates the test pattern.
				G: uint8(x*5 + y*11), //nolint:gosec // Overflow creates the test pattern.
				B: uint8(x*7 + y*13), //nolint:gosec // Overflow creates the test pattern.
				A: 0xff,
			})
		}
	}
	nrgba := image.NewNRGBA(bounds)
	draw.Draw(nrgba, bounds, rgba, bounds.Min, draw.Src)
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, rgba, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	ycbcr, err := jpeg.Decode(&jpegBytes)
	if err != nil {
		t.Fatal(err)
	}

	for name, img := range map[string]image.Image{
		"rgba":  rgba,
		"nrgba": nrgba,
		"ycbcr": ycbcr,
	} {
		t.Run(name, func(t *testing.T) {
			assertOpaque(t, img)
			var want bytes.Buffer
			upstream := gosixel.NewEncoder(&want)
			upstream.Colors = 256
			if err := upstream.Encode(img); err != nil {
				t.Fatalf("upstream Encode(): %v", err)
			}
			var got bytes.Buffer
			if err := newOpaqueEncoder(&got).encode(img); err != nil {
				t.Fatalf("opaque encode(): %v", err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("opaque payload differs from go-sixel v0.0.12: got %d bytes, want %d", got.Len(), want.Len())
			}
		})
	}
}

func BenchmarkOpaqueEncoderComparison(b *testing.B) {
	file, err := os.Open("../../../testdata/linuxmatters.png")
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()
	src, err := png.Decode(file)
	if err != nil {
		b.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1600))
	xdraw.CatmullRom.Scale(img, img.Bounds(), src, src.Bounds(), draw.Src, nil)
	assertOpaque(b, img)

	var baseline, candidate bytes.Buffer
	upstream := gosixel.NewEncoder(&baseline)
	upstream.Colors = 256
	if err := upstream.Encode(img); err != nil {
		b.Fatal(err)
	}
	if err := newOpaqueEncoder(&candidate).encode(img); err != nil {
		b.Fatal(err)
	}
	if !bytes.Equal(candidate.Bytes(), baseline.Bytes()) {
		b.Fatal("opaque payload differs from go-sixel v0.0.12")
	}
	payloadBytes := baseline.Len()

	b.Run("upstream-alpha", func(b *testing.B) {
		encoder := gosixel.NewEncoder(io.Discard)
		encoder.Colors = 256
		b.ReportAllocs()
		b.ReportMetric(float64(payloadBytes), "payload-B")
		for range b.N {
			if err := encoder.Encode(img); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("nyala-opaque", func(b *testing.B) {
		encoder := newOpaqueEncoder(io.Discard)
		b.ReportAllocs()
		b.ReportMetric(float64(payloadBytes), "payload-B")
		for range b.N {
			if err := encoder.encode(img); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func assertOpaque(tb testing.TB, img image.Image) {
	tb.Helper()
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := img.At(x, y).RGBA()
			if alpha != 0xffff {
				tb.Fatalf("pixel (%d,%d) alpha = %d, want 65535", x, y, alpha)
			}
		}
	}
}
