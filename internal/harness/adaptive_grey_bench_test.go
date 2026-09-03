//go:build diagnostics

// The scene-fitted grey ramp compose benchmarks.
//
// These are the grey siblings of BenchmarkAdaptiveCompose and
// BenchmarkStatic256Compose in adaptive_palette_diagnostics_test.go. They
// drive the same wide-gradient frame through ComposeBlockFrame in
// ColourXTermGrey, once with the OSC 4 palette query answered (the luma ramp
// engine) and once unanswered (the fixed xterm greyscale ramp). The first
// compose fits the ramp; every later iteration feeds the same frame, whose
// sample never drifts, so the loop measures the steady-state reuse path and
// must allocate nothing.
package harness

import (
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// benchGreyState builds a Block viewer State at the pinned grid in
// ColourXTermGrey with the given palette-query verdict.
func benchGreyState(paletteOK bool) viewer.State {
	st := viewer.NewState(CalibCols, CalibRows+1)
	st.Backend = viewer.BackendBlock
	st.Colour = viewer.ColourXTermGrey
	st.SetPaletteQueryOK(paletteOK)
	st.SetDiff(&block.GridDiff{})
	return st
}

// BenchmarkAdaptiveGreyCompose measures one ComposeBlockFrame in the
// scene-fitted grey mode, which includes the luma sample, the drift test, and
// any ramp re-fit.
func BenchmarkAdaptiveGreyCompose(b *testing.B) {
	frame := benchFrame()
	st := benchGreyState(true)
	b.ReportAllocs()
	for b.Loop() {
		if st.ComposeBlockFrame(frame) == nil {
			b.Fatal("ComposeBlockFrame returned nil")
		}
	}
}

// BenchmarkXTermGreyCompose is the control for BenchmarkAdaptiveGreyCompose:
// the same compose on the fixed xterm greyscale ramp with no ramp engine, so
// the difference is the scene-fitted cost.
func BenchmarkXTermGreyCompose(b *testing.B) {
	frame := benchFrame()
	st := benchGreyState(false)
	b.ReportAllocs()
	for b.Loop() {
		if st.ComposeBlockFrame(frame) == nil {
			b.Fatal("ComposeBlockFrame returned nil")
		}
	}
}
