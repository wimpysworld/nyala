package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/font"
	"golang.org/x/sys/unix"
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

// resolveProfileDir returns the profile directory to use. It tries, in order:
// the --profile flag value, the NYALA_PROFILE environment variable, a profile
// beneath XDG_DATA_HOME, then a profile beneath the user's home directory. It
// only resolves a path; creation and locking are deferred until launch.
func resolveProfileDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if value := os.Getenv("NYALA_PROFILE"); value != "" {
		return value, nil
	}
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "nyala", "profile"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve profile directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "nyala", "profile"), nil
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
	profileDir            string
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
// without a proxy adds no flag. When cfg.profileDir is non-empty it uses that
// resolved directory; empty preserves chromedp's temporary profile behaviour.
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
// It disables Blink's AutomationControlled feature and overrides chromedp's
// default enable-automation flag, so pages do not see navigator.webdriver as
// true. The false value emits no --enable-automation argument.
// It always adds --disable-smooth-scrolling: Chromium animates a keyboard
// scroll over about 150 ms, which a terminal pays for in frames, and the caret
// keys read the page's settled scroll offset back in the same dispatch that
// pressed the key, so an animated scroll would be read part way through.
func newAllocator(parent context.Context, cfg allocatorConfig) (context.Context, context.CancelFunc) {
	opts := []chromedp.ExecAllocatorOption{chromedp.ExecPath(cfg.execPath)}
	if cfg.profileDir != "" {
		opts = append(opts, chromedp.UserDataDir(cfg.profileDir))
	}
	for _, f := range launchFlags(cfg) {
		opts = append(opts, chromedp.Flag(f.name, f.value))
	}
	return chromedp.NewExecAllocator(parent, opts...)
}

// launchFlag is one Chromium command-line switch. value is a bool or a string,
// the two types chromedp.Flag accepts: a bool true emits --name, a bool false
// emits nothing, and a string emits --name=value.
type launchFlag struct {
	name  string
	value any
}

// chromedpDefaultFlags replicates chromedp.DefaultExecAllocatorOptions as a
// readable list, including the three switches chromedp.Headless sets. chromedp
// stores flags in an unexported map, so Nyala keeps its own copy to build argv
// for the detached shared browser from the same set the exec path uses.
var chromedpDefaultFlags = []launchFlag{
	{"no-first-run", true},
	{"no-default-browser-check", true},
	{"headless", true},
	{"hide-scrollbars", true},
	{"mute-audio", true},
	{"disable-background-networking", true},
	{"enable-features", "NetworkService,NetworkServiceInProcess"},
	{"disable-background-timer-throttling", true},
	{"disable-backgrounding-occluded-windows", true},
	{"disable-breakpad", true},
	{"disable-client-side-phishing-detection", true},
	{"disable-default-apps", true},
	{"disable-dev-shm-usage", true},
	{"disable-extensions", true},
	{"disable-features", "site-per-process,Translate,BlinkGenPropertyTrees"},
	{"disable-hang-monitor", true},
	{"disable-ipc-flooding-protection", true},
	{"disable-popup-blocking", true},
	{"disable-prompt-on-repost", true},
	{"disable-renderer-backgrounding", true},
	{"disable-sync", true},
	{"force-color-profile", "srgb"},
	{"metrics-recording-only", true},
	{"safebrowsing-disable-auto-update", true},
	{"enable-automation", true},
	{"password-store", "basic"},
	{"use-mock-keychain", true},
}

// launchFlags returns the full Chromium switch list for cfg: the chromedp
// defaults followed by Nyala's own switches, deduplicated by name with the last
// write winning, as the chromedp flag map does. So enable-automation false and
// mute-audio false replace the defaults and emit nothing. It excludes exec-path
// and user-data-dir, which each launch path sets itself. The result is pure, so
// both newAllocator and sharedArgv build from one source.
func launchFlags(cfg allocatorConfig) []launchFlag {
	flags := append([]launchFlag(nil), chromedpDefaultFlags...)
	flags = append(flags,
		launchFlag{"window-size", fmt.Sprintf("%d,%d", cfg.surfaceWidth, cfg.surfaceHeight)},
		launchFlag{"force-device-scale-factor", strconv.FormatFloat(cfg.scale, 'g', -1, 64)},
		launchFlag{"disable-blink-features", "AutomationControlled"},
		launchFlag{"enable-automation", false},
		launchFlag{"disable-smooth-scrolling", true},
	)
	for _, name := range gpuFlags(cfg.enableGPU) {
		flags = append(flags, launchFlag{name, true})
	}
	if cfg.enableAudio {
		flags = append(flags, launchFlag{"mute-audio", false})
		// Chromium blocks audible autoplay until a user gesture. Nyala has no input
		// yet, so audible media on a page would never start. Allow gesture-free
		// autoplay so it plays on load. Muted autoplay is already allowed, so this
		// pairs with the unmute above and rides the same live-path gate.
		flags = append(flags, launchFlag{"autoplay-policy", "no-user-gesture-required"})
	}
	for _, name := range frameRateLimitFlags(cfg.disableFrameRateLimit) {
		flags = append(flags, launchFlag{name, true})
	}
	for _, pair := range proxyFlags(cfg.proxy) {
		flags = append(flags, launchFlag{pair[0], pair[1]})
	}
	index := make(map[string]int, len(flags))
	out := flags[:0]
	for _, f := range flags {
		if i, seen := index[f.name]; seen {
			out[i].value = f.value
			continue
		}
		index[f.name] = len(out)
		out = append(out, f)
	}
	return out
}

// sharedArgv converts flags to the argv for a detached shared browser, in the
// order chromedp.ExecAllocator.Allocate emits it: the flags, then
// --user-data-dir=profileDir, --no-sandbox when running as root,
// --remote-debugging-port=0, --keep-alive-for-test so the browser survives its
// last closed target, then about:blank. A bool true flag emits --name, a bool
// false emits nothing, and any other value emits --name=value.
func sharedArgv(flags []launchFlag, profileDir string) []string {
	argv := make([]string, 0, len(flags)+5)
	for _, f := range flags {
		switch v := f.value.(type) {
		case bool:
			if v {
				argv = append(argv, "--"+f.name)
			}
		default:
			argv = append(argv, fmt.Sprintf("--%s=%v", f.name, v))
		}
	}
	argv = append(argv, "--user-data-dir="+profileDir)
	if os.Getuid() == 0 {
		argv = append(argv, "--no-sandbox")
	}
	argv = append(argv, "--remote-debugging-port=0", "--keep-alive-for-test", "about:blank")
	return argv
}

// spawnDetached starts execPath with argv in a new session so the browser
// outlives this process: it sets Setsid and, on purpose, no Pdeathsig, unlike
// chromedp's exec allocator. stdin, stdout, and stderr are opened on os.DevNull.
// The caller owns the returned process and must call Process.Release or Wait.
func spawnDetached(execPath string, argv []string) (*os.Process, error) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()
	cmd := exec.Command(execPath, argv...) //nolint:noctx // detached on purpose: no context may kill the shared browser
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// devToolsActivePortFile is the file Chromium writes into its user data
// directory once --remote-debugging-port=0 has bound a port.
const devToolsActivePortFile = "DevToolsActivePort"

// devToolsActivePortPath returns the DevToolsActivePort path under profileDir.
func devToolsActivePortPath(profileDir string) string {
	return filepath.Join(profileDir, devToolsActivePortFile)
}

// parseDevToolsActivePort parses DevToolsActivePort contents: line one is the
// bound port and line two is the /devtools/browser/<id> path. It returns the
// browser websocket URL ws://127.0.0.1:<port><path>, or an error when the file
// is empty, the port is not a number, or the second line is missing. Chromium
// writes the file in two steps, so a caller polls until the parse succeeds.
func parseDevToolsActivePort(data []byte) (string, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("DevToolsActivePort: incomplete file %q", data)
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return "", fmt.Errorf("DevToolsActivePort: bad port %q", lines[0])
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", port, strings.TrimSpace(lines[1])), nil
}

// launchPollInterval is the delay between checks while waiting for the shared
// browser to write DevToolsActivePort or to exit.
const launchPollInterval = 50 * time.Millisecond

// waitDevToolsActivePort polls the DevToolsActivePort file under profileDir
// until it parses to a websocket URL or ctx ends. It returns ctx.Err on timeout.
func waitDevToolsActivePort(ctx context.Context, profileDir string) (string, error) {
	path := devToolsActivePortPath(profileDir)
	ticker := time.NewTicker(launchPollInterval)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil {
			if wsURL, perr := parseDevToolsActivePort(data); perr == nil {
				return wsURL, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// connectFunc is the signature of connectShared, so a test can replace the
// websocket dial through the engine's connectSharedOp hook.
type connectFunc func(parent context.Context, wsURL string, timeout time.Duration) (context.Context, context.CancelFunc, error)

// connectShared dials the shared browser at wsURL through a chromedp remote
// allocator and proves the socket with Browser.getVersion bounded by timeout.
// NoModifyURL keeps the exact URL DevToolsActivePort gave, so chromedp does not
// rewrite it through /json/version. It returns a holder context that carries the
// Browser but no target, and a cancel that cancels that context then the
// allocator. On error it cancels both and returns the error.
//
// It allocates the Browser directly, the way chromedp's initContextBrowser
// does, instead of through chromedp.Run: under a remote allocator the first Run
// on a context without a target id creates an about:blank tab in some existing
// window, which could foreground over another client's page.
func connectShared(parent context.Context, wsURL string, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(parent, wsURL, chromedp.NoModifyURL)
	holder, holderCancel := chromedp.NewContext(allocCtx)
	cancel := func() {
		holderCancel()
		allocCancel()
	}
	c := chromedp.FromContext(holder)
	b, err := c.Allocator.Allocate(holder, chromedp.WithDialTimeout(timeout))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("connect shared browser %s: %w", wsURL, err)
	}
	c.Browser = b
	probeCtx, probeCancel := context.WithTimeout(holder, timeout)
	defer probeCancel()
	if _, _, _, _, _, err := browser.GetVersion().Do(cdp.WithExecutor(probeCtx, b)); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("connect shared browser %s: %w", wsURL, err)
	}
	return holder, cancel, nil
}

// newInstanceTarget creates this instance's page target in its own window on
// the holder's Browser and returns a child context attached to it. The child
// inherits the Browser, so its first chromedp.Run attaches to the target and
// creates nothing. A separate window keeps Browser.setContentsSize on one
// instance from resizing another.
func newInstanceTarget(holder context.Context) (context.Context, context.CancelFunc, error) {
	b := chromedp.FromContext(holder).Browser
	id, err := target.CreateTarget("about:blank").WithNewWindow(true).Do(cdp.WithExecutor(holder, b))
	if err != nil {
		return nil, nil, fmt.Errorf("create instance target: %w", err)
	}
	ctx, cancel := chromedp.NewContext(holder, chromedp.WithTargetID(id))
	return ctx, cancel, nil
}

// terminateBrowser sends SIGTERM to pid and polls with a null signal until the
// process is gone or ctx ends. An initial ESRCH counts as success, since the
// browser has already exited. Chromium refuses Browser.close from a remote
// client, so the last client terminates the shared browser this way.
func terminateBrowser(ctx context.Context, pid int) error {
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return fmt.Errorf("terminate browser %d: %w", pid, err)
	}
	ticker := time.NewTicker(launchPollInterval)
	defer ticker.Stop()
	for {
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("terminate browser %d: still alive: %w", pid, ctx.Err())
		case <-ticker.C:
		}
	}
}

// browserOwnsProfile reports whether pid is a Chromium running on profileDir.
// A pid read from the lock file can outlive its browser, and the kernel can
// hand the number to an unrelated process, so every signal to such a pid is
// gated on the process command line naming the profile. Linux exposes the
// command line in /proc; elsewhere ps prints it.
func browserOwnsProfile(ctx context.Context, pid int, profileDir string) bool {
	want := "--user-data-dir=" + profileDir
	if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil {
		return strings.Contains(string(data), want)
	}
	//nolint:gosec // ps receives a pid that Nyala formatted from an integer.
	out, err := exec.CommandContext(ctx, "ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.Contains(string(out), want)
}

// terminateOwnedBrowser is terminateBrowser gated on browserOwnsProfile. It is
// the terminate step of sharedCloseOp, whose pid comes from the lock file.
func terminateOwnedBrowser(ctx context.Context, pid int, profileDir string) error {
	if !browserOwnsProfile(ctx, pid, profileDir) {
		return nil
	}
	return terminateBrowser(ctx, pid)
}

// stopBrowser terminates pid with SIGTERM bounded by browserExitTimeout, then
// SIGKILL when it is still alive. launchShared uses it on a browser that
// answers no connection, or that it spawned and then failed to reach, because a
// fresh spawn on the same profile would collide with Chromium's ProcessSingleton.
// A pid whose command line does not name profileDir is left alone.
func stopBrowser(pid int, profileDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), browserExitTimeout)
	defer cancel()
	if !browserOwnsProfile(ctx, pid, profileDir) {
		return
	}
	if err := terminateBrowser(ctx, pid); err != nil && browserOwnsProfile(ctx, pid, profileDir) {
		_ = unix.Kill(pid, unix.SIGKILL)
	}
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

// Shared-browser launch budgets. devToolsWaitTimeout is a var so a launch test
// with a fake browser that never writes DevToolsActivePort can shorten it.
const (
	// maxLaunchAttempts caps the lock, spawn or connect, and attach cycle.
	maxLaunchAttempts = 3
	// adoptConnectTimeout bounds the probe of a browser a previous launcher left
	// running. A live pid that answers no connection is treated as wedged.
	adoptConnectTimeout = 2 * time.Second
	// sharedConnectTimeout bounds the dial and version probe of a browser this
	// launcher spawned or another launcher published.
	sharedConnectTimeout = 10 * time.Second
	// sharedLockTimeout bounds a client's wait for the launcher to downgrade.
	sharedLockTimeout = 60 * time.Second
	// browserExitTimeout bounds stopBrowser's SIGTERM wait before SIGKILL.
	browserExitTimeout = 10 * time.Second
)

var devToolsWaitTimeout = 30 * time.Second

// launchPrivate builds the exec allocator and first chromedp context for a
// private (temporary profile) engine. chromedp owns the process: its Cancel on
// the first context sends Browser.close, and the allocator removes the
// temporary user data directory.
func launchPrivate(cfg allocatorConfig) (context.Context, context.CancelFunc, func(context.Context) error) {
	allocCtx, allocCancel := newAllocator(context.Background(), cfg)
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
	return ctx, cancel, closeBrowser
}

// launchShared connects this engine to the profile's shared Chromium, spawning
// it when no other client has. Each attempt takes the lock in the right mode
// through attachShared, then creates this instance's own target with
// newInstanceTarget. A failed attempt unlocks the guard and retries, capped at
// maxLaunchAttempts. It returns the target context, a raw cancel that cancels the
// target then the websocket, and the sharedCloseOp graceful close.
func (e *ChromiumEngine) launchShared(cfg allocatorConfig) (context.Context, context.CancelFunc, func(context.Context) error, error) {
	if err := e.openProfileGuard(); err != nil {
		return nil, nil, nil, err
	}
	e.mu.Lock()
	guard := e.profileGuard
	connect := e.connectSharedOp
	e.mu.Unlock()
	if connect == nil {
		connect = connectShared
	}
	var errs []error
	for attempt := range maxLaunchAttempts {
		if attempt > 0 {
			time.Sleep(launchPollInterval)
		}
		holder, closeSocket, err := attachShared(guard, cfg, connect)
		if err == nil {
			var targetCtx context.Context
			var targetCancel context.CancelFunc
			targetCtx, targetCancel, err = newInstanceTarget(holder)
			if err == nil {
				cancel := func() {
					targetCancel()
					closeSocket()
				}
				closeTarget := func(context.Context) error { return chromedp.Cancel(targetCtx) }
				terminate := func(ctx context.Context, pid int) error {
					return terminateOwnedBrowser(ctx, pid, cfg.profileDir)
				}
				return targetCtx, cancel, sharedCloseOp(guard, closeTarget, terminate, closeSocket), nil
			}
			closeSocket()
		}
		errs = append(errs, err, guard.unlock())
	}
	return nil, nil, nil, fmt.Errorf("launch shared browser after %d attempts: %w", maxLaunchAttempts, errors.Join(errs...))
}

// attachShared runs one lock-and-connect attempt. The LOCK_EX winner is the
// launcher: it adopts or spawns the browser through spawnShared, then downgrades
// to LOCK_SH so waiting clients proceed. Every other engine waits for LOCK_SH,
// reads DevToolsActivePort, and connects. It returns the holder context and its
// socket cancel. On error the caller unlocks and retries, so a client whose
// launcher died, or whose browser is gone, becomes the launcher itself.
func attachShared(guard *profileGuard, cfg allocatorConfig, connect connectFunc) (context.Context, context.CancelFunc, error) {
	exclusive, err := guard.tryExclusive()
	if err != nil {
		return nil, nil, err
	}
	if exclusive {
		holder, closeSocket, err := spawnShared(guard, cfg, connect)
		if err != nil {
			return nil, nil, err
		}
		if err := guard.downgrade(); err != nil {
			closeSocket()
			return nil, nil, err
		}
		return holder, closeSocket, nil
	}
	lockCtx, stopLock := context.WithTimeout(context.Background(), sharedLockTimeout)
	defer stopLock()
	if err := guard.acquireShared(lockCtx); err != nil {
		return nil, nil, err
	}
	wsURL, err := readDevToolsActivePort(cfg.profileDir)
	if err != nil {
		return nil, nil, err
	}
	return connect(context.Background(), wsURL, sharedConnectTimeout)
}

// readDevToolsActivePort reads and parses the DevToolsActivePort file under
// profileDir once, without polling.
func readDevToolsActivePort(profileDir string) (string, error) {
	data, err := os.ReadFile(devToolsActivePortPath(profileDir))
	if err != nil {
		return "", err
	}
	return parseDevToolsActivePort(data)
}

// spawnShared runs under LOCK_EX. When the lock file names a live pid that
// still answers on its DevToolsActivePort, it adopts that browser and spawns
// nothing. A live pid that answers no connection is wedged or still shutting
// down, so it is stopped first: a fresh spawn on the same profile would collide
// with Chromium's ProcessSingleton. It then purges the tab-restore state,
// removes the stale port file, spawns the detached browser, records its pid,
// waits for the port file, and connects. On failure it stops the process it
// spawned. A goroutine reaps the child, so a null-signal liveness check on the
// pid stays truthful after the browser exits.
func spawnShared(guard *profileGuard, cfg allocatorConfig, connect connectFunc) (context.Context, context.CancelFunc, error) {
	profileDir := cfg.profileDir
	if pid, ok := guard.readPID(); ok && unix.Kill(pid, 0) == nil {
		if wsURL, err := readDevToolsActivePort(profileDir); err == nil {
			if holder, closeSocket, err := connect(context.Background(), wsURL, adoptConnectTimeout); err == nil {
				return holder, closeSocket, nil
			}
		}
		stopBrowser(pid, profileDir)
	}
	if err := purgeSessionState(profileDir); err != nil {
		return nil, nil, err
	}
	if err := os.Remove(devToolsActivePortPath(profileDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("remove stale %s: %w", devToolsActivePortFile, err)
	}
	proc, err := spawnDetached(cfg.execPath, sharedArgv(launchFlags(cfg), profileDir))
	if err != nil {
		return nil, nil, fmt.Errorf("spawn shared browser: %w", err)
	}
	pid := proc.Pid
	go func() { _, _ = proc.Wait() }()
	if err := guard.writePID(pid); err != nil {
		stopBrowser(pid, profileDir)
		return nil, nil, err
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), devToolsWaitTimeout)
	wsURL, err := waitDevToolsActivePort(waitCtx, profileDir)
	stopWait()
	if err != nil {
		stopBrowser(pid, profileDir)
		return nil, nil, fmt.Errorf("wait for %s: %w", devToolsActivePortFile, err)
	}
	holder, closeSocket, err := connect(context.Background(), wsURL, sharedConnectTimeout)
	if err != nil {
		stopBrowser(pid, profileDir)
		return nil, nil, err
	}
	return holder, closeSocket, nil
}

// sharedCloseOp builds the graceful close for a shared-browser session. It
// closes this instance's target, then tries LOCK_EX. Success means this is the
// last client, so it terminates the browser pid the launcher stored in the lock
// file, bounded by ctx, which carries the deadline teardownSession applies.
// Failure means another client lives, so the browser stays. The websocket
// closes before the terminate, so chromedp never logs a write on a socket the
// dying browser reset. It returns the first error. The funcs are parameters so
// a test can drive the decision without a browser.
func sharedCloseOp(guard *profileGuard, closeTarget func(context.Context) error, terminate func(context.Context, int) error, closeSocket func()) func(context.Context) error {
	return func(ctx context.Context) error {
		err := closeTarget(ctx)
		last, lockErr := guard.tryExclusive()
		if err == nil {
			err = lockErr
		}
		closeSocket()
		if last {
			if pid, ok := guard.readPID(); ok {
				if terr := terminate(ctx, pid); err == nil {
					err = terr
				}
			}
		}
		return err
	}
}

// launch resolves the executable from flag, then builds the session context. A
// private engine (empty profileDir) takes launchPrivate and owns its Chromium
// through chromedp. A persistent profile takes launchShared: one Chromium per
// profile, one target in its own window per engine. Both paths then fetch the
// window id, enable lifecycle events, and size the layout viewport and the
// captured surface together. It stores the context, its cancel, the graceful
// close, and the window id on the engine.
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
	profileDir := e.profileDir
	e.mu.Unlock()
	cfg := allocatorConfig{
		execPath:              execPath,
		profileDir:            profileDir,
		surfaceWidth:          surfW,
		surfaceHeight:         surfH,
		scale:                 z,
		proxy:                 e.proxy,
		enableGPU:             true,
		enableAudio:           audioAvailable(),
		disableFrameRateLimit: disableFrameRateLimit,
	}
	var (
		ctx          context.Context
		cancel       context.CancelFunc
		closeBrowser func(context.Context) error
	)
	if profileDir == "" {
		ctx, cancel, closeBrowser = launchPrivate(cfg)
	} else if ctx, cancel, closeBrowser, err = e.launchShared(cfg); err != nil {
		return err
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
	return enableProxyAuth(ctx, e.proxy)
}

func enableProxyAuth(ctx context.Context, cfg ProxyConfig) error {
	if !cfg.HasCredentials {
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
