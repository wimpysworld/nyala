package render

import (
	"io"
	"testing"
	"unsafe"
)

func TestFrameHeaderMatchesBaselineSize(t *testing.T) {
	if got, want := unsafe.Sizeof(Frame{}), uintptr(72); got != want {
		t.Fatalf("Frame size = %d bytes, want baseline %d", got, want)
	}
}

// stubRenderer is a trivial Renderer used to assert the interface seam.
type stubRenderer struct{}

func (stubRenderer) Render(_ Frame, _ io.Writer) error { return nil }

func (stubRenderer) Name() string { return "stub" }

// Compile-time assertion that stubRenderer satisfies Renderer.
var _ Renderer = stubRenderer{}

func TestStubRendererSatisfiesRenderer(t *testing.T) {
	var r Renderer = stubRenderer{}
	if r.Name() != "stub" {
		t.Fatalf("Name() = %q, want %q", r.Name(), "stub")
	}
	if err := r.Render(Frame{}, io.Discard); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
}

// TestCellWidth asserts ASCII is width 1, a CJK ideograph and an emoji width 2.
func TestCellWidth(t *testing.T) {
	if w := CellWidth("a"); w != 1 {
		t.Fatalf("ASCII width = %d, want 1", w)
	}
	if w := CellWidth("世"); w != 2 {
		t.Fatalf("CJK width = %d, want 2", w)
	}
	if w := CellWidth("\U0001F600"); w != 2 {
		t.Fatalf("emoji width = %d, want 2", w)
	}
}

// TestCellWidthAmbiguousPinnedToOne asserts the init() pin makes ambiguous-width
// runes count as one cell, matching the wezterm and kitty defaults, so the
// overlay's cell count stays independent of the process locale.
func TestCellWidthAmbiguousPinnedToOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
	}{
		{"right single quote U+2019", "’"},
		{"horizontal ellipsis U+2026", "…"},
	} {
		if w := CellWidth(tc.s); w != 1 {
			t.Fatalf("%s width = %d, want 1", tc.name, w)
		}
	}
}

// TestClipToCells asserts the clip budget counts display cells, drops a width-2
// cluster whole rather than half-emitting it, and yields empty for a
// non-positive budget.
func TestClipToCells(t *testing.T) {
	if got := ClipToCells("abc", 2); got != "ab" {
		t.Fatalf("ASCII clip = %q, want %q", got, "ab")
	}
	// A width-2 ideograph does not fit in a single remaining cell.
	if got := ClipToCells("世", 1); got != "" {
		t.Fatalf("width-2 over budget = %q, want empty", got)
	}
	if got := ClipToCells("世", 2); got != "世" {
		t.Fatalf("width-2 in budget = %q, want %q", got, "世")
	}
	if got := ClipToCells("abc", 0); got != "" {
		t.Fatalf("zero budget = %q, want empty", got)
	}
}
