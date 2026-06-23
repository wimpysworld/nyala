package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/wimpysworld/nyala/internal/render"
)

// chromiumNames lists the executable names tried on PATH, in order, when no
// explicit path is given. They cover the common Chromium and Chrome binaries
// across distributions.
var chromiumNames = []string{
	"chromium",
	"chromium-browser",
	"google-chrome",
	"google-chrome-stable",
	"chrome",
}

// resolveExecutable returns the Chromium or Chrome executable to launch. It
// tries, in order: the --browser flag value, the NYALA_BROWSER environment
// variable, the CHROME_PATH environment variable, then a PATH lookup for a
// known Chromium or Chrome binary name. It returns the first non-empty result
// and a clear error naming the search order when none resolve. The function is
// pure beyond reading the environment and PATH, so it is hermetically testable.
func resolveExecutable(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("NYALA_BROWSER"); v != "" {
		return v, nil
	}
	if v := os.Getenv("CHROME_PATH"); v != "" {
		return v, nil
	}
	for _, name := range chromiumNames {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf(
		"no Chromium found: set --browser, or the NYALA_BROWSER or CHROME_PATH "+
			"environment variable, or put one of %v on PATH",
		chromiumNames,
	)
}

// captureSize returns the source-pixel extent the screencast captures for the
// engine's active backend and resolved cell px. The block backend samples a
// sub-cell grid via render.BlockCaptureSize (quadrant scheme); the graphics
// backends capture a real cell-pixel bitmap via render.GraphicsCaptureSize. The
// engine derives this itself on every resize and backend switch, so the size is
// never passed in from the viewer.
func (e *ChromiumEngine) captureSize(cols, rows int) (pxW, pxH int) {
	if e.backend == "block" {
		return render.BlockCaptureSize(true, cols, rows)
	}
	return render.GraphicsCaptureSize(cols, rows, e.cellW, e.cellH)
}

// newAllocator builds a chromedp ExecAllocator for the resolved executable,
// sized to capW x capH. It sets the launch --window-size from the capture size
// and pins the device scale factor to 1. The window size matters because
// emulation.SetDeviceMetricsOverride resizes only the CSS viewport, not the
// captured content area; without it frames arrive at the default 1280x720. The
// device-metrics override at DSF 1 is not an allocator option, so it is applied
// on the context via applyDeviceMetrics once the context exists.
func newAllocator(parent context.Context, execPath string, capW, capH int) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		chromedp.WindowSize(capW, capH),
		chromedp.Flag("force-device-scale-factor", "1"),
	)
	return chromedp.NewExecAllocator(parent, opts...)
}

// applyDeviceMetrics sets the CSS viewport to capW x capH at device scale
// factor 1 on the live context. It runs after the context exists because the
// override is a CDP command on the page, not an allocator option. The launch
// --window-size set by newAllocator sizes the captured content area; this sizes
// the viewport to match.
func applyDeviceMetrics(ctx context.Context, capW, capH int) error {
	return chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(capW), int64(capH), 1, false),
	)
}

// launch resolves the executable from flag, builds the allocator and the
// chromedp context sized to the engine's current capture size, then applies the
// device-metrics override at DSF 1. It stores the context and its cancel on the
// engine. The full navigate path that calls this lands in Phase 4.
func (e *ChromiumEngine) launch(flag string) error {
	execPath, err := resolveExecutable(flag)
	if err != nil {
		return err
	}
	e.mu.Lock()
	capW, capH := e.captureW, e.captureH
	e.mu.Unlock()
	allocCtx, allocCancel := newAllocator(context.Background(), execPath, capW, capH)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	cancel := func() {
		ctxCancel()
		allocCancel()
	}
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.mu.Unlock()
	if err := applyDeviceMetrics(ctx, capW, capH); err != nil {
		cancel()
		return err
	}
	return nil
}
