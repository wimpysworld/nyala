//go:build live

package engine

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// liveTestPage is a self-contained data: URL with one line of text. A data: URL
// needs no network and no local file, so the live test stays hermetic once a
// Chromium resolves. The text drives the DOM snapshot, so the synced frame must
// carry at least one crisp GlyphRun.
const liveTestPage = "data:text/html," +
	"<html><body style='margin:0'><p>nyala live smoke</p></body></html>"

func TestLiveGraphicsStartupClearsBlockForcing(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}
	e := New("")
	t.Cleanup(func() { _ = e.Close() })
	if err := e.ConfigureCapture(backendBlock, 80, 24, 8, 16, false); err != nil {
		t.Fatal(err)
	}
	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	var captureCtx context.Context
	e.beforeInitialLock = func() {
		e.beforeInitialLock = nil
		captureCtx = e.ctx
		// Launch registered the Block stylesheet. Watch its live removal, then
		// switch to graphics before the initial capture takes the new settings.
		const watchRemoval = `(async () => {
			if (document.readyState === 'loading') {
				await new Promise(resolve => document.addEventListener('DOMContentLoaded', resolve, {once: true}));
			}
			const style = document.getElementById('nyala-font-forcing');
			if (!style) return false;
			window.forcingRemoved = new Promise(resolve => {
				const watcher = new MutationObserver(() => {
					if (style.isConnected) return;
					watcher.disconnect();
					resolve(true);
				});
				watcher.observe(style.parentNode, {childList: true});
			});
			return true;
		})()`
		var watchSet bool
		if err := chromedp.Run(captureCtx, chromedp.Evaluate(watchRemoval, &watchSet, awaitPromise)); err != nil {
			t.Fatal(err)
		}
		if !watchSet {
			t.Fatal("Block stylesheet was not present before graphics capture")
		}
		if err := e.ConfigureCapture(backendKitty, 80, 24, 8, 16, false); err != nil {
			t.Fatal(err)
		}
	}
	const testPage = "data:text/html,<html><body style='margin:0;background:white;color:black'><p>nyala live smoke</p></body></html>"
	if err := e.Navigate(testPage); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(captureCtx, 5*time.Second)
	defer cancel()
	var removed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate("window.forcingRemoved", &removed, awaitPromise)); err != nil {
		t.Fatalf("wait for Block stylesheet removal: %v", err)
	}
	if !removed || e.fontForcingActive() {
		t.Fatal("graphics capture retained the Block stylesheet")
	}
	var fill string
	if err := chromedp.Run(ctx, chromedp.Evaluate("getComputedStyle(document.querySelector('p')).webkitTextFillColor", &fill)); err != nil {
		t.Fatal(err)
	}
	if fill != "rgb(0, 0, 0)" {
		t.Fatalf("page text fill after removal = %s, want black", fill)
	}
	frames := 0
	for {
		select {
		case frame, open := <-e.Frames():
			if !open {
				t.Fatalf("graphics capture stopped: %v", e.StreamError())
			}
			frames++
			img, err := decodeScreencastNative(frame.EncodedPNG)
			if err != nil {
				t.Fatalf("decode Kitty frame: %v", err)
			}
			bounds := img.Bounds()
			// Only the text can change the fixture's white background. Glyph
			// antialiasing does not guarantee a fully black pixel.
			if countColour(img, [3]int{255, 255, 255}) < bounds.Dx()*bounds.Dy() {
				return
			}
		case <-ctx.Done():
			t.Fatalf("graphics capture did not paint the restored page text in %d frames", frames)
		}
	}
}

// TestLiveSyncedFrame launches a real Chromium, navigates to a self-contained
// page, and asserts that at least one synced frame arrives with non-empty Text
// (the crisp DOM overlay).
// The DOM snapshot publishes on its own goroutine, decoupled from frame emit, and
// ack-on-receive lets the first bitmap arrive before the first text publishes, so
// it reads frames until one carries text rather than demanding it on the first.
// The live build tag keeps it off the default build, `just test`, and CI: an
// untagged `go test ./...` never compiles it and never launches a browser. Run
// it with `just live`. It skips with a clear message when no Chromium resolves,
// so the tagged run degrades gracefully on a host without a browser, and uses a
// deadline so it fails fast rather than hanging.
func TestLiveSyncedFrame(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(80, 24)
	if err := e.Navigate(liveTestPage); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()

	for {
		select {
		case f, ok := <-e.Frames():
			if !ok {
				t.Fatal("frames channel closed before a text-bearing frame arrived")
			}
			if f.Img == nil {
				t.Fatal("synced frame has a nil bitmap")
			}
			if len(f.Text) > 0 {
				return // a synced frame with crisp text arrived
			}
			// A pre-publish frame carries no text yet; keep reading for the synced one.
		case <-deadline.C:
			t.Fatal("no synced frame with crisp text within the deadline")
		}
	}
}

// TestLiveBlockDensityLayoutWidth tests the block cell-density layout scale on
// a grid whose cell exceeds the 8 px density anchor. It navigates a
// self-contained page, then reads window.innerWidth, the CSS layout viewport
// width, and asserts it equals the density cssW = min(max(cols*8, 1024),
// cols*cellW) within one pixel: at cell 16 the page lays out at cols*8 CSS px
// through the constant Z = 2 rather than at the full cols*cellW surface. Run
// it with `just live`.
func TestLiveBlockDensityLayoutWidth(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	const cols, rows = 240, 48
	const cellW, cellH = 16, 32

	e := New("")
	e.SetCellPx(cellW, cellH)
	e.Resize(cols, rows)
	if err := e.Navigate(liveTestPage); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Wait for the first synced frame so the page has laid out before the read.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	select {
	case _, ok := <-e.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
	case <-deadline.C:
		t.Fatal("no synced frame within the deadline")
	}

	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		t.Fatal("engine context is nil after navigate")
	}

	var innerWidth float64
	if err := chromedp.Run(ctx, chromedp.Evaluate("window.innerWidth", &innerWidth)); err != nil {
		t.Fatalf("read window.innerWidth: %v", err)
	}

	want := float64(cols*cellW) / blockLayoutScale(cols, cellW)
	if math.Abs(innerWidth-want) > 1 {
		t.Fatalf("CSS viewport width = %g, want the density cssW = %g within 1px", innerWidth, want)
	}
}

// TestLiveEventuallyEmitsAfterFCPFrame proves the normal stream reaches a frame
// received after main-frame FCP. It uses lifecycle ordering, not pixel colours.
func TestLiveEventuallyEmitsAfterFCPFrame(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body style='margin:0'><p>nyala paint marker</p>" +
			"<div style='width:100vw;height:50vh;background:#ff0000'></div>" +
			"<div style='width:100vw;height:50vh;background:#0000ff'></div>" +
			"</body></html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(80, 24)
	if err := e.Navigate(srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Read until the normal stream reaches a frame received after FCP.
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case f, ok := <-e.Frames():
			if !ok {
				t.Fatal("frames channel closed before a painted frame")
			}
			if f.Img == nil {
				t.Fatal("frame has a nil bitmap")
			}
			if f.AfterFCP {
				return
			}
		case <-deadline.C:
			t.Fatal("no frame arrived after main-frame FCP within the deadline")
		}
	}
}

func TestLiveFrameArrivesBeforeFullLoad(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	hang := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><style>@keyframes pulse{from{opacity:.4}to{opacity:1}}" +
			"body{animation:pulse .05s infinite alternate}</style><body><p>nyala partial load</p>" +
			"<img src='/hang'></body></html>"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	mux.HandleFunc("/hang", func(_ http.ResponseWriter, _ *http.Request) {
		<-hang
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(hang)

	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(80, 24)

	navDone := make(chan error, 1)
	go func() { navDone <- e.Navigate(srv.URL) }()
	select {
	case err := <-navDone:
		if err != nil {
			t.Fatalf("navigate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Navigate blocked waiting for full page load")
	}
	t.Cleanup(func() { _ = e.Close() })

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	postCommit := 0
	for postCommit < 3 {
		select {
		case f, ok := <-e.Frames():
			if !ok {
				t.Fatal("frames channel closed before progressive frames arrived")
			}
			if !f.AfterFCP {
				continue
			}
			if !f.HasImage() {
				t.Fatal("post-FCP frame has no image payload")
			}
			postCommit++
		case <-deadline.C:
			t.Fatalf("received %d post-FCP frames while a subresource was held, want 3", postCommit)
		}
	}
}
