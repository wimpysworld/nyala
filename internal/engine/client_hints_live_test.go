//go:build live

package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestLiveMainDocumentSendsUserAgentClientHints(t *testing.T) {
	mainDocument := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			select {
			case mainDocument <- r.Header.Clone():
			default:
			}
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>nyala client hints live page</body></html>"))
	}))
	defer server.Close()

	_, ctx := liveEngineOn(t, server.URL)

	var webdriverIsFalse bool
	if err := chromedp.Run(ctx, chromedp.Evaluate("navigator.webdriver === false", &webdriverIsFalse)); err != nil {
		t.Fatalf("read navigator.webdriver: %v", err)
	}
	if !webdriverIsFalse {
		t.Fatal("navigator.webdriver is not false")
	}

	headers := <-mainDocument
	userAgent := headers.Get("User-Agent")
	if !strings.Contains(userAgent, "Chrome/") {
		t.Fatalf("User-Agent %q does not contain Chrome/", userAgent)
	}
	if strings.Contains(userAgent, "HeadlessChrome") {
		t.Fatalf("User-Agent %q contains HeadlessChrome", userAgent)
	}
	_, chromeVersion, _ := strings.Cut(userAgent, "Chrome/")
	chromeMajor, _, _ := strings.Cut(chromeVersion, ".")
	if chromeMajor == "" {
		t.Fatalf("User-Agent %q has an empty Chrome major version", userAgent)
	}

	var navigatorBrands []struct {
		Version string `json:"version"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate("navigator.userAgentData?.brands ?? []", &navigatorBrands)); err != nil {
		t.Fatalf("read navigator.userAgentData.brands: %v", err)
	}
	if len(navigatorBrands) == 0 {
		t.Fatal("navigator.userAgentData.brands is empty")
	}
	majorInNavigatorBrands := false
	for _, brand := range navigatorBrands {
		if brand.Version == chromeMajor {
			majorInNavigatorBrands = true
			break
		}
	}
	if !majorInNavigatorBrands {
		t.Fatalf("navigator.userAgentData.brands %#v does not contain User-Agent Chrome major version %q", navigatorBrands, chromeMajor)
	}

	secCHUA := headers.Get("Sec-CH-UA")
	if secCHUA == "" {
		t.Fatal("Sec-CH-UA is empty")
	}
	if got := headers.Get("Sec-CH-UA-Mobile"); got == "" {
		t.Fatal("Sec-CH-UA-Mobile is empty")
	}
	if got := headers.Get("Sec-CH-UA-Platform"); got == "" {
		t.Fatal("Sec-CH-UA-Platform is empty")
	}

	if !strings.Contains(secCHUA, `v="`+chromeMajor+`"`) {
		t.Fatalf("Sec-CH-UA %q does not contain User-Agent Chrome major version %q", secCHUA, chromeMajor)
	}
}
