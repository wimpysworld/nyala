package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/runtime"
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

// TestCursorCtxSource pins the pure context choice of the hover cursor read:
// the held layout-shift context wins, the cached fallback comes next, and a
// zero pair asks for a fresh isolated world.
func TestCursorCtxSource(t *testing.T) {
	tests := []struct {
		name         string
		held, cached runtime.ExecutionContextID
		want         runtime.ExecutionContextID
		wantCreate   bool
	}{
		{"held wins over cached", 7, 3, 7, false},
		{"held alone", 7, 0, 7, false},
		{"cached fallback", 0, 3, 3, false},
		{"neither, create", 0, 0, 0, true},
	}
	for _, tt := range tests {
		got, create := cursorCtxSource(tt.held, tt.cached)
		if got != tt.want || create != tt.wantCreate {
			t.Errorf("cursorCtxSource(%s) = (%d, %v), want (%d, %v)",
				tt.name, got, create, tt.want, tt.wantCreate)
		}
	}
}

// TestIsContextLostErr pins the invalidation predicate: only Chromium's
// "Cannot find context" evaluate error names a destroyed world.
func TestIsContextLostErr(t *testing.T) {
	if !isContextLostErr(errors.New("Cannot find context with specified id (-32000)")) {
		t.Error("context-lost error not recognised")
	}
	if isContextLostErr(errors.New("some other evaluate failure")) {
		t.Error("unrelated error treated as context lost")
	}
	if isContextLostErr(nil) {
		t.Error("nil error treated as context lost")
	}
}

// TestClearCursorCtx verifies the cached cursor context invalidation: a
// context-lost clear drops a matching id so the next hover recreates the
// world, and never clobbers a newer id cached after the failed read.
func TestClearCursorCtx(t *testing.T) {
	e := New("")
	e.mu.Lock()
	e.cursorCtxID = 5
	e.mu.Unlock()
	e.clearCursorCtx(5)
	e.mu.Lock()
	got := e.cursorCtxID
	e.mu.Unlock()
	if got != 0 {
		t.Errorf("cursorCtxID after matching clear = %d, want 0", got)
	}
	e.mu.Lock()
	e.cursorCtxID = 9
	e.mu.Unlock()
	e.clearCursorCtx(5)
	e.mu.Lock()
	got = e.cursorCtxID
	e.mu.Unlock()
	if got != 9 {
		t.Errorf("cursorCtxID after stale clear = %d, want 9", got)
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
