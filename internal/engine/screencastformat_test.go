package engine

import (
	"testing"

	"github.com/chromedp/cdproto/page"
)

// TestInitialScreencastFormatBackendMatrix pins the fixed backend-format policy:
// Block runs JPEG q90, and Kitty and Sixel always run PNG.
func TestInitialScreencastFormatBackendMatrix(t *testing.T) {
	cases := []struct {
		name       string
		backend    backend
		wantFormat page.ScreencastFormat
		wantQual   int
	}{
		{"block", backendBlock, page.ScreencastFormatJpeg, blockScreencastQuality},
		{"kitty", backendKitty, page.ScreencastFormatPng, 0},
		{"sixel", backendSixel, page.ScreencastFormatPng, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &ChromiumEngine{backend: tc.backend}
			got := e.initialScreencastFormat()
			if got.format != tc.wantFormat || got.quality != tc.wantQual {
				t.Fatalf("format = {%s %d}, want {%s %d}", got.format, got.quality, tc.wantFormat, tc.wantQual)
			}
		})
	}
}

// TestBackendCycleFormatSelection proves every backend switch re-picks the fixed
// policy, including both directions between Block JPEG and Sixel PNG.
func TestBackendCycleFormatSelection(t *testing.T) {
	e := &ChromiumEngine{backend: backendSixel}
	for _, tc := range []struct {
		backend backend
		format  page.ScreencastFormat
		quality int
	}{
		{backendKitty, page.ScreencastFormatPng, 0},
		{backendBlock, page.ScreencastFormatJpeg, blockScreencastQuality},
		{backendSixel, page.ScreencastFormatPng, 0},
		{backendBlock, page.ScreencastFormatJpeg, blockScreencastQuality},
		{backendKitty, page.ScreencastFormatPng, 0},
		{backendSixel, page.ScreencastFormatPng, 0},
	} {
		e.backend = tc.backend
		got := e.initialScreencastFormat()
		if got.format != tc.format || got.quality != tc.quality {
			t.Fatalf("backend %s format = {%s %d}, want {%s %d}", tc.backend, got.format, got.quality, tc.format, tc.quality)
		}
		e.screencastFmt = got
	}
}

// TestSixelFormatDoesNotDependOnWorkerCount proves every supported live decode-pool
// size keeps the Sixel wire format at PNG.
func TestSixelFormatDoesNotDependOnWorkerCount(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{"1", 1}, {"4", 4}, {"8", 8}} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(decodeWorkersEnv, tc.value)
			if got := decodeWorkerCount(); got != tc.want {
				t.Fatalf("decode workers = %d, want %d", got, tc.want)
			}
			e := &ChromiumEngine{backend: backendSixel}
			if got := e.initialScreencastFormat(); got.format != page.ScreencastFormatPng || got.quality != 0 {
				t.Fatalf("Sixel format = {%s %d}, want PNG with no quality", got.format, got.quality)
			}
		})
	}
}
