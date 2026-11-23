//go:build live

package engine

import (
	"context"
	"math"
	"net/url"
	"os/exec"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"golang.org/x/sys/unix"
)

// remoteInputPage needs no network or local files. The buttons mark the top
// corners, the field and range sit inside the page, and the body can scroll.
const remoteInputPage = `<html style="scrollbar-width:none"><body style="margin:0">
<button id="left" style="position:absolute;left:0;top:0;width:64px;height:16px">Left</button>
<button id="right" style="position:absolute;right:0;top:0;width:64px;height:16px">Right</button>
<input id="field" aria-label="Text" style="position:absolute;left:120px;top:80px;width:160px;height:32px;box-sizing:border-box">
<input id="range" aria-label="Range" type="range" value="0" style="position:absolute;left:0;top:160px;width:400px;height:16px;margin:0">
<div style="height:400vh;background:linear-gradient(white,black)"></div>
<script>
window.clicks = [];
window.keys = [];
document.addEventListener("click", e => clicks.push([e.target.id, e.clientX, e.clientY]));
document.addEventListener("keydown", e => keys.push(e.key));
</script></body></html>`

// TestLiveRemoteAttach checks both endpoint forms, page input, and isolation
// between engines that attach to the same browser. Closing an engine must
// leave the browser running.
func TestLiveRemoteAttach(t *testing.T) {
	execPath, err := resolveExecutable("")
	if err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}
	dir := t.TempDir()
	cfg := allocatorConfig{execPath: execPath, profileDir: dir, surfaceWidth: 640, surfaceHeight: 480, scale: 1}
	argv := sharedArgv(launchFlags(cfg), dir)
	cmd := exec.Command(execPath, argv...) //nolint:noctx // the test owns the lifetime and kills it in t.Cleanup
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chromium: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap the child as spawnShared does, or an exited process stays a zombie
	// that still answers the null signal the liveness check sends.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var wsURL string
	t.Cleanup(func() {
		// Let Chromium stop its children before TempDir removes their profile.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if conn, err := chromedp.DialContext(ctx, wsURL); err == nil {
			defer func() { _ = conn.Close() }()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			if err := conn.Write(ctx, &cdproto.Message{ID: 1, Method: browser.CommandClose}); err != nil {
				t.Logf("close chromium pid %d: %v", pid, err)
			}
			select {
			case <-waited:
				return
			case <-ctx.Done():
				t.Logf("chromium pid %d did not exit within 5 s of Browser.close", pid)
			}
		} else {
			t.Logf("connect for chromium pid %d cleanup: %v", pid, err)
		}
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Logf("chromium pid %d did not exit within 5 s of SIGKILL", pid)
		}
	})

	wsURL = livePortURL(t, dir)
	parsed, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parse %q: %v", wsURL, err)
	}
	httpURL := "http://" + parsed.Host

	cases := []struct{ name, endpoint string }{{"ws_url", wsURL}, {"http_url", httpURL}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, ctx := liveRemoteInputEngine(t, c.endpoint, 8, 16, 80)
			checkLiveRemoteInput(t, e, ctx)

			if err := e.Close(); err != nil {
				t.Fatalf("Close after attach through %s: %v", c.endpoint, err)
			}
			if err := unix.Kill(pid, 0); err != nil {
				t.Fatalf("FACT (remote %s) is false: browser pid %d is not alive after Close: %v", c.name, pid, err)
			}
			t.Logf("FACT (remote %s): input reached the page and Close left browser pid %d running", c.name, pid)
		})
	}
	t.Run("two_targets", func(t *testing.T) {
		checkLiveRemoteInputIsolation(t, wsURL, httpURL)
	})
}

func liveRemoteInputEngine(t *testing.T, endpoint string, cellW, cellH, cols int) (*ChromiumEngine, context.Context) {
	t.Helper()
	e := New("")
	e.SetRemoteDebuggingURL(endpoint)
	e.SetCellPx(cellW, cellH)
	e.Resize(cols, 24)
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Navigate("data:text/html," + url.PathEscape(remoteInputPage)); err != nil {
		t.Fatalf("navigate through %s: %v", endpoint, err)
	}
	liveReadFrame(t, e, "remote input fixture first frame")
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		t.Fatal("engine context is nil after navigate")
	}
	return e, ctx
}

func checkLiveRemoteInput(t *testing.T, e *ChromiumEngine, ctx context.Context) {
	t.Helper()
	e.SendMouse(MouseEvent{Action: MouseActionMove, Col: 0, Row: 0})
	if got, ok := waitForString(t, ctx, "document.querySelector('button:hover')?.id || ''", "left"); !ok {
		t.Fatalf("top-left hover target = %q, want left", got)
	}
	clickCells(e, 1, 0, 0)
	clickCells(e, 1, 20, 5)
	const clicks = `[["left",4,8],["field",164,88]]`
	if got := evalString(t, ctx, "JSON.stringify(window.clicks)"); got != clicks {
		t.Fatalf("page clicks = %s, want %s", got, clicks)
	}
	if got := evalString(t, ctx, "document.activeElement.id"); got != "field" {
		t.Fatalf("focused element after click = %q, want field", got)
	}
	e.SendText("n")
	e.SendText("y")
	checkRemoteField(t, ctx, "remote", "ny")
	if got := evalString(t, ctx, "window.keys.join('')"); got != "ny" {
		t.Fatalf("keydown events = %q, want ny", got)
	}

	dragCells(e, MouseButtonLeft, 0, [2]int{2, 10}, [2]int{20, 10}, [2]int{37, 10}, [2]int{37, 10})
	if got := evalFloat(t, ctx, "document.getElementById('range').valueAsNumber"); got < 65 || got > 85 {
		t.Fatalf("range value after drag = %g, want near 75", got)
	}
	e.SendMouse(MouseEvent{Action: MouseActionWheel, WheelDY: 1, Col: 40, Row: 15})
	if got, ok := waitForScroll(t, ctx); !ok {
		t.Fatalf("page scrollY after wheel = %g, want a positive displacement", got)
	}
}

func checkLiveRemoteInputIsolation(t *testing.T, wsURL, httpURL string) {
	t.Helper()
	// The first engine starts at a non-integer device scale. Resizing it
	// changes both its viewport width and its CSS pixels per cell.
	first, firstCtx := liveRemoteInputEngine(t, wsURL, 11, 18, 100)
	second, secondCtx := liveRemoteInputEngine(t, httpURL, 8, 16, 80)

	clickCells(first, 1, 20, 5)
	first.SendText("a")
	clickCells(second, 1, 20, 5)
	second.SendText("b")
	checkRemoteField(t, firstCtx, "first", "a")
	checkRemoteField(t, secondCtx, "second", "b")

	checkRemoteRightClick(t, first, firstCtx, 100, 1024, 402)
	first.Resize(60, 24)
	checkRemoteRightClick(t, first, firstCtx, 60, 660, 432)
	checkRemoteRightClick(t, second, secondCtx, 80, 640, 384)
	if got := evalFloat(t, firstCtx, "window.clicks.length"); got != 3 {
		t.Fatalf("first target received %g clicks, want 3", got)
	}
	if got := evalFloat(t, secondCtx, "window.clicks.length"); got != 2 {
		t.Fatalf("second target received %g clicks, want 2", got)
	}
}

func checkRemoteField(t *testing.T, ctx context.Context, name, want string) {
	t.Helper()
	if got := evalString(t, ctx, "document.getElementById('field').value"); got != want {
		t.Fatalf("%s target field = %q, want %q", name, got, want)
	}
}

func checkRemoteRightClick(t *testing.T, e *ChromiumEngine, ctx context.Context, cols, width, height int) {
	t.Helper()
	gotW := innerWidth(t, ctx)
	gotH := evalFloat(t, ctx, "window.innerHeight")
	if gotW != width || gotH != float64(height) {
		t.Fatalf("%d-column target viewport = %d×%g, want %d×%d", cols, gotW, gotH, width, height)
	}
	clickCells(e, 1, cols-1, 0)
	if got := evalString(t, ctx, "window.clicks.at(-1)[0]"); got != "right" {
		t.Fatalf("%d-column target click hit %q, want right", cols, got)
	}
	wantX := float64(width) * (float64(cols) - 0.5) / float64(cols)
	gotX := evalFloat(t, ctx, "window.clicks.at(-1)[1]")
	wantY := float64(height) / 48
	gotY := evalFloat(t, ctx, "window.clicks.at(-1)[2]")
	// Chromium exposes integer client coordinates for click events.
	if math.Abs(gotX-wantX) >= 1 || math.Abs(gotY-wantY) >= 1 {
		t.Fatalf("%d-column target click = (%g, %g), want near (%g, %g)", cols, gotX, gotY, wantX, wantY)
	}
}
