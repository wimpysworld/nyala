package engine

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

type userAgentMetadata = emulation.UserAgentMetadata

const nativeUserAgentMetadataExpression = `(async () => {
  const data = navigator.userAgentData;
  if (!data) return null;
  const metadata = {
    brands: data.brands,
    mobile: data.mobile,
    platform: data.platform
  };
  if (typeof data.getHighEntropyValues !== "function") return metadata;
  const hints = [
    "architecture",
    "bitness",
    "formFactors",
    "fullVersionList",
    "model",
    "platformVersion",
    "wow64"
  ];
  try {
    Object.assign(metadata, await data.getHighEntropyValues(hints));
  } catch {
    for (const hint of hints) {
      try {
        Object.assign(metadata, await data.getHighEntropyValues([hint]));
      } catch {}
    }
  }
  return metadata;
})()`

// BrowserVersion returns the driven browser's product normalised for display,
// for example "Chrome 138.0.7204.49" from the headless product
// "HeadlessChrome/138.0.7204.49". It returns an empty string when the version
// is unavailable, such as before the first session or after a fetch failure.
func (e *ChromiumEngine) BrowserVersion() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return formatBrowserVersion(e.browserProduct)
}

// fetchBrowserVersion caches the raw Browser.getVersion values and native user
// agent client hints once per engine. startSession calls it before the first
// override. A cached product skips both reads, so a crash rebuild reuses the
// same values. A Browser.getVersion failure leaves the cache empty. A browser
// without navigator.userAgentData keeps a nil metadata value and uses the
// user-agent string override without client hints.
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
	metadata := fetchNativeUserAgentMetadata(ctx)
	e.mu.Lock()
	e.browserProduct = product
	e.browserUserAgent = userAgent
	e.userAgentMetadata = metadata
	e.mu.Unlock()
}

func fetchNativeUserAgentMetadata(ctx context.Context) *userAgentMetadata {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><title>Nyala browser metadata</title>"))
		}),
		ReadHeaderTimeout: time.Second,
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()
	defer func() {
		_ = server.Close()
		<-serverDone
	}()

	// The initial about:blank page has an opaque origin and does not expose
	// navigator.userAgentData. A short-lived loopback page gives a temporary
	// target a trustworthy origin while the user-facing target stays blank.
	metadataURL := "http://" + listener.Addr().String()
	browserCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
	browserContextID, err := target.CreateBrowserContext().WithDisposeOnDetach(true).Do(browserCtx)
	if err != nil {
		return nil
	}
	defer func() { _ = target.DisposeBrowserContext(browserContextID).Do(browserCtx) }()
	targetID, err := target.CreateTarget("about:blank").
		WithBrowserContextID(browserContextID).
		WithNewWindow(true).
		Do(browserCtx)
	if err != nil {
		return nil
	}
	targetCtx, cancel := chromedp.NewContext(ctx, chromedp.WithTargetID(targetID))
	defer cancel()
	err = chromedp.Run(targetCtx, chromedp.Navigate(metadataURL))
	var metadata *userAgentMetadata
	if err == nil {
		metadata, err = evaluateNativeUserAgentMetadata(targetCtx)
	}
	if err != nil {
		return nil
	}
	return metadata
}

func evaluateNativeUserAgentMetadata(ctx context.Context) (*userAgentMetadata, error) {
	var metadata *userAgentMetadata
	err := chromedp.Run(ctx, chromedp.Evaluate(
		nativeUserAgentMetadataExpression,
		&metadata,
		func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
			return params.WithAwaitPromise(true)
		},
	))
	return metadata, err
}

func (e *ChromiumEngine) applyUserAgentOverride(ctx context.Context) error {
	e.mu.Lock()
	userAgent := formatBrowserUserAgent(e.browserUserAgent)
	metadata := e.userAgentMetadata
	e.mu.Unlock()
	if userAgent == "" {
		return nil
	}
	return chromedp.Run(ctx, userAgentOverride(userAgent, metadata))
}

func userAgentOverride(userAgent string, metadata *userAgentMetadata) *emulation.SetUserAgentOverrideParams {
	override := emulation.SetUserAgentOverride(userAgent)
	if metadata == nil {
		return override
	}
	return override.WithUserAgentMetadata(metadata)
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
