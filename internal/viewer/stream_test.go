package viewer

import (
	"image"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

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

	got, ok := drainFrames(frames, frameWith(1))
	if !ok {
		t.Fatal("drainFrames reported close on an open channel")
	}
	if w := got.Img.Bounds().Dx(); w != 4 {
		t.Fatalf("kept frame id = %d, want 4 (newest)", w)
	}
	if len(frames) != 0 {
		t.Fatalf("frames left queued = %d, want 0", len(frames))
	}
}

func TestDrainFramesNoQueueReturnsReceived(t *testing.T) {
	frames := make(chan render.Frame, 1)

	got, ok := drainFrames(frames, frameWith(7))
	if !ok {
		t.Fatal("drainFrames reported close on an open channel")
	}
	if w := got.Img.Bounds().Dx(); w != 7 {
		t.Fatalf("kept frame id = %d, want 7 (received)", w)
	}
}

func TestDrainFramesClosedMidDrainStops(t *testing.T) {
	frames := make(chan render.Frame, 2)
	frames <- frameWith(5)
	close(frames)

	got, ok := drainFrames(frames, frameWith(1))
	if ok {
		t.Fatal("drainFrames did not report close")
	}
	// The newest seen before close is returned; the caller stops the loop on !ok.
	if w := got.Img.Bounds().Dx(); w != 5 {
		t.Fatalf("returned frame id = %d, want 5 (last before close)", w)
	}
}
