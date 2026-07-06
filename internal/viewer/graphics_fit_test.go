package viewer

import (
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestGraphicsFitIsIdentity asserts the graphics path fits the engine-sized
// bitmap to the whole grid with no letterbox, even when the real terminal cell
// aspect is not 1:2. The engine sizes the graphics bitmap to the full grid, so a
// 1:2 FitImage would shrink it onto fewer cells and letterbox the view; the
// identity graphicsFit fills every cell from the top-left.
func TestGraphicsFitIsIdentity(t *testing.T) {
	st := NewState(120, 40, 1, 1)
	st.cellPx = cellPx{W: 16, H: 33} // a non-1:2 real terminal cell

	fit := st.graphicsFit()
	if fit.FitCols != st.Cols || fit.FitRows != st.Rows {
		t.Errorf("graphicsFit = %dx%d, want full grid %dx%d", fit.FitCols, fit.FitRows, st.Cols, st.Rows)
	}
	if fit.OffsetCol != 0 || fit.OffsetRow != 0 {
		t.Errorf("graphicsFit offset = (%d,%d), want (0,0) no letterbox", fit.OffsetCol, fit.OffsetRow)
	}

	// The old 1:2 FitImage letterboxes this non-1:2 geometry onto fewer cells; the
	// contrast is what the identity fit corrects.
	if bug := block.FitImage(st.Cols*16, st.Rows*33, st.Cols, st.Rows); bug.FitCols == st.Cols && bug.FitRows == st.Rows {
		t.Skip("FitImage does not letterbox this geometry, so the identity fit has nothing to correct")
	}
}
