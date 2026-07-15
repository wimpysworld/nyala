package sixel

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"strings"
	"sync"
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
