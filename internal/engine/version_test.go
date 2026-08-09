package engine

import (
	"reflect"
	"testing"

	"github.com/chromedp/cdproto/emulation"
)

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

func TestUserAgentOverridePreservesNativeMetadata(t *testing.T) {
	metadata := &userAgentMetadata{
		Brands: []*emulation.UserAgentBrandVersion{
			{Brand: "Not/A)Brand", Version: "8"},
			{Brand: "Chromium", Version: "138"},
		},
		FullVersionList: []*emulation.UserAgentBrandVersion{
			{Brand: "Not/A)Brand", Version: "8.0.0.0"},
			{Brand: "Chromium", Version: "138.0.7204.49"},
		},
		Platform:        "Linux",
		PlatformVersion: "6.12.0",
		Architecture:    "x86",
		Bitness:         "64",
		Model:           "",
		Mobile:          false,
		Wow64:           false,
		FormFactors:     []string{"Desktop"},
	}
	override := userAgentOverride("native UA", metadata)
	if override.UserAgent != "native UA" {
		t.Fatalf("UserAgent = %q, want native UA", override.UserAgent)
	}
	if !reflect.DeepEqual(override.UserAgentMetadata, metadata) {
		t.Fatalf("UserAgentMetadata = %#v, want %#v", override.UserAgentMetadata, metadata)
	}
}

func TestUserAgentOverrideWithoutMetadata(t *testing.T) {
	override := userAgentOverride("legacy UA", nil)
	if override.UserAgentMetadata != nil {
		t.Fatalf("UserAgentMetadata = %#v, want nil", override.UserAgentMetadata)
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
