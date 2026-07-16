package block

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"reflect"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

func TestAveragePairExact(t *testing.T) {
	for a := range 256 {
		for b := range 256 {
			got := averagePair(uint8(a), uint8(b)) //nolint:gosec // loop bounds are byte values
			want := uint8((a + b) / 2)             //nolint:gosec // average is in byte range
			if got != want {
				t.Fatalf("averagePair(%d,%d) = %d, want %d", a, b, got, want)
			}
		}
	}
}

func assertQuadrantGridAndBytes(t *testing.T, br Renderer, frame render.Frame) {
	t.Helper()
	got, want := br.Compose(frame), legacyCompose(br, frame)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("fast Quadrant grid differs from generic oracle")
	}
	var gotBytes, wantBytes bytes.Buffer
	if err := (&GridDiff{}).EmitTrueColour(&gotBytes, got); err != nil {
		t.Fatal(err)
	}
	if err := (&GridDiff{}).EmitTrueColour(&wantBytes, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes.Bytes(), wantBytes.Bytes()) {
		t.Fatal("fast Quadrant terminal bytes differ from generic oracle")
	}
}

func TestQuadrantExactSourceMatchesGeneric(t *testing.T) {
	const cols, rows = 11, 7
	br := Renderer{Cols: cols, Rows: rows, Scheme: SchemeQuadrant}
	rng := rand.New(rand.NewSource(2)) //nolint:gosec // deterministic test fixture
	frames := make([]render.Frame, 0, 18)
	for i := range 18 {
		img := image.NewRGBA(image.Rect(0, 0, cols*cellPxW, rows*cellPxH))
		if i == 0 {
			for p := range img.Pix {
				img.Pix[p] = 0
			}
		} else if i == 1 {
			for p := 0; p < len(img.Pix); p += 4 {
				img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = 0, 1, 255, 254
			}
		} else if _, err := rng.Read(img.Pix); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, render.Frame{
			Img: img,
			Text: []render.GlyphRun{{
				Col: 0, Row: 0, Text: "A界", FG: color.RGBA{R: 254, G: 1, B: 255, A: 7},
				X: 1, W: 15, CellW: 8, DisplayCells: 3,
			}},
		})
	}
	for i, frame := range frames {
		img := frame.Img.(*image.RGBA)
		if !exactQuadrantSource(img, cols, rows) {
			t.Fatalf("frame %d did not select exact Quadrant source", i)
		}
		assertQuadrantGridAndBytes(t, br, frame)
	}
}

func TestQuadrantSourceFallbacks(t *testing.T) {
	const cols, rows = 5, 4
	br := Renderer{Cols: cols, Rows: rows, Scheme: SchemeQuadrant}
	w, h := cols*cellPxW, rows*cellPxH
	base := image.NewRGBA(image.Rect(0, 0, w+4, h+4))
	padded := &image.RGBA{
		Pix:    make([]byte, (w*4+8)*h),
		Stride: w*4 + 8,
		Rect:   image.Rect(0, 0, w, h),
	}
	cases := []struct {
		name string
		img  *image.RGBA
	}{
		{"nil", nil},
		{"empty", image.NewRGBA(image.Rect(0, 0, 0, 0))},
		{"undersized", image.NewRGBA(image.Rect(0, 0, w-1, h-1))},
		{"oversized", image.NewRGBA(image.Rect(0, 0, w+1, h+1))},
		{"odd", image.NewRGBA(image.Rect(0, 0, w-1, h+1))},
		{"subimage", base.SubImage(image.Rect(1, 1, w+1, h+1)).(*image.RGBA)},
		{"non-zero origin", image.NewRGBA(image.Rect(2, 3, w+2, h+3))},
		{"padded stride", padded},
		{"malformed-safe empty", &image.RGBA{Pix: []byte{1}, Stride: -1, Rect: image.Rect(4, 4, 4, 4)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if exactQuadrantSource(tc.img, cols, rows) {
				t.Fatal("fallback input selected exact Quadrant source")
			}
			assertQuadrantGridAndBytes(t, br, render.Frame{Img: tc.img})
		})
	}
	short := &image.RGBA{Pix: make([]byte, w*4*h-1), Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	if exactQuadrantSource(short, cols, rows) {
		t.Fatal("short backing storage selected exact Quadrant source")
	}
	if exactQuadrantSource(image.NewRGBA(image.Rect(0, 0, w, h)), int(^uint(0)>>1), rows) {
		t.Fatal("overflowing geometry selected exact Quadrant source")
	}
}
