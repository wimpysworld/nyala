package harness

import (
	"errors"
	"image"
	"io"
	"testing"
)

// These tests exercise the sixel PSNR noise-floor failure path with no
// rasteriser, so they stay in the default `just test` gate. They force every
// sixel round trip to fail by swapping the package's graphics backend table for
// a stub whose render errors, and assert the failure surfaces rather than
// collapsing to a silent zero floor.

// errStubRender is the canonical error the failing sixel render stub returns.
var errStubRender = errors.New("stub sixel render failure")

// swapFailingSixel replaces the sixel entry in graphicsBackends with a stub
// whose render always fails, and registers a cleanup that restores the original
// table. It is the all-fail injection point for the noise-floor failure path.
func swapFailingSixel(t *testing.T) {
	t.Helper()
	orig := graphicsBackends
	t.Cleanup(func() { graphicsBackends = orig })
	swapped := make([]graphicsBackend, len(orig))
	copy(swapped, orig)
	for i := range swapped {
		if swapped[i].mode == ModeSixel {
			swapped[i].render = func(*image.RGBA, io.Writer) error { return errStubRender }
		}
	}
	graphicsBackends = swapped
}

// TestSixelRoundTripPSNRSurfacesEncodeError checks a failing sixel render makes
// sixelRoundTripPSNR return a non-nil error rather than a silent zero PSNR.
func TestSixelRoundTripPSNRSurfacesEncodeError(t *testing.T) {
	swapFailingSixel(t)
	ref := image.NewRGBA(image.Rect(0, 0, 4, 4))
	psnr, err := sixelRoundTripPSNR(ref)
	if err == nil {
		t.Fatalf("sixelRoundTripPSNR returned err=nil psnr=%v, want non-nil error", psnr)
	}
}

// TestNoiseFloorSurfacesAllFail checks that when every sixel round trip fails,
// the failure propagates through collectSixelPSNRs and measureSixelPSNRNoiseFloor
// instead of returning a clean zero floor.
func TestNoiseFloorSurfacesAllFail(t *testing.T) {
	swapFailingSixel(t)
	ref := image.NewRGBA(image.Rect(0, 0, 4, 4))

	if samples, err := collectSixelPSNRs(ref); err == nil {
		t.Fatalf("collectSixelPSNRs returned err=nil samples=%v, want non-nil error", samples)
	}

	inputs := []CalibInput{{Card: "stub.png", Src: ref}}
	if floor, err := measureSixelPSNRNoiseFloor(inputs); err == nil {
		t.Fatalf("measureSixelPSNRNoiseFloor returned err=nil floor=%v, want non-nil error", floor)
	}
}
