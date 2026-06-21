package sixel

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"

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

	if !strings.HasPrefix(out, "\x1bPq") {
		t.Errorf("payload does not start with DCS introducer ESC P q: %q", out[:min(len(out), 8)])
	}
	if !strings.HasSuffix(out, "\x1b\\") {
		t.Errorf("payload does not end with String Terminator ESC backslash")
	}

	body := strings.TrimSuffix(strings.TrimPrefix(out, "\x1bPq"), "\x1b\\")
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
	if got := buf.String(); got != "\x1bPq\x1b\\" {
		t.Fatalf("nil-image payload = %q, want framed-but-empty %q", got, "\x1bPq\x1b\\")
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
