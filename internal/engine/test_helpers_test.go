package engine

import (
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

func recvFrame(t *testing.T, frames <-chan render.Frame) render.Frame {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frame")
		return render.Frame{}
	}
}
