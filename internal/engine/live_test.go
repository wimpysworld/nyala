//go:build live

package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// liveTestPage is a self-contained data: URL with one line of text. A data: URL
// needs no network and no local file, so the live test stays hermetic once a
// Chromium resolves. The text drives the DOM snapshot, so the synced frame must
// carry at least one crisp GlyphRun.
const liveTestPage = "data:text/html," +
	"<html><body style='margin:0'><p>nyala live smoke</p></body></html>"

// TestLiveSyncedFrame is the live-tagged integration test (decision 5). It
// launches a real Chromium, navigates to a self-contained page, and asserts at
// least one synced frame arrives with non-empty Text (the crisp DOM overlay).
// The live build tag keeps it off the default build, `just test`, and CI: an
// untagged `go test ./...` never compiles it and never launches a browser. Run
// it with `just live`. It skips with a clear message when no Chromium resolves,
// so the tagged run degrades gracefully on a host without a browser, and uses a
// context timeout via a select deadline so it fails fast rather than hanging.
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

	select {
	case f, ok := <-e.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
		if f.Img == nil {
			t.Fatal("synced frame has a nil bitmap")
		}
		if len(f.Text) == 0 {
			t.Fatal("synced frame has no crisp text runs")
		}
	case <-deadline.C:
		t.Fatal("no synced frame within the deadline")
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
		_, _ = w.Write([]byte("<html><body><p>nyala partial load</p><img src='/hang'></body></html>"))
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

	select {
	case f, ok := <-e.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
		if f.Img == nil {
			t.Fatal("frame has a nil bitmap")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no frame arrived before full page load")
	}
}
