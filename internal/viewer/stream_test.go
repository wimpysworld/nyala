package viewer

import (
	"image"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestResolveScheme asserts the interactive --blocks resolution: half and
// quadrant map straight through, auto and sextant render sextant on a supported
// terminal and degrade to quadrant on an unsupported one.
func TestResolveScheme(t *testing.T) {
	supportedEnv := []string{"TERM=xterm-kitty"}
	unsupportedEnv := []string{"TERM=xterm-256color"}
	cases := []struct {
		name   string
		blocks string
		env    []string
		scheme block.Scheme
	}{
		{"quadrant", "quadrant", supportedEnv, block.SchemeQuadrant},
		{"half", "half", supportedEnv, block.SchemeHalfBlock},
		{"auto supported", "auto", supportedEnv, block.SchemeSextant},
		{"auto unsupported", "auto", unsupportedEnv, block.SchemeQuadrant},
		{"sextant supported", "sextant", supportedEnv, block.SchemeSextant},
		{"sextant unsupported", "sextant", unsupportedEnv, block.SchemeQuadrant},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveScheme(c.blocks, c.env); got != c.scheme {
				t.Errorf("resolveScheme(%q, env) = %d, want %d", c.blocks, got, c.scheme)
			}
		})
	}
}

// frameWith builds a frame whose bitmap width tags its identity, so a test can
// assert which queued frame survived the drain.
func frameWith(id int) render.Frame {
	return render.Frame{Img: image.NewRGBA(image.Rect(0, 0, id, 1))}
}

func TestDrainFramesKeepsNewest(t *testing.T) {
	frames := make(chan render.Frame, 4)
	frames <- frameWith(2)
	frames <- frameWith(3)
	frames <- frameWith(4)

	got, drained, ok := drainFrames(frames, frameWith(1))
	if !ok {
		t.Fatal("drainFrames reported close on an open channel")
	}
	if w := got.Img.Bounds().Dx(); w != 4 {
		t.Fatalf("kept frame id = %d, want 4 (newest)", w)
	}
	if drained != 3 {
		t.Fatalf("drained count = %d, want 3 (coalesced frames feed the badge total)", drained)
	}
	if len(frames) != 0 {
		t.Fatalf("frames left queued = %d, want 0", len(frames))
	}
}

func TestDrainFramesNoQueueReturnsReceived(t *testing.T) {
	frames := make(chan render.Frame, 1)

	got, drained, ok := drainFrames(frames, frameWith(7))
	if !ok {
		t.Fatal("drainFrames reported close on an open channel")
	}
	if w := got.Img.Bounds().Dx(); w != 7 {
		t.Fatalf("kept frame id = %d, want 7 (received)", w)
	}
	if drained != 0 {
		t.Fatalf("drained count = %d, want 0 (nothing queued)", drained)
	}
}

func TestDrainFramesClosedMidDrainStops(t *testing.T) {
	frames := make(chan render.Frame, 2)
	frames <- frameWith(5)
	close(frames)

	got, _, ok := drainFrames(frames, frameWith(1))
	if ok {
		t.Fatal("drainFrames did not report close")
	}
	// The newest seen before close is returned; the caller stops the loop on !ok.
	if w := got.Img.Bounds().Dx(); w != 5 {
		t.Fatalf("returned frame id = %d, want 5 (last before close)", w)
	}
}
