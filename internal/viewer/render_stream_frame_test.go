package viewer

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

// TestRenderStreamFrameGraphicsNoSecondDownscale asserts that the graphics path
// encodes an engine-sized bitmap at its exact dimensions. The engine's painted
// height can fall under the grid extent by a sub-cell row band on a
// fractional-density terminal, and that steady-state shape must pass through
// untouched: neither the retired aspect-fit letterbox nor the post-zoom grid
// fit (fitSixelToGrid, which acts only on frames a cell or more away) may
// rescale it.
func TestRenderStreamFrameGraphicsNoSecondDownscale(t *testing.T) {
	const cols, rows = 40, 20
	const cellW, cellH = 8, 16
	st := NewState(cols, rows)
	st.cellPx = cellPx{W: cellW, H: cellH}
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel

	// Grid extent 320x320; the engine paints 320x314 under a fractional density
	// (a sub-cell shortfall). Any refit would move these exact dimensions.
	const imgW, imgH = 320, 314
	img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: img}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(sixel) = %v, want nil", err)
	}
	raster := out.Bytes()
	if i := bytes.Index(raster, []byte("\x1bPq")); i >= 0 {
		raster = raster[i:]
	}
	decoded, err := renderdecode.DecodeSixel(raster)
	if err != nil {
		t.Fatalf("DecodeSixel: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != imgW || b.Dy() != imgH {
		t.Errorf("decoded sixel = %dx%d, want engine-sized %dx%d", b.Dx(), b.Dy(), imgW, imgH)
	}
}

func TestRenderStreamFrameDoesNoPaletteTimingWhenUnarmed(t *testing.T) {
	state := NewState(8, 4)
	st := &state
	st.Backend = BackendBlock
	st.Colour = ColourXTerm256
	st.paletteQueryOK = true
	st.SetDiff(&block.GridDiff{})
	st.paletteNow = func() time.Time {
		panic("palette clock read while timing was unarmed")
	}

	img := paletteSolidImage(16, 12, color.RGBA{R: 20, G: 40, B: 60, A: 0xff})
	if err := st.RenderStreamFrame(render.Frame{Img: img}, io.Discard); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	if !reflect.DeepEqual(st.paletteObservation, paletteFrameObservation{}) {
		t.Fatalf("unarmed render retained an observation: %+v", st.paletteObservation)
	}
}

func TestRenderStreamFrameClearsPaletteObservationForOtherPaths(t *testing.T) {
	state := NewState(8, 4)
	st := &state
	st.SetDiff(&block.GridDiff{})
	st.paletteNow = steppedPaletteClock()
	img := paletteSolidImage(16, 12, color.RGBA{R: 20, G: 40, B: 60, A: 0xff})

	st.Backend = BackendBlock
	st.Colour = ColourXTerm256
	st.paletteQueryOK = true
	if err := st.renderStreamFrame(render.Frame{Img: img}, io.Discard, false, true); err != nil {
		t.Fatalf("adaptive render: %v", err)
	}
	if st.paletteObservation.paletteChecks != 1 {
		t.Fatal("adaptive render produced no observation")
	}

	cases := []struct {
		name    string
		setup   func()
		frame   render.Frame
		out     io.Writer
		wantErr bool
	}{
		{name: "static colour", setup: func() { st.Backend, st.Colour, st.paletteQueryOK = BackendBlock, ColourXTerm256, false }, frame: render.Frame{Img: img}, out: io.Discard},
		{name: "truecolour", setup: func() { st.Backend, st.Colour, st.paletteQueryOK = BackendBlock, ColourTrue, true }, frame: render.Frame{Img: img}, out: io.Discard},
		{name: "Sixel", setup: func() { st.Backend = BackendSixel }, frame: render.Frame{Img: img}, out: io.Discard},
		{name: "Kitty", setup: func() { st.Backend = BackendKitty }, frame: render.Frame{EncodedPNG: encodedPNG(t, img)}, out: io.Discard},
		{name: "nil image", setup: func() { st.Backend = BackendBlock }, frame: render.Frame{}, out: io.Discard},
		{name: "failed", setup: func() {
			st.Backend, st.Colour, st.paletteQueryOK = BackendBlock, ColourTrue, true
			st.diff.Reset()
		}, frame: render.Frame{Img: img}, out: failingFrameWriter{}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			err := st.renderStreamFrame(tc.frame, tc.out, false, true)
			if (err != nil) != tc.wantErr {
				t.Fatalf("render error = %v, want error %t", err, tc.wantErr)
			}
			if !reflect.DeepEqual(st.paletteObservation, paletteFrameObservation{}) {
				t.Fatalf("render retained an adaptive observation: %+v", st.paletteObservation)
			}
			if st.paletteTiming {
				t.Fatal("render left palette timing armed")
			}
		})
	}
}

type failingFrameWriter struct{}

func (failingFrameWriter) Write([]byte) (int, error) {
	return 0, errors.New("frame write failed")
}

// TestRenderStreamFrameGraphicsZeroGrid asserts decoded graphics frames remain a
// no-op when either grid dimension is zero. A direct Kitty PNG remains ahead of
// this guard because it is already a complete terminal payload source.
func TestRenderStreamFrameGraphicsZeroGrid(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for _, grid := range []struct {
		cols int
		rows int
	}{
		{cols: 0, rows: 4},
		{cols: 8, rows: 0},
	} {
		for _, backend := range []Backend{BackendKitty, BackendSixel} {
			st := NewState(grid.cols, grid.rows)
			st.Backend = backend
			var out bytes.Buffer
			if err := st.RenderStreamFrame(render.Frame{Img: img}, &out); err != nil {
				t.Fatalf("RenderStreamFrame(%v, %dx%d) = %v, want nil", backend, grid.cols, grid.rows, err)
			}
			if out.Len() != 0 {
				t.Errorf("RenderStreamFrame(%v, %dx%d) wrote %d bytes, want none", backend, grid.cols, grid.rows, out.Len())
			}
		}
	}

	st := NewState(0, 0)
	st.Backend = BackendKitty
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{EncodedPNG: encodedPNG(t, img)}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(direct Kitty PNG, zero grid) = %v, want nil", err)
	}
	if out.Len() == 0 {
		t.Fatal("direct Kitty PNG on zero grid wrote no protocol bytes")
	}
}

// TestRenderStreamFrameDegenerateGuardBothBackends asserts that a zero-extent
// frame is a no-op on both the block and graphics paths.
func TestRenderStreamFrameDegenerateGuardBothBackends(t *testing.T) {
	frame := render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 0, 0))}
	for _, backend := range []Backend{BackendBlock, BackendSixel} {
		st := NewState(40, 20)
		st.SetEnabled([]Backend{BackendSixel, BackendBlock})
		st.Backend = backend
		if backend == BackendBlock {
			st.SetDiff(&block.GridDiff{})
		}
		var out bytes.Buffer
		if err := st.RenderStreamFrame(frame, &out); err != nil {
			t.Fatalf("RenderStreamFrame(%v) on zero-extent image = %v, want nil", backend, err)
		}
		if out.Len() != 0 {
			t.Errorf("RenderStreamFrame(%v) wrote %d bytes for zero-extent image, want no-op", backend, out.Len())
		}
	}
}
