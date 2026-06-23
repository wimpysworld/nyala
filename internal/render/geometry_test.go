package render

import "testing"

func TestBlockCaptureSize(t *testing.T) {
	cases := []struct {
		name         string
		quadrant     bool
		cols, rows   int
		wantW, wantH int
	}{
		{"quadrant 80x24", true, 80, 24, 160, 96},
		{"quadrant 1x1", true, 1, 1, 2, 4},
		{"half-block 80x24", false, 80, 24, 80, 48},
		{"half-block 1x1", false, 1, 1, 1, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotW, gotH := BlockCaptureSize(c.quadrant, c.cols, c.rows)
			if gotW != c.wantW || gotH != c.wantH {
				t.Errorf("BlockCaptureSize(%v, %d, %d) = (%d, %d), want (%d, %d)",
					c.quadrant, c.cols, c.rows, gotW, gotH, c.wantW, c.wantH)
			}
		})
	}
}

func TestGraphicsCaptureSize(t *testing.T) {
	cases := []struct {
		name                     string
		cols, rows, cellW, cellH int
		wantW, wantH             int
	}{
		{"80x24 at 8x16", 80, 24, 8, 16, 640, 384},
		{"1x1 at 10x20", 1, 1, 10, 20, 10, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotW, gotH := GraphicsCaptureSize(c.cols, c.rows, c.cellW, c.cellH)
			if gotW != c.wantW || gotH != c.wantH {
				t.Errorf("GraphicsCaptureSize(%d, %d, %d, %d) = (%d, %d), want (%d, %d)",
					c.cols, c.rows, c.cellW, c.cellH, gotW, gotH, c.wantW, c.wantH)
			}
		})
	}
}
