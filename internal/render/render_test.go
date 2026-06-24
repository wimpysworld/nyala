package render

import (
	"io"
	"testing"
)

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

// TestZeroFrameStatsNil asserts the zero Frame carries no stats, so the
// production path stays free of capture timing.
func TestZeroFrameStatsNil(t *testing.T) {
	var f Frame
	if f.Stats != nil {
		t.Fatalf("zero Frame.Stats = %v, want nil", f.Stats)
	}
}
