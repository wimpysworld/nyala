package viewer

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestCaptureGridSinkFillsOnBlockDraw asserts the capture-only grid sink, when
// set, receives the same composed letterbox grid the block path draws, and stays
// untouched when nil. One compose yields both the drawn bytes and the serialised
// grid, so the bundle's terminal-model.json never drifts from the terminal output.
func TestCaptureGridSinkFillsOnBlockDraw(t *testing.T) {
	st := NewState(8, 4, 16, 16)
	src := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			src.Set(x, y, color.RGBA{R: 0xff, A: 0xff})
		}
	}

	var sink block.Grid
	st.SetCaptureGrid(&sink)

	var buf bytes.Buffer
	if err := st.RenderFrame(src, &buf); err != nil {
		t.Fatalf("RenderFrame: %v", err)
	}

	if sink.Cols != st.Cols || sink.Rows != st.Rows {
		t.Errorf("sink grid = %dx%d, want %dx%d", sink.Cols, sink.Rows, st.Cols, st.Rows)
	}
	if len(sink.Cells) != st.Cols*st.Rows {
		t.Fatalf("sink grid cells = %d, want %d", len(sink.Cells), st.Cols*st.Rows)
	}
	if buf.Len() == 0 {
		t.Errorf("RenderFrame wrote no terminal bytes")
	}
}

func TestCaptureGridSinkNilByDefault(t *testing.T) {
	st := NewState(8, 4, 16, 16)
	if st.captureGrid != nil {
		t.Errorf("captureGrid must default nil")
	}
	src := image.NewRGBA(image.Rect(0, 0, 16, 16))
	var buf bytes.Buffer
	// With no sink set, drawing must not panic and must still write bytes.
	if err := st.RenderFrame(src, &buf); err != nil {
		t.Fatalf("RenderFrame: %v", err)
	}
}
