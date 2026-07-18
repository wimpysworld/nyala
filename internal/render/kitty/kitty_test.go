package kitty

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/renderdecode"
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
	if got := r.Name(); got != "kitty" {
		t.Fatalf("Name() = %q, want %q", got, "kitty")
	}
}

func TestRenderEmitsFramedAPCPayload(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(knownFrame(t), &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "\x1b_G") {
		t.Errorf("payload does not start with APC introducer ESC _ G: %q", out[:min(len(out), 8)])
	}
	if !strings.HasSuffix(out, "\x1b\\") {
		t.Errorf("payload does not end with String Terminator ESC backslash")
	}

	inner := strings.TrimSuffix(strings.TrimPrefix(out, "\x1b_G"), "\x1b\\")
	semi := strings.IndexByte(inner, ';')
	if semi < 0 {
		t.Fatalf("payload has no options/payload separator %q", ';')
	}
	opts, b64 := inner[:semi], inner[semi+1:]

	// The options must declare zlib compression and the image dimensions so a
	// decoder can reverse the payload. The encoder omits f=32 because RGBA is
	// the protocol default; an explicit non-RGBA format would appear here.
	for _, want := range []string{"o=z", "s=8", "v=12"} {
		if !strings.Contains(opts, want) {
			t.Errorf("options %q missing %q", opts, want)
		}
	}
	if strings.Contains(opts, "f=") {
		t.Errorf("options %q carries an explicit format, want default RGBA", opts)
	}

	if len(b64) == 0 {
		t.Fatalf("payload body is empty, want non-empty base64 data")
	}
}

func TestRenderPayloadRoundTrips(t *testing.T) {
	f := knownFrame(t)
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(f, &buf); err != nil {
		t.Fatalf("Render() = %v", err)
	}

	inner := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "\x1b_G"), "\x1b\\")
	b64 := inner[strings.IndexByte(inner, ';')+1:]

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}

	b := f.Img.Bounds()
	src := f.Img.(*image.RGBA)
	dec := kitty.Decoder{Decompress: true, Format: kitty.RGBA, Width: b.Dx(), Height: b.Dy()}
	img, err := dec.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode payload (base64, zlib, image): %v", err)
	}

	got, ok := img.(*image.RGBA)
	if !ok {
		t.Fatalf("decoded image is %T, want *image.RGBA", img)
	}
	if got.Bounds() != b {
		t.Fatalf("decoded bounds = %v, want %v", got.Bounds(), b)
	}
	for y := range b.Dy() {
		for x := range b.Dx() {
			if got.RGBAAt(x, y) != src.RGBAAt(x, y) {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got.RGBAAt(x, y), src.RGBAAt(x, y))
			}
		}
	}
}

// largeFrame builds an RGBA frame big enough that the base64 payload exceeds
// kitty.MaxChunkSize, forcing the chunked transmit path. Each pixel varies so
// the zlib-compressed payload stays large.
func largeFrame(t *testing.T) render.Frame {
	t.Helper()
	const w, h = 256, 256
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x),
				G: uint8(y),
				B: uint8((x ^ y) & 0xff),
				A: 0xff,
			})
		}
	}
	return render.Frame{Img: img}
}

func TestRenderEmitsDisplayAction(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(knownFrame(t), &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "a=T") {
		t.Errorf("output missing display action key a=T: %q", buf.String())
	}
}

func TestRenderChunksLargePayload(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(largeFrame(t), &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	out := buf.String()

	if strings.Count(out, "\x1b_G") < 2 {
		t.Fatalf("large payload not chunked: want more than one APC introducer, got %d", strings.Count(out, "\x1b_G"))
	}
	if !strings.Contains(out, "m=1") {
		t.Errorf("chunked output missing m=1 continuation key")
	}
	if !strings.HasSuffix(out, "\x1b\\") {
		t.Fatalf("chunked output does not end with String Terminator")
	}
	lastFrame := out[strings.LastIndex(out, "\x1b_G"):]
	if !strings.HasPrefix(lastFrame, "\x1b_Gm=0;") {
		t.Errorf("final frame does not carry m=0: %q", lastFrame[:min(len(lastFrame), 16)])
	}
}

func TestRenderRoundTripsThroughDecode(t *testing.T) {
	cases := []struct {
		name string
		f    render.Frame
	}{
		{"small single frame", knownFrame(t)},
		{"large multi frame", largeFrame(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r Renderer
			var buf bytes.Buffer
			if err := r.Render(tc.f, &buf); err != nil {
				t.Fatalf("Render() = %v", err)
			}
			got, err := renderdecode.DecodeKitty(buf.Bytes())
			if err != nil {
				t.Fatalf("DecodeKitty() = %v", err)
			}
			b := tc.f.Img.Bounds()
			if got.Bounds() != b {
				t.Fatalf("decoded bounds = %v, want %v", got.Bounds(), b)
			}
			src := tc.f.Img.(*image.RGBA)
			for y := range b.Dy() {
				for x := range b.Dx() {
					if got.RGBAAt(x, y) != src.RGBAAt(x, y) {
						t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got.RGBAAt(x, y), src.RGBAAt(x, y))
					}
				}
			}
		})
	}
}

func TestRenderNilImageEmitsEmptyFramedPayload(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(render.Frame{}, &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b_G") || !strings.HasSuffix(out, "\x1b\\") {
		t.Fatalf("nil-image payload = %q, want framed APC envelope", out)
	}
	if strings.Contains(out, ";") {
		t.Fatalf("nil-image payload = %q, want no payload separator", out)
	}
}

func TestRenderDeterministic(t *testing.T) {
	var r Renderer
	f := knownFrame(t)
	var a, b bytes.Buffer
	if err := r.Render(f, &a); err != nil {
		t.Fatalf("Render() a = %v", err)
	}
	if err := r.Render(f, &b); err != nil {
		t.Fatalf("Render() b = %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("payload differs across runs on the same Frame")
	}
}
