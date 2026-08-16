package engine

import (
	"strings"
	"testing"
)

// TestPointerCursorRecordAndRead verifies the record and read pair: the getter
// is empty before any read, reports the last recorded value, and a recorded
// empty string clears the stored value.
func TestPointerCursorRecordAndRead(t *testing.T) {
	e := New("")
	if got := e.PointerCursor(); got != "" {
		t.Errorf("PointerCursor before any read = %q, want empty", got)
	}
	e.recordPointerCursor("pointer")
	if got := e.PointerCursor(); got != "pointer" {
		t.Errorf("PointerCursor = %q, want %q", got, "pointer")
	}
	e.recordPointerCursor("")
	if got := e.PointerCursor(); got != "" {
		t.Errorf("PointerCursor after empty record = %q, want empty", got)
	}
}

// TestHoverReadsCursor pins the pure decision of which mouse dispatches read
// the computed cursor: only a hover move, a move carrying MouseButtonNone.
// A press, release, wheel, or drag must never evaluate in the page.
func TestHoverReadsCursor(t *testing.T) {
	tests := []struct {
		name   string
		action MouseAction
		button MouseButton
		want   bool
	}{
		{"hover move", MouseActionMove, MouseButtonNone, true},
		{"left drag", MouseActionMove, MouseButtonLeft, false},
		{"middle drag", MouseActionMove, MouseButtonMiddle, false},
		{"right drag", MouseActionMove, MouseButtonRight, false},
		{"left press", MouseActionPress, MouseButtonLeft, false},
		{"right press", MouseActionPress, MouseButtonRight, false},
		{"left release", MouseActionRelease, MouseButtonLeft, false},
		{"wheel", MouseActionWheel, MouseButtonNone, false},
	}
	for _, tt := range tests {
		if got := hoverReadsCursor(tt.action, tt.button); got != tt.want {
			t.Errorf("hoverReadsCursor(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestPointerCursorExpr pins the shape of the in-page read: it hits
// elementFromPoint at the dispatch position, reads only the cursor property,
// and guards a null element so getComputedStyle never throws.
func TestPointerCursorExpr(t *testing.T) {
	expr := pointerCursorExpr(84.5, 297)
	if !strings.Contains(expr, ".cursor") {
		t.Errorf("expression %q does not read .cursor", expr)
	}
	if !strings.Contains(expr, "elementFromPoint(84.5, 297)") {
		t.Errorf("expression %q does not call elementFromPoint at the point", expr)
	}
	if !strings.Contains(expr, "el ? getComputedStyle(el)") {
		t.Errorf("expression %q does not guard a null element", expr)
	}
	if strings.Contains(expr, "getComputedStyle(document.elementFromPoint") {
		t.Errorf("expression %q passes elementFromPoint straight to getComputedStyle, so a null element would throw", expr)
	}
}
