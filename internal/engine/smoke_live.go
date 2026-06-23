//go:build live

package engine

import (
	"fmt"
	"io"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// SmokeStaticFrame is the live proof-of-path: it launches a browser, navigates
// to url at the given block grid, takes one synced frame from the continuous
// frame loop, and renders it once to w through the viewer's block path. Navigate
// starts the latest-wins screencast and the frame loop, so this receives from
// Frames() rather than starting its own screencast. It returns the number of
// crisp text runs the synced frame carries so a caller can assert text arrived.
// The live build tag guards it, so the default build and `go test ./...` never
// compile it and never launch a browser. Run it from the live-tagged smoke test
// or a manual harness on a real terminal with Chromium.
func SmokeStaticFrame(w io.Writer, url, browserFlag string, cols, rows int) (runs int, err error) {
	e := &ChromiumEngine{backend: "block", browserFlag: browserFlag}
	e.Resize(cols, rows)
	if err := e.Navigate(url); err != nil {
		return 0, err
	}
	defer e.Close()

	var f render.Frame
	select {
	case f = <-e.Frames():
	case <-time.After(10 * time.Second):
		return 0, fmt.Errorf("engine: await first frame: timed out")
	}

	st := viewer.NewState(cols, rows, f.Img.Bounds().Dx(), f.Img.Bounds().Dy())
	if err := st.RenderFrame(f.Img, w); err != nil {
		return 0, err
	}
	return len(f.Text), nil
}
