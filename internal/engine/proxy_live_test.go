//go:build live

package engine

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// proxyLiveUser and proxyLivePassword are the credentials the live proxy
// demands. They differ from any default, so a credential mismatch in the engine
// surfaces as a 407 rather than a silent pass-through.
const (
	proxyLiveUser     = "nyala-proxy-user"
	proxyLivePassword = "s3cr3t-proxy-pass"
)

// TestLiveProxyAuthenticatedFrame verifies that a page loads through an
// authenticating HTTP forward proxy. It runs two
// in-process httptest servers: a plain http:// upstream serving one line of
// text, and a forward proxy. The proxy returns 407 with a Proxy-Authenticate
// header when Proxy-Authorization is missing or wrong, and forwards the GET
// upstream when the Basic credentials match. The proxy env carries the
// credentials and the proxy address, set via t.Setenv before New, because New
// parses the proxy environment once. The engine must drive Chromium through the
// proxy, answer the 407 over CDP, and emit a synced frame with a non-empty
// bitmap and crisp text from the upstream page.
//
// The live build tag keeps it off the default build, `just test`, and CI. An
// untagged `go test ./...` never compiles it and never launches a browser. Run
// it with `just live`. It skips with a clear message when no Chromium resolves,
// and uses a select deadline so it fails fast rather than hanging.
func TestLiveProxyAuthenticatedFrame(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	// Upstream: a plain http:// page with one line of text. The text drives the
	// DOM snapshot, so a synced frame must carry at least one crisp GlyphRun.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body style='margin:0'><p>nyala proxy live page</p></body></html>"))
	}))
	defer upstream.Close()

	// Proxy: an authenticating HTTP forward proxy. It demands Basic credentials
	// with a 407, then forwards the absolute-URI GET upstream. A plain http://
	// upstream means Chromium sends a forward GET, not a CONNECT, so plain
	// forwarding is enough and no CONNECT handling is needed.
	var sawAuth bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !proxyCredentialsOK(r) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="nyala"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		sawAuth = true

		// Forward the absolute-URI GET upstream. Chromium addresses a forward
		// proxy with the full target URL in the request line, so r.URL is
		// absolute and usable directly.
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, r.URL.String(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	// The proxy env must be set before New, which parses it once. Point the proxy
	// URL at the proxy address with the credentials embedded, and clear the other
	// proxy and bypass vars so only this one wins.
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	withCreds := "http://" + proxyLiveUser + ":" + proxyLivePassword + "@" + proxyURL.Host
	for _, name := range []string{"https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "no_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("HTTPS_PROXY", withCreds)
	// Both httptest servers bind to 127.0.0.1, and Chromium bypasses the proxy
	// for loopback by default, so it would reach the upstream directly and never
	// hit the proxy. The <-loopback> directive subtracts loopback from the
	// implicit bypass, forcing the loopback upstream through the proxy. The engine
	// feeds NO_PROXY straight into --proxy-bypass-list, so this is how the test
	// drives that flag.
	t.Setenv("NO_PROXY", "<-loopback>")

	e := New("")
	if !e.proxy.HasCredentials {
		t.Fatalf("expected parsed proxy credentials, got %+v", e.proxy)
	}
	e.SetCellPx(8, 16)
	e.Resize(80, 24)
	if err := e.Navigate(upstream.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// The first frame can arrive before the proxied page paints its text, so wait
	// for a frame that carries both a bitmap and a crisp text run, within one
	// generous deadline. A frame with text proves the upstream HTML reached the
	// page through the proxy.
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()

	for {
		select {
		case f, ok := <-e.Frames():
			if !ok {
				t.Fatal("frames channel closed before a text frame arrived")
			}
			if f.Img == nil {
				t.Fatal("synced frame has a nil bitmap")
			}
			if len(f.Text) == 0 {
				continue
			}
			if !sawAuth {
				t.Fatal("proxy never saw an authenticated request; the page did not route through the proxy")
			}
			return
		case <-deadline.C:
			t.Fatal("no synced text frame within the deadline; page did not load through the proxy")
		}
	}
}

// proxyCredentialsOK reports whether the request carries the expected Basic
// Proxy-Authorization. It compares against the live credentials, so a missing or
// wrong header fails and triggers the 407 the engine must answer over CDP.
func proxyCredentialsOK(r *http.Request) bool {
	const prefix = "Basic "
	hdr := r.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(hdr, prefix) {
		return false
	}
	want := base64.StdEncoding.EncodeToString([]byte(proxyLiveUser + ":" + proxyLivePassword))
	return hdr[len(prefix):] == want
}
