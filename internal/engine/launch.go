package engine

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/font"
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

// surfaceFloorPx is the minimum captured-surface dimension. A 0x0 pty (the agent
// shell reports no pixel size) would otherwise launch a blank, zero-size surface
// that Chromium cannot lay out. The floor keeps the layout viewport, the captured
// surface, and the screencast cap non-degenerate so the page still renders.
const surfaceFloorPx = 16

// surfaceSize returns the real CSS-pixel viewport the page lays out into: the
// terminal grid in cell pixels, cols*cellW by rows*cellH, the same for every
// backend. It is one of the three independent sizing values: the layout viewport
// (setDeviceMetricsOverride), the captured surface (Browser.SetContentsSize), and
// the screencast cap (maxWidth/maxHeight) all take this real viewport, while the
// Each axis is clamped to surfaceFloorPx so a 0x0 grid resolves to a small
// non-zero surface rather than a blank one.
func (e *ChromiumEngine) surfaceSize(cols, rows int) (pxW, pxH int) {
	w, h := render.GraphicsCaptureSize(cols, rows, e.cellW, e.cellH)
	if w < surfaceFloorPx {
		w = surfaceFloorPx
	}
	if h < surfaceFloorPx {
		h = surfaceFloorPx
	}
	return w, h
}

// screencastCap returns the painted device surface the screencast is capped to:
// the paintedSurface derivation of the real surface at the active scale Z. On the
// block backend Z > 1 shrinks the CSS viewport, so the painted surface (round of
// the floored CSS height times Z) is at or just under surfaceSize, and capping the
// screencast to it keeps Chromium from downscaling a frame it painted one row over
// a cap set from an independently rounded surfaceSize. On a graphics backend the
// density Z keeps the painted surface at surfaceSize within one rounded row, so
// the captured pixel count stays set by the terminal grid. It takes the
// same (cols, rows) surfaceSize does and reads no engine field beyond the scale, so
// callers pass the grid they already hold under mu.
func (e *ChromiumEngine) screencastCap(cols, rows int) (pxW, pxH int) {
	surfW, surfH := e.surfaceSize(cols, rows)
	_, _, paintW, paintH := paintedSurface(surfW, surfH, e.layoutScale())
	return paintW, paintH
}

// textOverlayBackend reports whether the active backend consumes the crisp
// DOM-text overlay. Only the block backend folds Frame.Text onto its cell grid;
// the graphics backends (sixel, kitty) draw the bitmap alone and ignore the
// overlay, so composeFrame skips the DOM snapshot for them. It takes e.mu because
// the frame loop calls it off the mu-holding viewer goroutines that set e.backend.
func (e *ChromiumEngine) textOverlayBackend() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.backend.UsesTextOverlay()
}

type allocatorConfig struct {
	execPath              string
	surfaceWidth          int
	surfaceHeight         int
	scale                 float64
	proxy                 ProxyConfig
	enableGPU             bool
	enableAudio           bool
	disableFrameRateLimit bool
}

// newAllocator builds a chromedp ExecAllocator for the resolved executable,
// sized to the configured real device-pixel surface (surfaceSize). It sets the
// launch --window-size from the surface size and the configured device scale factor:
// each backend passes its continuous Z (the block layout scale or the graphics
// density scale) so the page lays out at surf/Z CSS pixels. The window size matters because
// emulation.SetDeviceMetricsOverride resizes only the CSS viewport, not the
// captured content area; the surface is then sized authoritatively with
// Browser.SetContentsSize once the context and window id exist. When
// cfg.proxy.Server is set it adds --proxy-server; it adds --proxy-bypass-list only
// when both cfg.proxy.Server and cfg.proxy.Bypass are set, so a stray NO_PROXY
// without a proxy adds no flag.
// When enableGPU is set it adds the gpuFlags pair: --enable-gpu turns off the
// forced SwiftShader (CPU) default of headless Chromium so a real driver can serve
// WebGL, and --enable-unsafe-swiftshader keeps a software WebGL floor for a host
// with no usable GPU.
// When enableAudio is set it appends the bool false for mute-audio, overriding the
// mute-audio=true that Headless sets: chromedp flags are last-write-wins by map key,
// and this append lands after DefaultExecAllocatorOptions. The bool false emits no
// flag at all (audio on); a string "false" would emit --mute-audio=false and keep
// Chromium muted. It also sets autoplay-policy=no-user-gesture-required so audible
// media plays on load: Chromium otherwise blocks audible autoplay until a user
// gesture, and Nyala has no input yet. The caller gates audio on a detected host
// sink. disableFrameRateLimit appends Chromium's matching launch switch when
// requested; false preserves the default allocator options.
// It always adds --disable-smooth-scrolling: Chromium animates a keyboard
// scroll over about 150 ms, which a terminal pays for in frames, and the caret
// keys read the page's settled scroll offset back in the same dispatch that
// pressed the key, so an animated scroll would be read part way through.
func newAllocator(parent context.Context, cfg allocatorConfig) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(cfg.execPath),
		chromedp.WindowSize(cfg.surfaceWidth, cfg.surfaceHeight),
		chromedp.Flag("force-device-scale-factor", strconv.FormatFloat(cfg.scale, 'g', -1, 64)),
		chromedp.Flag("disable-smooth-scrolling", true),
	)
	for _, name := range gpuFlags(cfg.enableGPU) {
		opts = append(opts, chromedp.Flag(name, true))
	}
	if cfg.enableAudio {
		opts = append(opts, chromedp.Flag("mute-audio", false))
		// Chromium blocks audible autoplay until a user gesture. Nyala has no input
		// yet, so audible media on a page would never start. Allow gesture-free
		// autoplay so it plays on load. Muted autoplay is already allowed, so this
		// pairs with the unmute above and rides the same live-path gate.
		opts = append(opts, chromedp.Flag("autoplay-policy", "no-user-gesture-required"))
	}
	for _, name := range frameRateLimitFlags(cfg.disableFrameRateLimit) {
		opts = append(opts, chromedp.Flag(name, true))
	}
	for _, pair := range proxyFlags(cfg.proxy) {
		opts = append(opts, chromedp.Flag(pair[0], pair[1]))
	}
	return chromedp.NewExecAllocator(parent, opts...)
}

// frameRateLimitFlags returns Chromium's launch-time frame-rate-limit switch
// only when the matching Nyala option is enabled. The pure result keeps flag
// presence and absence unit-testable without launching Chromium.
func frameRateLimitFlags(disable bool) []string {
	if !disable {
		return nil
	}
	return []string{"disable-frame-rate-limit"}
}

// gpuFlags returns the GPU-related Chromium launch switches newAllocator appends,
// in order, and nothing when the GPU is not enabled. --enable-gpu turns off the
// forced SwiftShader default of headless Chromium and reverts to normal driver
// selection, so a GPU-backed Mesa driver can serve WebGL. That flag alone is not
// enough: Chromium removed the automatic SwiftShader software WebGL fallback
// (deprecated from 130, off by default from 144), so a host with no usable
// GPU gets no WebGL context at all. A kmscon console is exactly that host, since
// kmscon holds the DRM card node and headless GL autodetection on Linux needs X11.
// --enable-unsafe-swiftshader is the opt-in Chromium now requires for that software
// fallback, and hardware GL still wins wherever a GPU works, so it only adds a
// floor. The trade-off: SwiftShader JITs shader code in a weaker sandbox posture
// and software WebGL is slow, which is why hardware stays preferred. The pure
// result keeps the flag set unit-testable without launching Chromium.
func gpuFlags(enable bool) []string {
	if !enable {
		return nil
	}
	return []string{"enable-gpu", "enable-unsafe-swiftshader"}
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

// paintedSurface derives the CSS layout dimensions and the resulting painted
// device surface from a target real surface surfW x surfH at device scale Z. The
// CSS dimensions are chosen so the page reflows to the target width yet never
// paints past the screencast cap: the width divides exactly (Z is surface over
// cssW by construction), so a round is correct, but the height takes a floor so
// cssH*Z <= surfH.
// A round of the height would let cssH*Z land one device row over surfH (the
// 1437*1.975 = 2839 > 2838 fault), so Chromium would downscale every frame; the
// floor keeps the painted surface at or under the cap. It returns the CSS
// dimensions and the painted device surface paintW = round(cssW*Z), paintH =
// round(cssH*Z), the surface Chromium actually paints and the screencast cap and
// the compose model both take, so the engine's model matches the delivered bitmap.
//
// The paint model holds exactly only while Z equals the launch
// --force-device-scale-factor: headless Chromium paints at the launch density for
// the whole process, and a later SetDeviceMetricsOverride changes layout alone.
// After a mid-session zoom to a larger Z the delivered bitmap is the complete css
// viewport at the launch density, so it is undersized against paint = css x Z and
// the viewer scales it onto the terminal grid (see the Kitty c=/r= placement keys
// and the Sixel grid upscale).
func paintedSurface(surfW, surfH int, z float64) (cssW, cssH, paintW, paintH int) {
	cssW = int(math.Round(float64(surfW) / z))
	cssH = int(math.Floor(float64(surfH) / z))
	paintW = int(math.Round(float64(cssW) * z))
	paintH = int(math.Round(float64(cssH) * z))
	return cssW, cssH, paintW, paintH
}

// applySizing sizes the layout viewport and the captured surface together on the
// live context, in the CSS pixels paintedSurface derives at device scale factor Z.
// It sets the CSS layout viewport with emulation.SetDeviceMetricsOverride(cssW,
// cssH, Z), then sizes the captured content area with Browser.SetContentsSize on
// windowID (width and height in DIP, which equal the CSS pixels). The two are
// distinct CDP concepts: the device-metrics override resizes only the layout
// viewport, never the captured surface, so the surface call is required or frames
// arrive cropped or blank. Under Z > 1 both must be the CSS dimensions, not the
// device surface, so the page lays out at the target width. The screencast cap
// (maxWidth/maxHeight, set by startScreencast) takes the painted device surface
// from the same paintedSurface computation, so Chromium never downscales a frame
// to fit a cap set one row short of the painted rows.
//
// Measured constraint (Chromium 150): the paint density is pinned to the launch
// --force-device-scale-factor and the live Z here changes layout only, so sizing
// the contents at any other derivation cannot change the delivered pixel density.
// Sizing them at the window extent instead of the CSS pair broke the Block
// contract (the bitmap must span exactly the css viewport) whenever Z fell below
// the launch scale, so the CSS pair is authoritative for both calls.
func applySizing(ctx context.Context, windowID browser.WindowID, surfW, surfH int, z float64) error {
	cssW, cssH, _, _ := paintedSurface(surfW, surfH, z)
	if err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(cssW), int64(cssH), z, false),
	); err != nil {
		return err
	}
	return chromedp.Run(ctx,
		browser.SetContentsSize(windowID).WithWidth(int64(cssW)).WithHeight(int64(cssH)),
	)
}

// enableLifecycle enables the Page domain and lifecycle events once per session
// before navigation. CDP emits Page.lifecycleEvent only after
// SetLifecycleEventsEnabled, so without it the navigate waiter cannot match a
// loaderID from the "init" event and the paint milestone never fires.
func enableLifecycle(ctx context.Context) error {
	return chromedp.Run(ctx,
		page.Enable(),
		page.SetLifecycleEventsEnabled(true),
	)
}

// injectFontStylesheet registers the font-forcing stylesheet script to run on
// every new document. It seeds the cell size from snapshotCellPx and installs the
// script with Page.addScriptToEvaluateOnNewDocument, so the stylesheet applies to
// the first document this session loads and to every later one, including the
// document that follows a crash-recovery restart. It records the returned
// ScriptIdentifier under mu so a later backend switch can remove the script. It
// runs after enableLifecycle and before navigation so the script is registered
// before the first document loads.
func (e *ChromiumEngine) injectFontStylesheet(ctx context.Context) error {
	e.mu.Lock()
	cellW, cellH := e.snapshotCellPx()
	paintText := e.diagnosticTextPaint
	e.mu.Unlock()
	script := buildStylesheetScriptWithPaint(font.FiraCodeTTF(), cellW, cellH, paintText)
	var id page.ScriptIdentifier
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		var err error
		id, err = page.AddScriptToEvaluateOnNewDocument(script).Do(runCtx)
		return err
	})); err != nil {
		return err
	}
	e.mu.Lock()
	e.fontForcingID = id
	e.forcedCellW, e.forcedCellH = cellW, cellH
	e.mu.Unlock()
	return nil
}

// launch resolves the executable from flag, builds the allocator and the
// chromedp context sized to the device-pixel surface, fetches
// the browser window id, enables lifecycle events, then sizes the layout viewport
// and the captured surface together. It stores the context, its cancel, and the
// window id on the engine.
func (e *ChromiumEngine) launch(flag string) error {
	if e.proxyErr != nil {
		return e.proxyErr
	}
	execPath, err := resolveExecutable(flag)
	if err != nil {
		return err
	}
	e.mu.Lock()
	surfW, surfH := e.surfaceSize(e.cols, e.rows)
	forcing := e.backend.UsesTextOverlay()
	z := e.layoutScale()
	disableFrameRateLimit := e.disableFrameRateLimit
	e.mu.Unlock()
	enableGPU := true
	enableAudio := audioAvailable()
	allocCtx, allocCancel := newAllocator(context.Background(), allocatorConfig{
		execPath:              execPath,
		surfaceWidth:          surfW,
		surfaceHeight:         surfH,
		scale:                 z,
		proxy:                 e.proxy,
		enableGPU:             enableGPU,
		enableAudio:           enableAudio,
		disableFrameRateLimit: disableFrameRateLimit,
	})
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	cancel := func() {
		ctxCancel()
		allocCancel()
	}
	closeBrowser := func(closeCtx context.Context) error {
		if err := chromedp.Cancel(closeCtx); err != nil {
			return err
		}
		allocCancel()
		return nil
	}
	e.mu.Lock()
	e.ctx = ctx
	e.closeBrowserOp = closeBrowser
	e.cancel = cancel
	e.mu.Unlock()
	// Fetch the window id once per session so SetContentsSize can size the captured
	// surface here and on every restart. GetWindowForTarget runs through chromedp.Run
	// so it gets the target executor context.
	var windowID browser.WindowID
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		id, _, gerr := browser.GetWindowForTarget().Do(runCtx)
		windowID = id
		return gerr
	})); err != nil {
		e.teardownSession()
		return err
	}
	e.mu.Lock()
	e.windowID = windowID
	e.mu.Unlock()
	if err := enableLifecycle(ctx); err != nil {
		e.teardownSession()
		return err
	}
	// Force the page onto the monospace grid only for the block backend, which folds
	// the crisp text overlay onto its cell grid. The graphics backends draw the
	// bitmap alone, so forcing the font would distort a page they render faithfully.
	if forcing {
		if err := e.injectFontStylesheet(ctx); err != nil {
			e.teardownSession()
			return err
		}
		// Register the layout-shift observer and its binding on the block backend, so a
		// reflow after the last navigation, resize, scroll, or networkAlmostIdle trigger
		// re-decodes the text and the overlay tracks the final layout. It shares
		// syncFontForcing's enable path, so a cycle away and back reconciles it with the
		// font forcing. A CDP miss degrades the trigger rather than the launch.
		e.enableLayoutShift(ctx)
	}
	if err := applySizing(ctx, windowID, surfW, surfH, z); err != nil {
		e.teardownSession()
		return err
	}
	e.mu.Lock()
	e.sizedSurfaceW, e.sizedSurfaceH, e.sizedScale = surfW, surfH, z
	e.mu.Unlock()
	if err := e.enableProxyAuth(ctx); err != nil {
		e.teardownSession()
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
	// fetch.Enable with no RequestPattern set pauses every request, not just the
	// ones carrying an auth challenge. That blanket pause is load-bearing: the
	// listener's proxyAuthContinueRequest arm continues each paused request
	// unchanged, so the no-pattern enable plus the continue drain keeps traffic
	// flowing while answering proxy auth. Narrowing the patterns would skip the
	// drain but also risk missing an auth challenge, so the blanket enable stays.
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
