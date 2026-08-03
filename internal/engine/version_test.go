package engine

import "testing"

// TestFormatBrowserVersion asserts the display normalisation of the raw
// Browser.getVersion product: a leading Headless is stripped from the name and
// the slash becomes a space, an empty product stays empty, and a product
// without a version keeps just the normalised name.
func TestFormatBrowserVersion(t *testing.T) {
	cases := []struct {
		name    string
		product string
		want    string
	}{
		{"empty", "", ""},
		{"headless chrome", "HeadlessChrome/138.0.7204.49", "Chrome 138.0.7204.49"},
		{"chromium", "Chromium/120.0.6099.109", "Chromium 120.0.6099.109"},
		{"headed chrome", "Chrome/138.0.7204.49", "Chrome 138.0.7204.49"},
		{"name only", "HeadlessChrome", "Chrome"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatBrowserVersion(c.product); got != c.want {
				t.Fatalf("formatBrowserVersion(%q) = %q, want %q", c.product, got, c.want)
			}
		})
	}
}

func TestFormatBrowserUserAgent(t *testing.T) {
	const headlessUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/138.0.7204.49 Safari/537.36"
	const want = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.7204.49 Safari/537.36"
	if got := formatBrowserUserAgent(headlessUA); got != want {
		t.Fatalf("formatBrowserUserAgent(%q) = %q, want %q", headlessUA, got, want)
	}
}

// TestBrowserVersionUnavailable asserts the engine reports an empty version
// before any session has cached the Browser.getVersion product.
func TestBrowserVersionUnavailable(t *testing.T) {
	e := New("")
	if got := e.BrowserVersion(); got != "" {
		t.Fatalf("BrowserVersion() = %q before a session, want empty", got)
	}
}
