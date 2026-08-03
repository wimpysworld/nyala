package engine

import (
	"context"
	"strings"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// BrowserVersion returns the driven browser's product normalised for display,
// for example "Chrome 138.0.7204.49" from the headless product
// "HeadlessChrome/138.0.7204.49". It returns an empty string when the version
// is unavailable, such as before the first session or after a fetch failure.
func (e *ChromiumEngine) BrowserVersion() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return formatBrowserVersion(e.browserProduct)
}

// fetchBrowserVersion caches the raw Browser.getVersion product string once
// per engine. startSession calls it on the fresh session context; a cached
// product skips the CDP call, so a crash rebuild does not refetch, and a
// fetch failure leaves the cache empty so BrowserVersion degrades to an
// empty string rather than failing the session.
func (e *ChromiumEngine) fetchBrowserVersion(ctx context.Context) {
	e.mu.Lock()
	cached := e.browserProduct
	e.mu.Unlock()
	if cached != "" {
		return
	}
	var product string
	var userAgent string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		_, p, _, ua, _, gerr := browser.GetVersion().Do(runCtx)
		product = p
		userAgent = ua
		return gerr
	}))
	if err != nil {
		return
	}
	e.mu.Lock()
	e.browserProduct = product
	e.browserUserAgent = userAgent
	e.mu.Unlock()
}

func (e *ChromiumEngine) applyUserAgentOverride(ctx context.Context) error {
	e.mu.Lock()
	userAgent := formatBrowserUserAgent(e.browserUserAgent)
	e.mu.Unlock()
	if userAgent == "" {
		return nil
	}
	return chromedp.Run(ctx, emulation.SetUserAgentOverride(userAgent))
}

// formatBrowserVersion normalises a Browser.getVersion product string for
// display. The product arrives as "Name/Version" and headless builds prefix
// the name with "Headless", so "HeadlessChrome/138.0.7204.49" becomes
// "Chrome 138.0.7204.49". An empty product yields an empty string.
func formatBrowserVersion(product string) string {
	name, version, ok := strings.Cut(product, "/")
	name = strings.TrimPrefix(name, "Headless")
	if !ok || version == "" {
		return name
	}
	if name == "" {
		return version
	}
	return name + " " + version
}

func formatBrowserUserAgent(userAgent string) string {
	return strings.ReplaceAll(userAgent, "HeadlessChrome", "Chrome")
}
