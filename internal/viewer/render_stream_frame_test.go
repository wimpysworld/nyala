package viewer

import (
	"bytes"
	"image"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

// TestRenderStreamFrameGraphicsNoSecondDownscale asserts that the graphics path
// encodes the engine-sized bitmap at its exact dimensions.
func TestRenderStreamFrameGraphicsNoSecondDownscale(t *testing.T) {
	const cols, rows = 40, 20
	const cellW, cellH = 8, 16
	st := NewState(cols, rows)
	st.cellPx = cellPx{W: cellW, H: cellH}
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel

	// A second fit-based downscale would turn this 320x170 bitmap into 320x160.
	const imgW, imgH = 320, 170
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
