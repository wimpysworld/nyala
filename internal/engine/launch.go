package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
	"github.com/wimpysworld/nyala/internal/render"
)

// chromiumNames lists the executable names tried on PATH, in order, when no
// explicit path is given. They cover the common Chromium and Chrome binaries
// across distributions.
var chromiumNames = []string{
	"chromium",
	"chromium-browser",
	"google-chrome",
	"google-chrome-stable",
	"chrome",
}

// resolveExecutable returns the Chromium or Chrome executable to launch. It
// tries, in order: the --browser flag value, the NYALA_BROWSER environment
// variable, the CHROME_PATH environment variable, then a PATH lookup for a
// known Chromium or Chrome binary name. It returns the first non-empty result
// and a clear error naming the search order when none resolve. The function is
// pure beyond reading the environment and PATH, so it is hermetically testable.
func resolveExecutable(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("NYALA_BROWSER"); v != "" {
		return v, nil
	}
	if v := os.Getenv("CHROME_PATH"); v != "" {
		return v, nil
	}
	for _, name := range chromiumNames {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf(
		"no Chromium found: set --browser, or the NYALA_BROWSER or CHROME_PATH "+
			"environment variable, or put one of %v on PATH",
		chromiumNames,
	)
}

// captureSize returns the source-pixel extent the screencast captures for the
// engine's active backend and resolved cell px. The block backend samples a
// sub-cell grid via render.BlockCaptureSize (quadrant scheme); the graphics
// backends capture a real cell-pixel bitmap via render.GraphicsCaptureSize. The
// engine derives this itself on every resize and backend switch, so the size is
// never passed in from the viewer.
func (e *ChromiumEngine) captureSize(cols, rows int) (pxW, pxH int) {
	if e.backend == "block" {
		return render.BlockCaptureSize(true, cols, rows)
	}
	return render.GraphicsCaptureSize(cols, rows, e.cellW, e.cellH)
}

// newAllocator builds a chromedp ExecAllocator for the resolved executable,
// sized to capW x capH. It sets the launch --window-size from the capture size
// and pins the device scale factor to 1. The window size matters because
// emulation.SetDeviceMetricsOverride resizes only the CSS viewport, not the
// captured content area; without it frames arrive at the default 1280x720. The
// device-metrics override at DSF 1 is not an allocator option, so it is applied
// on the context via applyDeviceMetrics once the context exists. When cfg has a
// Server it adds --proxy-server; it adds --proxy-bypass-list only when both a
// Server and a Bypass are set, so a stray NO_PROXY without a proxy adds no flag.
func newAllocator(parent context.Context, execPath string, capW, capH int, cfg ProxyConfig) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		chromedp.WindowSize(capW, capH),
		chromedp.Flag("force-device-scale-factor", "1"),
	)
	for _, pair := range proxyFlags(cfg) {
		opts = append(opts, chromedp.Flag(pair[0], pair[1]))
	}
	return chromedp.NewExecAllocator(parent, opts...)
}

// proxyFlags returns the proxy-related Chromium flag name/value pairs newAllocator
// appends for cfg, in order. It adds proxy-server only when a Server is set, and
// proxy-bypass-list only when both a Server and a Bypass are set, so a stray
// NO_PROXY without a proxy adds no flag. A zero ProxyConfig yields no pairs. The
// chromedp ExecAllocatorOption that newAllocator builds from these pairs is an
// opaque func value, so this pure builder is the seam a unit test asserts on.
func proxyFlags(cfg ProxyConfig) [][2]string {
	if cfg.Server == "" {
		return nil
	}
	pairs := [][2]string{{"proxy-server", cfg.Server}}
	if cfg.Bypass != "" {
		pairs = append(pairs, [2]string{"proxy-bypass-list", cfg.Bypass})
	}
	return pairs
}

// applyDeviceMetrics sets the CSS viewport to capW x capH at device scale
// factor 1 on the live context. It runs after the context exists because the
// override is a CDP command on the page, not an allocator option. The launch
// --window-size set by newAllocator sizes the captured content area; this sizes
// the viewport to match.
func applyDeviceMetrics(ctx context.Context, capW, capH int) error {
	return chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(capW), int64(capH), 1, false),
	)
}

// launch resolves the executable from flag, builds the allocator and the
// chromedp context sized to the engine's current capture size, then applies the
// device-metrics override at DSF 1. It stores the context and its cancel on the
// engine. The full navigate path that calls this lands in Phase 4.
func (e *ChromiumEngine) launch(flag string) error {
	if e.proxyErr != nil {
		return e.proxyErr
	}
	execPath, err := resolveExecutable(flag)
	if err != nil {
		return err
	}
	e.mu.Lock()
	capW, capH := e.captureW, e.captureH
	e.mu.Unlock()
	allocCtx, allocCancel := newAllocator(context.Background(), execPath, capW, capH, e.proxy)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	cancel := func() {
		ctxCancel()
		allocCancel()
	}
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.mu.Unlock()
	if err := applyDeviceMetrics(ctx, capW, capH); err != nil {
		cancel()
		return err
	}
	if err := e.enableProxyAuth(ctx); err != nil {
		cancel()
		return err
	}
	return nil
}

// proxyAuthActionKind names the CDP call the auth listener runs for one event.
// It lets the pure proxyAuthAction decision be asserted in a unit test without a
// browser; the listener maps each kind to the matching fetch command.
type proxyAuthActionKind int

const (
	// proxyAuthNone means the event is not one the listener answers; do nothing.
	proxyAuthNone proxyAuthActionKind = iota
	// proxyAuthContinueWithAuth means answer Fetch.continueWithAuth with Resp.
	proxyAuthContinueWithAuth
	// proxyAuthContinueRequest means answer Fetch.continueRequest to drain the
	// pause that Fetch enablement created.
	proxyAuthContinueRequest
)

// proxyAuthAction describes the single CDP call the auth listener should run for
// one Fetch event. Kind selects the call; RequestID targets it; Resp carries the
// AuthChallengeResponse for the continueWithAuth arm and is nil otherwise.
type proxyAuthAction struct {
	Kind      proxyAuthActionKind
	RequestID fetch.RequestID
	Resp      *fetch.AuthChallengeResponse
}

// proxyAuthDecision is the pure decision the auth listener runs for one Fetch
// event. Given the event, the per-session answered-id set, and the proxy config,
// it returns the CDP action to run and records the request id in answered so a
// repeat challenge for the same request is answered Default instead of looping.
// It runs no CDP itself: the listener owns chromedp.Run. Keeping the decision
// pure lets it be driven with synthesised events in a unit test.
//
// An EventAuthRequired from a Proxy source is answered ProvideCredentials with
// the parsed user and password the first time, then Default on any repeat for
// that request id. A challenge from any other source is answered Default, since
// only proxy auth is in scope; Chromium's net stack handles the rest. An
// EventRequestPaused is continued unchanged to drain the pause that enabling
// Fetch with handleAuthRequests creates. Any other event yields a no-op.
func proxyAuthDecision(ev any, answered map[fetch.RequestID]bool, cfg ProxyConfig) proxyAuthAction {
	switch e := ev.(type) {
	case *fetch.EventAuthRequired:
		resp := &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseDefault}
		if !answered[e.RequestID] && e.AuthChallenge != nil && e.AuthChallenge.Source == fetch.AuthChallengeSourceProxy {
			resp = &fetch.AuthChallengeResponse{
				Response: fetch.AuthChallengeResponseResponseProvideCredentials,
				Username: cfg.User,
				Password: cfg.Password,
			}
		}
		answered[e.RequestID] = true
		return proxyAuthAction{Kind: proxyAuthContinueWithAuth, RequestID: e.RequestID, Resp: resp}
	case *fetch.EventRequestPaused:
		return proxyAuthAction{Kind: proxyAuthContinueRequest, RequestID: e.RequestID}
	default:
		return proxyAuthAction{Kind: proxyAuthNone}
	}
}

// enableProxyAuth enables the Fetch domain with handleAuthRequests and registers
// a ListenTarget handler that answers proxy auth challenges over CDP. It is a
// no-op when the parsed config carries no credentials, so the credential-free
// and no-proxy paths register no listener and call no fetch.Enable, and pay zero
// per-request Fetch cost. With credentials it mirrors the watchCrash listener:
// the answered-id set is created per session here (not a shared engine field), so
// a rebuild that re-runs launch resets it. The proxy user and password live on
// the set-once e.proxy and need no lock; the listener runs its CDP call in a
// goroutine and never holds e.mu across chromedp.Run.
func (e *ChromiumEngine) enableProxyAuth(ctx context.Context) error {
	if !e.proxy.HasCredentials {
		return nil
	}
	if err := chromedp.Run(ctx, fetch.Enable().WithHandleAuthRequests(true)); err != nil {
		return err
	}
	answered := make(map[fetch.RequestID]bool)
	cfg := e.proxy
	chromedp.ListenTarget(ctx, func(ev any) {
		action := proxyAuthDecision(ev, answered, cfg)
		switch action.Kind {
		case proxyAuthContinueWithAuth:
			go func() { _ = chromedp.Run(ctx, fetch.ContinueWithAuth(action.RequestID, action.Resp)) }()
		case proxyAuthContinueRequest:
			go func() { _ = chromedp.Run(ctx, fetch.ContinueRequest(action.RequestID)) }()
		case proxyAuthNone:
		}
	})
	return nil
}
