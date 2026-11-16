//go:build live

package engine

import (
	"context"
	"net/url"
	"os/exec"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"golang.org/x/sys/unix"
)

// TestLiveRemoteAttach drives the remote attach path against a Chromium the
// test spawned itself: the engine attaches through SetRemoteDebuggingURL,
// renders a frame, closes, and leaves the browser running. The ws_url subtest
// passes the DevToolsActivePort socket URL as given and the http_url subtest
// passes http://127.0.0.1:<port>, which chromedp resolves through
// /json/version. Each subtest logs a "FACT (remote ...)" line.
//
// Run it with `just live`. It skips when no Chromium resolves.
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
		name, endpoint := c.name, c.endpoint
		t.Run(name, func(t *testing.T) {
			e := New("")
			e.SetRemoteDebuggingURL(endpoint)
			e.SetCellPx(8, 16)
			e.Resize(80, 24)
			if err := e.Navigate(liveTestPage); err != nil {
				t.Fatalf("navigate through %s: %v", endpoint, err)
			}
			t.Cleanup(func() { _ = e.Close() })
			liveReadFrame(t, e, name+" first frame")
			if err := e.Close(); err != nil {
				t.Fatalf("Close after attach through %s: %v", endpoint, err)
			}
			if err := unix.Kill(pid, 0); err != nil {
				t.Fatalf("FACT (remote %s) is false: browser pid %d is not alive after Close: %v", name, pid, err)
			}
			t.Logf("FACT (remote %s): engine attached through %s, rendered a frame, and Close left browser pid %d running", name, endpoint, pid)
		})
	}
}
