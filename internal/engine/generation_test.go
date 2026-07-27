package engine

import (
	"image"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestInferFrameGen covers the frame-to-generation inference: before any publish the
// generation is unknown, and after a publish a bitmap is inferred at the latest
// published generation regardless of its scroll, since scroll never bumps the
// layout-generation counter.
func TestInferFrameGen(t *testing.T) {
	if gen, ok := inferFrameGen(0, false); ok || gen != 0 {
		t.Errorf("before first publish: got (gen %d, ok %v), want (0, false)", gen, ok)
	}
	if gen, ok := inferFrameGen(7, true); !ok || gen != 7 {
		t.Errorf("after publish: got (gen %d, ok %v), want (7, true)", gen, ok)
	}
}

// TestLayoutGenExprCoercesUndefined proves the generation read expression coerces an
// undefined counter (no observer installed) to 0, so a graphics backend or a failed
// observer install never fails the snapshot read.
func TestLayoutGenExprCoercesUndefined(t *testing.T) {
	if !strings.Contains(layoutGenExpr, "||0") {
		t.Errorf("layoutGenExpr must coerce undefined to 0, got %q", layoutGenExpr)
	}
	if !strings.Contains(layoutGenExpr, layoutGenProperty) {
		t.Errorf("layoutGenExpr must read the generation property, got %q", layoutGenExpr)
	}
}

// TestComposeFramePairsPublishedGeneration proves composeFrame composes a published
// run set at the bitmap's inferred generation, then draws bitmap-only once the set
// drops to nil (the error hold expiring), so holdDecision's staleness bound keeps its
// authority. A published set at generation 5 composes; a later nil set draws the
// bitmap alone rather than holding stale text past its bound. lastComposed is the
// frame loop's carried hold state.
func TestComposeFramePairsPublishedGeneration(t *testing.T) {
	e := &ChromiumEngine{backend: backendBlock, cols: 120, rows: 40, cellW: 8, cellH: 16}
	ts := &textSync{}
	want := []render.GlyphRun{{Col: 0, Row: 0, Text: "hi", X: 0, Y: 0}}
	ts.store(want, nil, 0, 5)

	var lastComposed []render.GlyphRun
	img := image.NewRGBA(image.Rect(0, 0, 8, 16))
	meta := &page.ScreencastFrameMetadata{ScrollOffsetY: 0}
	first := e.composeFrame(pendingFrame{img: img, meta: meta}, ts, &lastComposed)
	if len(first.Text) != 1 || first.Text[0].Text != "hi" {
		t.Fatalf("same-generation compose = %v, want the published run", first.Text)
	}

	// The error hold expired to nil: the compose draws the bitmap alone, so the
	// staleness bound wins over any compose-level hold.
	ts.store(nil, nil, 0, 5)
	second := e.composeFrame(pendingFrame{img: img, meta: meta}, ts, &lastComposed)
	if len(second.Text) != 0 {
		t.Fatalf("nil-run-set compose = %v, want bitmap-only", second.Text)
	}
}

// TestComposeFrameShiftsOnScrollMismatch proves the same-generation scroll path
// shifts the overlay onto the bitmap's scroll rather than holding it, so the crisp
// text tracks the moving page. The cell is 16 CSS px tall, so a bitmap scrolled 160
// CSS px past the snapshot moves every run up ten rows: a run at row 15 lands at row
// 5 and a run at row 0 scrolls off the top and is dropped.
func TestComposeFrameShiftsOnScrollMismatch(t *testing.T) {
	e := &ChromiumEngine{backend: backendBlock, cols: 120, rows: 40, cellW: 8, cellH: 16}
	ts := &textSync{}
	runs := []render.GlyphRun{
		{Col: 0, Row: 15, Text: "keep", X: 0, Y: 15 * 16},
		{Col: 0, Row: 0, Text: "gone", X: 0, Y: 0},
	}
	ts.store(runs, nil, 0, 3)

	var lastComposed []render.GlyphRun
	meta := &page.ScreencastFrameMetadata{ScrollOffsetY: 160}
	frame := e.composeFrame(pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 8, 16)), meta: meta}, ts, &lastComposed)
	if len(frame.Text) != 1 {
		t.Fatalf("scroll-mismatch compose folded %d runs, want 1 (the off-top run dropped)", len(frame.Text))
	}
	if frame.Text[0].Text != "keep" || frame.Text[0].Row != 5 {
		t.Fatalf("shifted run = %q at row %d, want \"keep\" at row 5 (15 - 10)", frame.Text[0].Text, frame.Text[0].Row)
	}
}
